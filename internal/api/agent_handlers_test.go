package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/store"
)

func TestAgentHookCapture(t *testing.T) {
	srv := testServer(t)
	mem := srv.mem
	post := func(body string) *httptest.ResponseRecorder {
		return do(t, srv, "POST", "/v1/agent/hooks", body)
	}
	// PostToolUse
	rec := post(`{"hook_event_name":"PostToolUse","session_id":"s1","cwd":"/home/u/myproj",
		"tool_name":"Bash","tool_input":{"command":"ls"},"tool_response":"file.txt","tool_use_id":"tu1"}`)
	if rec.Code != 200 {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	facts, err := mem.Recall(context.Background(), "agent-myproj", "/agent-sessions/s1/tool-tu1", 1)
	if err != nil || len(facts) != 1 {
		t.Fatal(facts, err)
	}
	if !strings.Contains(facts[0].Body, "Bash") || !strings.Contains(facts[0].Body, "file.txt") {
		t.Fatalf("body: %s", facts[0].Body)
	}
	// Author/Writer/SourceRef: prove the capture is attributed the way the
	// handler claims, not just "some fact got written".
	if facts[0].Author != "agent-hook" || facts[0].Writer != "agent-hook" || facts[0].SourceRef != "claude-code" {
		t.Fatalf("provenance: author=%q writer=%q sourceRef=%q", facts[0].Author, facts[0].Writer, facts[0].SourceRef)
	}
	// Capture is ephemeral by design: expiry is set, roughly agentCaptureTTL
	// (30d) out, never nil/immediate.
	if facts[0].ExpiresAt == nil {
		t.Fatal("expected non-nil ExpiresAt on capture write")
	}
	until := time.Until(*facts[0].ExpiresAt)
	if until < 29*24*time.Hour || until > 31*24*time.Hour {
		t.Fatalf("ExpiresAt not ~30d out: %v", until)
	}

	// SessionStart, UserPromptSubmit, Stop
	for _, body := range []string{
		`{"hook_event_name":"SessionStart","session_id":"s1","cwd":"/home/u/myproj","source":"startup"}`,
		`{"hook_event_name":"UserPromptSubmit","session_id":"s1","cwd":"/home/u/myproj","prompt_id":"p1","prompt":"fix the bug"}`,
		`{"hook_event_name":"Stop","session_id":"s1","cwd":"/home/u/myproj","last_assistant_message":"done"}`,
	} {
		if rec := post(body); rec.Code != 200 {
			t.Fatalf("%s -> %d", body, rec.Code)
		}
	}
	keys, err := mem.ListKeys(context.Background(), "agent-myproj", "/agent-sessions/s1")
	if err != nil || len(keys) != 4 {
		t.Fatalf("keys: %v %v", keys, err)
	}

	// SessionStart body carries cwd= and the source.
	startFacts, err := mem.Recall(context.Background(), "agent-myproj", "/agent-sessions/s1/start", 1)
	if err != nil || len(startFacts) != 1 {
		t.Fatal(startFacts, err)
	}
	if !strings.Contains(startFacts[0].Body, "cwd=/home/u/myproj") || !strings.Contains(startFacts[0].Body, "startup") {
		t.Fatalf("start body: %q", startFacts[0].Body)
	}

	// Stop body carries the last assistant message.
	stopFacts, err := mem.Recall(context.Background(), "agent-myproj", "/agent-sessions/s1/stop", 1)
	if err != nil || len(stopFacts) != 1 {
		t.Fatal(stopFacts, err)
	}
	if !strings.Contains(stopFacts[0].Body, "done") {
		t.Fatalf("stop body: %q", stopFacts[0].Body)
	}

	// unknown event: 200 ignored, nothing stored
	if rec := post(`{"hook_event_name":"Notification","session_id":"s1","cwd":"/home/u/myproj"}`); rec.Code != 200 ||
		!strings.Contains(rec.Body.String(), "ignored") {
		t.Fatalf("unknown event: %d %s", rec.Code, rec.Body.String())
	}
	// missing session_id: ignored
	if rec := post(`{"hook_event_name":"Stop","cwd":"/x"}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), "ignored") {
		t.Fatalf("no session: %d %s", rec.Code, rec.Body.String())
	}
	// bad json: 400
	if rec := post(`{`); rec.Code != 400 {
		t.Fatalf("bad json: %d", rec.Code)
	}

	// still exactly 4 keys: none of the ignored/bad requests above wrote anything
	keys, err = mem.ListKeys(context.Background(), "agent-myproj", "/agent-sessions/s1")
	if err != nil || len(keys) != 4 {
		t.Fatalf("keys after ignored/bad requests: %v %v", keys, err)
	}
}

// TestAgentHookMissingCorrelationID proves PostToolUse and UserPromptSubmit
// events with no tool_use_id / prompt_id are ignored (200) and write
// nothing, the same way a missing session_id already is: without that ID
// there is no stable, non-colliding key segment to write under.
func TestAgentHookMissingCorrelationID(t *testing.T) {
	srv := testServer(t)
	mem := srv.mem
	post := func(body string) *httptest.ResponseRecorder {
		return do(t, srv, "POST", "/v1/agent/hooks", body)
	}
	cases := []string{
		`{"hook_event_name":"PostToolUse","session_id":"s1","cwd":"/home/u/myproj","tool_name":"Bash","tool_input":{},"tool_response":""}`,
		`{"hook_event_name":"UserPromptSubmit","session_id":"s1","cwd":"/home/u/myproj","prompt":"hi"}`,
	}
	for _, body := range cases {
		rec := post(body)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ignored") {
			t.Fatalf("%s -> %d %s", body, rec.Code, rec.Body.String())
		}
	}
	keys, err := mem.ListKeys(context.Background(), "agent-myproj", "/agent-sessions/s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("expected zero keys written, got %v", keys)
	}
}

// TestAgentHookIDsEmptyAfterSanitize proves that a session_id/prompt_id/
// tool_use_id which sanitizes down to the empty string (all-punctuation
// input like "." or "%%%") is treated as absent - 200 ignored, nothing
// written - rather than silently colliding at a shared key like
// "/agent-sessions//start" or "/agent-sessions/s1/prompt-".
func TestAgentHookIDsEmptyAfterSanitize(t *testing.T) {
	srv := testServer(t)
	mem := srv.mem
	post := func(body string) *httptest.ResponseRecorder {
		return do(t, srv, "POST", "/v1/agent/hooks", body)
	}
	cases := []string{
		`{"hook_event_name":"SessionStart","session_id":".","cwd":"/home/u/p","source":"s"}`,
		`{"hook_event_name":"SessionStart","session_id":"..","cwd":"/home/u/p","source":"s"}`,
		`{"hook_event_name":"SessionStart","session_id":"%%%","cwd":"/home/u/p","source":"s"}`,
		`{"hook_event_name":"UserPromptSubmit","session_id":"s1","cwd":"/home/u/p","prompt_id":"...","prompt":"hi"}`,
		`{"hook_event_name":"PostToolUse","session_id":"s1","cwd":"/home/u/p","tool_name":"Bash","tool_use_id":"/"}`,
	}
	for _, body := range cases {
		rec := post(body)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ignored") {
			t.Fatalf("%s -> %d %s", body, rec.Code, rec.Body.String())
		}
	}
	keys, err := mem.ListKeys(context.Background(), "agent-p", "/agent-sessions/")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("expected zero keys written, got %v", keys)
	}
}

// TestAgentHookPromptClipMultibyte proves the 2000-rune clip cuts on a
// rune boundary even for a prompt made entirely of multibyte characters:
// a byte-oriented clip would split a rune and produce invalid UTF-8.
func TestAgentHookPromptClipMultibyte(t *testing.T) {
	srv := testServer(t)
	mem := srv.mem
	prompt := strings.Repeat("é", 2500)
	body := `{"hook_event_name":"UserPromptSubmit","session_id":"s1","cwd":"/home/u/myproj","prompt_id":"p1","prompt":"` + prompt + `"}`
	rec := do(t, srv, "POST", "/v1/agent/hooks", body)
	if rec.Code != 200 {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	facts, err := mem.Recall(context.Background(), "agent-myproj", "/agent-sessions/s1/prompt-p1", 1)
	if err != nil || len(facts) != 1 {
		t.Fatal(facts, err)
	}
	got := facts[0].Body
	if !utf8.ValidString(got) {
		t.Fatalf("clipped body is not valid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("expected ellipsis suffix, got %q", got)
	}
	if n := utf8.RuneCountInString(got); n != hookClip+3 {
		t.Fatalf("rune count = %d, want %d (%d clip + 3 ellipsis)", n, hookClip+3, hookClip)
	}
}

// TestAgentHookSessionStartBodyClipped proves the SessionStart capture
// body ("cwd=... source=...") is clipped like every other capture event:
// before this, SessionStart was the one event whose body bypassed
// clipStr, so an oversized cwd could write an unbounded fact body.
func TestAgentHookSessionStartBodyClipped(t *testing.T) {
	srv := testServer(t)
	mem := srv.mem
	hugeCWD := "/home/u/" + strings.Repeat("x", 2500)
	body := `{"hook_event_name":"SessionStart","session_id":"s1","cwd":"` + hugeCWD + `","source":"startup"}`
	rec := do(t, srv, "POST", "/v1/agent/hooks", body)
	if rec.Code != 200 {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	// AgentNamespace slugs the cwd's basename, which is entirely "x"s here.
	ns := AgentNamespace(hugeCWD)
	facts, err := mem.Recall(context.Background(), ns, "/agent-sessions/s1/start", 1)
	if err != nil || len(facts) != 1 {
		t.Fatal(facts, err)
	}
	got := facts[0].Body
	if !utf8.ValidString(got) {
		t.Fatalf("clipped body is not valid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("expected ellipsis suffix (body must be clipped), got %q", got)
	}
	if n := utf8.RuneCountInString(got); n != hookClip+3 {
		t.Fatalf("rune count = %d, want %d (%d clip + 3 ellipsis)", n, hookClip+3, hookClip)
	}
}

// TestAgentHookBodyTooLarge proves an oversized hook payload is rejected
// with 400 (via http.MaxBytesReader failing the JSON decode) instead of
// being read into memory unbounded.
func TestAgentHookBodyTooLarge(t *testing.T) {
	srv := testServer(t)
	huge := `{"hook_event_name":"UserPromptSubmit","session_id":"s1","cwd":"/x","prompt_id":"p1","prompt":"` +
		strings.Repeat("x", maxAgentHookBody+1) + `"}`
	rec := do(t, srv, "POST", "/v1/agent/hooks", huge)
	if rec.Code != 400 {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
}

// TestAgentHookStoreFailure proves a genuine store failure (as opposed to
// a caller-shaped problem like empty IDs) surfaces as 500, not 400 - the
// doc comment on handleAgentHook promises 400 is reserved for "can't even
// parse the request".
func TestAgentHookStoreFailure(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := New(testLogger(), Deps{Memory: memory.New(db, nil)})
	// Close the DB out from under the store: the next write fails for a
	// reason that has nothing to do with the request body.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rec := do(t, srv, "POST", "/v1/agent/hooks",
		`{"hook_event_name":"SessionStart","session_id":"s1","cwd":"/home/u/myproj","source":"startup"}`)
	if rec.Code != 500 {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentNamespace(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/home/u/My Proj_2", "agent-my-proj-2"},
		{"", "agent-default"},
		// "/" has no letters/numbers in its basename ("/") but the cwd
		// itself is non-empty, so it hashes rather than sharing the
		// literal "agent-default" bucket with a truly-absent cwd.
		{"/", "agent-2a0c975e"},
		// Non-ASCII (CJK) basenames get their own namespace instead of
		// all collapsing into "agent-default".
		{"/home/u/项目", "agent-项目"},
		// Punctuation-only basename with a non-empty cwd: hash fallback,
		// not "agent-default".
		{"/home/u/...", "agent-c01a9474"},
	}
	for _, c := range cases {
		if got := AgentNamespace(c.in); got != c.want {
			t.Errorf("AgentNamespace(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// TestAgentHookSanitizesIDs proves a hostile session_id/tool_use_id cannot
// escape the /agent-sessions/ subtree by injecting path segments (e.g. "..")
// into the memory key: sanitization must confine every write under the
// session's own prefix regardless of attacker-controlled ID content.
func TestAgentHookSanitizesIDs(t *testing.T) {
	srv := testServer(t)
	mem := srv.mem
	rec := do(t, srv, "POST", "/v1/agent/hooks", `{"hook_event_name":"SessionStart","session_id":"../../etc/passwd","cwd":"/home/u/myproj","source":"startup"}`)
	if rec.Code != 200 {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	keys, err := mem.ListKeys(context.Background(), "agent-myproj", "/agent-sessions/")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if strings.Contains(k, "..") || strings.Count(k, "/") != 3 {
			t.Fatalf("key escaped session subtree: %q", k)
		}
	}
	if len(keys) != 1 {
		t.Fatalf("keys: %v", keys)
	}
}

// TestAgentHookBadJSON proves malformed JSON is rejected with 400 rather
// than silently ignored, distinguishing "we couldn't parse this" from
// "we parsed it and chose not to store it".
func TestAgentHookBadJSON(t *testing.T) {
	srv := testServer(t)
	rec := do(t, srv, "POST", "/v1/agent/hooks", `not json`)
	if rec.Code != 400 {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
}

// agentContextOutFromBody decodes an agent/context response body for
// assertions that need fields, not just substring matches.
func agentContextOutFromBody(t *testing.T, rec *httptest.ResponseRecorder) agentContextOut {
	t.Helper()
	var out agentContextOut
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	return out
}

// TestAgentContext covers the endpoint's core contract: a high-importance
// decision fact surfaces in the assembled context, an /agent-sessions/
// capture does not (it's ephemeral hook noise, not durable project
// knowledge), cwd resolves to the same namespace AgentNamespace derives
// for hook capture, q switches to hybrid search, and neither ns nor cwd
// is a 400 (malformed request, nothing to look up).
func TestAgentContext(t *testing.T) {
	srv := testServer(t)
	ctx := context.Background()
	if _, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: "agent-myproj", Key: "/decisions/auth", Body: "auth uses jwt via jose",
		Author: "test", Writer: "test", Importance: 0.8,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: "agent-myproj", Key: "/agent-sessions/s0/tool-x", Body: "noise",
		Author: "agent-hook", Writer: "agent-hook",
	}); err != nil {
		t.Fatal(err)
	}

	rec := do(t, srv, "GET", "/v1/agent/context?cwd=/home/u/myproj", "")
	if rec.Code != 200 {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	out := agentContextOutFromBody(t, rec)
	if !strings.Contains(out.Context, "auth uses jwt") {
		t.Fatalf("missing decision fact: %q", out.Context)
	}
	if !strings.Contains(out.Context, "agent-myproj") {
		t.Fatalf("missing namespace heading: %q", out.Context)
	}
	if strings.Contains(out.Context, "noise") {
		t.Fatalf("agent-sessions capture leaked into context: %q", out.Context)
	}

	rec = do(t, srv, "GET", "/v1/agent/context?ns=agent-myproj&q=jwt", "")
	if rec.Code != 200 {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "fact_ids") {
		t.Fatalf("missing fact_ids: %s", rec.Body.String())
	}

	rec = do(t, srv, "GET", "/v1/agent/context", "")
	if rec.Code != 400 {
		t.Fatalf("neither ns nor cwd: %d %s", rec.Code, rec.Body.String())
	}
}

// TestAgentContextEmptyNamespaceHasEmptyContext proves that a namespace
// with zero entities and zero facts returns Context == "" - not the bare
// "## Project memory (ns)\n" header, which would otherwise be a non-empty
// but useless string that hookcli's empty-context guard can't catch.
// Namespace and an empty (never nil-vs-empty-matters here) fact_ids list
// are still returned.
func TestAgentContextEmptyNamespaceHasEmptyContext(t *testing.T) {
	srv := testServer(t)

	rec := do(t, srv, "GET", "/v1/agent/context?ns=agent-empty", "")
	if rec.Code != 200 {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	out := agentContextOutFromBody(t, rec)
	if out.Context != "" {
		t.Fatalf("expected empty context for an empty namespace, got %q", out.Context)
	}
	if out.Namespace != "agent-empty" {
		t.Fatalf("expected namespace echoed, got %q", out.Namespace)
	}
	if len(out.FactIDs) != 0 {
		t.Fatalf("expected empty fact_ids, got %v", out.FactIDs)
	}
}

// TestAgentContextEntitiesLine proves the "Known entities" line appears
// exactly when the namespace has /entities/ facts, and that those entity
// facts themselves never appear as "- [key] body" bullet lines - they're
// surfaced only via the entities line, never duplicated into the ranked
// fact list.
func TestAgentContextEntitiesLine(t *testing.T) {
	srv := testServer(t)
	ctx := context.Background()
	if _, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: "ns", Key: "/svc/db", Body: "primary is pg-1", Writer: "t",
	}); err != nil {
		t.Fatal(err)
	}

	// No entities yet: no "Known entities" line.
	rec := do(t, srv, "GET", "/v1/agent/context?ns=ns", "")
	if rec.Code != 200 {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	out := agentContextOutFromBody(t, rec)
	if strings.Contains(out.Context, "Known entities") {
		t.Fatalf("entities line present with no entities: %q", out.Context)
	}

	if _, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: "ns", Key: "/entities/alice-chen", Body: "Alice Chen",
		Attributes: map[string]any{"mention_count": 4.0}, Writer: "enricher",
	}); err != nil {
		t.Fatal(err)
	}

	rec = do(t, srv, "GET", "/v1/agent/context?ns=ns", "")
	if rec.Code != 200 {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	out = agentContextOutFromBody(t, rec)
	if !strings.Contains(out.Context, "Known entities: Alice Chen") {
		t.Fatalf("missing entities line: %q", out.Context)
	}
	// The entity fact's own key must never surface as a bullet line.
	if strings.Contains(out.Context, "[/entities/alice-chen]") {
		t.Fatalf("entity fact leaked as a bullet line: %q", out.Context)
	}
}

// TestAgentContextMaxTokens proves max_tokens actually bounds the
// assembled context: a small budget over many facts yields a shorter
// context and a fact_ids list that is a subset of the unbounded one.
func TestAgentContextMaxTokens(t *testing.T) {
	srv := testServer(t)
	ctx := context.Background()
	for i := 0; i < 15; i++ {
		if _, err := srv.mem.Write(ctx, memory.WriteInput{
			Namespace: "ns", Key: fmt.Sprintf("/notes/%02d", i),
			Body: strings.Repeat("word ", 40), Writer: "t",
		}); err != nil {
			t.Fatal(err)
		}
	}

	full := agentContextOutFromBody(t, do(t, srv, "GET", "/v1/agent/context?ns=ns", ""))
	small := agentContextOutFromBody(t, do(t, srv, "GET", "/v1/agent/context?ns=ns&max_tokens=20", ""))

	if len(small.Context) >= len(full.Context) {
		t.Fatalf("max_tokens did not shrink context: small=%d full=%d", len(small.Context), len(full.Context))
	}
	if len(small.FactIDs) >= len(full.FactIDs) {
		t.Fatalf("max_tokens did not shrink fact_ids: small=%d full=%d", len(small.FactIDs), len(full.FactIDs))
	}
	fullSet := map[string]bool{}
	for _, id := range full.FactIDs {
		fullSet[id] = true
	}
	for _, id := range small.FactIDs {
		if !fullSet[id] {
			t.Fatalf("small fact_ids not a subset of full: %v not in %v", small.FactIDs, full.FactIDs)
		}
	}
}

// TestAgentContextSearchQuery proves the q path returns scored hybrid-
// search hits (the seeded jwt fact) rather than the important/recent pool.
func TestAgentContextSearchQuery(t *testing.T) {
	srv := testServer(t)
	ctx := context.Background()
	if _, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: "ns", Key: "/decisions/auth", Body: "auth uses jwt via jose", Writer: "t",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: "ns", Key: "/decisions/unrelated", Body: "storage uses postgres", Writer: "t",
	}); err != nil {
		t.Fatal(err)
	}

	out := agentContextOutFromBody(t, do(t, srv, "GET", "/v1/agent/context?ns=ns&q=jwt", ""))
	if !strings.Contains(out.Context, "auth uses jwt") {
		t.Fatalf("q path missing scored hit: %q", out.Context)
	}
	if len(out.FactIDs) == 0 {
		t.Fatalf("q path returned no fact_ids: %+v", out)
	}
}

// TestAgentContextDeterministicOrder proves repeated calls over the same
// data produce byte-identical context: whatever order Recall and the
// ranking sort settle on, it doesn't vary from call to call. It does not
// exercise the sort's key tie-break - CreatedAt ties are effectively
// unreachable here (Recall already orders by key, and real writes carry
// nanosecond-resolution timestamps) - that tie-break stays in the code as
// defense in depth, not because this test depends on it firing.
func TestAgentContextDeterministicOrder(t *testing.T) {
	srv := testServer(t)
	ctx := context.Background()
	for i := 0; i < 8; i++ {
		if _, err := srv.mem.Write(ctx, memory.WriteInput{
			Namespace: "ns", Key: fmt.Sprintf("/notes/%02d", i), Body: "note", Writer: "t",
		}); err != nil {
			t.Fatal(err)
		}
	}
	first := agentContextOutFromBody(t, do(t, srv, "GET", "/v1/agent/context?ns=ns", ""))
	for i := 0; i < 5; i++ {
		again := agentContextOutFromBody(t, do(t, srv, "GET", "/v1/agent/context?ns=ns", ""))
		if again.Context != first.Context {
			t.Fatalf("non-deterministic context on call %d:\n%q\nvs\n%q", i, again.Context, first.Context)
		}
	}
}

// TestAgentHookBlockedPolicy proves a per-namespace "block" defense policy
// (SetDefensePolicy) rejects sensitive content arriving through the hook
// capture path exactly the way it would through a direct Write: 200
// "blocked" with the namespace echoed, not a store failure and not a
// silent drop. Mirrors TestAgentHookStoreFailure's pattern of building the
// store directly to reach a seam the HTTP surface doesn't expose a knob
// for.
func TestAgentHookBlockedPolicy(t *testing.T) {
	srv := testServer(t)
	srv.mem.SetDefensePolicy("agent-myproj", "block")

	rec := do(t, srv, "POST", "/v1/agent/hooks",
		`{"hook_event_name":"PostToolUse","session_id":"s1","cwd":"/home/u/myproj",`+
			`"tool_name":"Bash","tool_input":{},"tool_response":"token ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789","tool_use_id":"tu1"}`)
	if rec.Code != 200 {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"status":"blocked"`) {
		t.Fatalf("expected blocked status: %s", body)
	}
	if !strings.Contains(body, `"namespace":"agent-myproj"`) {
		t.Fatalf("expected namespace echo: %s", body)
	}
	keys, err := srv.mem.ListKeys(context.Background(), "agent-myproj", "/agent-sessions/")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("blocked content must not be stored: %v", keys)
	}
}

// TestAgentHookSourceRefDefaultsToClaudeCode proves a hook payload with no
// "source" field at all (the common case: Claude Code's own PostToolUse/
// Stop/UserPromptSubmit events never carry one - see code.claude.com/docs/
// en/hooks) still attributes the fact to "claude-code", not an empty
// string.
func TestAgentHookSourceRefDefaultsToClaudeCode(t *testing.T) {
	srv := testServer(t)
	rec := do(t, srv, "POST", "/v1/agent/hooks",
		`{"hook_event_name":"Stop","session_id":"s1","cwd":"/home/u/myproj","last_assistant_message":"done"}`)
	if rec.Code != 200 {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	facts, err := srv.mem.Recall(context.Background(), "agent-myproj", "/agent-sessions/s1/stop", 1)
	if err != nil || len(facts) != 1 {
		t.Fatal(facts, err)
	}
	if facts[0].SourceRef != "claude-code" {
		t.Fatalf("source_ref = %q, want claude-code (no source field sent)", facts[0].SourceRef)
	}
}

// TestAgentHookSourceRefFromExplicitAgent proves an explicit non-empty
// "source" field (as hookcli's Cursor translator sets unconditionally to
// "cursor") is trusted verbatim as the fact's SourceRef, distinguishing
// Cursor-originated captures from Claude Code's own.
func TestAgentHookSourceRefFromExplicitAgent(t *testing.T) {
	srv := testServer(t)
	rec := do(t, srv, "POST", "/v1/agent/hooks",
		`{"hook_event_name":"PostToolUse","session_id":"s1","cwd":"/home/u/myproj","source":"cursor",`+
			`"tool_name":"Edit","tool_input":{},"tool_response":"","tool_use_id":"tu1"}`)
	if rec.Code != 200 {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	facts, err := srv.mem.Recall(context.Background(), "agent-myproj", "/agent-sessions/s1/tool-tu1", 1)
	if err != nil || len(facts) != 1 {
		t.Fatal(facts, err)
	}
	if facts[0].SourceRef != "cursor" {
		t.Fatalf("source_ref = %q, want cursor", facts[0].SourceRef)
	}
}

// TestAgentHookSourceRefHostileInputNeverErrors proves a hostile "source"
// value (path-traversal-shaped text, embedded control characters) is
// sanitized down to an allowlisted string or the default, per the
// error-class rule that a well-formed payload is never a 400 - this is
// metadata sanitization, not a parse failure.
func TestAgentHookSourceRefHostileInputNeverErrors(t *testing.T) {
	srv := testServer(t)
	cases := []struct {
		name   string
		source string
		key    string
	}{
		{"pathTraversal", "../evil", "/agent-sessions/s1/tool-tu-path"},
		{"controlChars", "cur\x00sor\x1b[31m", "/agent-sessions/s1/tool-tu-ctrl"},
	}
	for _, c := range cases {
		// json.Marshal (not %q, which is Go-escape syntax like \x00 - not
		// valid inside a JSON string literal) to embed the hostile source
		// as a properly-escaped JSON string.
		sourceJSON, err := json.Marshal(c.source)
		if err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`{"hook_event_name":"PostToolUse","session_id":"s1","cwd":"/home/u/myproj","source":%s,`+
			`"tool_name":"Edit","tool_input":{},"tool_response":"","tool_use_id":%q}`,
			sourceJSON, strings.TrimPrefix(c.key, "/agent-sessions/s1/tool-"))
		rec := do(t, srv, "POST", "/v1/agent/hooks", body)
		if rec.Code != 200 {
			t.Fatalf("%s: hostile source must never 400, got %d: %s", c.name, rec.Code, rec.Body.String())
		}
		facts, err := srv.mem.Recall(context.Background(), "agent-myproj", c.key, 1)
		if err != nil || len(facts) != 1 {
			t.Fatalf("%s: %v %v", c.name, facts, err)
		}
		if strings.ContainsAny(facts[0].SourceRef, "./\x00\x1b") {
			t.Fatalf("%s: source_ref not sanitized: %q", c.name, facts[0].SourceRef)
		}
		if facts[0].SourceRef == "" {
			t.Fatalf("%s: source_ref must never be empty", c.name)
		}
	}
}

// TestAgentHookSourceRefSessionStartReasonIsNotAnAgentIdentity is the
// regression pin for the real conflict in reusing Claude Code's own
// "source" JSON field (code.claude.com/docs/en/hooks documents SessionStart
// as sending source: "startup"|"resume"|"clear"|"compact"|"fork") as the
// carrier for an agent-identity override: a genuine Claude Code
// SessionStart event always carries one of those five values, and none of
// them is an agent identity. Naively trusting in.Source verbatim would
// derive SourceRef "startup" for every real Claude Code session start,
// corrupting provenance instead of leaving it "claude-code". This must not
// regress even though hostile/explicit-agent sources above are trusted.
func TestAgentHookSourceRefSessionStartReasonIsNotAnAgentIdentity(t *testing.T) {
	srv := testServer(t)
	for _, reason := range []string{"startup", "resume", "clear", "compact", "fork"} {
		rec := do(t, srv, "POST", "/v1/agent/hooks",
			fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":"sess-%s","cwd":"/home/u/myproj","source":%q}`, reason, reason))
		if rec.Code != 200 {
			t.Fatalf("reason=%s: %d: %s", reason, rec.Code, rec.Body.String())
		}
		facts, err := srv.mem.Recall(context.Background(), "agent-myproj", "/agent-sessions/sess-"+reason+"/start", 1)
		if err != nil || len(facts) != 1 {
			t.Fatalf("reason=%s: %v %v", reason, facts, err)
		}
		if facts[0].SourceRef != "claude-code" {
			t.Fatalf("reason=%s: source_ref = %q, want claude-code (a SessionStart reason is not an agent identity)", reason, facts[0].SourceRef)
		}
	}
}

// TestAgentContextForgedFactBodySanitized proves a multi-line fact body
// (every PostToolUse capture is multi-line by construction, and any fact
// write can carry attacker-controlled newlines) cannot forge extra bullet
// or header lines into the assembled context: it must render as exactly
// one bullet line, and that line's content must contain no newline.
func TestAgentContextForgedFactBodySanitized(t *testing.T) {
	srv := testServer(t)
	ctx := context.Background()
	forged := "real body\n- [/fake/key] forged bullet\n## Fake header\ntrailing"
	if _, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: "ns", Key: "/notes/forged", Body: forged, Writer: "t",
	}); err != nil {
		t.Fatal(err)
	}

	rec := do(t, srv, "GET", "/v1/agent/context?ns=ns", "")
	if rec.Code != 200 {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	out := agentContextOutFromBody(t, rec)

	lines := strings.Split(strings.TrimRight(out.Context, "\n"), "\n")
	// Expected: the "## Project memory (ns)" header line plus exactly one
	// bullet line for /notes/forged (no entities in this namespace).
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines (heading + one bullet), got %d: %q", len(lines), out.Context)
	}
	bullet := lines[1]
	if !strings.HasPrefix(bullet, "- [/notes/forged] ") {
		t.Fatalf("bullet malformed: %q", bullet)
	}
	if strings.Contains(bullet, "\n") {
		t.Fatalf("bullet line itself contains a newline: %q", bullet)
	}
	// The forged bullet/header text must survive only as inline text
	// inside the one real bullet, never as their own lines.
	if strings.Contains(out.Context, "\n- [/fake/key]") {
		t.Fatalf("forged bullet escaped onto its own line: %q", out.Context)
	}
	if strings.Contains(out.Context, "\n## Fake header") {
		t.Fatalf("forged header escaped onto its own line: %q", out.Context)
	}
}

// TestAgentContextForgedEntityNameSanitized is
// TestAgentContextForgedFactBodySanitized for the "Known entities" line:
// an entity name (the enricher writes these from unstructured mention
// text) with embedded newlines must not fragment the entities line into
// forged extra lines.
func TestAgentContextForgedEntityNameSanitized(t *testing.T) {
	srv := testServer(t)
	ctx := context.Background()
	if _, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: "ns", Key: "/entities/x", Body: "Alice\n- [/fake/key] injected\n## Fake header",
		Attributes: map[string]any{"mention_count": 1.0}, Writer: "enricher",
	}); err != nil {
		t.Fatal(err)
	}

	rec := do(t, srv, "GET", "/v1/agent/context?ns=ns", "")
	if rec.Code != 200 {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	out := agentContextOutFromBody(t, rec)

	lines := strings.Split(strings.TrimRight(out.Context, "\n"), "\n")
	// Expected: the heading line plus exactly one "Known entities:" line
	// (no other facts in this namespace).
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines (heading + entities line), got %d: %q", len(lines), out.Context)
	}
	if !strings.HasPrefix(lines[1], "Known entities: ") {
		t.Fatalf("entities line malformed: %q", lines[1])
	}
	if strings.Contains(out.Context, "\n- [/fake/key]") || strings.Contains(out.Context, "\n## Fake header") {
		t.Fatalf("forged content escaped the entities line: %q", out.Context)
	}
}

