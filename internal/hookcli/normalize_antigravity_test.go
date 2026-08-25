package hookcli

import (
	"encoding/json"
	"testing"
)

// TestTranslateAntigravityPreInvocationFirstCallIsSessionStart pins the
// invocationNum==0 -> SessionStart mapping: session_id from conversationId,
// cwd from workspacePaths[0], source "antigravity".
func TestTranslateAntigravityPreInvocationFirstCallIsSessionStart(t *testing.T) {
	raw := []byte(`{
		"invocationNum": 0,
		"initialNumSteps": 0,
		"conversationId": "conv-ag-1",
		"workspacePaths": ["/home/u/agproj"],
		"transcriptPath": "~/.gemini/antigravity/brain/conv-ag-1/.system_generated/logs/transcript.jsonl",
		"artifactDirectoryPath": "~/.gemini/antigravity/brain/conv-ag-1"
	}`)
	out, ok, err := translateAntigravity("PreInvocation", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("invocationNum==0 must be captured (ok=true)")
	}
	env := decodeEnvelope(t, out)
	if env.HookEventName != "SessionStart" {
		t.Fatalf("hook_event_name = %q, want SessionStart", env.HookEventName)
	}
	if env.SessionID != "conv-ag-1" {
		t.Fatalf("session_id = %q, want conv-ag-1", env.SessionID)
	}
	if env.CWD != "/home/u/agproj" {
		t.Fatalf("cwd = %q, want workspacePaths[0]", env.CWD)
	}
	if env.Source != "antigravity" {
		t.Fatalf("source = %q, want antigravity", env.Source)
	}
}

// TestTranslateAntigravityPreInvocationLaterCallSkips is the distractor
// case: invocationNum != 0 (a later model call in the same conversation's
// agentic loop, not the conversation's first) must be skipped, not
// captured as a second SessionStart.
func TestTranslateAntigravityPreInvocationLaterCallSkips(t *testing.T) {
	raw := []byte(`{
		"invocationNum": 3,
		"initialNumSteps": 10,
		"conversationId": "conv-ag-1",
		"workspacePaths": ["/home/u/agproj"]
	}`)
	out, ok, err := translateAntigravity("PreInvocation", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("invocationNum!=0 must not be captured (ok=false)")
	}
	if out != nil {
		t.Fatalf("skipped invocation must return nil bytes, got %s", out)
	}
}

