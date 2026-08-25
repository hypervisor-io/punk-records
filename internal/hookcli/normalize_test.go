package hookcli

import (
	"encoding/json"
	"strings"
	"testing"
)

// claudeEnvelopeOut mirrors the server's agentHookIn tags (internal/api/
// agent_handlers.go) so tests can decode Normalize's output and assert on
// actual field values, not just "no error".
type claudeEnvelopeOut struct {
	HookEventName        string          `json:"hook_event_name"`
	SessionID            string          `json:"session_id"`
	CWD                  string          `json:"cwd"`
	Source               string          `json:"source"`
	Prompt               string          `json:"prompt"`
	PromptID             string          `json:"prompt_id"`
	ToolName             string          `json:"tool_name"`
	ToolInput            json.RawMessage `json:"tool_input"`
	ToolResponse         json.RawMessage `json:"tool_response"`
	ToolUseID            string          `json:"tool_use_id"`
	LastAssistantMessage string          `json:"last_assistant_message"`
}

func decodeEnvelope(t *testing.T, raw []byte) claudeEnvelopeOut {
	t.Helper()
	var env claudeEnvelopeOut
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v: %s", err, raw)
	}
	return env
}

// TestNormalizeCursorSessionStart: sessionStart carries no per-event cwd
// field (cursor.com/docs/hooks lists only session_id, is_background_agent,
// composer_mode as sessionStart-specific), so cwd must come from the base
// workspace_roots field. session_id in the translated envelope must be
// conversation_id, not the sessionStart-only "session_id" field: that field
// docs.com only guarantees on sessionStart/sessionEnd, while conversation_id
// is documented as present on every hook event, so it is the only field
// that can key a session consistently across a translated PostToolUse/Stop
// pair from the same conversation.
func TestNormalizeCursorSessionStart(t *testing.T) {
	raw := []byte(`{
		"hook_event_name": "sessionStart",
		"conversation_id": "conv-abc123",
		"session_id": "ide-session-xyz",
		"workspace_roots": ["/home/u/myproj"],
		"is_background_agent": false
	}`)
	out, ok, err := Normalize("cursor", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("sessionStart must be captured (ok=true)")
	}
	env := decodeEnvelope(t, out)
	if env.HookEventName != "SessionStart" {
		t.Fatalf("hook_event_name = %q, want SessionStart", env.HookEventName)
	}
	if env.SessionID != "conv-abc123" {
		t.Fatalf("session_id = %q, want conversation_id value conv-abc123 (not the sessionStart-only session_id field)", env.SessionID)
	}
	if env.CWD != "/home/u/myproj" {
		t.Fatalf("cwd = %q, want workspace_roots[0]", env.CWD)
	}
	if env.Source != "cursor" {
		t.Fatalf("source = %q, want cursor", env.Source)
	}
}

// TestNormalizeCursorBeforeSubmitPrompt pins the beforeSubmitPrompt ->
// UserPromptSubmit mapping, the exact prompt text passthrough, and -
// critically - that prompt_id is sourced from Cursor's per-generation
// generation_id base field. Without prompt_id, the server's
// UserPromptSubmit case (agent_handlers.go's sanitizeID(in.PromptID) ==
// "" gate) drops the whole event as "ignored", so every translated Cursor
// prompt would silently vanish.
func TestNormalizeCursorBeforeSubmitPrompt(t *testing.T) {
	raw := []byte(`{
		"hook_event_name": "beforeSubmitPrompt",
		"conversation_id": "conv-1",
		"generation_id": "gen-xyz-1",
		"workspace_roots": ["/p"],
		"prompt": "fix the flaky test in scheduler_test.go"
	}`)
	out, ok, err := Normalize("cursor", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("beforeSubmitPrompt must be captured")
	}
	env := decodeEnvelope(t, out)
	if env.HookEventName != "UserPromptSubmit" {
		t.Fatalf("hook_event_name = %q, want UserPromptSubmit", env.HookEventName)
	}
	if env.Prompt != "fix the flaky test in scheduler_test.go" {
		t.Fatalf("prompt = %q", env.Prompt)
	}
	if env.PromptID != "gen-xyz-1" {
		t.Fatalf("prompt_id = %q, want generation_id value gen-xyz-1", env.PromptID)
	}
	if env.SessionID != "conv-1" || env.CWD != "/p" {
		t.Fatalf("session_id/cwd not carried through: %+v", env)
	}
}

