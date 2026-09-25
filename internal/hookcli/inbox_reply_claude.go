package hookcli

import (
	"encoding/json"
)

// Claude Code and Codex inbox reply writers (task M6). Both clients use
// the same hook reply contract, verified 2026-09-25 against:
//
//   - Claude Code: https://code.claude.com/docs/en/hooks (installed
//     2.1.282). SessionStart and UserPromptSubmit accept
//     {"hookSpecificOutput":{"hookEventName":<event>,"additionalContext":<text>}}.
//     Stop accepts {"decision":"block","reason":<text>} and sends
//     stop_hook_active=true on re-entry. Claude Code ends the turn after
//     8 consecutive blocks. additionalContext, reason and plain stdout
//     are capped at 10,000 characters: longer text is saved to a file
//     and the model gets a path plus a 2,000-character preview.
//   - Codex: https://developers.openai.com/codex/hooks (installed
//     codex-cli 0.156.1). The same nested additionalContext on
//     SessionStart/UserPromptSubmit. Stop "expects JSON on stdout when
//     it exits 0. Plain text output is invalid"; exit 0 with no output
//     is success. {"decision":"block","reason":<text>} turns reason
//     into a continuation prompt. stop_hook_active is present, and no
//     consecutive-block cap is documented, so punk's own cap is the only
//     bound. Model-visible hook output spills to disk past about 2,500
//     tokens.
//
// A Stop event can only carry a continuation, never context: a Stop
// hookSpecificOutput.additionalContext also keeps the turn going on
// Claude Code. So a Stop without a reserved continuation (cap reached,
// stop_hook_active, or messaging off) prints nothing and fetches
// nothing: CanCarry makes the inbox skip the lease entirely.
//
// Idle wake: Claude Code documents an asyncRewake handler field that
// wakes an idle session on exit code 2, with no minimum version stated.
// Codex background hooks never start a turn. Neither is used here.
// --mode wait is a bounded blocking Stop hook (worker mode), not a wake.

// Render caps cover the whole delivery envelope in bytes, including the
// header, footer, neutralisation and truncation notes. Bytes are at least
// characters, so Claude's 10,000-character cap holds. Codex's limit is
// in tokens; 7,000 bytes stays well under 2,500 tokens for typical text,
// and a spill still delivers a preview plus the saved-file path.
const (
	claudeInboxMaxRender = 9000
	codexInboxMaxRender  = 7000
)

func init() {
	for _, c := range []struct {
		name string
		max  int
	}{{"claude-code", claudeInboxMaxRender}, {"codex", codexInboxMaxRender}} {
		RegisterInboxClient(InboxClient{
			Name:           c.name,
			AllowContinue:  true,
			ContinueEvent:  claudeContinueEvent,
			CanCarry:       claudeCanCarry,
			MaxRenderBytes: c.max,
			Reply:          claudeInboxReply,
		})
	}
}

// claudeContinueEvent: only Stop has a continuation contract.
func claudeContinueEvent(p InboxPayload) bool { return p.Event == "Stop" }

// claudeCanCarry: SessionStart and UserPromptSubmit carry context; Stop
// carries content only as a continuation. Any other event (or a payload
// without hook_event_name) carries nothing.
func claudeCanCarry(p InboxPayload, cont bool) bool {
	switch p.Event {
	case "SessionStart", "UserPromptSubmit":
		return true
	case "Stop":
		return cont
	}
	return false
}

// claudeInboxReply writes the Claude-shaped reply. Every path without
// deliverable text prints nothing, which both clients treat as "no
// decision" on every wired event.
func claudeInboxReply(req InboxReplyRequest) InboxReply {
	d := req.Delivery
	if d.Rendered == "" {
		return InboxReply{}
	}
	var v any
	switch req.Payload.Event {
	case "SessionStart", "UserPromptSubmit":
		v = map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName":     req.Payload.Event,
			"additionalContext": d.Rendered,
		}}
	case "Stop":
		if !d.Continue {
			return InboxReply{}
		}
		v = map[string]any{"decision": "block", "reason": d.Rendered}
	default:
		return InboxReply{}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return InboxReply{}
	}
	return InboxReply{Out: append(raw, '\n'), Delivered: true}
}
