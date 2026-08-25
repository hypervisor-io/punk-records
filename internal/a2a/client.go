package a2a

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is an A2A client: Punk Records uses it to delegate work OUT to a
// remote A2A agent. Endpoint is the remote's JSON-RPC URL (the AgentCard's
// "url" field); Token, if set, is sent as a bearer credential.
type Client struct {
	HTTP     *http.Client
	Endpoint string
	Token    string
}

// NewClient returns a client for a remote A2A JSON-RPC endpoint.
func NewClient(endpoint, token string) *Client {
	return &Client{HTTP: &http.Client{Timeout: 60 * time.Second}, Endpoint: endpoint, Token: token}
}

// CardURL derives the well-known Agent Card URL from a base URL. If base
// already points at a JSON file it is returned unchanged.
func CardURL(base string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, ".json") {
		return base
	}
	return base + "/.well-known/agent-card.json"
}

// FetchCard retrieves and parses a remote Agent Card. base may be the host
// root or the full well-known URL.
func FetchCard(ctx context.Context, hc *http.Client, base, token string) (*AgentCard, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, CardURL(base), nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch card: http %d", resp.StatusCode)
	}
	var card AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		return nil, fmt.Errorf("decode card: %w", err)
	}
	return &card, nil
}

// SendMessage delegates a message to the remote agent (message/send) and
// returns the resulting Task.
func (c *Client) SendMessage(ctx context.Context, msg Message, cfg *SendMessageConfig) (*Task, error) {
	var t Task
	if err := c.call(ctx, MethodMessageSend, SendMessageParams{Message: msg, Configuration: cfg}, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// GetTask fetches a remote task by id (tasks/get).
func (c *Client) GetTask(ctx context.Context, id string, historyLength int) (*Task, error) {
	var t Task
	if err := c.call(ctx, MethodTasksGet, TaskQueryParams{ID: id, HistoryLength: historyLength}, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// CancelTask cancels a remote task (tasks/cancel).
func (c *Client) CancelTask(ctx context.Context, id string) (*Task, error) {
	var t Task
	if err := c.call(ctx, MethodTasksCancel, TaskIDParams{ID: id}, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// FetchCard fetches the remote's card using this client's credentials. base
// defaults to the endpoint host when empty.
func (c *Client) FetchCard(ctx context.Context, base string) (*AgentCard, error) {
	if base == "" {
		base = c.Endpoint
	}
	return FetchCard(ctx, c.HTTP, base, c.Token)
}

// StreamMessage delegates a message and streams events (message/stream).
// onEvent receives the discriminating kind ("task", "status-update",
// "artifact-update") and the raw JSON result of each SSE frame. It returns
// when the stream ends (terminal status-update or server close).
func (c *Client) StreamMessage(ctx context.Context, msg Message, cfg *SendMessageConfig, onEvent func(kind string, raw json.RawMessage) error) error {
	body, err := json.Marshal(Request{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: MethodMessageStream,
		Params: mustMarshal(SendMessageParams{Message: msg, Configuration: cfg}),
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stream: http %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var frame struct {
			Result json.RawMessage `json:"result"`
			Error  *RPCError       `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			continue
		}
		if frame.Error != nil {
			return frame.Error
		}
		var disc struct {
			Kind  string `json:"kind"`
			Final bool   `json:"final"`
		}
		_ = json.Unmarshal(frame.Result, &disc)
		if onEvent != nil {
			if err := onEvent(disc.Kind, frame.Result); err != nil {
				return err
			}
		}
		if disc.Kind == KindStatusUpdate && disc.Final {
			return nil
		}
	}
	if err := sc.Err(); err != nil && err != io.EOF {
		return err
	}
	return nil
}

// call performs one JSON-RPC request and unmarshals the result into out.
func (c *Client) call(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(Request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: method, Params: mustMarshal(params)})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: http %d", method, resp.StatusCode)
	}
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rpc); err != nil {
		return fmt.Errorf("%s: decode response: %w", method, err)
	}
	if rpc.Error != nil {
		return rpc.Error
	}
	if out != nil && len(rpc.Result) > 0 {
		return json.Unmarshal(rpc.Result, out)
	}
	return nil
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return b
}

// TextMessage is a convenience constructor for a single-text-part user message.
func TextMessage(text string) Message {
	return Message{Kind: KindMessage, Role: RoleUser, Parts: []Part{{Kind: KindTextPart, Text: text}}}
}
