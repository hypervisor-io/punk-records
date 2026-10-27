package hookcli

import "encoding/json"

// File hooks need one JSON object. TaskStart/UserPromptSubmit injection was
// repaired in 4.1.20 (CHANGELOG.md); no Stop block or wake API exists here.
func init() {
	RegisterInboxClient(InboxClient{Name: "cline", CanCarry: clineCanCarry,
		GuidanceEvents: []string{"TaskStart", "UserPromptSubmit"}, MaxRenderBytes: 32 * 1024, Reply: clineInboxReply})
}

func clineCanCarry(p InboxPayload, _ bool) bool {
	return p.Event == "TaskStart" || p.Event == "UserPromptSubmit"
}

type clineReply struct {
	Cancel              bool   `json:"cancel"`
	ContextModification string `json:"contextModification,omitempty"`
}

// clineInboxReply writes Cline's one-object reply. The routing guidance
// (Delivery.Guidance) is prepended to any rendered envelope on a
// context-carrying event; on an empty inbox the guidance alone is still a
// valid {"cancel":false,"contextModification":...} object (Cline has no
// wake transport, so no wake wording). Other events keep the bare
// {"cancel":false} minimum.
func clineInboxReply(r InboxReplyRequest) InboxReply {
	text := ""
	if clineCanCarry(r.Payload, false) {
		text = r.Delivery.Guidance + r.Delivery.Rendered
	}
	out, _ := json.Marshal(clineReply{ContextModification: text})
	return InboxReply{Out: append(out, '\n'), Delivered: text != ""}
}