// TestAgentContextOversizedFactDoesNotEmptyContext proves one oversized
// fact ahead of a short one no longer empties the whole context.
// TokenBudget stops at the first fact that doesn't fit; before bodies were
// clipped at assembly, a single multi-thousand-rune fact could exceed even
// a generous default budget and starve out every fact behind it. Once
// clipped to agentContextFactBodyClip runes, both facts fit under the
// default budget.
func TestAgentContextOversizedFactDoesNotEmptyContext(t *testing.T) {
	srv := testServer(t)
	ctx := context.Background()
	if _, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: "ns", Key: "/notes/huge", Body: strings.Repeat("x", 9000), Writer: "t", Importance: 0.9,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: "ns", Key: "/notes/small", Body: "a short fact", Writer: "t", Importance: 0.9,
	}); err != nil {
		t.Fatal(err)
	}

	rec := do(t, srv, "GET", "/v1/agent/context?ns=ns", "")
	if rec.Code != 200 {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	out := agentContextOutFromBody(t, rec)
	if out.Context == "" || len(out.FactIDs) == 0 {
		t.Fatalf("oversized fact emptied the whole context: %q, fact_ids=%v", out.Context, out.FactIDs)
	}
	if !strings.Contains(out.Context, "/notes/huge") {
		t.Fatalf("clipped huge fact should still appear (clipped): %q", out.Context)
	}
	if !strings.Contains(out.Context, "/notes/small") {
		t.Fatalf("small fact starved out by the unclipped huge one: %q", out.Context)
	}
	if len(out.FactIDs) != 2 {
		t.Fatalf("expected both facts to survive clipping+budget, got %v", out.FactIDs)
	}
}

