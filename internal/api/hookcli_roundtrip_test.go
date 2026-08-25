package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/hookcli"
)

// TestCursorHookRoundTripsThroughServerContract is the server-contract test
// the T1.1 review flagged as missing: internal/hookcli's own tests only
// prove Normalize's *output* looks right by decoding it with a hand-copied
// mirror struct. That leaves a real gap - if any translated field (a JSON
// tag typo, a dropped correlation ID, a renamed key) ever drifts from what
// this package's actual agentHookIn/handleAgentHook decode and require,
// hookcli's tests would still pass while every translated Cursor event
// silently turns into a 200 "ignored" in production.
//
// For each Cursor event this translator maps, this test: (1) calls the
// real hookcli.Normalize, (2) POSTs the translated bytes to the real
// handleAgentHook via this package's own test harness (testServer/do - the
// same ones every other agent_handlers_test.go test uses), and (3) asserts
// the response is "stored" with the exact key handleAgentHook's own
// key-construction logic would produce for those field values - not a
// separately-hardcoded guess at the key, so a future change to either side
// of the contract (translator or handler) that breaks the other fails
// here.
func TestCursorHookRoundTripsThroughServerContract(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		// wantBodyContains is optional: extra substrings the stored fact's
		// body must contain, pinning that content (not just a key) survived
		// translation (findings #2 afterFileEdit edits, #4 stop/sessionEnd
		// terminal content).
		wantBodyContains []string
	}{
		{
			name: "sessionStart",
			raw:  `{"hook_event_name":"sessionStart","conversation_id":"conv-rt-1","workspace_roots":["/home/u/rtproj"],"session_id":"ide-rt-1"}`,
		},
		{
			name:             "beforeSubmitPrompt",
			raw:              `{"hook_event_name":"beforeSubmitPrompt","conversation_id":"conv-rt-2","generation_id":"gen-rt-2","workspace_roots":["/home/u/rtproj"],"prompt":"hello from the round trip test"}`,
			wantBodyContains: []string{"hello from the round trip test"},
		},
		{
			// No generation_id at all: proves the synthesized fallback
			// prompt_id (finding #1) is itself enough to make the server
			// accept and store the event, not just decode without error.
			name:             "beforeSubmitPromptNoGenerationID",
			raw:              `{"hook_event_name":"beforeSubmitPrompt","conversation_id":"conv-rt-2b","workspace_roots":["/home/u/rtproj"],"prompt":"fallback path"}`,
			wantBodyContains: []string{"fallback path"},
		},
		{
			name:             "postToolUse",
			raw:              `{"hook_event_name":"postToolUse","conversation_id":"conv-rt-3","cwd":"/home/u/rtproj","tool_name":"read_file","tool_input":{"path":"main.go"},"tool_output":{"content":"package main"},"tool_use_id":"tu-rt-3"}`,
			wantBodyContains: []string{"read_file", "package main"},
		},
		{
			name: "afterFileEdit",
			raw:  `{"hook_event_name":"afterFileEdit","conversation_id":"conv-rt-4","workspace_roots":["/home/u/rtproj"],"file_path":"/home/u/rtproj/foo.go","edits":[{"old_string":"a","new_string":"b"}]}`,
			// finding #2: the edits array (the actual change content) must
			// survive into the stored fact, not just the file path.
			wantBodyContains: []string{"old_string", `"a"`, `"b"`},
		},
		{
			name: "stop",
			raw:  `{"hook_event_name":"stop","conversation_id":"conv-rt-5","workspace_roots":["/home/u/rtproj"],"status":"completed","loop_count":2}`,
			// finding #4: stop's fact body must carry the terminal status,
			// not be empty.
			wantBodyContains: []string{"completed"},
		},
		{
			name: "sessionEnd",
			raw:  `{"hook_event_name":"sessionEnd","conversation_id":"conv-rt-6","session_id":"ide-rt-6","workspace_roots":["/home/u/rtproj"],"reason":"user_close","final_status":"ok"}`,
			// finding #4: sessionEnd's fact body must carry reason+final_status.
			wantBodyContains: []string{"user_close", "ok"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			translated, ok, err := hookcli.Normalize("cursor", []byte(tc.raw))
			if err != nil {
				t.Fatalf("Normalize: %v", err)
			}
			if !ok {
				t.Fatalf("Normalize: event unexpectedly skipped (ok=false): %s", tc.raw)
			}

			// Decode with the server's OWN struct - the same decode
			// handleAgentHook itself performs - so the expected key below
			// is derived from the identical field semantics the server
			// uses, not a separately re-guessed copy of them.
			var in agentHookIn
			if err := json.Unmarshal(translated, &in); err != nil {
				t.Fatalf("translated envelope not decodable as agentHookIn: %v: %s", err, translated)
			}

			srv := testServer(t)
			rec := do(t, srv, "POST", "/v1/agent/hooks", string(translated))
			if rec.Code != 200 {
				t.Fatalf("POST /v1/agent/hooks: %d: %s", rec.Code, rec.Body.String())
			}
			var resp struct {
				Status    string `json:"status"`
				Namespace string `json:"namespace"`
				Key       string `json:"key"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("response not decodable: %v: %s", err, rec.Body.String())
			}
			if resp.Status != "stored" {
				t.Fatalf("status = %q, want stored (translated envelope: %s)", resp.Status, translated)
			}

			sid := sanitizeID(in.SessionID)
			var wantKeySuffix string
			switch in.HookEventName {
			case "SessionStart":
				wantKeySuffix = "/start"
			case "UserPromptSubmit":
				pid := sanitizeID(in.PromptID)
				if pid == "" {
					t.Fatalf("test setup: translated envelope has no usable prompt_id: %s", translated)
				}
				wantKeySuffix = "/prompt-" + pid
			case "PostToolUse":
				tuid := sanitizeID(in.ToolUseID)
				if tuid == "" {
					t.Fatalf("test setup: translated envelope has no usable tool_use_id: %s", translated)
				}
				wantKeySuffix = "/tool-" + tuid
			case "Stop":
				wantKeySuffix = "/stop"
			default:
				t.Fatalf("unexpected translated hook_event_name %q", in.HookEventName)
			}
			wantKey := "/agent-sessions/" + sid + wantKeySuffix
			if resp.Key != wantKey {
				t.Fatalf("stored key = %q, want %q (derived from the server's own sanitizeID/key-construction logic)", resp.Key, wantKey)
			}

			ns := AgentNamespace(in.CWD)
			if resp.Namespace != ns {
				t.Fatalf("stored namespace = %q, want %q", resp.Namespace, ns)
			}
			facts, err := srv.mem.Recall(context.Background(), ns, wantKey, 1)
			if err != nil || len(facts) != 1 {
				t.Fatalf("stored fact not recallable at %s%s: %v %v", ns, wantKey, facts, err)
			}
			if facts[0].Key != wantKey {
				t.Fatalf("recalled fact key = %q, want %q", facts[0].Key, wantKey)
			}
			for _, want := range tc.wantBodyContains {
				if !strings.Contains(facts[0].Body, want) {
					t.Fatalf("fact body missing %q: %q", want, facts[0].Body)
				}
			}
		})
	}
}

// TestOpenCodeHookRoundTripsThroughServerContract is T1.3 review finding
// #2's round-trip contract test for the OpenCode plugin's ENVELOPE SHAPE.
// Unlike Cursor, OpenCode has no Go-side translator (hookcli.Normalize):
// the generated JS plugin (internal/hookcli/opencode_plugin.go) POSTs
// Claude-shaped envelopes to /v1/agent/hooks directly.
//
// What this test actually proves, precisely: for each of these
// hand-written envelope literals (believed, at the time they were
// written, to match what the plugin's postHook(...) calls serialize),
// the real handleAgentHook accepts it, stores it under the expected key/
// namespace, and preserves SourceRef/body content. That is a genuine
// server-contract guarantee, but it is bounded by the word "literal": the
// JSON bodies below are typed by hand, not produced by executing the
// plugin. A drift in the plugin TEMPLATE itself - a renamed field, a
// dropped tool_use_id, a removed role gate on chat.message - changes
// what the real plugin sends without changing a single byte here, so
// this test alone cannot catch template drift; nor can hookcli's own
// byte-exact golden test (TestConnectOpenCodeGoldenContent), which pins
// the template's bytes but never executes them.
//
// TestOpenCodePluginNodeRoundTripsThroughRealServer (internal/api/
// opencode_node_roundtrip_test.go) closes that specific gap: it renders
// the real plugin, drives its hooks under node, and lets the ACTUAL
// generated HTTP requests hit this same real server - proving the chain
// this test only asserts by hand-typed proxy. Confirmed by mutation: with
// tool_use_id hardcoded empty, or the chat.message role gate removed,
// this test (unchanged) still passes while the node round-trip test
// fails.
func TestOpenCodeHookRoundTripsThroughServerContract(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		// wantBodyContains: extra substrings the stored fact's body must
		// contain, pinning that content (not just a key) survived - in
		// particular finding #3's requirement that the Stop fact body is
		// non-empty (last_assistant_message:"status=idle" on
		// session.idle), not the empty body Cursor's own stop event was
		// originally missing too.
		wantBodyContains []string
	}{
		{
			// Mirrors the "event" hook's session.created branch.
			name: "sessionStart",
			raw:  `{"hook_event_name":"SessionStart","session_id":"oc-rt-1","cwd":"/home/u/ocproj","source":"opencode"}`,
		},
		{
			// Mirrors the "chat.message" hook (finding #8): messageID
			// present, used verbatim as prompt_id.
			name:             "userPromptSubmit",
			raw:              `{"hook_event_name":"UserPromptSubmit","session_id":"oc-rt-2","prompt_id":"msg-oc-rt-2","prompt":"hello from the opencode round trip test","cwd":"/home/u/ocproj","source":"opencode"}`,
			wantBodyContains: []string{"hello from the opencode round trip test"},
		},
		{
			// A hand-picked "msg-"-prefixed id in the shape the plugin's
			// fnv1aHex fallback produces when messageID is absent (finding
			// #8) - "msg-9f8e7d6c" is NOT derived by actually running
			// fnv1aHex against these field values, so this case only pins
			// that the SERVER accepts and stores an id shaped like the
			// fallback; it does not prove the plugin computes this
			// specific hash for this input (that would be a pretense this
			// Go-only test cannot back up without reimplementing
			// fnv1aHex's JS in Go). The real fallback derivation, hash and
			// all, is exercised end-to-end by
			// TestOpenCodePluginNodeRoundTripsThroughRealServer's
			// no-messageID chat.message case (internal/api/
			// opencode_node_roundtrip_test.go).
			name:             "userPromptSubmitFallbackID",
			raw:              `{"hook_event_name":"UserPromptSubmit","session_id":"oc-rt-2b","prompt_id":"msg-9f8e7d6c","prompt":"fallback id path","cwd":"/home/u/ocproj","source":"opencode"}`,
			wantBodyContains: []string{"fallback id path"},
		},
		{
			// Mirrors the "tool.execute.after" hook.
			name:             "postToolUse",
			raw:              `{"hook_event_name":"PostToolUse","session_id":"oc-rt-3","cwd":"/home/u/ocproj","tool_name":"bash","tool_input":{"command":"ls"},"tool_response":"file1\nfile2","tool_use_id":"call-rt-3","source":"opencode"}`,
			wantBodyContains: []string{"bash", "file1"},
		},
		{
			// Mirrors the "event" hook's session.idle branch (finding
			// #3): last_assistant_message carries "status=idle" rather
			// than being left out, so the stored Stop fact has a
			// non-empty body.
			name:             "stopOnSessionIdle",
			raw:              `{"hook_event_name":"Stop","session_id":"oc-rt-4","cwd":"/home/u/ocproj","last_assistant_message":"status=idle","source":"opencode"}`,
			wantBodyContains: []string{"status=idle"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var in agentHookIn
			if err := json.Unmarshal([]byte(tc.raw), &in); err != nil {
				t.Fatalf("envelope not decodable as agentHookIn: %v: %s", err, tc.raw)
			}

			srv := testServer(t)
			rec := do(t, srv, "POST", "/v1/agent/hooks", tc.raw)
			if rec.Code != 200 {
				t.Fatalf("POST /v1/agent/hooks: %d: %s", rec.Code, rec.Body.String())
			}
			var resp struct {
				Status    string `json:"status"`
				Namespace string `json:"namespace"`
				Key       string `json:"key"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("response not decodable: %v: %s", err, rec.Body.String())
			}
			if resp.Status != "stored" {
				t.Fatalf("status = %q, want stored (envelope: %s)", resp.Status, tc.raw)
			}

			sid := sanitizeID(in.SessionID)
			var wantKeySuffix string
			switch in.HookEventName {
			case "SessionStart":
				wantKeySuffix = "/start"
			case "UserPromptSubmit":
				pid := sanitizeID(in.PromptID)
				if pid == "" {
					t.Fatalf("test setup: envelope has no usable prompt_id: %s", tc.raw)
				}
				wantKeySuffix = "/prompt-" + pid
			case "PostToolUse":
				tuid := sanitizeID(in.ToolUseID)
				if tuid == "" {
					t.Fatalf("test setup: envelope has no usable tool_use_id: %s", tc.raw)
				}
				wantKeySuffix = "/tool-" + tuid
			case "Stop":
				wantKeySuffix = "/stop"
			default:
				t.Fatalf("unexpected hook_event_name %q", in.HookEventName)
			}
			wantKey := "/agent-sessions/" + sid + wantKeySuffix
			if resp.Key != wantKey {
				t.Fatalf("stored key = %q, want %q (derived from the server's own sanitizeID/key-construction logic)", resp.Key, wantKey)
			}

			ns := AgentNamespace(in.CWD)
			if resp.Namespace != ns {
				t.Fatalf("stored namespace = %q, want %q", resp.Namespace, ns)
			}
			facts, err := srv.mem.Recall(context.Background(), ns, wantKey, 1)
			if err != nil || len(facts) != 1 {
				t.Fatalf("stored fact not recallable at %s%s: %v %v", ns, wantKey, facts, err)
			}
			if facts[0].Key != wantKey {
				t.Fatalf("recalled fact key = %q, want %q", facts[0].Key, wantKey)
			}
			// SourceRef must reflect the plugin's own "opencode" source
			// field (sourceRefFor), proving provenance survives the round
			// trip, not just key/namespace placement.
			if facts[0].SourceRef != "opencode" {
				t.Fatalf("SourceRef = %q, want %q", facts[0].SourceRef, "opencode")
			}
			for _, want := range tc.wantBodyContains {
				if !strings.Contains(facts[0].Body, want) {
					t.Fatalf("fact body missing %q: %q", want, facts[0].Body)
				}
			}
			if tc.name == "stopOnSessionIdle" && facts[0].Body == "" {
				t.Fatal("finding #3: Stop fact body must not be empty")
			}
		})
	}
}

