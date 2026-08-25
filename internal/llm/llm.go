// Package llm is the provider hook: named OpenAI-compatible profiles
// (base_url + api_key env + model). ai.enabled=false swaps a client that
// fails closed with ErrAIDisabled; the coordination plane never depends
// on a model being reachable.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// ErrAIDisabled is the typed refusal when ai.enabled=false.
var ErrAIDisabled = errors.New("llm: ai is disabled")

// Profile is one named endpoint configuration.
type Profile struct {
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"` // env var HOLDING the key; never the key itself
	Model     string `yaml:"model"`
}

// Tool is a function the model may call.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage // JSON Schema for arguments
}

// ToolCall is the model asking for one tool invocation.
type ToolCall struct {
	ID   string
	Name string
	Args json.RawMessage
}

// Turn is one message of the conversation we maintain.
type Turn struct {
	Role       string // system | user | assistant | tool
	Content    string
	ToolCalls  []ToolCall // assistant turns only
	ToolCallID string     // tool turns only
}

// Result is one model response plus usage.
type Result struct {
	Content          string
	ToolCalls        []ToolCall
	PromptTokens     int
	CompletionTokens int
}

// Client is the minimal chat surface the runtime needs.
type Client interface {
	Chat(ctx context.Context, turns []Turn, tools []Tool) (*Result, error)
	Model() string
}

// Manager resolves profiles to clients and records every call (including
// failures) into llm_calls for cost governance.
type Manager struct {
	Enabled  bool
	profiles map[string]Profile
	db       *store.DB
	now      func() time.Time
	Prices   *PriceTable // nil disables local cost rating
}

func NewManager(enabled bool, profiles map[string]Profile, db *store.DB, now func() time.Time) *Manager {
	if now == nil {
		now = time.Now
	}
	return &Manager{Enabled: enabled, profiles: profiles, db: db, now: now}
}

// Client returns a recording client for the named profile. Disabled AI
// yields a client whose Chat always returns ErrAIDisabled (no llm_calls
// row: a short-circuit is not a call).
func (m *Manager) Client(profile string) (Client, error) {
	if profile == "" {
		profile = "default"
	}
	if !m.Enabled {
		return disabledClient{}, nil
	}
	p, ok := m.profiles[profile]
	if !ok {
		return nil, fmt.Errorf("llm: unknown profile %q", profile)
	}
	if p.Model == "" {
		return nil, fmt.Errorf("llm: profile %q has no model", profile)
	}
	opts := []option.RequestOption{}
	if p.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(p.BaseURL))
	}
	if p.APIKeyEnv != "" {
		key := os.Getenv(p.APIKeyEnv)
		if key == "" {
			return nil, fmt.Errorf("llm: profile %q: env %s is empty", profile, p.APIKeyEnv)
		}
		opts = append(opts, option.WithAPIKey(key))
	}
	oc := openai.NewClient(opts...)
	return &recordingClient{m: m, profile: profile, model: p.Model, c: &oc}, nil
}

type disabledClient struct{}

func (disabledClient) Chat(context.Context, []Turn, []Tool) (*Result, error) {
	return nil, ErrAIDisabled
}
func (disabledClient) Model() string { return "disabled" }

type recordingClient struct {
	m       *Manager
	profile string
	model   string
	c       *openai.Client
}

func (r *recordingClient) Model() string { return r.model }

// TaskIDKey threads the current task id into llm_calls rows.
type ctxKey struct{}
type agentKey struct{}

// WithAgent tags a context with the calling agent for cost attribution.
func WithAgent(ctx context.Context, agent string) context.Context {
	return context.WithValue(ctx, agentKey{}, agent)
}

func agentFrom(ctx context.Context) string {
	if v, ok := ctx.Value(agentKey{}).(string); ok {
		return v
	}
	return ""
}

// WithTaskID tags a context so the call ledger can attribute spend.
func WithTaskID(ctx context.Context, taskID string) context.Context {
	return context.WithValue(ctx, ctxKey{}, taskID)
}

