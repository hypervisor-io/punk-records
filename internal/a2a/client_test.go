package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCardURL(t *testing.T) {
	cases := map[string]string{
		"https://agent.example":                             "https://agent.example/.well-known/agent-card.json",
		"https://agent.example/":                            "https://agent.example/.well-known/agent-card.json",
		"https://agent.example/.well-known/agent-card.json": "https://agent.example/.well-known/agent-card.json",
		"https://agent.example/custom.json":                 "https://agent.example/custom.json",
	}
	for in, want := range cases {
		if got := CardURL(in); got != want {
			t.Errorf("CardURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFetchCard(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(AgentCard{
			ProtocolVersion: ProtocolVersion, Name: "remote", URL: "http://x/rpc",
			Capabilities: map[string]bool{"streaming": true},
			Skills:       []AgentSkill{{ID: "s1", Name: "Skill"}},
		})
	}))
	defer srv.Close()

	card, err := FetchCard(context.Background(), nil, srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/.well-known/agent-card.json" || gotAuth != "Bearer tok" {
		t.Fatalf("path %q auth %q", gotPath, gotAuth)
	}
	if card.Name != "remote" || !card.Capabilities["streaming"] || len(card.Skills) != 1 {
		t.Fatalf("card: %+v", card)
	}
}

func TestFetchCardErrors(t *testing.T) {
	t.Run("non-200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()
		if _, err := FetchCard(context.Background(), nil, srv.URL, ""); err == nil || err.Error() != "fetch card: http 404" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("bad json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "{")
		}))
		defer srv.Close()
		if _, err := FetchCard(context.Background(), nil, srv.URL, ""); err == nil {
			t.Fatal("expected decode error")
		}
	})
}

// rpcServer answers JSON-RPC posts with handler(method, params).
func rpcServer(t *testing.T, handler func(method string, params json.RawMessage) (any, *RPCError)) (*httptest.Server, *[]Request) {
	t.Helper()
	var seen []Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("bad request shape: %s %s", r.Method, r.Header.Get("Content-Type"))
		}
		var req Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		seen = append(seen, req)
		result, rpcErr := handler(req.Method, req.Params)
		_ = json.NewEncoder(w).Encode(Response{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rpcErr})
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestClientSendMessage(t *testing.T) {
	srv, seen := rpcServer(t, func(method string, params json.RawMessage) (any, *RPCError) {
		var p SendMessageParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &RPCError{Code: ErrInvalidParams, Message: err.Error()}
		}
		return Task{Kind: KindTask, ID: "remote-1", Status: TaskStatus{State: StateSubmitted},
			Metadata: map[string]any{"echo": p.Message.Text(), "blocking": p.Configuration != nil && p.Configuration.Blocking}}, nil
	})
	c := NewClient(srv.URL, "secret")

	got, err := c.SendMessage(context.Background(), TextMessage("do it"), &SendMessageConfig{Blocking: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "remote-1" || got.Status.State != StateSubmitted {
		t.Fatalf("task: %+v", got)
	}
	if got.Metadata["echo"] != "do it" || got.Metadata["blocking"] != true {
		t.Fatalf("params did not round-trip: %+v", got.Metadata)
	}
	if len(*seen) != 1 || (*seen)[0].Method != MethodMessageSend || (*seen)[0].JSONRPC != "2.0" {
		t.Fatalf("requests: %+v", *seen)
	}
}

func TestClientGetAndCancel(t *testing.T) {
	srv, seen := rpcServer(t, func(method string, params json.RawMessage) (any, *RPCError) {
		switch method {
		case MethodTasksGet:
			var p TaskQueryParams
			_ = json.Unmarshal(params, &p)
			return Task{ID: p.ID, Metadata: map[string]any{"historyLength": float64(p.HistoryLength)}}, nil
		case MethodTasksCancel:
			var p TaskIDParams
			_ = json.Unmarshal(params, &p)
			return Task{ID: p.ID, Status: TaskStatus{State: StateCanceled}}, nil
		}
		return nil, &RPCError{Code: ErrMethodNotFound, Message: "no"}
	})
	c := NewClient(srv.URL, "")

	got, err := c.GetTask(context.Background(), "t-9", 5)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "t-9" || got.Metadata["historyLength"] != float64(5) {
		t.Fatalf("get: %+v", got)
	}
	canceled, err := c.CancelTask(context.Background(), "t-9")
	if err != nil {
		t.Fatal(err)
	}
	if canceled.Status.State != StateCanceled {
		t.Fatalf("cancel: %+v", canceled)
	}
	if (*seen)[0].Method != MethodTasksGet || (*seen)[1].Method != MethodTasksCancel {
		t.Fatalf("methods: %+v", *seen)
	}
}

func TestClientBearerHeader(t *testing.T) {
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(Response{JSONRPC: "2.0", Result: Task{ID: "x"}})
	}))
	defer srv.Close()

	if _, err := NewClient(srv.URL, "tok").GetTask(context.Background(), "x", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := NewClient(srv.URL, "").GetTask(context.Background(), "x", 0); err != nil {
		t.Fatal(err)
	}
	if len(auths) != 2 || auths[0] != "Bearer tok" || auths[1] != "" {
		t.Fatalf("auth headers: %q", auths)
	}
}

