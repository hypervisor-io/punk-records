package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "llm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db
}

// fakeOpenAI serves one canned chat completion.
func fakeOpenAI(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const okCompletion = `{
  "id": "cmpl-1", "object": "chat.completion", "created": 1,
  "model": "test-model",
  "choices": [{"index": 0, "finish_reason": "stop",
    "message": {"role": "assistant", "content": "hello from fake"}}],
  "usage": {"prompt_tokens": 42, "completion_tokens": 7, "total_tokens": 49}
}`

func TestChatRecordsCall(t *testing.T) {
	db := testDB(t)
	srv := fakeOpenAI(t, 200, okCompletion)
	t.Setenv("TEST_LLM_KEY", "sk-test")

	m := NewManager(true, map[string]Profile{
		"default": {BaseURL: srv.URL, APIKeyEnv: "TEST_LLM_KEY", Model: "test-model"},
	}, db, func() time.Time { return time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC) })

	c, err := m.Client("default")
	if err != nil {
		t.Fatal(err)
	}
	// llm_calls.task_id is FK-checked: the task must exist
	if _, err := db.ExecContext(context.Background(), db.Rebind(`
		INSERT INTO tasks (id, status, created_at, updated_at)
		VALUES ('task-123', 'working', $1, $1)`),
		store.TimeToDB(time.Now())); err != nil {
		t.Fatal(err)
	}
	ctx := WithTaskID(context.Background(), "task-123")
	res, err := c.Chat(ctx, []Turn{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "hello from fake" || res.PromptTokens != 42 || res.CompletionTokens != 7 {
		t.Fatalf("res = %+v", res)
	}

	var taskID, errCol string
	var pt, ct int
	err = db.QueryRowContext(ctx,
		`SELECT task_id, prompt_tokens, completion_tokens, COALESCE(error,'') FROM llm_calls`).
		Scan(&taskID, &pt, &ct, &errCol)
	if err != nil {
		t.Fatal(err)
	}
	if taskID != "task-123" || pt != 42 || ct != 7 || errCol != "" {
		t.Fatalf("llm_calls row: task=%s pt=%d ct=%d err=%q", taskID, pt, ct, errCol)
	}
}

func TestChatRecordsFailure(t *testing.T) {
	db := testDB(t)
	srv := fakeOpenAI(t, 500, `{"error": {"message": "boom"}}`)
	t.Setenv("TEST_LLM_KEY", "sk-test")
	m := NewManager(true, map[string]Profile{
		"default": {BaseURL: srv.URL, APIKeyEnv: "TEST_LLM_KEY", Model: "test-model"},
	}, db, nil)

	c, err := m.Client("default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Chat(context.Background(), []Turn{{Role: "user", Content: "u"}}, nil); err == nil {
		t.Fatal("500 did not error")
	}
	var errCol string
	if err := db.QueryRowContext(context.Background(),
		`SELECT COALESCE(error,'') FROM llm_calls`).Scan(&errCol); err != nil {
		t.Fatal(err)
	}
	if errCol == "" {
		t.Fatal("failure not recorded in llm_calls")
	}
}

func TestDisabledFailsClosed(t *testing.T) {
	db := testDB(t)
	m := NewManager(false, nil, db, nil)
	c, err := m.Client("default")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Chat(context.Background(), []Turn{{Role: "user", Content: "u"}}, nil)
	if !errors.Is(err, ErrAIDisabled) {
		t.Fatalf("err = %v, want ErrAIDisabled", err)
	}
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM llm_calls`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("disabled short-circuit wrote an llm_calls row")
	}
}

func TestUnknownProfile(t *testing.T) {
	m := NewManager(true, map[string]Profile{}, nil, nil)
	if _, err := m.Client("ghost"); err == nil {
		t.Fatal("unknown profile accepted")
	}
}

func TestToolCallParsing(t *testing.T) {
	body := `{
  "id": "cmpl-2", "object": "chat.completion", "created": 1, "model": "m",
  "choices": [{"index": 0, "finish_reason": "tool_calls",
    "message": {"role": "assistant", "content": "",
      "tool_calls": [{"id": "call_1", "type": "function",
        "function": {"name": "susanoo__get_incident", "arguments": "{\"id\":7}"}}]}}],
  "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
}`
	srv := fakeOpenAI(t, 200, body)
	t.Setenv("TEST_LLM_KEY", "sk-test")
	m := NewManager(true, map[string]Profile{
		"default": {BaseURL: srv.URL, APIKeyEnv: "TEST_LLM_KEY", Model: "m"},
	}, testDB(t), nil)
	c, err := m.Client("default")
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Chat(context.Background(), []Turn{{Role: "user", Content: "u"}},
		[]Tool{{Name: "susanoo__get_incident", Description: "d", Schema: json.RawMessage(`{"type":"object"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "susanoo__get_incident" {
		t.Fatalf("tool calls = %+v", res.ToolCalls)
	}
}