// TestAgentContextSearchQueryExcludesReservedPrefixes proves the q
// (hybrid-search) path applies the same /agent-sessions/ and /entities/
// exclusion as the default ranked-pool path: the doc comment on
// handleAgentContext frames the exclusion as an endpoint-level property,
// not something specific to one branch, so a session capture that happens
// to match the query must not leak into a q= response either.
func TestAgentContextSearchQueryExcludesReservedPrefixes(t *testing.T) {
	srv := testServer(t)
	ctx := context.Background()
	// A session capture whose body matches the query term.
	if _, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: "ns", Key: "/agent-sessions/s0/tool-x", Body: "jwt secret leaked here",
		Author: "agent-hook", Writer: "agent-hook",
	}); err != nil {
		t.Fatal(err)
	}
	// A durable fact that should legitimately surface for the same query.
	if _, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: "ns", Key: "/decisions/auth", Body: "auth uses jwt via jose", Writer: "t",
	}); err != nil {
		t.Fatal(err)
	}

	out := agentContextOutFromBody(t, do(t, srv, "GET", "/v1/agent/context?ns=ns&q=jwt", ""))
	if strings.Contains(out.Context, "agent-sessions") || strings.Contains(out.Context, "leaked") {
		t.Fatalf("q path re-admitted an /agent-sessions/ capture: %q", out.Context)
	}
	if !strings.Contains(out.Context, "auth uses jwt") {
		t.Fatalf("q path lost the legitimate hit alongside the exclusion: %q", out.Context)
	}
}