func taskIDFrom(ctx context.Context) any {
	if v, ok := ctx.Value(ctxKey{}).(string); ok && v != "" {
		return v
	}
	return nil
}

func (r *recordingClient) Chat(ctx context.Context, turns []Turn, tools []Tool) (*Result, error) {
	msgs := make([]openai.ChatCompletionMessageParamUnion, 0, len(turns))
	for _, t := range turns {
		switch t.Role {
		case "system":
			msgs = append(msgs, openai.SystemMessage(t.Content))
		case "user":
			msgs = append(msgs, openai.UserMessage(t.Content))
		case "assistant":
			if len(t.ToolCalls) > 0 {
				calls := make([]openai.ChatCompletionMessageToolCallUnionParam, 0, len(t.ToolCalls))
				for _, tc := range t.ToolCalls {
					calls = append(calls, openai.ChatCompletionMessageToolCallUnionParam{
						OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
							ID: tc.ID,
							Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
								Name:      tc.Name,
								Arguments: string(tc.Args),
							},
						},
					})
				}
				asst := openai.ChatCompletionAssistantMessageParam{ToolCalls: calls}
				if t.Content != "" {
					asst.Content.OfString = openai.String(t.Content)
				}
				msgs = append(msgs, openai.ChatCompletionMessageParamUnion{OfAssistant: &asst})
			} else {
				msgs = append(msgs, openai.AssistantMessage(t.Content))
			}
		case "tool":
			msgs = append(msgs, openai.ToolMessage(t.Content, t.ToolCallID))
		default:
			return nil, fmt.Errorf("llm: unknown role %q", t.Role)
		}
	}

	params := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(r.model),
		Messages: msgs,
	}
	for _, tl := range tools {
		var schema map[string]any
		if len(tl.Schema) > 0 {
			if err := json.Unmarshal(tl.Schema, &schema); err != nil {
				return nil, fmt.Errorf("llm: tool %s schema: %w", tl.Name, err)
			}
		}
		params.Tools = append(params.Tools, openai.ChatCompletionFunctionTool(
			openai.FunctionDefinitionParam{
				Name:        tl.Name,
				Description: openai.String(tl.Description),
				Parameters:  schema,
			}))
	}

	start := r.m.now()
	resp, err := r.c.Chat.Completions.New(ctx, params)
	latency := r.m.now().Sub(start).Milliseconds()

	res := &Result{}
	var callErr any
	if err != nil {
		callErr = err.Error()
	} else if len(resp.Choices) == 0 {
		err = errors.New("llm: empty choices")
		callErr = err.Error()
	} else {
		choice := resp.Choices[0]
		res.Content = choice.Message.Content
		for _, tc := range choice.Message.ToolCalls {
			res.ToolCalls = append(res.ToolCalls, ToolCall{
				ID:   tc.ID,
				Name: tc.Function.Name,
				Args: json.RawMessage(tc.Function.Arguments),
			})
		}
		res.PromptTokens = int(resp.Usage.PromptTokens)
		res.CompletionTokens = int(resp.Usage.CompletionTokens)
	}

	if r.m.db != nil {
		var cost int64
		if r.m.Prices != nil {
			cost, _ = r.m.Prices.Rate(r.model, res.PromptTokens, res.CompletionTokens)
		}
		_, dbErr := r.m.db.ExecContext(ctx, r.m.db.Rebind(`
			INSERT INTO llm_calls (task_id, profile, model, prompt_tokens, completion_tokens, latency_ms, error, created_at, cost_microusd, agent_name)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`),
			taskIDFrom(ctx), r.profile, r.model,
			res.PromptTokens, res.CompletionTokens, latency, callErr,
			store.TimeToDB(r.m.now()), cost, agentFrom(ctx))
		if dbErr != nil {
			return nil, fmt.Errorf("llm: record call: %w", dbErr)
		}
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}
