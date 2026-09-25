package hookcli

import "encoding/json"

// File hooks need one JSON object. TaskStart/UserPromptSubmit injection was
// repaired in 4.1.20 (CHANGELOG.md); no Stop block or wake API exists here.
func init() {
	RegisterInboxClient(InboxClient{Name: "cline", CanCarry: clineCanCarry, MaxRenderBytes: 32 * 1024, Reply: clineInboxReply})
}

func clineCanCarry(p InboxPayload, _ bool) bool {
	return p.Event == "TaskStart" || p.Event == "UserPromptSubmit"
}

type clineReply struct {
	Cancel              bool   `json:"cancel"`
	ContextModification string `json:"contextModification,omitempty"`
}

func clineInboxReply(r InboxReplyRequest) InboxReply {
	text := ""
	if clineCanCarry(r.Payload, false) {
		text = r.Delivery.Rendered
	}
	out, _ := json.Marshal(clineReply{ContextModification: text})
	return InboxReply{Out: append(out, '\n'), Delivered: text != ""}
}