// TestAgentContextSearchQueryOverfetchesPastExclusion proves the q
// (hybrid-search) path over-fetches candidates before applying the
// /agent-sessions/ exclusion: HybridSearchScored's own limit is applied
// before that filter, so a namespace dominated by session captures that
// happen to outscore the durable facts must not filter down to zero
// durable results just because they don't fit in the first
// agentContextPoolCap (20) ranked hits. All 27 facts share identical body
// text (so FTS relevance ties) and the 25 session captures are seeded
// with a higher Importance than the 2 durable facts, which deterministically
// ranks every capture ahead of both durable facts - exactly the shape
// that emptied the q path's result before the over-fetch fix.
func TestAgentContextSearchQueryOverfetchesPastExclusion(t *testing.T) {
	srv := testServer(t)
	ctx := context.Background()
	const body = "release process uses canary rollout"
	// Distinct session IDs (not distinct tool-use IDs within one session):
	// HybridSearchScored's diversification caps hits at 3 per key-subtree
	// (first two path segments, i.e. "/agent-sessions/<sid>"), so 25 hits
	// under the SAME session id would already be self-limiting regardless
	// of the over-fetch fix. Spreading them across 25 sessions gives each
	// its own subtree, matching the many-concurrent/serial-sessions shape
	// that actually starves the q path in production.
	for i := 0; i < 25; i++ {
		if _, err := srv.mem.Write(ctx, memory.WriteInput{
			Namespace: "ns", Key: fmt.Sprintf("/agent-sessions/s%02d/tool-x", i), Body: body,
			Author: "agent-hook", Writer: "agent-hook", Importance: 1.0,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: "ns", Key: "/decisions/rollout-a", Body: body, Writer: "t", Importance: 0.0,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: "ns", Key: "/decisions/rollout-b", Body: body, Writer: "t", Importance: 0.0,
	}); err != nil {
		t.Fatal(err)
	}

	out := agentContextOutFromBody(t, do(t, srv, "GET", "/v1/agent/context?ns=ns&q=canary", ""))
	if strings.Contains(out.Context, "agent-sessions") {
		t.Fatalf("q path re-admitted an /agent-sessions/ capture: %q", out.Context)
	}
	if !strings.Contains(out.Context, "/decisions/rollout-a") || !strings.Contains(out.Context, "/decisions/rollout-b") {
		t.Fatalf("durable facts ranked below the 20-hit cap were lost entirely: %q fact_ids=%v", out.Context, out.FactIDs)
	}
}

// TestAgentContextImportanceFirstBucketing proves the ranked pool sorts
// important facts (Importance >= 0.5) ahead of unimportant ones even when
// recency alone would rank them in the opposite order: seed the important
// fact first (older) and the unimportant one second (newer), and confirm
// the important fact still lands first in fact_ids under a budget that
// only has room for one.
func TestAgentContextImportanceFirstBucketing(t *testing.T) {
	srv := testServer(t)
	ctx := context.Background()
	important, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: "ns", Key: "/notes/important", Body: "important older fact", Writer: "t", Importance: 0.8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: "ns", Key: "/notes/unimportant", Body: "unimportant newer fact", Writer: "t", Importance: 0.1,
	}); err != nil {
		t.Fatal(err)
	}

	out := agentContextOutFromBody(t, do(t, srv, "GET", "/v1/agent/context?ns=ns", ""))
	if len(out.FactIDs) < 2 {
		t.Fatalf("expected both facts under the default budget, got %v", out.FactIDs)
	}
	if out.FactIDs[0] != important.ID {
		t.Fatalf("importance bucket did not rank first: fact_ids=%v want important=%s first", out.FactIDs, important.ID)
	}
}

