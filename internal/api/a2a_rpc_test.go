package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/a2a"
	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/registry"
	"github.com/hypervisor-io/punk-records/internal/route"
	"github.com/hypervisor-io/punk-records/internal/store"
	"github.com/hypervisor-io/punk-records/internal/task"
)

// a2aServer wires a Server with ledger, router, bus and db so the full A2A
// surface (streaming, push) is exercisable. Auth is off (Keys nil).
func a2aServer(t *testing.T) *Server {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "a2a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	specDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(specDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(specDir, "agents", "database.md"), []byte(dbAgentSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := registry.New(specDir, db, slog.New(slog.DiscardHandler))
	if err := reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	clk := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { clk = clk.Add(time.Millisecond); return clk }
	eventBus := bus.New()
	ledger := task.NewLedger(db, now)
	ledger.Notify = func(taskID, status string) {
		eventBus.Publish(bus.Event{Kind: "task_status", Key: taskID, Data: map[string]string{"status": status}})
	}
	router := route.New(db, reg, ledger, nil, now)
	s := New(testLogger(), Deps{
		Ledger: ledger, Router: router, Reg: reg, Bus: eventBus, DB: db,
		DefaultBudget: task.Budget{Tokens: 1000, ToolCalls: 10, WallMS: 60000, Subagents: 1},
	})
	s.MountAgentCard("v1.0.0")
	return s
}