func TestClientSurfacesRPCError(t *testing.T) {
	srv, _ := rpcServer(t, func(method string, params json.RawMessage) (any, *RPCError) {
		return nil, &RPCError{Code: ErrTaskNotFound, Message: "task not found"}
	})
	_, err := NewClient(srv.URL, "").GetTask(context.Background(), "nope", 0)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != ErrTaskNotFound || err.Error() != "task not found" {
		t.Fatalf("err = %v", err)
	}
}

func TestClientHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "")
	if _, err := c.GetTask(context.Background(), "x", 0); err == nil || err.Error() != "tasks/get: http 502" {
		t.Fatalf("call err = %v", err)
	}
	if err := c.StreamMessage(context.Background(), TextMessage("x"), nil, nil); err == nil || err.Error() != "stream: http 502" {
		t.Fatalf("stream err = %v", err)
	}
}

func TestClientFetchCardDefaultsToEndpoint(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewEncoder(w).Encode(AgentCard{Name: "n"})
	}))
	defer srv.Close()
	card, err := NewClient(srv.URL, "").FetchCard(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if card.Name != "n" || path != "/.well-known/agent-card.json" {
		t.Fatalf("card %+v path %q", card, path)
	}
}

func TestClientStreamMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		var req Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method != MethodMessageStream {
			t.Errorf("method = %q", req.Method)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		frame := func(v any) {
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": v})
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
		}
		frame(Task{Kind: KindTask, ID: "s-1", Status: TaskStatus{State: StateSubmitted}})
		fmt.Fprint(w, ": keepalive comment\n\n")
		fmt.Fprint(w, "data: not json\n\n")
		frame(ArtifactUpdateEvent{Kind: KindArtifactUpdate, TaskID: "s-1", Artifact: Artifact{ArtifactID: "a1"}})
		frame(StatusUpdateEvent{Kind: KindStatusUpdate, TaskID: "s-1", Status: TaskStatus{State: StateWorking}})
		frame(StatusUpdateEvent{Kind: KindStatusUpdate, TaskID: "s-1", Status: TaskStatus{State: StateCompleted}, Final: true})
		// Anything after the final frame must be ignored by the client.
		frame(StatusUpdateEvent{Kind: KindStatusUpdate, TaskID: "s-1", Status: TaskStatus{State: StateFailed}})
	}))
	defer srv.Close()

	var kinds []string
	err := NewClient(srv.URL, "").StreamMessage(context.Background(), TextMessage("go"), nil, func(kind string, raw json.RawMessage) error {
		kinds = append(kinds, kind)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{KindTask, KindArtifactUpdate, KindStatusUpdate, KindStatusUpdate}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
}

func TestClientStreamMessageCallbackErrorStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 3; i++ {
			b, _ := json.Marshal(map[string]any{"result": StatusUpdateEvent{Kind: KindStatusUpdate}})
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
	}))
	defer srv.Close()
	stop := errors.New("stop")
	calls := 0
	err := NewClient(srv.URL, "").StreamMessage(context.Background(), TextMessage("go"), nil, func(string, json.RawMessage) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("err = %v calls = %d", err, calls)
	}
}

func TestClientStreamMessageRPCErrorFrame(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `data: {"jsonrpc":"2.0","error":{"code":-32001,"message":"task not found"}}`+"\n\n")
	}))
	defer srv.Close()
	err := NewClient(srv.URL, "").StreamMessage(context.Background(), TextMessage("go"), nil, nil)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != ErrTaskNotFound {
		t.Fatalf("err = %v", err)
	}
}

func TestClientStreamMessageEndsCleanlyOnServerClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := json.Marshal(map[string]any{"result": StatusUpdateEvent{Kind: KindStatusUpdate}})
		fmt.Fprintf(w, "data: %s\n\n", b)
	}))
	defer srv.Close()
	if err := NewClient(srv.URL, "").StreamMessage(context.Background(), TextMessage("go"), nil, nil); err != nil {
		t.Fatalf("non-final stream close should not error, got %v", err)
	}
}
