package hookcli

import (
	"encoding/json"
	"strings"
	"testing"
)

// decodeHermesEnvelope decodes a translated envelope into the same field
// set the server's agentHookIn reads, so assertions below check the exact
// tags that travel over the wire rather than a paraphrase of them.
func decodeHermesEnvelope(t *testing.T, raw []byte) claudeEnvelope {
	t.Helper()
	var env claudeEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("translated envelope is not valid JSON: %v: %s", err, raw)
	}
	return env
}

// TestTranslateHermesSessionStart pins the on_session_start mapping and the
// three fields Hermes shares with Claude Code's own envelope shape.
func TestTranslateHermesSessionStart(t *testing.T) {
	raw := []byte(`{"hook_event_name":"on_session_start","session_id":"sess-1","cwd":"/repo",
		"tool_name":null,"tool_input":null,"extra":{"model":"hermes-4","platform":"cli"}}`)
	out, ok, err := translateHermes(raw)
	if err != nil || !ok {
		t.Fatalf("translate: ok=%v err=%v", ok, err)
	}
	env := decodeHermesEnvelope(t, out)
	if env.HookEventName != "SessionStart" {
		t.Fatalf("hook_event_name = %q, want SessionStart", env.HookEventName)
	}
	if env.SessionID != "sess-1" || env.CWD != "/repo" || env.Source != "hermes" {
		t.Fatalf("envelope base fields: %+v", env)
	}
}

// TestTranslateHermesPreLLMCallCarriesPromptAndID covers the one mapping
// the server will silently DROP if it gets it wrong: agent_handlers.go
// ignores any UserPromptSubmit whose prompt_id sanitizes to empty, and
// Hermes' pre_llm_call payload carries no id of its own, so the synthesized
// fallback is load-bearing rather than cosmetic.
func TestTranslateHermesPreLLMCallCarriesPromptAndID(t *testing.T) {
	raw := []byte(`{"hook_event_name":"pre_llm_call","session_id":"sess-2","cwd":"/repo",
		"extra":{"user_message":"why is the build red","is_first_turn":true,
		"conversation_history":[{"role":"user","content":"earlier"}],"model":"hermes-4"}}`)
	out, ok, err := translateHermes(raw)
	if err != nil || !ok {
		t.Fatalf("translate: ok=%v err=%v", ok, err)
	}
	env := decodeHermesEnvelope(t, out)
	if env.HookEventName != "UserPromptSubmit" {
		t.Fatalf("hook_event_name = %q, want UserPromptSubmit", env.HookEventName)
	}
	if env.Prompt != "why is the build red" {
		t.Fatalf("prompt = %q", env.Prompt)
	}
	if env.PromptID == "" {
		t.Fatal("prompt_id is empty; the server drops every such prompt as ignored")
	}
	// The fallback must be deterministic in session+prompt: the same turn
	// replayed produces the same capture key rather than a fresh row.
	again, _, _ := translateHermes(raw)
	if decodeHermesEnvelope(t, again).PromptID != env.PromptID {
		t.Fatal("prompt_id is not deterministic for identical input")
	}
	// ...and must differ when the prompt differs, or two distinct prompts
	// in one session would collapse onto one capture key.
	other := []byte(strings.Replace(string(raw), "why is the build red", "why is the deploy red", 1))
	otherOut, _, _ := translateHermes(other)
	if decodeHermesEnvelope(t, otherOut).PromptID == env.PromptID {
		t.Fatal("prompt_id collides across different prompts in the same session")
	}
	// conversation_history is deliberately not carried into the envelope:
	// it is the whole transcript and would be re-forwarded every turn.
	if strings.Contains(string(out), "earlier") {
		t.Fatalf("conversation_history leaked into the envelope: %s", out)
	}
}

// TestTranslateHermesPostToolCallPrefersRealToolCallID checks the documented
// per-call id is used verbatim when present, and that the raw result value
// is relayed as tool_response under the server's own field name.
func TestTranslateHermesPostToolCallPrefersRealToolCallID(t *testing.T) {
	raw := []byte(`{"hook_event_name":"post_tool_call","session_id":"sess-3","cwd":"/repo",
		"tool_name":"terminal","tool_input":{"command":"ls -la"},
		"extra":{"tool_call_id":"tc-99","task_id":"task-1","result":"file1\nfile2","duration_ms":150}}`)
	out, ok, err := translateHermes(raw)
	if err != nil || !ok {
		t.Fatalf("translate: ok=%v err=%v", ok, err)
	}
	env := decodeHermesEnvelope(t, out)
	if env.HookEventName != "PostToolUse" || env.ToolName != "terminal" {
		t.Fatalf("envelope: %+v", env)
	}
	if env.ToolUseID != "tc-99" {
		t.Fatalf("tool_use_id = %q, want the payload's own tool_call_id", env.ToolUseID)
	}
	if string(env.ToolInput) != `{"command":"ls -la"}` {
		t.Fatalf("tool_input = %s", env.ToolInput)
	}
	if string(env.ToolResponse) != `"file1\nfile2"` {
		t.Fatalf("tool_response = %s", env.ToolResponse)
	}
}

