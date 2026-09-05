package hookcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// codexFixture loads one of the captured native Codex 0.153.4 hook payloads
// from testdata/codex-0.153.4/. Those fixtures mirror the upstream
// codex-rs/hooks/src/schema.rs input shapes at rust-v0.153.4 (commit
// 3d2ee51ca2d5db578f328aa75e20aa22c0197c9a): snake_case fields, a Codex
// turn_id extension on every turn-scoped event, and no prompt_id anywhere.
func codexFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "codex-0.153.4", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
}

// TestNormalizeCodexSessionStart pins the SessionStart -> SessionStart
// mapping and that env.Source is hardcoded "codex" rather than copied from
// Codex's own session-start "source" field ("startup"|"resume"|"clear"|
// "compact" per the upstream session_start_source_schema) - that field is a
// REASON the session started, not an agent identity, the same collision
// Copilot's translator documents (TestNormalizeCopilotSessionStart).
// Codex's SessionStart carries no turn_id (upstream
// SessionStartCommandInput has no turn_id field, unlike the turn-scoped
// events) - nothing to map here.
func TestNormalizeCodexSessionStart(t *testing.T) {
	out, ok, err := Normalize("codex", codexFixture(t, "session_start.json"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("SessionStart must be captured (ok=true)")
	}
	env := decodeEnvelope(t, out)
	if env.HookEventName != "SessionStart" {
		t.Fatalf("hook_event_name = %q, want SessionStart", env.HookEventName)
	}
	if env.SessionID != "sess-codex-0153-1" {
		t.Fatalf("session_id = %q, want sess-codex-0153-1", env.SessionID)
	}
	if env.CWD != "/home/u/punkrecords" {
		t.Fatalf("cwd = %q", env.CWD)
	}
	if env.Source != "codex" {
		t.Fatalf("source = %q, want hardcoded codex (not Codex's own session-start \"source\" reason field)", env.Source)
	}
}

// TestNormalizeCodexUserPromptSubmitTurnID is the core defect C01 fixes:
// the native 0.153.4 UserPromptSubmit schema carries turn_id and NO
// prompt_id (upstream UserPromptSubmitCommandInput), while the old
// passthrough (RunFrom routing "codex" to Run) forwarded those bytes
// unchanged and the server silently dropped every Codex prompt as
// "ignored" (agent_handlers.go's UserPromptSubmit case requires a
// non-empty sanitized prompt_id). The normalizer must map turn_id
// verbatim onto prompt_id - the native per-turn id IS the stable capture
// identity - and must never hash prompt text to invent one.
func TestNormalizeCodexUserPromptSubmitTurnID(t *testing.T) {
	out, ok, err := Normalize("codex", codexFixture(t, "user_prompt_submit.json"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("UserPromptSubmit must be captured")
	}
	env := decodeEnvelope(t, out)
	if env.HookEventName != "UserPromptSubmit" {
		t.Fatalf("hook_event_name = %q, want UserPromptSubmit", env.HookEventName)
	}
	if env.Prompt != "normalize native codex hook events" {
		t.Fatalf("prompt = %q", env.Prompt)
	}
	if env.PromptID != "turn-codex-0001" {
		t.Fatalf("prompt_id = %q, want native turn_id verbatim (turn-codex-0001), not a hash or empty", env.PromptID)
	}
	if env.SessionID != "sess-codex-0153-1" {
		t.Fatalf("session_id = %q, want sess-codex-0153-1", env.SessionID)
	}
}

// TestNormalizeCodexUserPromptSubmitPreservesSuppliedPromptID pins the
// "preserve a supplied prompt_id" half of the contract: a payload that
// already carries prompt_id (e.g. a wrapper or a future upstream schema
// that adds one) keeps it verbatim; turn_id is only the fallback, never
// an override.
func TestNormalizeCodexUserPromptSubmitPreservesSuppliedPromptID(t *testing.T) {
	raw := []byte(`{
		"session_id": "sess-x",
		"turn_id": "turn-x",
		"cwd": "/p",
		"hook_event_name": "UserPromptSubmit",
		"model": "gpt-5.1-codex",
		"permission_mode": "default",
		"prompt": "hello",
		"prompt_id": "supplied-id-123"
	}`)
	out, ok, err := Normalize("codex", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("UserPromptSubmit must be captured")
	}
	env := decodeEnvelope(t, out)
	if env.PromptID != "supplied-id-123" {
		t.Fatalf("prompt_id = %q, want the supplied prompt_id preserved verbatim, not replaced by turn_id", env.PromptID)
	}
}

// TestNormalizeCodexUserPromptSubmitNoIdentityStaysEmpty pins the remaining
// boundary: with neither prompt_id nor turn_id the field stays empty (the
// server then reports the event "ignored", fail-open) rather than
// synthesizing an id from the prompt text - hashing prompt text as identity
// would collapse two distinct turns that happen to share text.
func TestNormalizeCodexUserPromptSubmitNoIdentityStaysEmpty(t *testing.T) {
	raw := []byte(`{
		"session_id": "sess-x",
		"cwd": "/p",
		"hook_event_name": "UserPromptSubmit",
		"prompt": "hello"
	}`)
	out, ok, err := Normalize("codex", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("UserPromptSubmit must still map (ok=true); empty identity is the server's call, not the translator's")
	}
	env := decodeEnvelope(t, out)
	if env.PromptID != "" {
		t.Fatalf("prompt_id = %q, want empty: never hash prompt text as identity", env.PromptID)
	}
}

// TestNormalizeCodexPostToolUse pins the PostToolUse passthrough fields:
// Codex's native field names already match the server's envelope
// (tool_name/tool_input/tool_response/tool_use_id, per the upstream
// PostToolUseCommandInput schema), so the translator must carry them
// untouched - tool IDs especially, verbatim.
func TestNormalizeCodexPostToolUse(t *testing.T) {
	out, ok, err := Normalize("codex", codexFixture(t, "post_tool_use.json"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("PostToolUse must be captured")
	}
	env := decodeEnvelope(t, out)
	if env.HookEventName != "PostToolUse" {
		t.Fatalf("hook_event_name = %q, want PostToolUse", env.HookEventName)
	}
	if env.ToolName != "shell" {
		t.Fatalf("tool_name = %q, want shell", env.ToolName)
	}
	if env.ToolUseID != "call-codex-9f8e7d6c" {
		t.Fatalf("tool_use_id = %q, want call-codex-9f8e7d6c preserved verbatim", env.ToolUseID)
	}
	assertJSONEqual(t, env.ToolInput, `{"command": ["git", "status", "--short"]}`, "tool_input")
	assertJSONEqual(t, env.ToolResponse, `{"output": " M internal/hookcli/normalize.go", "exit_code": 0}`, "tool_response")
}

// assertJSONEqual compares got (Normalize's re-encoded raw field) against
// want semantically - field order and whitespace are not load-bearing, the
// decoded value is.
func assertJSONEqual(t *testing.T, got json.RawMessage, want, field string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s not valid JSON: %v: %s", field, err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("test setup: want %s not valid JSON: %v", field, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("%s = %s, want %s", field, got, want)
	}
}

// TestNormalizeCodexStop pins the Stop mapping: Codex's Stop carries a real
// (nullable) last_assistant_message - unlike Cursor/Copilot, there is
// nothing to synthesize, and the message passes through verbatim.
func TestNormalizeCodexStop(t *testing.T) {
	out, ok, err := Normalize("codex", codexFixture(t, "stop.json"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("Stop must be captured")
	}
	env := decodeEnvelope(t, out)
	if env.HookEventName != "Stop" {
		t.Fatalf("hook_event_name = %q, want Stop", env.HookEventName)
	}
	if env.LastAssistantMessage != "Normalized the native Codex hook payloads." {
		t.Fatalf("last_assistant_message = %q", env.LastAssistantMessage)
	}
	if env.SessionID != "sess-codex-0153-1" {
		t.Fatalf("session_id = %q, want sess-codex-0153-1", env.SessionID)
	}
}

// TestNormalizeCodexStopNullAssistantMessage pins the nullable field:
// upstream types last_assistant_message as NullableString, so a literal
// null must decode cleanly to the empty string rather than erroring.
func TestNormalizeCodexStopNullAssistantMessage(t *testing.T) {
	raw := []byte(`{
		"session_id": "sess-x",
		"turn_id": "turn-x",
		"cwd": "/p",
		"hook_event_name": "Stop",
		"stop_hook_active": false,
		"last_assistant_message": null
	}`)
	out, ok, err := Normalize("codex", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("Stop must be captured")
	}
	env := decodeEnvelope(t, out)
	if env.LastAssistantMessage != "" {
		t.Fatalf("last_assistant_message = %q, want empty for a literal null", env.LastAssistantMessage)
	}
}

// TestNormalizeCodexUnmappedEventsSkipped pins that native events with no
// envelope mapping (punk wires only SessionStart/UserPromptSubmit/
// PostToolUse/Stop - see codexHookEvents) are skipped silently, never an
// error, matching Normalize's documented contract for a recognized agent.
func TestNormalizeCodexUnmappedEventsSkipped(t *testing.T) {
	for _, ev := range []string{"PreToolUse", "SessionEnd", "PreCompact", "PostCompact", "Interrupt"} {
		raw := []byte(`{"session_id":"s","cwd":"/p","hook_event_name":"` + ev + `"}`)
		if _, ok, err := Normalize("codex", raw); err != nil || ok {
			t.Fatalf("%s: ok=%v err=%v, want ok=false err=nil", ev, ok, err)
		}
	}
}

// TestNormalizeCodexMalformedPayload is the fail-open boundary: garbage in
// yields an error (noted on errw by the caller) and ok=false, never a
// panic or a partial envelope.
func TestNormalizeCodexMalformedPayload(t *testing.T) {
	if _, ok, err := Normalize("codex", []byte(`{not json`)); err == nil || ok {
		t.Fatalf("ok=%v err=%v, want ok=false err!=nil", ok, err)
	}
}