// TestNormalizeCursorBeforeSubmitPromptFallsBackWhenGenerationIDMissing:
// when a payload genuinely has no generation_id, the translator must still
// synthesize a non-empty, deterministic prompt_id (fnv32a over
// conversation_id + prompt) rather than leave the prompt uncaptured. Two
// distinct prompts in the same conversation must not collide.
func TestNormalizeCursorBeforeSubmitPromptFallsBackWhenGenerationIDMissing(t *testing.T) {
	raw := []byte(`{
		"hook_event_name": "beforeSubmitPrompt",
		"conversation_id": "conv-2",
		"workspace_roots": ["/p"],
		"prompt": "first prompt"
	}`)
	out, ok, err := Normalize("cursor", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("beforeSubmitPrompt with no generation_id must still be captured")
	}
	env := decodeEnvelope(t, out)
	if env.PromptID == "" {
		t.Fatal("prompt_id must be synthesized, not left empty, when generation_id is absent")
	}
	wantID := promptIDFallback("conv-2", "first prompt")
	if env.PromptID != wantID {
		t.Fatalf("prompt_id = %q, want deterministic fallback %q", env.PromptID, wantID)
	}

	// Distractor: a different prompt in the same conversation must not
	// collide, and re-deriving the same inputs must be stable.
	otherRaw := []byte(`{
		"hook_event_name": "beforeSubmitPrompt",
		"conversation_id": "conv-2",
		"workspace_roots": ["/p"],
		"prompt": "second, different prompt"
	}`)
	otherOut, _, err := Normalize("cursor", otherRaw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	otherEnv := decodeEnvelope(t, otherOut)
	if otherEnv.PromptID == env.PromptID {
		t.Fatalf("different prompt text must not collide on prompt_id: both %q", env.PromptID)
	}
	if promptIDFallback("conv-2", "first prompt") != wantID {
		t.Fatal("promptIDFallback must be deterministic across calls")
	}
}

// TestNormalizeCursorPostToolUse pins field-for-field renaming: Cursor's
// tool_output becomes the server's tool_response (the server has no
// tool_output tag at all - internal/api/agent_handlers.go:21-33), and
// tool_input/tool_name/tool_use_id pass through unchanged.
func TestNormalizeCursorPostToolUse(t *testing.T) {
	raw := []byte(`{
		"hook_event_name": "postToolUse",
		"conversation_id": "conv-2",
		"cwd": "/repo",
		"tool_name": "read_file",
		"tool_input": {"path":"main.go"},
		"tool_output": {"content":"package main"},
		"tool_use_id": "tu-77"
	}`)
	out, ok, err := Normalize("cursor", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("postToolUse must be captured")
	}
	env := decodeEnvelope(t, out)
	if env.HookEventName != "PostToolUse" {
		t.Fatalf("hook_event_name = %q, want PostToolUse", env.HookEventName)
	}
	if env.ToolName != "read_file" {
		t.Fatalf("tool_name = %q", env.ToolName)
	}
	if env.ToolUseID != "tu-77" {
		t.Fatalf("tool_use_id = %q", env.ToolUseID)
	}
	if strings.TrimSpace(string(env.ToolInput)) != `{"path":"main.go"}` {
		t.Fatalf("tool_input = %s", env.ToolInput)
	}
	if strings.TrimSpace(string(env.ToolResponse)) != `{"content":"package main"}` {
		t.Fatalf("tool_response (from cursor's tool_output) = %s", env.ToolResponse)
	}
	// cwd's own event-specific field must win over workspace_roots when
	// both would otherwise be candidates.
	if env.CWD != "/repo" {
		t.Fatalf("cwd = %q, want event-specific cwd field /repo", env.CWD)
	}
}

// TestNormalizeCursorAfterFileEdit: afterFileEdit has no tool_name,
// tool_input, or tool_use_id of its own (cursor.com/docs/hooks: file_path,
// edits only), so the translator must synthesize all three. tool_use_id
// must be deterministic (same file_path -> same id) so the server's
// sanitizeID-non-empty gate (internal/api/agent_handlers.go:163-166) never
// silently drops every translated file edit, and so retranslating the same
// event twice produces the same capture key rather than a new one each
// time.
func TestNormalizeCursorAfterFileEdit(t *testing.T) {
	raw := []byte(`{
		"hook_event_name": "afterFileEdit",
		"conversation_id": "conv-3",
		"workspace_roots": ["/repo"],
		"file_path": "/repo/internal/foo/foo.go",
		"edits": [{"old_string":"a","new_string":"b"}]
	}`)
	out, ok, err := Normalize("cursor", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("afterFileEdit must be captured")
	}
	env := decodeEnvelope(t, out)
	if env.HookEventName != "PostToolUse" {
		t.Fatalf("hook_event_name = %q, want PostToolUse", env.HookEventName)
	}
	if env.ToolName != "Edit" {
		t.Fatalf("tool_name = %q, want Edit", env.ToolName)
	}
	var input struct {
		FilePath string `json:"file_path"`
		Edits    []struct {
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		} `json:"edits"`
	}
	if err := json.Unmarshal(env.ToolInput, &input); err != nil {
		t.Fatalf("tool_input not decodable: %v: %s", err, env.ToolInput)
	}
	if input.FilePath != "/repo/internal/foo/foo.go" {
		t.Fatalf("tool_input.file_path = %q", input.FilePath)
	}
	// The whole point of afterFileEdit capture is the change content -
	// without edits carried through, the stored fact has a path and
	// nothing else.
	if len(input.Edits) != 1 || input.Edits[0].OldString != "a" || input.Edits[0].NewString != "b" {
		t.Fatalf("tool_input.edits not carried through: %+v (raw %s)", input.Edits, env.ToolInput)
	}
	if env.ToolUseID == "" {
		t.Fatal("tool_use_id must be non-empty (server drops PostToolUse with no tool_use_id)")
	}
	wantID := fileEditToolUseID("/repo/internal/foo/foo.go")
	if env.ToolUseID != wantID {
		t.Fatalf("tool_use_id = %q, want deterministic %q", env.ToolUseID, wantID)
	}

	// Distractor: a different file_path must produce a different id, and
	// re-deriving from the same file_path a second time must be stable.
	otherRaw := []byte(`{
		"hook_event_name": "afterFileEdit",
		"conversation_id": "conv-3",
		"workspace_roots": ["/repo"],
		"file_path": "/repo/internal/bar/bar.go",
		"edits": [{"old_string":"x","new_string":"y"}]
	}`)
	otherOut, _, err := Normalize("cursor", otherRaw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	otherEnv := decodeEnvelope(t, otherOut)
	if otherEnv.ToolUseID == env.ToolUseID {
		t.Fatalf("different file_path must not collide on tool_use_id: both %q", env.ToolUseID)
	}
	if fileEditToolUseID("/repo/internal/foo/foo.go") != wantID {
		t.Fatal("fileEditToolUseID must be deterministic across calls")
	}
}

// TestNormalizeCursorStop: Cursor's stop event carries only status/
// loop_count (cursor.com/docs/agent/hooks) - there is no assistant-message
// text to populate last_assistant_message verbatim, but leaving it empty
// would store a Stop fact with no content at all. status is populated
// into last_assistant_message so the terminal fact carries the loop's
// outcome.
func TestNormalizeCursorStop(t *testing.T) {
	raw := []byte(`{
		"hook_event_name": "stop",
		"conversation_id": "conv-4",
		"workspace_roots": ["/p"],
		"status": "completed",
		"loop_count": 3
	}`)
	out, ok, err := Normalize("cursor", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("stop must be captured")
	}
	env := decodeEnvelope(t, out)
	if env.HookEventName != "Stop" {
		t.Fatalf("hook_event_name = %q, want Stop", env.HookEventName)
	}
	if env.SessionID != "conv-4" {
		t.Fatalf("session_id = %q", env.SessionID)
	}
	if env.LastAssistantMessage != "status=completed" {
		t.Fatalf("last_assistant_message = %q, want status=completed", env.LastAssistantMessage)
	}
}

// TestNormalizeCursorSessionEnd: cursor.com/docs/hooks documents sessionEnd
// as a distinct event from stop (session_id, reason, duration_ms,
// final_status, error_message vs stop's status/loop_count) but both are
// terminal-of-conversation events with no other Claude Code equivalent, so
// sessionEnd is mapped to Stop too (see the comment on this case in
// normalize.go for the full rationale).
func TestNormalizeCursorSessionEnd(t *testing.T) {
	raw := []byte(`{
		"hook_event_name": "sessionEnd",
		"conversation_id": "conv-5",
		"session_id": "ide-session-999",
		"workspace_roots": ["/p"],
		"reason": "user_closed",
		"final_status": "completed"
	}`)
	out, ok, err := Normalize("cursor", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("sessionEnd must be captured")
	}
	env := decodeEnvelope(t, out)
	if env.HookEventName != "Stop" {
		t.Fatalf("hook_event_name = %q, want Stop (sessionEnd mapped to Stop)", env.HookEventName)
	}
	if env.SessionID != "conv-5" {
		t.Fatalf("session_id = %q, want conversation_id conv-5", env.SessionID)
	}
	// reason+final_status must land in last_assistant_message so the
	// terminal fact carries content instead of an empty body.
	if env.LastAssistantMessage != "reason=user_closed final_status=completed" {
		t.Fatalf("last_assistant_message = %q, want reason=user_closed final_status=completed", env.LastAssistantMessage)
	}
}

// TestNormalizeCursorUnmappedEventSkips is the distractor case: a real
// Cursor event name (preToolUse) that this task does not map must skip
// silently (ok=false, no error), not error out and not fabricate an
// envelope.
func TestNormalizeCursorUnmappedEventSkips(t *testing.T) {
	raw := []byte(`{
		"hook_event_name": "preToolUse",
		"conversation_id": "conv-6",
		"tool_name": "read_file",
		"tool_input": {"path":"x"}
	}`)
	out, ok, err := Normalize("cursor", raw)
	if err != nil {
		t.Fatalf("unmapped event must not error: %v", err)
	}
	if ok {
		t.Fatal("unmapped event must report ok=false")
	}
	if out != nil {
		t.Fatalf("unmapped event must return nil bytes, got %s", out)
	}
}

// TestNormalizeCursorMalformedJSON: bad JSON from the agent must error
// (there is nothing valid to translate), not panic and not silently skip.
func TestNormalizeCursorMalformedJSON(t *testing.T) {
	_, ok, err := Normalize("cursor", []byte(`{not valid`))
	if err == nil {
		t.Fatal("malformed cursor payload must return an error")
	}
	if ok {
		t.Fatal("malformed payload must report ok=false")
	}
}

// TestNormalizeUnknownAgentErrors: an agent name with no registered
// translator must error, per the task spec ("Unknown agent name = error").
func TestNormalizeUnknownAgentErrors(t *testing.T) {
	_, ok, err := Normalize("some-made-up-agent", []byte(`{}`))
	if err == nil {
		t.Fatal("unknown agent must return an error")
	}
	if ok {
		t.Fatal("unknown agent must report ok=false")
	}
}

// TestNormalizeAgentNameIsCaseInsensitive: the registry lookup must
// lowercase "from" before matching, so "Cursor"/"CURSOR" resolve the same
// translator as "cursor" rather than erroring as an unknown agent purely
// because of letter case.
func TestNormalizeAgentNameIsCaseInsensitive(t *testing.T) {
	raw := []byte(`{
		"hook_event_name": "sessionStart",
		"conversation_id": "conv-ci",
		"workspace_roots": ["/p"]
	}`)
	for _, from := range []string{"Cursor", "CURSOR", "CuRsOr"} {
		out, ok, err := Normalize(from, raw)
		if err != nil {
			t.Fatalf("from=%q: unexpected error: %v", from, err)
		}
		if !ok {
			t.Fatalf("from=%q: must be captured", from)
		}
		env := decodeEnvelope(t, out)
		if env.Source != "cursor" {
			t.Fatalf("from=%q: source = %q, want cursor", from, env.Source)
		}
	}
}