// TestTranslateHermesPostToolCallSynthesizesIDPerCall covers the fallback
// path: with no tool_call_id, two DIFFERENT calls in one session must not
// collapse onto a single capture key (the server would silently overwrite
// one with the other), while an identical replay must stay stable.
func TestTranslateHermesPostToolCallSynthesizesIDPerCall(t *testing.T) {
	first := []byte(`{"hook_event_name":"post_tool_call","session_id":"s","cwd":"/r",
		"tool_name":"terminal","tool_input":{"command":"ls"},
		"extra":{"task_id":"t1","result":"a","duration_ms":10}}`)
	// Same tool and same arguments, different measured duration: this is
	// the "ran the same command twice" case a tool_name+tool_input-only
	// hash would merge.
	second := []byte(`{"hook_event_name":"post_tool_call","session_id":"s","cwd":"/r",
		"tool_name":"terminal","tool_input":{"command":"ls"},
		"extra":{"task_id":"t1","result":"a","duration_ms":11}}`)

	firstOut, _, err := translateHermes(first)
	if err != nil {
		t.Fatal(err)
	}
	secondOut, _, err := translateHermes(second)
	if err != nil {
		t.Fatal(err)
	}
	firstID := decodeHermesEnvelope(t, firstOut).ToolUseID
	secondID := decodeHermesEnvelope(t, secondOut).ToolUseID
	if firstID == "" || secondID == "" {
		t.Fatal("synthesized tool_use_id is empty; the server drops such PostToolUse events")
	}
	if firstID == secondID {
		t.Fatal("two distinct calls collapsed onto one synthesized tool_use_id")
	}
	replayOut, _, _ := translateHermes(first)
	if decodeHermesEnvelope(t, replayOut).ToolUseID != firstID {
		t.Fatal("synthesized tool_use_id is not deterministic for identical input")
	}
}

// TestTranslateHermesPostLLMCallCarriesAssistantText pins the one thing
// that makes Hermes' Stop mapping better than every other adapter's: real
// assistant text instead of a synthesized status string.
func TestTranslateHermesPostLLMCallCarriesAssistantText(t *testing.T) {
	raw := []byte(`{"hook_event_name":"post_llm_call","session_id":"sess-4","cwd":"/repo",
		"extra":{"assistant_response":"The build is red because of a missing migration.","model":"hermes-4"}}`)
	out, ok, err := translateHermes(raw)
	if err != nil || !ok {
		t.Fatalf("translate: ok=%v err=%v", ok, err)
	}
	env := decodeHermesEnvelope(t, out)
	if env.HookEventName != "Stop" {
		t.Fatalf("hook_event_name = %q, want Stop", env.HookEventName)
	}
	if env.LastAssistantMessage != "The build is red because of a missing migration." {
		t.Fatalf("last_assistant_message = %q", env.LastAssistantMessage)
	}
}

// TestTranslateHermesSkipsUnwiredEvents pins that the events deliberately
// left out of hermesHookEvents translate to nothing rather than to a
// half-populated envelope - including on_session_end, whose Stop mapping
// would overwrite post_llm_call's richer fact on the same capture key.
func TestTranslateHermesSkipsUnwiredEvents(t *testing.T) {
	for _, ev := range []string{"on_session_end", "pre_tool_call", "on_session_reset", "subagent_start", ""} {
		raw := []byte(`{"hook_event_name":"` + ev + `","session_id":"s","cwd":"/r","extra":{"completed":true}}`)
		out, ok, err := translateHermes(raw)
		if err != nil {
			t.Fatalf("%s: unexpected error %v", ev, err)
		}
		if ok {
			t.Fatalf("%s: translated to %s, want skipped", ev, out)
		}
	}
}

// TestTranslateHermesMalformedPayloadErrors pins that a payload which is
// not valid JSON is an error (not a silent skip), so RunFromHermes reports
// it on stderr instead of forwarding garbage.
func TestTranslateHermesMalformedPayloadErrors(t *testing.T) {
	if _, ok, err := translateHermes([]byte(`{"hook_event_name":`)); err == nil || ok {
		t.Fatalf("malformed payload: ok=%v err=%v, want an error", ok, err)
	}
}

// TestNormalizeRoutesHermes pins the registry wiring itself: Normalize must
// resolve "hermes" (case-insensitively, like every other agent) rather than
// only translateHermes being reachable directly.
func TestNormalizeRoutesHermes(t *testing.T) {
	raw := []byte(`{"hook_event_name":"on_session_start","session_id":"s","cwd":"/r"}`)
	for _, from := range []string{"hermes", "Hermes", "HERMES"} {
		out, ok, err := Normalize(from, raw)
		if err != nil || !ok {
			t.Fatalf("%s: ok=%v err=%v", from, ok, err)
		}
		if decodeHermesEnvelope(t, out).Source != "hermes" {
			t.Fatalf("%s: source not set to hermes: %s", from, out)
		}
	}
}
