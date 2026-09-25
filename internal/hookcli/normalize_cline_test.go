package hookcli

import (
	"encoding/json"
	"testing"
)

// Verified 2026-09-25 against Cline's current extension file-hook source:
// https://github.com/cline/cline/blob/main/apps/vscode/src/core/hooks/templates.ts
// https://github.com/cline/cline/blob/main/apps/vscode/src/core/hooks/utils.ts
// Not the separate SDK AgentPlugin API to which the public docs now redirect.
func TestNormalizeClineNativePayloads(t *testing.T) {
	for _, tc := range []struct{ event, extra, want, prompt, tool, result string }{
		{"TaskStart", `"taskStart":{"taskMetadata":{"initialTask":"build"}}`, "SessionStart", "build", "", ""},
		{"TaskResume", `"taskResume":{"taskMetadata":{"taskId":"t"}}`, "SessionStart", "", "", ""},
		{"UserPromptSubmit", `"userPromptSubmit":{"prompt":"next","attachments":[]}`, "UserPromptSubmit", "next", "", ""},
		{"PostToolUse", `"postToolUse":{"toolName":"read_file","parameters":{"path":"a.go"},"result":"contents","success":true,"executionTimeMs":12}`, "PostToolUse", "", "read_file", `"contents"`},
		{"TaskComplete", `"taskComplete":{"taskMetadata":{"result":"finished"}}`, "Stop", "", "", "finished"},
		{"TaskCancel", `"taskCancel":{"taskMetadata":{"completionStatus":"cancelled"}}`, "Stop", "", "", "cancelled"},
	} {
		t.Run(tc.event, func(t *testing.T) {
			raw := []byte(`{"taskId":"task-1","hookName":"` + tc.event + `","workspaceRoots":["/work/one","/work/two"],"timestamp":"123",` + tc.extra + `}`)
			got, ok, err := Normalize("CLINE", raw)
			if err != nil || !ok {
				t.Fatalf("normalize: %s %t %v", got, ok, err)
			}
			var env claudeEnvelope
			if err := json.Unmarshal(got, &env); err != nil {
				t.Fatal(err)
			}
			if env.Source != "cline" || env.SessionID != "task-1" || env.CWD != "/work/one" || env.HookEventName != tc.want || env.Prompt != tc.prompt || env.ToolName != tc.tool {
				t.Fatalf("envelope=%+v", env)
			}
			if tc.want == "PostToolUse" && (env.ToolUseID == "" || string(env.ToolResponse) != tc.result || string(env.ToolInput) != `{"path":"a.go"}`) {
				t.Fatalf("tool fields=%+v", env)
			}
			if tc.want == "UserPromptSubmit" && env.PromptID == "" {
				t.Fatal("prompt dropped without ID")
			}
			if tc.want == "Stop" && env.LastAssistantMessage != tc.result {
				t.Fatalf("result=%q", env.LastAssistantMessage)
			}
		})
	}
	if _, ok, err := Normalize("cline", []byte(`{"taskId":"t","hookName":"Notification"}`)); err != nil || ok {
		t.Fatalf("notification mapped: %t %v", ok, err)
	}
	for _, raw := range []string{`{`, `null`, `{"hookName":"TaskStart"}`} {
		if _, _, err := Normalize("cline", []byte(raw)); err == nil {
			t.Fatalf("invalid payload accepted %s", raw)
		}
	}
}

func TestNormalizeClineLegacyAndDistinctCalls(t *testing.T) {
	var ids []string
	for _, stamp := range []string{"1", "2"} {
		raw := []byte(`{"taskId":"t","hookName":"PostToolUse","timestamp":"` + stamp + `","postToolUse":{"tool":"read_file","parameters":{},"result":"ok"}}`)
		b, ok, err := Normalize("cline", raw)
		if err != nil || !ok {
			t.Fatal(err)
		}
		var env claudeEnvelope
		if err := json.Unmarshal(b, &env); err != nil {
			t.Fatal(err)
		}
		if env.ToolName != "read_file" {
			t.Fatal(env.ToolName)
		}
		ids = append(ids, env.ToolUseID)
	}
	if ids[0] == ids[1] {
		t.Fatal("different timestamp calls collapsed")
	}
}
