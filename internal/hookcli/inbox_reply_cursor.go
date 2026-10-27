package hookcli

import "encoding/json"

// Cursor inbox adapter (M7). Contract: cursor.com/docs/agent/hooks,
// fetched 2026-09-25 (see /research/hook-clients/cursor in the
// punk-agent-messaging namespace):
//
//   - sessionStart output supports {"additional_context": <text>} -
//     injected into the conversation's initial system context - and is
//     fire-and-forget otherwise (continue/user_message are accepted by
//     the schema but not enforced). This is Cursor's only inbox
//     catch-up surface: beforeSubmitPrompt's output schema is exactly
//     {"continue": bool, "user_message": opt} with no context field at
//     all, so the per-turn catch-up the plan sketched for Cursor does
//     not exist and beforeSubmitPrompt is deliberately never wired for
//     the inbox (punk's capture entry keeps printing {"continue":true}
//     there, unchanged - see RunFrom in hookcli.go).
//   - stop output {"followup_message": <text>} auto-submits the text as
//     the next user message; the input's loop_count counts follow-ups
//     already triggered and the client's per-script loop_limit defaults
//     to 5. Punk's own continuation cap (default 5 per 10 minutes,
//     inbox.go) rides underneath that client cap.
//   - Neither event REQUIRES a reply, so the fail-open and
//     nothing-to-deliver answer for both is an empty stdout - exactly
//     like Cursor's five non-blocking capture events behave today. The
//     one exception is sessionStart's routing identity block on an empty
//     inbox (P1-A), which is context, not a delivery.
//
// session_id exists only on sessionStart/sessionEnd and equals
// conversation_id; the built-in parser (parseCursorInboxPayload,
// inbox.go) already reads conversation_id first.

func init() {
	RegisterInboxClient(InboxClient{
		Name:           "cursor",
		AllowContinue:  true,
		CanCarry:       cursorInboxCanCarry,
		GuidanceEvents: []string{"sessionStart"},
		Reply:          replyCursorInbox,
	})
}

// cursorInboxCanCarry: sessionStart carries context on every fire; stop
// carries content ONLY as a continuation (a followup_message), so a stop
// event whose continuation was refused (cap, downgrade) must not even
// lease the messages - they stay unread for the next sessionStart.
// beforeSubmitPrompt and every other wired Cursor event carry nothing.
func cursorInboxCanCarry(p InboxPayload, cont bool) bool {
	switch p.Event {
	case "sessionStart":
		return true
	case "stop":
		return cont
	}
	return false
}

// replyCursorInbox writes Cursor's reply. sessionStart carries the
// routing guidance (Delivery.Guidance) ahead of any rendered envelope;
// on an empty inbox the guidance alone is a valid additional_context
// object (Cursor has no wake transport, so no wake wording). stop carries
// content only as a continuation.
func replyCursorInbox(req InboxReplyRequest) InboxReply {
	d := req.Delivery
	switch req.Payload.Event {
	case "sessionStart":
		text := d.Guidance + d.Rendered
		if text == "" {
			return InboxReply{}
		}
		return InboxReply{Out: cursorJSONLine(map[string]any{"additional_context": text}), Delivered: d.Rendered != ""}
	case "stop":
		// Rendered without a granted continuation cannot be placed:
		// followup_message is the only content field stop has, and
		// printing it without a reserved slot would defeat the cap.
		if d.Rendered == "" || !d.Continue {
			return InboxReply{}
		}
		return InboxReply{Out: cursorJSONLine(map[string]any{"followup_message": d.Rendered}), Delivered: true}
	}
	return InboxReply{}
}

func cursorJSONLine(v map[string]any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return append(b, '\n')
}
