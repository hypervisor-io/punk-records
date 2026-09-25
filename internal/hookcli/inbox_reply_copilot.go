package hookcli

import "encoding/json"

// GitHub Copilot CLI inbox adapter (M7). Contract: docs.github.com/en/
// copilot/reference/hooks-reference, fetched 2026-09-25 (see
// /research/hook-clients/copilot in the punk-agent-messaging
// namespace):
//
//   - sessionStart output processing is "Optional - can inject
//     additionalContext into the session" (Hook events table); the flat
//     {"additionalContext": <text>} shape printed here matches the
//     docs' postToolUse/notification output blocks and the shape
//     RunFromCopilot already prints for memory context. Fail-safe: an
//     unrecognized field is ignored, never an error.
//   - userPromptSubmitted is deliberately NOT an inbox surface: the
//     docs state command/HTTP config-file hook output for that event is
//     DROPPED (modifiedPrompt is honored by SDK programmatic hooks
//     only). Wiring it would lease messages, print text Copilot
//     discards, and ack content that was never delivered - exactly the
//     failure mode the ack-after-print contract forbids. Copilot's
//     catch-up is therefore sessionStart-only, plus agentStop
//     continuation.
//   - agentStop (punk wires it under its PascalCase alias Stop) accepts
//     {"decision": "block", "reason": <text>}; reason becomes the
//     prompt of the forced next turn. The input's stop_hook_active is
//     true when a prior block already forced this turn, and the CLI
//     overrides the hook after 8 consecutive blocks (documented runaway
//     guard) - both are enforced in inbox.go (StopHookActive always
//     wins; punk's own cap, default 5 per 10 minutes, sits under the
//     client's 8).
//   - Stdout discipline: Copilot strips {"type":"progress"} lines and
//     JSON-parses the REST as ONE document - two JSON objects on stdout
//     concatenate into invalid JSON and are ignored. Every reply here
//     is exactly one JSON line. No event punk wires REQUIRES a reply,
//     so the fail-open answer is empty stdout (docs: empty/unparseable
//     output falls through to default behavior).

func init() {
	RegisterInboxClient(InboxClient{
		Name:          "copilot",
		AllowContinue: true,
		CanCarry:      copilotInboxCanCarry,
		Reply:         replyCopilotInbox,
	})
}

// copilotInboxCanCarry: SessionStart carries context on every fire;
// Stop carries content only as a block continuation. UserPromptSubmit's
// output is dropped by the client and PostToolUse/SessionEnd are not
// inbox surfaces, so they never even lease.
func copilotInboxCanCarry(p InboxPayload, cont bool) bool {
	switch p.Event {
	case "SessionStart":
		return true
	case "Stop":
		return cont
	}
	return false
}

func replyCopilotInbox(req InboxReplyRequest) InboxReply {
	d := req.Delivery
	if d.Rendered == "" {
		return InboxReply{}
	}
	switch req.Payload.Event {
	case "SessionStart":
		return InboxReply{Out: copilotJSONLine(map[string]any{"additionalContext": d.Rendered}), Delivered: true}
	case "Stop":
		if !d.Continue {
			return InboxReply{}
		}
		return InboxReply{Out: copilotJSONLine(map[string]any{"decision": "block", "reason": d.Rendered}), Delivered: true}
	}
	return InboxReply{}
}

func copilotJSONLine(v map[string]any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return append(b, '\n')
}
