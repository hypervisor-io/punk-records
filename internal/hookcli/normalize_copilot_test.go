package hookcli

import (
	"strings"
	"testing"
)

// TestNormalizeCopilotSessionStart pins the SessionStart -> SessionStart
// mapping and, critically, that env.Source is hardcoded "copilot" rather
// than copied from Copilot's own sessionStart "source" field
// ("startup"|"resume"|"new") - that field is a REASON the session started,
// not an agent identity, the same collision Claude Code's own SessionStart
// "source" enum documents (claudeSessionStartSourceReasons). Using it
// verbatim as env.Source would corrupt provenance the same way.
func TestNormalizeCopilotSessionStart(t *testing.T) {
	raw := []byte(`{
		"hook_event_name": "SessionStart",
		"session_id": "sess-abc123",
		"timestamp": "2026-08-01T12:00:00Z",
		"cwd": "/home/u/myproj",
		"source": "startup"
	}`)
	out, ok, err := Normalize("copilot", raw)
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
	if env.SessionID != "sess-abc123" {
		t.Fatalf("session_id = %q, want sess-abc123", env.SessionID)
	}
	if env.CWD != "/home/u/myproj" {
		t.Fatalf("cwd = %q", env.CWD)
	}
	if env.Source != "copilot" {
		t.Fatalf("source = %q, want hardcoded copilot (not Copilot's own sessionStart \"source\" reason field)", env.Source)
	}
}

