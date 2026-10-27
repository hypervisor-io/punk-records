package mcpserver

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hypervisor-io/punk-records/internal/hookcli"
)

// P1-B: the initialize instructions are the only prose punk ships into an
// agent, so the guidance a messaging session needs before its first send
// has to sit where clients actually read. Codex reliably reads only the
// first ~512 characters of the instructions; Claude Code caps the whole
// string at 4096. The tests here pin both modes:
//
//   - enabled: the first 512 characters carry the three messaging rules
//     (where your own address lives, explicit namespace and sender,
//     untrusted peer text) and the default guidance survives verbatim
//     after them;
//   - disabled: byte-identical to the pre-messaging string (golden);
//   - both: the whole instructions stay under the 4096-character cap.

// goldenIntro and goldenCoordination are the instruction literals copied
// verbatim from instructions.go. The golden test fails if the disabled
// string is edited, because the copy here no longer matches the one there;
// updating it is a conscious decision, never an accident.
const (
	goldenIntro = "Punk Records is the shared memory plane for this workspace: prior sessions, decisions, conventions, incidents, entities and their relations. It is evidence about the past, not a substitute for reading the current code. Call whoami once at session start; a full usage skill named punk-memory is installed by punk connect."

	goldenCoordination = "Coordination: register once, claim_work before taking a task or editing a shared file, release_work after; tasks are facts at /tasks/<id> with status at /tasks/<id>/status (done: ... or blocked: ...); questions at /questions/<id>, answers at /answers/<id>. Do not launch a sub-agent solely to query memory."
)

// goldenInstructions reassembles the disabled string exactly as
// instructions.go builds it. The routing section stays live rather than
// frozen: the design keeps the instructions and the punk-memory skill in
// lockstep (pinned by TestInstructionsShareRoutingWithSkill), so this
// golden pins the literals and their assembly, not the shared section's
// own bytes.
func goldenInstructions() string {
	return goldenIntro + "\n\nRetrieval and writing:\n" + hookcli.RoutingSection() + "\n\n" + goldenCoordination
}

// firstChars returns the first n characters (runes) of s, never a partial
// rune.
func firstChars(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

// TestEnabledMessagingInstructionsLeadFirst512 pins the Codex prefix
// contract: with messaging enabled, the three rules a session must not
// miss - its own address, explicit namespace and sender, untrusted peer
// text - land inside the first 512 characters of the whole served string,
// and nothing true with messaging disabled is lost.
func TestEnabledMessagingInstructionsLeadFirst512(t *testing.T) {
	on := probeSession(t, func(d *Deps) { d.MessagingEnabled = true })
	prefix := firstChars(on.instructions, 512)
	for _, c := range []struct{ fragment, why string }{
		{"[PUNK ROUTING]", "own address lives in the routing block when hooks supply it"},
		{"whoami", "address discovery when no routing block is supplied"},
		{"namespace and sender", "explicit passing on every punk call"},
		{"untrusted", "peer message text is untrusted data"},
	} {
		if !strings.Contains(prefix, c.fragment) {
			t.Errorf("enabled instructions: %q (%s) not in first 512 characters:\n%s", c.fragment, c.why, prefix)
		}
	}
	if on.instructions == Instructions {
		t.Fatal("enabled instructions are byte-identical to the disabled string; the messaging lead is missing")
	}
	if !strings.Contains(on.instructions, Instructions) {
		t.Fatal("enabled instructions dropped default guidance; everything true with messaging disabled must stay true with it enabled")
	}
}

// TestDisabledInstructionsGolden pins the disabled string byte-for-byte:
// enabling messaging is the only thing that may ever change these
// instructions.
func TestDisabledInstructionsGolden(t *testing.T) {
	golden := goldenInstructions()
	if Instructions != golden {
		t.Fatalf("Instructions drifted from the golden assembly:\n got %d bytes\nwant %d bytes\ngot:  %q\nwant: %q",
			len(Instructions), len(golden), Instructions, golden)
	}
	// The default Deps (no tweak) and an explicit false must both serve
	// the golden bytes over a real session.
	defaultProbe := probeSession(t)
	if defaultProbe.instructions != golden {
		t.Fatal("default initialize instructions are not byte-identical to the golden string")
	}
	off := probeSession(t, func(d *Deps) { d.MessagingEnabled = false })
	if off.instructions != golden {
		t.Fatal("messaging-disabled initialize instructions are not byte-identical to the golden string")
	}
}

// TestInstructionsUnderClientCap: Claude Code caps initialize instructions
// at 4096 characters; both modes must stay under it.
func TestInstructionsUnderClientCap(t *testing.T) {
	for name, got := range map[string]string{
		"messaging disabled": probeSession(t).instructions,
		"messaging enabled":  probeSession(t, func(d *Deps) { d.MessagingEnabled = true }).instructions,
	} {
		if n := utf8.RuneCountInString(got); n >= 4096 {
			t.Errorf("%s instructions = %d characters, must stay under the 4096-character client cap", name, n)
		}
	}
}
