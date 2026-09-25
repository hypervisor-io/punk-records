package hookcli

import "encoding/json"

// Antigravity inbox adapter (M7). Contract: antigravity.google/docs/
// hooks, fetched 2026-09-25 (see /research/hook-clients/antigravity in
// the punk-agent-messaging namespace):
//
//   - PreInvocation output {"injectSteps": [{"ephemeralMessage":
//     <text>}]} injects a transient system message before the model
//     call. PreInvocation fires before EVERY model invocation
//     (invocationNum is 0-indexed per call), so unlike the capture
//     path's invocationNum==0 gate (translateAntigravity, normalize.go)
//     the inbox injects on every invocation while unread messages
//     exist - the messages disappear once acked, so a quiet inbox
//     prints nothing. injectSteps is Optional; silence is valid.
//   - Stop output decision is REQUIRED: "continue" prevents the stop
//     and re-enters the execution loop, any other value allows it
//     (docs' own wording). reason is the ONLY message carrier: "if
//     decision is continue, this message is injected as a system
//     message" - a bare {"decision":"continue"} would re-enter the loop
//     with no content, so the continuation reply is always
//     {"decision":"continue","reason":<rendered>}. The fail-open and
//     not-delivered answer is {"decision":"allow"}, printed on every
//     Stop invocation exactly like printAntigravityStopReply does for
//     the capture path (hookcli.go).
//   - Antigravity documents NO loop cap and has no stop_hook_active
//     equivalent; punk's own continuation cap is the only guard. The
//     Stop input's fullyIdle (false while background tasks run) is
//     parsed into InboxPayload by the built-in parser but not gated on
//     here: the docs do not forbid continuing with background tasks
//     running, and the messages are addressed to the session, not to a
//     task.
//   - The event never comes from the payload (Antigravity payloads do
//     not name it); cmdHookInbox's --event flag carries it, baked into
//     the hook entry by punkInboxHookCommand (inbox_wire.go), the same
//     workaround ConnectAntigravity uses for capture.

func init() {
	RegisterInboxClient(InboxClient{
		Name:          "antigravity",
		AllowContinue: true,
		CanCarry:      antigravityInboxCanCarry,
		Reply:         replyAntigravityInbox,
	})
}

// antigravityInboxCanCarry: PreInvocation carries injectSteps on every
// fire; Stop carries content only inside a continuation's reason. A
// Stop whose continuation was refused (cap) must not lease messages it
// would then have to leave undelivered - they wait for the next
// PreInvocation instead.
func antigravityInboxCanCarry(p InboxPayload, cont bool) bool {
	switch p.Event {
	case "PreInvocation":
		return true
	case "Stop":
		return cont
	}
	return false
}

func replyAntigravityInbox(req InboxReplyRequest) InboxReply {
	d := req.Delivery
	switch req.Payload.Event {
	case "Stop":
		// decision is Required on Stop on every path - delivered or
		// not, fail-open included (mirrors printAntigravityStopReply's
		// "never leave a blocking hook hanging" discipline).
		if d.Rendered != "" && d.Continue {
			return InboxReply{Out: antigravityJSONLine(map[string]any{
				"decision": "continue", "reason": d.Rendered}), Delivered: true}
		}
		return InboxReply{Out: antigravityJSONLine(map[string]any{"decision": "allow"})}
	case "PreInvocation":
		if d.Rendered == "" {
			return InboxReply{}
		}
		return InboxReply{Out: antigravityJSONLine(map[string]any{
			"injectSteps": []map[string]string{{"ephemeralMessage": d.Rendered}},
		}), Delivered: true}
	}
	return InboxReply{}
}

func antigravityJSONLine(v map[string]any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return append(b, '\n')
}
