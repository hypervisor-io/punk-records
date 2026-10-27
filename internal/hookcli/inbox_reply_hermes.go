package hookcli

import "encoding/json"

// Hermes Agent inbox adapter (M7 addendum: the user's goal includes
// Hermes and no other task covers it). Contract: hermes-agent.
// nousresearch.com/docs/user-guide/features/hooks (same source
// RunFromHermes' injection path is built on):
//
//   - pre_llm_call runs before EVERY model call and accepts a flat
//     {"context": <text>} shell-hook reply that Hermes appends to the
//     user message. That makes Hermes a per-turn catch-up client: every
//     turn re-checks the inbox, so messages arrive at the next model
//     call regardless of when they were sent. (RunFromHermes' memory
//     injection gates the session block to extra.is_first_turn; the
//     inbox needs no such gate - acked messages stop appearing, so a
//     quiet inbox carries only the routing identity block.)
//   - Hermes has no stop hook that can continue and no wake mechanism:
//     AllowContinue is false, so --mode continue/wait downgrade to
//     context in inbox.go before anything is leased. Catch-up only.
//   - No wired Hermes event REQUIRES a reply (pre_llm_call treats a
//     missing reply as "no context to add"; the rest are observation
//     hooks whose return value is ignored), so the fail-open answer is
//     empty stdout, matching RunFromHermes.

func init() {
	RegisterInboxClient(InboxClient{
		Name:           "hermes",
		CanCarry:       hermesInboxCanCarry,
		GuidanceEvents: []string{"pre_llm_call"},
		Reply:          replyHermesInbox,
	})
}

// hermesInboxCanCarry: pre_llm_call is the only Hermes event with a
// content-carrying reply contract.
func hermesInboxCanCarry(p InboxPayload, cont bool) bool {
	return p.Event == "pre_llm_call"
}

// replyHermesInbox writes the flat {"context": <text>} pre_llm_call
// reply. The routing guidance (Delivery.Guidance) is prepended to any
// rendered envelope, and on an empty inbox the guidance alone is still a
// valid single JSON object (Hermes has no wake transport, so the block
// carries no wake wording).
func replyHermesInbox(req InboxReplyRequest) InboxReply {
	d := req.Delivery
	if req.Payload.Event != "pre_llm_call" {
		return InboxReply{}
	}
	text := d.Guidance + d.Rendered
	if text == "" {
		return InboxReply{}
	}
	b, err := json.Marshal(map[string]any{"context": text})
	if err != nil {
		return InboxReply{}
	}
	return InboxReply{Out: append(b, '\n'), Delivered: d.Rendered != ""}
}
