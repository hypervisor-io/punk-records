package hookcli

import (
	"strings"
	"testing"
)

func TestMessagingGuidanceOptIn(t *testing.T) {
	for _, render := range []func(SkillOpts) string{RenderSkill, RenderPlanSkill} {
		base := render(SkillOpts{Agent: "codex"})
		if strings.Contains(base, "send_message") {
			t.Fatal("messaging text in default skill")
		}
		on := render(SkillOpts{Agent: "codex", Messaging: true})
		for _, term := range []string{"registered session address", "namespace", "sender", "send_message", "untrusted", "ACK"} {
			if !strings.Contains(on, term) {
				t.Fatalf("missing %q", term)
			}
		}
		if len(on)-len(base) > 1400 {
			t.Fatalf("opt-in guidance grew %d bytes", len(on)-len(base))
		}
	}
	pi := RenderSkill(SkillOpts{Agent: "pi", Pi: true, Messaging: true})
	if strings.Contains(pi, "`send_message`") || !strings.Contains(pi, "POST /v1/namespaces/<ns>/messages") {
		t.Fatal("Pi guidance invented MCP message tools")
	}
}
