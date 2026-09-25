package hookcli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"unicode/utf8"
)

// RunFromCline composes memory capture/context and the shared Inbox delivery
// into one native reply. The merging writer writes directly to stdout, so
// Inbox only ACKs after the complete merged reply was successfully printed.
func RunFromCline(stdin io.Reader, baseURL, apiKey string, messaging bool, out, errw io.Writer) error {
	baseURL = strings.TrimRight(baseURL, "/")
	raw, err := io.ReadAll(io.LimitReader(stdin, maxStdinBytes+1))
	if err != nil || len(raw) > maxStdinBytes {
		fmt.Fprintln(errw, "punk hook: cline: cannot read bounded payload")
		fmt.Fprintln(out, `{"cancel":false}`)
		return nil
	}
	translated, ok, err := Normalize("cline", raw)
	if err != nil {
		fmt.Fprintln(errw, "punk hook: cline:", err)
		fmt.Fprintln(out, `{"cancel":false}`)
		return nil
	}
	var memory string
	if ok {
		forwardHook(baseURL, apiKey, translated, errw)
		var p hookPayload
		_ = json.Unmarshal(translated, &p)
		var native clinePayload
		_ = json.Unmarshal(raw, &native)
		var params url.Values
		switch native.HookName {
		case "TaskStart":
			params = sessionParams(p.CWD, p.SessionID, "cline:TaskStart")
		case "UserPromptSubmit":
			if p.Prompt != "" {
				params = turnParams(p.CWD, p.SessionID, p.Prompt, p.PromptID)
			}
		}
		if params != nil {
			memory, _ = fetchContext(baseURL, apiKey, params, errw)
		}
	}
	w := &clineContextWriter{out: out, memory: memory}
	return Inbox(InboxOpts{Client: "cline", Mode: "context", BaseURL: baseURL, APIKey: apiKey, Namespace: namespaceOverride, Enabled: messaging}, bytes.NewReader(raw), w, errw)
}

type clineContextWriter struct {
	out    io.Writer
	memory string
}

func (w *clineContextWriter) Write(p []byte) (int, error) {
	var reply clineReply
	if err := json.Unmarshal(p, &reply); err != nil {
		return 0, err
	}
	// Cline truncates after 50,000 UTF-16 units. A conservative byte bound
	// also bounds UTF-16 units; reserve the complete shared Inbox envelope
	// (hard-capped at 32 KiB by the adapter), clipping memory only.
	memory := w.memory
	room := 50000 - len(reply.ContextModification) - 2
	if room < 0 {
		return 0, fmt.Errorf("cline: inbox exceeds native context limit")
	}
	if len(memory) > room {
		memory = memory[:room]
		for !utf8.ValidString(memory) {
			memory = memory[:len(memory)-1]
		}
	}
	if memory != "" {
		if reply.ContextModification != "" {
			reply.ContextModification = memory + "\n\n" + reply.ContextModification
		} else {
			reply.ContextModification = memory
		}
	}
	raw, err := json.Marshal(reply)
	if err != nil {
		return 0, err
	}
	raw = append(raw, '\n')
	n, err := w.out.Write(raw)
	if err == nil && n != len(raw) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return 0, err
	}
	return len(p), nil
}