// TestAgentContextPoolCap proves the candidate pool never exceeds the
// spec's 20-fact cap even when a namespace has more than that many
// eligible facts and the token budget is generous enough to fit them all:
// the cap is applied to the pool before budgeting, not derived from it.
// The seed count and the asserted bound are both hardcoded (not
// agentContextPoolCap) so a regression that loosens the constant itself
// still fails this test instead of silently redefining "the cap".
func TestAgentContextPoolCap(t *testing.T) {
	srv := testServer(t)
	ctx := context.Background()
	const seeded = 30 // > the spec's 20-fact pool cap
	for i := 0; i < seeded; i++ {
		if _, err := srv.mem.Write(ctx, memory.WriteInput{
			Namespace: "ns", Key: fmt.Sprintf("/notes/%03d", i), Body: "n", Writer: "t",
		}); err != nil {
			t.Fatal(err)
		}
	}

	out := agentContextOutFromBody(t, do(t, srv, "GET", "/v1/agent/context?ns=ns&max_tokens=100000", ""))
	const wantCap = 20
	if len(out.FactIDs) > wantCap {
		t.Fatalf("pool cap not enforced: got %d fact_ids (seeded %d), want <= %d", len(out.FactIDs), seeded, wantCap)
	}
}

// TestAgentContextEntitiesLineWithinBudget proves fat/oversized entity
// names cannot blow the context out to hundreds of KB even under a
// vanishingly small max_tokens: names are clipped to
// agentContextEntityNameClip runes and capped at 10 by Profile, so the
// entities line - and therefore the whole context, since a fully consumed
// budget yields zero facts - stays bounded regardless of max_tokens.
func TestAgentContextEntitiesLineWithinBudget(t *testing.T) {
	srv := testServer(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if _, err := srv.mem.Write(ctx, memory.WriteInput{
			Namespace: "ns", Key: fmt.Sprintf("/entities/e%02d", i),
			Body:       strings.Repeat(fmt.Sprintf("name%d-", i), 60), // well over 120 runes
			Attributes: map[string]any{"mention_count": float64(10 - i)}, Writer: "enricher",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: "ns", Key: "/notes/a", Body: "short fact body", Writer: "t",
	}); err != nil {
		t.Fatal(err)
	}

	rec := do(t, srv, "GET", "/v1/agent/context?ns=ns&max_tokens=1", "")
	if rec.Code != 200 {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	out := agentContextOutFromBody(t, rec)

	// The docstring's claim: a fully consumed budget (max_tokens=1) must
	// yield zero facts, not merely a small context string.
	if len(out.FactIDs) != 0 {
		t.Fatalf("exhausted budget must yield zero facts, got %v", out.FactIDs)
	}

	// Sane bound per-name (clip + ellipsis) and overall - nowhere near the
	// 200KB an unclipped fat-entities namespace could otherwise produce.
	const sane = 4*1*4 + 3000 // 4x maxTokens*4 bytes + fixed slack for the clipped entities line
	if len(out.Context) > sane {
		t.Fatalf("context far larger than clipped-entities bound: got %d bytes (want <= %d): %q", len(out.Context), sane, out.Context)
	}
	for _, line := range strings.Split(out.Context, "\n") {
		if !strings.HasPrefix(line, "Known entities: ") {
			continue
		}
		names := strings.Split(strings.TrimPrefix(line, "Known entities: "), ", ")
		if len(names) > 10 {
			t.Fatalf("more than 10 entities in the entities line: %d", len(names))
		}
		for _, n := range names {
			if rc := utf8.RuneCountInString(n); rc > agentContextEntityNameClip+3 {
				t.Fatalf("entity name exceeds clip+ellipsis: %q (%d runes)", n, rc)
			}
		}
	}
}

// TestAgentContextEntitiesLineCountsAgainstBudget proves the entities
// line's own token cost is subtracted from the budget passed to fact
// ranking, distinct from name clipping (TestAgentContextEntitiesLineWithinBudget):
// a fact that would fit under max_tokens on its own must not survive once
// the entities line's cost is taken into account.
func TestAgentContextEntitiesLineCountsAgainstBudget(t *testing.T) {
	srv := testServer(t)
	ctx := context.Background()
	names := []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo"}
	for i, n := range names {
		if _, err := srv.mem.Write(ctx, memory.WriteInput{
			Namespace: "ns", Key: fmt.Sprintf("/entities/e%d", i), Body: n,
			Attributes: map[string]any{"mention_count": float64(len(names) - i)}, Writer: "enricher",
		}); err != nil {
			t.Fatal(err)
		}
	}
	factBody := strings.Repeat("word ", 20)
	if _, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: "ns", Key: "/notes/a", Body: factBody, Writer: "t",
	}); err != nil {
		t.Fatal(err)
	}

	header := "## Project memory (ns)\n"
	entitiesLine := "Known entities: " + strings.Join(names, ", ") + "\n"
	entitiesCost := memory.EstimateTokens(header + entitiesLine)
	factCost := memory.EstimateTokens(factBody)

	// Covers the fact on its own (maxTokens > factCost), but not once the
	// entities line's cost is subtracted first (maxTokens - entitiesCost <
	// factCost).
	maxTokens := entitiesCost + factCost - 1

	rec := do(t, srv, "GET", fmt.Sprintf("/v1/agent/context?ns=ns&max_tokens=%d", maxTokens), "")
	if rec.Code != 200 {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	out := agentContextOutFromBody(t, rec)
	if !strings.Contains(out.Context, "Known entities:") {
		t.Fatalf("entities line missing: %q", out.Context)
	}
	if len(out.FactIDs) != 0 {
		t.Fatalf("expected the entities line's cost to price the fact out of the remaining budget, got fact_ids=%v context=%q", out.FactIDs, out.Context)
	}
}

// turnTestServer is testServer with per-turn injection enabled and an
// optional inject component list - the Deps knobs the context-injection
// features ride in on.
func turnTestServer(t *testing.T, turnTokens int, inject []string) *Server {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	var clkMu sync.Mutex
	clk := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	now := func() time.Time {
		clkMu.Lock()
		defer clkMu.Unlock()
		clk = clk.Add(time.Millisecond)
		return clk
	}
	return New(testLogger(), Deps{Memory: memory.New(db, now), TurnContextTokens: turnTokens, Inject: inject})
}

// TestAgentTurnContext pins the mode=turn path: a
// prompt-scoped hybrid-search block under its own "## Relevant memory"
// header, empty when the feature is disabled or q is absent, and deduped
// against fact IDs already injected into the same session.
func TestAgentTurnContext(t *testing.T) {
	srv := turnTestServer(t, 600, nil)
	ctx := context.Background()
	if _, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: "agent-myproj", Key: "/decisions/auth", Body: "auth uses jwt via jose",
		Author: "test", Writer: "test", Importance: 0.8,
	}); err != nil {
		t.Fatal(err)
	}

	// Turn fetch without sid: the matching fact comes back under the turn
	// header, not the session header.
	rec := do(t, srv, "GET", "/v1/agent/context?cwd=/home/u/myproj&mode=turn&q=jwt", "")
	if rec.Code != 200 {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	out := agentContextOutFromBody(t, rec)
	if !strings.Contains(out.Context, "## Relevant memory") || !strings.Contains(out.Context, "auth uses jwt") {
		t.Fatalf("turn context = %q", out.Context)
	}
	if strings.Contains(out.Context, "## Project memory") {
		t.Fatalf("turn context must not use the session header: %q", out.Context)
	}
	if len(out.FactIDs) != 1 {
		t.Fatalf("fact_ids = %v", out.FactIDs)
	}

	// No q: empty context, never an error.
	rec = do(t, srv, "GET", "/v1/agent/context?cwd=/home/u/myproj&mode=turn", "")
	if out := agentContextOutFromBody(t, rec); rec.Code != 200 || out.Context != "" {
		t.Fatalf("no-q turn: %d %q", rec.Code, out.Context)
	}
}

