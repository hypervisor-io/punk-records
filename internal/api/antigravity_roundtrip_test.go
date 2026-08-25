package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/hookcli"
	"github.com/hypervisor-io/punk-records/internal/memory"
)

// TestAntigravityHookRoundTripsThroughServerContract is Antigravity's
// analog of TestCursorHookRoundTripsThroughServerContract, but drives the
// FULL hookcli.RunFromAntigravity entry point (stdin -> translate ->
// forward -> reply) against a real httptest.Server wrapping this
// package's own Server.Router() - the exact handleAgentHook and
// handleAgentContext handlers a production deployment would run - rather
// than calling a translator function and hand-POSTing its output. Unlike
// Cursor (whose translator is reachable through the shared
// hookcli.Normalize/RunFrom dispatch), Antigravity is wired through its
// own RunFromAntigravity entry point (see that function's doc comment in
// hookcli.go for why), so this test exercises that entire path end to
// end, not just translateAntigravity's output shape.
//
// For each wired event this proves: (1) the real handleAgentHook accepts
// the translated envelope and stores it under the exact key/namespace its
// own key-construction logic would produce (not a separately-hardcoded
// guess), and (2) content that matters (stepIdx, terminationReason,
// fullyIdle) survives translation into the stored fact body.
func TestAntigravityHookRoundTripsThroughServerContract(t *testing.T) {
	srv := testServer(t)
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	cases := []struct {
		name             string
		event            string
		raw              string
		wantBodyContains []string
	}{
		{
			name:  "preInvocationFirstCallIsSessionStart",
			event: "PreInvocation",
			raw:   `{"invocationNum":0,"initialNumSteps":0,"conversationId":"conv-agrt-1","workspacePaths":["/home/u/agrtproj"]}`,
		},
		{
			name:             "postToolUse",
			event:            "PostToolUse",
			raw:              `{"stepIdx":9,"error":"","conversationId":"conv-agrt-2","workspacePaths":["/home/u/agrtproj"]}`,
			wantBodyContains: []string{"stepIdx", "9", "AntigravityStep"},
		},
		{
			name:             "postToolUseWithError",
			event:            "PostToolUse",
			raw:              `{"stepIdx":10,"error":"exit status 1: round trip failure","conversationId":"conv-agrt-2b","workspacePaths":["/home/u/agrtproj"]}`,
			wantBodyContains: []string{"round trip failure", "AntigravityStep"},
		},
		{
			name:             "stop",
			event:            "Stop",
			raw:              `{"executionNum":1,"terminationReason":"model_stop","fullyIdle":true,"conversationId":"conv-agrt-3","workspacePaths":["/home/u/agrtproj"]}`,
			wantBodyContains: []string{"model_stop", "fullyIdle=true"},
		},
		{
			name:             "stopWithError",
			event:            "Stop",
			raw:              `{"executionNum":2,"terminationReason":"error","error":"round trip system error","fullyIdle":true,"conversationId":"conv-agrt-3b","workspacePaths":["/home/u/agrtproj"]}`,
			wantBodyContains: []string{"terminationReason=error", "fullyIdle=true", "error=round trip system error"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errw bytes.Buffer
			if err := hookcli.RunFromAntigravity(tc.event, strings.NewReader(tc.raw), httpSrv.URL, "", &out, &errw); err != nil {
				t.Fatalf("RunFromAntigravity: %v", err)
			}

			// Re-derive the expected key/namespace from the server's own
			// agentHookIn semantics by decoding the same fields
			// translateAntigravity would have produced, via a second,
			// independent parse of the ORIGINAL native payload (not the
			// translated bytes, which RunFromAntigravity doesn't expose) -
			// conversationId/workspacePaths[0] map to session_id/cwd
			// one-to-one for every case here.
			var native struct {
				ConversationID string   `json:"conversationId"`
				WorkspacePaths []string `json:"workspacePaths"`
				StepIdx        int      `json:"stepIdx"`
			}
			if err := json.Unmarshal([]byte(tc.raw), &native); err != nil {
				t.Fatalf("test setup: native payload not decodable: %v", err)
			}
			sid := sanitizeID(native.ConversationID)
			cwd := ""
			if len(native.WorkspacePaths) > 0 {
				cwd = native.WorkspacePaths[0]
			}
			ns := AgentNamespace(cwd)

			var wantKeySuffix string
			switch tc.event {
			case "PreInvocation":
				wantKeySuffix = "/start"
			case "PostToolUse":
				wantKeySuffix = "/tool-step" + strconv.Itoa(native.StepIdx)
			case "Stop":
				wantKeySuffix = "/stop"
			default:
				t.Fatalf("unhandled test event %q", tc.event)
			}
			wantKey := "/agent-sessions/" + sid + wantKeySuffix

			facts, err := srv.mem.Recall(context.Background(), ns, wantKey, 1)
			if err != nil || len(facts) != 1 {
				t.Fatalf("stored fact not recallable at %s%s: %v %v (stderr: %s)", ns, wantKey, facts, err, errw.String())
			}
			if facts[0].Key != wantKey {
				t.Fatalf("recalled fact key = %q, want %q", facts[0].Key, wantKey)
			}
			if facts[0].SourceRef != "antigravity" {
				t.Fatalf("SourceRef = %q, want antigravity", facts[0].SourceRef)
			}
			for _, want := range tc.wantBodyContains {
				if !strings.Contains(facts[0].Body, want) {
					t.Fatalf("fact body missing %q: %q", want, facts[0].Body)
				}
			}
		})
	}
}

// TestAntigravityHookRoundTripInjectsContextOnFirstInvocation proves the
// PreInvocation -> context-injection path end to end: a fact seeded
// through the real server beforehand must come back inside the driver's
// injectSteps[0].ephemeralMessage on invocationNum==0, mirroring how
// TestPiExtensionNodeRoundTripsThroughRealServer/
// TestOpenCodePluginNodeRoundTripsThroughRealServer prove injection for
// their own agents.
func TestAntigravityHookRoundTripInjectsContextOnFirstInvocation(t *testing.T) {
	srv := testServer(t)
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	const cwd = "/home/u/agrtinjectproj"
	const sentinel = "antigravity round trip context injection sentinel fact 7c1d4e"
	if _, err := srv.mem.Write(context.Background(), memory.WriteInput{
		Namespace:  AgentNamespace(cwd),
		Key:        "/decisions/agrt-context",
		Body:       sentinel,
		Author:     "test",
		Writer:     "test",
		Importance: 0.8,
	}); err != nil {
		t.Fatal(err)
	}

	const raw = `{"invocationNum":0,"initialNumSteps":0,"conversationId":"conv-agrt-inject","workspacePaths":["` + cwd + `"]}`
	var out, errw bytes.Buffer
	if err := hookcli.RunFromAntigravity("PreInvocation", strings.NewReader(raw), httpSrv.URL, "", &out, &errw); err != nil {
		t.Fatal(err)
	}

	var result struct {
		InjectSteps []struct {
			EphemeralMessage string `json:"ephemeralMessage"`
		} `json:"injectSteps"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("stdout not valid injectSteps JSON: %v: %s (stderr: %s)", err, out.String(), errw.String())
	}
	if len(result.InjectSteps) != 1 {
		t.Fatalf("expected exactly one injected step, got %d: %s", len(result.InjectSteps), out.String())
	}
	if !strings.Contains(result.InjectSteps[0].EphemeralMessage, sentinel) {
		t.Fatalf("expected injected context to contain the seeded sentinel fact, got: %q", result.InjectSteps[0].EphemeralMessage)
	}
}