// TestTranslateAntigravityPostToolUse pins the stepIdx-derived tool_use_id,
// the synthesized self-describing tool_name, and error-carrying
// tool_response, and that a successful step (empty error) leaves
// tool_response empty rather than fabricating content.
func TestTranslateAntigravityPostToolUse(t *testing.T) {
	raw := []byte(`{
		"stepIdx": 5,
		"error": "exit status 1",
		"conversationId": "conv-ag-2",
		"workspacePaths": ["/repo"]
	}`)
	out, ok, err := translateAntigravity("PostToolUse", raw)
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
	if env.ToolName != "AntigravityStep" {
		t.Fatalf("tool_name = %q, want the synthesized AntigravityStep placeholder (no real tool name is documented on PostToolUse)", env.ToolName)
	}
	if env.ToolUseID != "step5" {
		t.Fatalf("tool_use_id = %q, want step5", env.ToolUseID)
	}
	if env.SessionID != "conv-ag-2" || env.CWD != "/repo" {
		t.Fatalf("session_id/cwd not carried through: %+v", env)
	}
	var input struct {
		StepIdx int `json:"stepIdx"`
	}
	if err := json.Unmarshal(env.ToolInput, &input); err != nil {
		t.Fatalf("tool_input not decodable: %v: %s", err, env.ToolInput)
	}
	if input.StepIdx != 5 {
		t.Fatalf("tool_input.stepIdx = %d, want 5", input.StepIdx)
	}
	var resp string
	if err := json.Unmarshal(env.ToolResponse, &resp); err != nil {
		t.Fatalf("tool_response not decodable: %v: %s", err, env.ToolResponse)
	}
	if resp != "exit status 1" {
		t.Fatalf("tool_response = %q, want the error message", resp)
	}

	// Distractor: a different stepIdx must produce a different tool_use_id,
	// and a successful (empty-error) step must not fabricate a
	// tool_response.
	okRaw := []byte(`{"stepIdx": 6, "error": "", "conversationId": "conv-ag-2", "workspacePaths": ["/repo"]}`)
	okOut, _, err := translateAntigravity("PostToolUse", okRaw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	okEnv := decodeEnvelope(t, okOut)
	if okEnv.ToolUseID == env.ToolUseID {
		t.Fatalf("different stepIdx must not collide on tool_use_id: both %q", env.ToolUseID)
	}
	if okEnv.ToolUseID != "step6" {
		t.Fatalf("tool_use_id = %q, want step6", okEnv.ToolUseID)
	}
	if okEnv.ToolName != "AntigravityStep" {
		t.Fatalf("tool_name = %q, want AntigravityStep", okEnv.ToolName)
	}
	if len(okEnv.ToolResponse) != 0 {
		t.Fatalf("successful step must not fabricate a tool_response, got %s", okEnv.ToolResponse)
	}
}

// TestTranslateAntigravityStop pins the terminationReason+fullyIdle ->
// last_assistant_message mapping - there is no assistant message text
// anywhere in Antigravity's documented Stop payload, so this synthesized
// content is what keeps the terminal fact's body non-empty.
func TestTranslateAntigravityStop(t *testing.T) {
	raw := []byte(`{
		"executionNum": 1,
		"terminationReason": "model_stop",
		"error": "",
		"fullyIdle": true,
		"conversationId": "conv-ag-3",
		"workspacePaths": ["/repo"]
	}`)
	out, ok, err := translateAntigravity("Stop", raw)
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
	if env.SessionID != "conv-ag-3" {
		t.Fatalf("session_id = %q, want conv-ag-3", env.SessionID)
	}
	want := "terminationReason=model_stop fullyIdle=true"
	if env.LastAssistantMessage != want {
		t.Fatalf("last_assistant_message = %q, want %q", env.LastAssistantMessage, want)
	}
}

// TestTranslateAntigravityStopWithError verifies a non-empty Stop-event
// error field (documented: "Optional. The error message if termination
// was caused by a system error.") is appended to last_assistant_message as
// " error=<message>" - Stop's payload otherwise carries no assistant text
// at all, so this is the only place a system-error message shows up in
// the terminal fact's body.
func TestTranslateAntigravityStopWithError(t *testing.T) {
	raw := []byte(`{
		"executionNum": 2,
		"terminationReason": "error",
		"error": "panic: system error mid-execution",
		"fullyIdle": true,
		"conversationId": "conv-ag-err",
		"workspacePaths": ["/repo"]
	}`)
	out, ok, err := translateAntigravity("Stop", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("Stop must be captured")
	}
	env := decodeEnvelope(t, out)
	want := "terminationReason=error fullyIdle=true error=panic: system error mid-execution"
	if env.LastAssistantMessage != want {
		t.Fatalf("last_assistant_message = %q, want %q", env.LastAssistantMessage, want)
	}
}

// TestTranslateAntigravityStopFullyIdleFalse is a distractor pinning the
// boolean actually flows through (not just a fixed "true" string).
func TestTranslateAntigravityStopFullyIdleFalse(t *testing.T) {
	raw := []byte(`{
		"terminationReason": "max_steps_exceeded",
		"fullyIdle": false,
		"conversationId": "conv-ag-4",
		"workspacePaths": ["/repo"]
	}`)
	out, ok, err := translateAntigravity("Stop", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("Stop must be captured")
	}
	env := decodeEnvelope(t, out)
	want := "terminationReason=max_steps_exceeded fullyIdle=false"
	if env.LastAssistantMessage != want {
		t.Fatalf("last_assistant_message = %q, want %q", env.LastAssistantMessage, want)
	}
}

// TestTranslateAntigravityUnwiredEventsSkip verifies PreToolUse and
// PostInvocation - both real, documented Antigravity events that
// ConnectAntigravity deliberately never wires (see
// connect_antigravity.go's antigravityGroupEvents/antigravityFlatEvents
// doc comments) - are skipped (ok=false, no error) if they ever somehow
// reach the translator, exactly like an unmapped event for any other
// agent. Also covers an empty --event.
func TestTranslateAntigravityUnwiredEventsSkip(t *testing.T) {
	for _, event := range []string{"PreToolUse", "PostInvocation", "", "SomeFutureEvent"} {
		raw := []byte(`{"conversationId":"conv-x","workspacePaths":["/p"]}`)
		out, ok, err := translateAntigravity(event, raw)
		if err != nil {
			t.Fatalf("event=%q: unexpected error: %v", event, err)
		}
		if ok {
			t.Fatalf("event=%q: must report ok=false", event)
		}
		if out != nil {
			t.Fatalf("event=%q: must return nil bytes, got %s", event, out)
		}
	}
}

// TestTranslateAntigravityMalformedJSON: bad JSON from the agent must
// error (there is nothing valid to translate), not panic and not silently
// skip - mirrors TestNormalizeCursorMalformedJSON.
func TestTranslateAntigravityMalformedJSON(t *testing.T) {
	_, ok, err := translateAntigravity("Stop", []byte(`{not valid`))
	if err == nil {
		t.Fatal("malformed antigravity payload must return an error")
	}
	if ok {
		t.Fatal("malformed payload must report ok=false")
	}
}

// TestTranslateAntigravityNoWorkspacePathsYieldsEmptyCWD: an absent/empty
// workspacePaths must not panic on an out-of-range index, and cwd should
// simply be empty (AgentNamespace's own "agent-default" fallback handles
// that server-side).
func TestTranslateAntigravityNoWorkspacePathsYieldsEmptyCWD(t *testing.T) {
	raw := []byte(`{"invocationNum":0,"conversationId":"conv-ag-5"}`)
	out, ok, err := translateAntigravity("PreInvocation", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("must still be captured")
	}
	env := decodeEnvelope(t, out)
	if env.CWD != "" {
		t.Fatalf("cwd = %q, want empty when workspacePaths is absent", env.CWD)
	}
}