// TestAgentTurnContextDisabled pins that TurnContextTokens == 0 keeps
// mode=turn a no-op (empty context, 200) even when facts would match.
func TestAgentTurnContextDisabled(t *testing.T) {
	srv := turnTestServer(t, 0, nil)
	ctx := context.Background()
	if _, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: "agent-myproj", Key: "/decisions/auth", Body: "auth uses jwt via jose",
		Author: "test", Writer: "test",
	}); err != nil {
		t.Fatal(err)
	}
	rec := do(t, srv, "GET", "/v1/agent/context?cwd=/home/u/myproj&mode=turn&q=jwt", "")
	if out := agentContextOutFromBody(t, rec); rec.Code != 200 || out.Context != "" {
		t.Fatalf("disabled turn: %d %q", rec.Code, out.Context)
	}
}

// TestAgentTurnContextDedup pins the injected-IDs bookkeeping: a
// session-start fetch carrying sid records what it injected, and a later
// mode=turn fetch for the same sid never re-injects those facts; a turn
// fetch's own injections dedup the next turn the same way.
func TestAgentTurnContextDedup(t *testing.T) {
	srv := turnTestServer(t, 600, nil)
	ctx := context.Background()
	if _, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: "agent-myproj", Key: "/decisions/auth", Body: "auth uses jwt via jose",
		Author: "test", Writer: "test", Importance: 0.8,
	}); err != nil {
		t.Fatal(err)
	}

	// Session start with sid: fact injected and recorded.
	rec := do(t, srv, "GET", "/v1/agent/context?cwd=/home/u/myproj&sid=s1", "")
	out := agentContextOutFromBody(t, rec)
	if rec.Code != 200 || len(out.FactIDs) != 1 {
		t.Fatalf("session start: %d %v", rec.Code, out.FactIDs)
	}

	// Same session's turn fetch: the already-injected fact is filtered, so
	// the context is empty.
	rec = do(t, srv, "GET", "/v1/agent/context?cwd=/home/u/myproj&mode=turn&q=jwt&sid=s1", "")
	if out := agentContextOutFromBody(t, rec); rec.Code != 200 || out.Context != "" {
		t.Fatalf("deduped turn: %d %q", rec.Code, out.Context)
	}

	// A different session has no such bookkeeping: the fact comes back.
	rec = do(t, srv, "GET", "/v1/agent/context?cwd=/home/u/myproj&mode=turn&q=jwt&sid=s2", "")
	out = agentContextOutFromBody(t, rec)
	if rec.Code != 200 || !strings.Contains(out.Context, "auth uses jwt") {
		t.Fatalf("other-session turn: %d %q", rec.Code, out.Context)
	}
	// ...and that turn's own injection dedups the next fetch in s2.
	rec = do(t, srv, "GET", "/v1/agent/context?cwd=/home/u/myproj&mode=turn&q=jwt&sid=s2", "")
	if out := agentContextOutFromBody(t, rec); rec.Code != 200 || out.Context != "" {
		t.Fatalf("second turn in s2: %d %q", rec.Code, out.Context)
	}
}