// callA2A issues a JSON-RPC request and returns the raw result and any error.
func callA2A(t *testing.T, s *Server, method string, params any) (json.RawMessage, *a2a.RPCError) {
	t.Helper()
	praw, _ := json.Marshal(params)
	body, _ := json.Marshal(a2a.Request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: method, Params: praw})
	req := httptest.NewRequest(http.MethodPost, "/v1/a2a", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: http %d: %s", method, rec.Code, rec.Body.String())
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *a2a.RPCError   `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%s: decode response: %v (%s)", method, err, rec.Body.String())
	}
	return resp.Result, resp.Error
}

func textMessage(text string) a2a.Message {
	return a2a.Message{Parts: []a2a.Part{{Kind: a2a.KindTextPart, Text: text}}}
}

func TestA2AMessageSendAndGet(t *testing.T) {
	s := a2aServer(t)
	res, rerr := callA2A(t, s, a2a.MethodMessageSend, a2a.SendMessageParams{Message: textMessage("investigate the pool leak")})
	if rerr != nil {
		t.Fatalf("send error: %+v", rerr)
	}
	var tk a2a.Task
	if err := json.Unmarshal(res, &tk); err != nil {
		t.Fatal(err)
	}
	if tk.Kind != a2a.KindTask || tk.ID == "" {
		t.Fatalf("bad task: %+v", tk)
	}
	if tk.ContextID == "" {
		t.Fatal("want generated contextId")
	}
	if len(tk.History) != 1 || tk.History[0].Role != a2a.RoleUser {
		t.Fatalf("want 1 user message in history, got %+v", tk.History)
	}
	if tk.Status.State.Terminal() || tk.Status.State == a2a.StateUnknown {
		t.Fatalf("unexpected state %q", tk.Status.State)
	}

	// tasks/get round-trips the same task
	res2, rerr := callA2A(t, s, a2a.MethodTasksGet, a2a.TaskQueryParams{ID: tk.ID})
	if rerr != nil {
		t.Fatalf("get error: %+v", rerr)
	}
	var got a2a.Task
	if err := json.Unmarshal(res2, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != tk.ID {
		t.Fatalf("get returned %q want %q", got.ID, tk.ID)
	}
}

func TestA2AContinueTask(t *testing.T) {
	s := a2aServer(t)
	res, _ := callA2A(t, s, a2a.MethodMessageSend, a2a.SendMessageParams{Message: textMessage("first")})
	var tk a2a.Task
	_ = json.Unmarshal(res, &tk)

	res2, rerr := callA2A(t, s, a2a.MethodMessageSend, a2a.SendMessageParams{
		Message: a2a.Message{TaskID: tk.ID, Parts: []a2a.Part{{Kind: a2a.KindTextPart, Text: "second"}}}})
	if rerr != nil {
		t.Fatalf("continue error: %+v", rerr)
	}
	var tk2 a2a.Task
	_ = json.Unmarshal(res2, &tk2)
	if tk2.ID != tk.ID {
		t.Fatalf("continue made a new task %q != %q", tk2.ID, tk.ID)
	}
	if len(tk2.History) != 2 {
		t.Fatalf("want 2 messages after continue, got %d", len(tk2.History))
	}
}

func TestA2AMethodNotFound(t *testing.T) {
	s := a2aServer(t)
	_, rerr := callA2A(t, s, "does/notExist", map[string]any{})
	if rerr == nil || rerr.Code != a2a.ErrMethodNotFound {
		t.Fatalf("want method-not-found, got %+v", rerr)
	}
}

func TestA2AInvalidParams(t *testing.T) {
	s := a2aServer(t)
	_, rerr := callA2A(t, s, a2a.MethodMessageSend, a2a.SendMessageParams{Message: a2a.Message{}})
	if rerr == nil || rerr.Code != a2a.ErrInvalidParams {
		t.Fatalf("want invalid-params for empty message, got %+v", rerr)
	}
}

func TestA2ATaskNotFound(t *testing.T) {
	s := a2aServer(t)
	_, rerr := callA2A(t, s, a2a.MethodTasksGet, a2a.TaskQueryParams{ID: "nope"})
	if rerr == nil || rerr.Code != a2a.ErrTaskNotFound {
		t.Fatalf("want task-not-found, got %+v", rerr)
	}
}

func TestA2ACancel(t *testing.T) {
	s := a2aServer(t)
	res, _ := callA2A(t, s, a2a.MethodMessageSend, a2a.SendMessageParams{Message: textMessage("cancel me")})
	var tk a2a.Task
	_ = json.Unmarshal(res, &tk)

	res2, rerr := callA2A(t, s, a2a.MethodTasksCancel, a2a.TaskIDParams{ID: tk.ID})
	if rerr != nil {
		t.Fatalf("cancel error: %+v", rerr)
	}
	var canceled a2a.Task
	_ = json.Unmarshal(res2, &canceled)
	if canceled.Status.State != a2a.StateCanceled {
		t.Fatalf("want canceled, got %q", canceled.Status.State)
	}

	// second cancel is refused: terminal state is not cancelable
	_, rerr = callA2A(t, s, a2a.MethodTasksCancel, a2a.TaskIDParams{ID: tk.ID})
	if rerr == nil || rerr.Code != a2a.ErrTaskNotCancelable {
		t.Fatalf("want not-cancelable, got %+v", rerr)
	}
}

func TestA2APushConfigCRUD(t *testing.T) {
	s := a2aServer(t)
	res, _ := callA2A(t, s, a2a.MethodMessageSend, a2a.SendMessageParams{Message: textMessage("watch me")})
	var tk a2a.Task
	_ = json.Unmarshal(res, &tk)

	set, rerr := callA2A(t, s, a2a.MethodPushConfigSet, a2a.TaskPushNotificationConfig{
		TaskID: tk.ID, PushNotificationConfig: a2a.PushNotificationConfig{URL: "https://hook.example/a2a", Token: "sekret"}})
	if rerr != nil {
		t.Fatalf("push set error: %+v", rerr)
	}
	var stored a2a.TaskPushNotificationConfig
	_ = json.Unmarshal(set, &stored)
	if stored.PushNotificationConfig.ID == "" {
		t.Fatal("want a generated config id")
	}

	list, rerr := callA2A(t, s, a2a.MethodPushConfigList, map[string]string{"id": tk.ID})
	if rerr != nil {
		t.Fatalf("push list error: %+v", rerr)
	}
	var cfgs []a2a.TaskPushNotificationConfig
	_ = json.Unmarshal(list, &cfgs)
	if len(cfgs) != 1 || cfgs[0].PushNotificationConfig.URL != "https://hook.example/a2a" {
		t.Fatalf("want 1 config, got %+v", cfgs)
	}

	_, rerr = callA2A(t, s, a2a.MethodPushConfigDelete, map[string]string{"id": tk.ID, "pushNotificationConfigId": stored.PushNotificationConfig.ID})
	if rerr != nil {
		t.Fatalf("push delete error: %+v", rerr)
	}
	list2, _ := callA2A(t, s, a2a.MethodPushConfigList, map[string]string{"id": tk.ID})
	var cfgs2 []a2a.TaskPushNotificationConfig
	_ = json.Unmarshal(list2, &cfgs2)
	if len(cfgs2) != 0 {
		t.Fatalf("want 0 configs after delete, got %+v", cfgs2)
	}
}

func TestA2AStreamResubscribe(t *testing.T) {
	s := a2aServer(t)
	res, _ := callA2A(t, s, a2a.MethodMessageSend, a2a.SendMessageParams{Message: textMessage("stream me")})
	var tk a2a.Task
	_ = json.Unmarshal(res, &tk)
	// drive to terminal so resubscribe emits the snapshot and returns
	if _, rerr := callA2A(t, s, a2a.MethodTasksCancel, a2a.TaskIDParams{ID: tk.ID}); rerr != nil {
		t.Fatalf("cancel: %+v", rerr)
	}

	praw, _ := json.Marshal(a2a.TaskIDParams{ID: tk.ID})
	body, _ := json.Marshal(a2a.Request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: a2a.MethodTasksResubscribe, Params: praw})
	req := httptest.NewRequest(http.MethodPost, "/v1/a2a", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("want SSE content-type, got %q", ct)
	}
	out := rec.Body.String()
	if !bytes.Contains([]byte(out), []byte("data: ")) {
		t.Fatalf("want SSE data frame, got %q", out)
	}
	if !bytes.Contains([]byte(out), []byte(`"state":"canceled"`)) {
		t.Fatalf("want canceled snapshot in stream, got %q", out)
	}
}

func TestA2AAgentCard(t *testing.T) {
	s := a2aServer(t)
	req := httptest.NewRequest(http.MethodGet, "/.well-known/agent-card.json", nil)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("http %d", rec.Code)
	}
	var card struct {
		ProtocolVersion string `json:"protocolVersion"`
		PreferredTrans  string `json:"preferredTransport"`
		Capabilities    struct {
			Streaming bool `json:"streaming"`
		} `json:"capabilities"`
		Skills []struct {
			ID string `json:"id"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &card); err != nil {
		t.Fatal(err)
	}
	if card.ProtocolVersion != a2a.ProtocolVersion || card.PreferredTrans != "JSONRPC" {
		t.Fatalf("bad card header: %+v", card)
	}
	if !card.Capabilities.Streaming {
		t.Fatal("want streaming capability advertised")
	}
	found := false
	for _, sk := range card.Skills {
		if sk.ID == "database" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want database skill in card, got %+v", card.Skills)
	}
}