// TestCopilotHookRoundTripsThroughServerContract is the same server-contract
// guarantee TestCursorHookRoundTripsThroughServerContract pins, applied to
// GitHub Copilot CLI's translator (hookcli.translateCopilot, registered as
// Normalize("copilot", ...)): for each Copilot event the translator maps,
// this test calls the real hookcli.Normalize, POSTs the translated bytes to
// the real handleAgentHook, and asserts the response is "stored" with the
// exact key/namespace handleAgentHook's own logic would produce - not a
// separately-hardcoded guess - so a drift in either side of the contract
// (translator field tags or the server's agentHookIn/handleAgentHook) fails
// here rather than silently turning every translated Copilot event into a
// 200 "ignored" in production.
func TestCopilotHookRoundTripsThroughServerContract(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		// wantBodyContains: extra substrings the stored fact's body must
		// contain, pinning that content (not just a key) survived
		// translation - in particular that PostToolUse's tool_result content
		// and Stop/SessionEnd's terminal reason both land in the body, not
		// an empty one.
		wantBodyContains []string
	}{
		{
			name: "sessionStart",
			raw:  `{"hook_event_name":"SessionStart","session_id":"sess-cp-rt-1","cwd":"/home/u/cprtproj","source":"startup"}`,
		},
		{
			name:             "userPromptSubmit",
			raw:              `{"hook_event_name":"UserPromptSubmit","session_id":"sess-cp-rt-2","cwd":"/home/u/cprtproj","prompt":"hello from the copilot round trip test"}`,
			wantBodyContains: []string{"hello from the copilot round trip test"},
		},
		{
			name:             "postToolUse",
			raw:              `{"hook_event_name":"PostToolUse","session_id":"sess-cp-rt-3","timestamp":"2026-08-01T12:00:00Z","cwd":"/home/u/cprtproj","tool_name":"bash","tool_input":{"command":"ls"},"tool_result":{"result_type":"success","text_result_for_llm":"file1\nfile2"}}`,
			wantBodyContains: []string{"bash", "file1"},
		},
		{
			name: "stop",
			raw:  `{"hook_event_name":"Stop","session_id":"sess-cp-rt-4","cwd":"/home/u/cprtproj","stop_reason":"end_turn","stop_hook_active":false}`,
			// stop_reason must carry into the stored fact's body, not be
			// left empty.
			wantBodyContains: []string{"end_turn"},
		},
		{
			name: "sessionEnd",
			raw:  `{"hook_event_name":"SessionEnd","session_id":"sess-cp-rt-5","cwd":"/home/u/cprtproj","reason":"user_exit"}`,
			// sessionEnd's own reason must carry into the stored fact's
			// body (mapped to the same Stop key as agentStop above).
			wantBodyContains: []string{"user_exit"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			translated, ok, err := hookcli.Normalize("copilot", []byte(tc.raw))
			if err != nil {
				t.Fatalf("Normalize: %v", err)
			}
			if !ok {
				t.Fatalf("Normalize: event unexpectedly skipped (ok=false): %s", tc.raw)
			}

			var in agentHookIn
			if err := json.Unmarshal(translated, &in); err != nil {
				t.Fatalf("translated envelope not decodable as agentHookIn: %v: %s", err, translated)
			}

			srv := testServer(t)
			rec := do(t, srv, "POST", "/v1/agent/hooks", string(translated))
			if rec.Code != 200 {
				t.Fatalf("POST /v1/agent/hooks: %d: %s", rec.Code, rec.Body.String())
			}
			var resp struct {
				Status    string `json:"status"`
				Namespace string `json:"namespace"`
				Key       string `json:"key"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("response not decodable: %v: %s", err, rec.Body.String())
			}
			if resp.Status != "stored" {
				t.Fatalf("status = %q, want stored (translated envelope: %s)", resp.Status, translated)
			}

			sid := sanitizeID(in.SessionID)
			var wantKeySuffix string
			switch in.HookEventName {
			case "SessionStart":
				wantKeySuffix = "/start"
			case "UserPromptSubmit":
				pid := sanitizeID(in.PromptID)
				if pid == "" {
					t.Fatalf("test setup: translated envelope has no usable prompt_id: %s", translated)
				}
				wantKeySuffix = "/prompt-" + pid
			case "PostToolUse":
				tuid := sanitizeID(in.ToolUseID)
				if tuid == "" {
					t.Fatalf("test setup: translated envelope has no usable tool_use_id: %s", translated)
				}
				wantKeySuffix = "/tool-" + tuid
			case "Stop":
				wantKeySuffix = "/stop"
			default:
				t.Fatalf("unexpected translated hook_event_name %q", in.HookEventName)
			}
			wantKey := "/agent-sessions/" + sid + wantKeySuffix
			if resp.Key != wantKey {
				t.Fatalf("stored key = %q, want %q (derived from the server's own sanitizeID/key-construction logic)", resp.Key, wantKey)
			}

			ns := AgentNamespace(in.CWD)
			if resp.Namespace != ns {
				t.Fatalf("stored namespace = %q, want %q", resp.Namespace, ns)
			}
			facts, err := srv.mem.Recall(context.Background(), ns, wantKey, 1)
			if err != nil || len(facts) != 1 {
				t.Fatalf("stored fact not recallable at %s%s: %v %v", ns, wantKey, facts, err)
			}
			if facts[0].Key != wantKey {
				t.Fatalf("recalled fact key = %q, want %q", facts[0].Key, wantKey)
			}
			// SourceRef must reflect translateCopilot's hardcoded "copilot"
			// source, proving provenance survives the round trip and is
			// never Copilot's own sessionStart "source" reason value
			// ("startup") - the exact collision claudeSessionStartSourceReasons
			// guards against.
			if facts[0].SourceRef != "copilot" {
				t.Fatalf("SourceRef = %q, want %q", facts[0].SourceRef, "copilot")
			}
			for _, want := range tc.wantBodyContains {
				if !strings.Contains(facts[0].Body, want) {
					t.Fatalf("fact body missing %q: %q", want, facts[0].Body)
				}
			}
		})
	}
}