// TestNormalizeCopilotUserPromptSubmit pins the prompt passthrough and -
// critically - that prompt_id is synthesized (Copilot's userPromptSubmitted/
// UserPromptSubmit payload carries no id field at all: session_id,
// timestamp, cwd, prompt only, per docs.github.com/en/copilot/reference/
// hooks-reference's own schema block). Without prompt_id, the server's
// UserPromptSubmit case (agent_handlers.go's sanitizeID(in.PromptID) == ""
// gate) drops the whole event as "ignored".
func TestNormalizeCopilotUserPromptSubmit(t *testing.T) {
	raw := []byte(`{
		"hook_event_name": "UserPromptSubmit",
		"session_id": "sess-1",
		"cwd": "/p",
		"prompt": "fix the flaky test in scheduler_test.go"
	}`)
	out, ok, err := Normalize("copilot", raw)
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
	if env.Prompt != "fix the flaky test in scheduler_test.go" {
		t.Fatalf("prompt = %q", env.Prompt)
	}
	if env.PromptID == "" {
		t.Fatal("prompt_id must be synthesized, not left empty - Copilot's payload has no id field")
	}
	wantID := promptIDFallback("sess-1", "fix the flaky test in scheduler_test.go")
	if env.PromptID != wantID {
		t.Fatalf("prompt_id = %q, want deterministic fallback %q", env.PromptID, wantID)
	}

	// Distractor: a different prompt in the same session must not collide.
	otherRaw := []byte(`{
		"hook_event_name": "UserPromptSubmit",
		"session_id": "sess-1",
		"cwd": "/p",
		"prompt": "a second, different prompt"
	}`)
	otherOut, _, err := Normalize("copilot", otherRaw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	otherEnv := decodeEnvelope(t, otherOut)
	if otherEnv.PromptID == env.PromptID {
		t.Fatalf("different prompt text must not collide on prompt_id: both %q", env.PromptID)
	}
}

// TestNormalizeCopilotPostToolUse pins field renaming: Copilot's
// tool_result.text_result_for_llm becomes the content inside the server's
// tool_response, tool_input/tool_name pass through unchanged, and -
// critically - tool_use_id is synthesized (Copilot's postToolUse payload
// carries no call-identifying id anywhere in its documented schema).
func TestNormalizeCopilotPostToolUse(t *testing.T) {
	raw := []byte(`{
		"hook_event_name": "PostToolUse",
		"session_id": "sess-2",
		"timestamp": "2026-08-01T12:00:01.000Z",
		"cwd": "/repo",
		"tool_name": "bash",
		"tool_input": {"command":"ls"},
		"tool_result": {"result_type":"success","text_result_for_llm":"file1\nfile2"}
	}`)
	out, ok, err := Normalize("copilot", raw)
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
	if env.ToolName != "bash" {
		t.Fatalf("tool_name = %q", env.ToolName)
	}
	if strings.TrimSpace(string(env.ToolInput)) != `{"command":"ls"}` {
		t.Fatalf("tool_input = %s", env.ToolInput)
	}
	if !strings.Contains(string(env.ToolResponse), "file1") {
		t.Fatalf("tool_response must carry text_result_for_llm content, got %s", env.ToolResponse)
	}
	if env.ToolUseID == "" {
		t.Fatal("tool_use_id must be synthesized, not left empty - Copilot's payload has no id field")
	}
	wantID := copilotToolUseID("sess-2", "bash", "2026-08-01T12:00:01.000Z", []byte(`{"command":"ls"}`))
	if env.ToolUseID != wantID {
		t.Fatalf("tool_use_id = %q, want deterministic %q", env.ToolUseID, wantID)
	}

	// Distractor: a second, distinct call (different timestamp) in the
	// same session with IDENTICAL tool_name/tool_input must not collide -
	// two runs of the same shell command are two separate captures, not
	// one that silently overwrites the other.
	secondRaw := []byte(`{
		"hook_event_name": "PostToolUse",
		"session_id": "sess-2",
		"timestamp": "2026-08-01T12:00:05.000Z",
		"cwd": "/repo",
		"tool_name": "bash",
		"tool_input": {"command":"ls"},
		"tool_result": {"result_type":"success","text_result_for_llm":"file1\nfile2"}
	}`)
	secondOut, _, err := Normalize("copilot", secondRaw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	secondEnv := decodeEnvelope(t, secondOut)
	if secondEnv.ToolUseID == env.ToolUseID {
		t.Fatalf("two distinct calls with the same tool_name/tool_input at different timestamps must not collide on tool_use_id: both %q", env.ToolUseID)
	}
}

// TestNormalizeCopilotPostToolUseMissingToolResult: a postToolUse payload
// with no tool_result at all (should not happen per the docs, but must not
// panic) still produces a valid, non-empty envelope.
func TestNormalizeCopilotPostToolUseMissingToolResult(t *testing.T) {
	raw := []byte(`{
		"hook_event_name": "PostToolUse",
		"session_id": "sess-2b",
		"timestamp": "2026-08-01T12:00:00Z",
		"cwd": "/repo",
		"tool_name": "bash",
		"tool_input": {"command":"ls"}
	}`)
	out, ok, err := Normalize("copilot", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("PostToolUse must still be captured with no tool_result")
	}
	env := decodeEnvelope(t, out)
	if env.ToolUseID == "" {
		t.Fatal("tool_use_id must still be synthesized")
	}
}

// TestNormalizeCopilotStop: agentStop/Stop carries no assistant-message
// text anywhere in its documented payload, so stop_reason and
// stop_hook_active must be packed into last_assistant_message rather than
// leaving the stored fact's body empty.
func TestNormalizeCopilotStop(t *testing.T) {
	raw := []byte(`{
		"hook_event_name": "Stop",
		"session_id": "sess-3",
		"cwd": "/repo",
		"transcript_path": "/tmp/transcript.json",
		"stop_reason": "end_turn",
		"stop_hook_active": true
	}`)
	out, ok, err := Normalize("copilot", raw)
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
	if !strings.Contains(env.LastAssistantMessage, "end_turn") {
		t.Fatalf("last_assistant_message must carry stop_reason, got %q", env.LastAssistantMessage)
	}
	if !strings.Contains(env.LastAssistantMessage, "true") {
		t.Fatalf("last_assistant_message must carry stop_hook_active, got %q", env.LastAssistantMessage)
	}
}

// TestNormalizeCopilotSessionEnd: sessionEnd, like Cursor's own sessionEnd,
// also maps to the Claude-shaped Stop event (a session can terminate -
// "abort"/"timeout"/"user_exit" - without an intervening agentStop having
// fired), and must carry its own reason into last_assistant_message so the
// terminal fact is never empty.
func TestNormalizeCopilotSessionEnd(t *testing.T) {
	raw := []byte(`{
		"hook_event_name": "SessionEnd",
		"session_id": "sess-4",
		"cwd": "/repo",
		"reason": "user_exit"
	}`)
	out, ok, err := Normalize("copilot", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("SessionEnd must be captured")
	}
	env := decodeEnvelope(t, out)
	if env.HookEventName != "Stop" {
		t.Fatalf("hook_event_name = %q, want Stop (SessionEnd has no direct Claude Code equivalent)", env.HookEventName)
	}
	if !strings.Contains(env.LastAssistantMessage, "user_exit") {
		t.Fatalf("last_assistant_message must carry the sessionEnd reason, got %q", env.LastAssistantMessage)
	}
}

// TestNormalizeCopilotUnmappedEventSkips: PreToolUse (permission gating,
// deliberately never wired - see ConnectCopilot's own doc comment) must be
// skipped, not errored.
func TestNormalizeCopilotUnmappedEventSkips(t *testing.T) {
	raw := []byte(`{
		"hook_event_name": "PreToolUse",
		"session_id": "sess-5",
		"tool_name": "bash",
		"tool_input": {"command":"rm -rf /"}
	}`)
	out, ok, err := Normalize("copilot", raw)
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

// TestNormalizeCopilotMalformedJSON: bad JSON from the agent must error,
// not panic and not silently skip.
func TestNormalizeCopilotMalformedJSON(t *testing.T) {
	_, ok, err := Normalize("copilot", []byte(`{not valid`))
	if err == nil {
		t.Fatal("malformed copilot payload must return an error")
	}
	if ok {
		t.Fatal("malformed payload must report ok=false")
	}
}

// TestNormalizeCopilotAgentNameIsCaseInsensitive mirrors
// TestNormalizeAgentNameIsCaseInsensitive for the copilot registry entry.
func TestNormalizeCopilotAgentNameIsCaseInsensitive(t *testing.T) {
	raw := []byte(`{
		"hook_event_name": "SessionStart",
		"session_id": "sess-ci",
		"cwd": "/p"
	}`)
	for _, from := range []string{"Copilot", "COPILOT", "CoPiLoT"} {
		out, ok, err := Normalize(from, raw)
		if err != nil {
			t.Fatalf("from=%q: unexpected error: %v", from, err)
		}
		if !ok {
			t.Fatalf("from=%q: must be captured", from)
		}
		env := decodeEnvelope(t, out)
		if env.Source != "copilot" {
			t.Fatalf("from=%q: source = %q, want copilot", from, env.Source)
		}
	}
}