// TestAgentContextInjectComponents pins the memory.inject knob: the
// directives line appears only when asked for, "entities"/"facts" can be
// dropped individually, and layout order stays directives-entities-facts.
func TestAgentContextInjectComponents(t *testing.T) {
	srv := turnTestServer(t, 0, []string{"directives", "facts"})
	ctx := context.Background()
	if _, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: "ns", Key: "/entities/pg", Body: `{"name":"pg-1"}`,
		Author: "test", Writer: "test",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: "ns", Key: "/decisions/auth", Body: "auth uses jwt",
		Author: "test", Writer: "test", Importance: 0.8,
	}); err != nil {
		t.Fatal(err)
	}

	rec := do(t, srv, "GET", "/v1/agent/context?ns=ns", "")
	out := agentContextOutFromBody(t, rec)
	if rec.Code != 200 {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(out.Context, "background knowledge") {
		t.Fatalf("directives line missing: %q", out.Context)
	}
	if strings.Contains(out.Context, "Known entities:") {
		t.Fatalf("entities line present despite not being in inject: %q", out.Context)
	}
	if !strings.Contains(out.Context, "auth uses jwt") {
		t.Fatalf("facts missing: %q", out.Context)
	}
}

// TestAgentContextProfileCard pins the profile component: facts under /profile/ in the global ProfileNamespace
// appear in EVERY project namespace's session block under "About the
// user", and an empty card leaves the block untouched.
func TestAgentContextProfileCard(t *testing.T) {
	srv := turnTestServer(t, 0, nil) // default inject includes profile
	ctx := context.Background()
	if _, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: ProfileNamespace, Key: "/profile/tz", Body: "works from IST",
		Author: "user", Writer: "card-cli",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: "agent-proja", Key: "/decisions/x", Body: "uses sqlite",
		Author: "t", Writer: "t",
	}); err != nil {
		t.Fatal(err)
	}

	rec := do(t, srv, "GET", "/v1/agent/context?cwd=/home/u/proja", "")
	out := agentContextOutFromBody(t, rec)
	if rec.Code != 200 || !strings.Contains(out.Context, "About the user:\n- works from IST") {
		t.Fatalf("profile card missing: %d %q", rec.Code, out.Context)
	}
	// A different project gets the same card.
	rec = do(t, srv, "GET", "/v1/agent/context?cwd=/home/u/projb", "")
	out = agentContextOutFromBody(t, rec)
	if !strings.Contains(out.Context, "works from IST") {
		t.Fatalf("card must be cross-project: %q", out.Context)
	}
	// Card-only context still counts as non-empty.
	if out.Context == "" {
		t.Fatal("card-only context must not be blanked")
	}
}

// TestAgentContextSummaryComponent pins the "summary" inject component:
// the latest session's rolling summary appears as a "Last session:"
// line even though /agent-sessions/ is excluded from the fact pool.
func TestAgentContextSummaryComponent(t *testing.T) {
	srv := turnTestServer(t, 0, nil) // default inject includes summary
	ctx := context.Background()
	if _, err := srv.mem.Write(ctx, memory.WriteInput{
		Namespace: "agent-proj", Key: "/agent-sessions/s1/summary",
		Body:   "shipped per-turn injection; consolidation triggers still open",
		Writer: "session-summary",
	}); err != nil {
		t.Fatal(err)
	}
	rec := do(t, srv, "GET", "/v1/agent/context?ns=agent-proj", "")
	out := agentContextOutFromBody(t, rec)
	if rec.Code != 200 || !strings.Contains(out.Context, "Last session: shipped per-turn injection") {
		t.Fatalf("summary line missing: %d %q", rec.Code, out.Context)
	}
	if len(out.FactIDs) != 0 {
		t.Fatalf("summary must not appear as a fact bullet: %v", out.FactIDs)
	}
}
