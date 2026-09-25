package hookcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestClineCombinedCaptureMemoryAndInbox(t *testing.T) {
	for _, messaging := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[messaging], func(t *testing.T) {
			t.Setenv("PUNK_MESSAGING", "")
			t.Setenv("PUNK_MESSAGING_FROM", "")
			t.Setenv("PUNK_NAMESPACE", "")
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			var out, errw bytes.Buffer
			var mu sync.Mutex
			var captured claudeEnvelope
			ack, requests := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch r.URL.Path {
				case "/v1/agent/hooks":
					if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
						t.Error(err)
					}
					_, _ = io.WriteString(w, `{"status":"stored"}`)
				case "/v1/agent/context":
					_, _ = io.WriteString(w, `{"context":"memory facts"}`)
				case "/v1/agent/namespace":
					_, _ = io.WriteString(w, `{"namespace":"team"}`)
				case "/v1/namespaces/team/members":
					requests++
					_, _ = io.WriteString(w, `{"status":"registered"}`)
				case "/v1/namespaces/team/messages":
					requests++
					if r.URL.Query().Get("agent") != "cline:task-1" || r.URL.Query().Get("leased_by") == "" {
						t.Error("bad address/lease")
					}
					_, _ = io.WriteString(w, `{"messages":[{"id":"m","namespace":"team","sender":"lead","recipient":"cline:task-1","body":"peer body","created_at":"now"}]}`)
				case "/v1/namespaces/team/messages/ack":
					// The composed reply, not an intermediate buffer, must already be written.
					if !strings.Contains(out.String(), "memory facts") || !strings.Contains(out.String(), "peer body") {
						t.Error("ACK before composed stdout")
					}
					ack++
					_, _ = io.WriteString(w, `{"acked":1}`)
				default:
					t.Errorf("unexpected %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			if err := RunFromCline(strings.NewReader(`{"taskId":"task-1","hookName":"TaskStart","workspaceRoots":["/work"],"taskStart":{"taskMetadata":{"initialTask":"build"}}}`), srv.URL, "", messaging, &out, &errw); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			var reply map[string]any
			if err := json.Unmarshal(out.Bytes(), &reply); err != nil {
				t.Fatalf("not one JSON reply: %q %v", out.String(), err)
			}
			if reply["cancel"] != false || reply["decision"] != nil || !strings.Contains(reply["contextModification"].(string), "memory facts") {
				t.Fatal(reply)
			}
			if captured.SessionID != "task-1" || captured.Source != "cline" || captured.HookEventName != "SessionStart" {
				t.Fatalf("capture=%+v", captured)
			}
			if messaging {
				if ack != 1 {
					t.Fatalf("acks=%d err=%s", ack, errw.String())
				}
			} else if ack != 0 || requests != 0 {
				t.Fatalf("disabled messaging requests %d ack %d", requests, ack)
			}
		})
	}
}

type clineFailWriter struct{}

func (clineFailWriter) Write([]byte) (int, error) { return 0, errors.New("stdout closed") }

func TestClineComposeWriterFailure(t *testing.T) {
	w := clineContextWriter{out: clineFailWriter{}, memory: "memory"}
	if _, err := w.Write([]byte(`{"cancel":false,"contextModification":"peer"}`)); err == nil {
		t.Fatal("write failure swallowed before ACK")
	}
}

func TestClineContextBudgetPreservesInboxTail(t *testing.T) {
	// Upstream hook-factory.ts slices contextModification after 50,000 JS
	// characters. Never let memory push an ACKed message outside that limit.
	var out bytes.Buffer
	w := clineContextWriter{out: &out, memory: strings.Repeat("🦕", 20000)}
	peer := strings.Repeat("p", 32*1024-8) + "INBOXEND"
	raw, _ := json.Marshal(clineReply{ContextModification: peer})
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	var reply clineReply
	if err := json.Unmarshal(out.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if len(reply.ContextModification) > 50000 || !strings.HasSuffix(reply.ContextModification, "INBOXEND") {
		t.Fatalf("context length=%d tail lost", len(reply.ContextModification))
	}
	c, _ := lookupInboxClient("cline")
	if c.MaxRenderBytes != 32*1024 {
		t.Fatalf("uncapped inbox adapter: %+v", c)
	}
}

func TestClineCaptureFailOpenAndNoToolInjection(t *testing.T) {
	t.Setenv("PUNK_MESSAGING", "0")
	for _, raw := range []string{`{`, `{"taskId":"t","hookName":"Notification"}`, `{"taskId":"t","hookName":"PostToolUse","postToolUse":{"toolName":"read_file","result":"ok"}}`} {
		var out, errw bytes.Buffer
		if err := RunFrom("cline", strings.NewReader(raw), "http://127.0.0.1:1", "", &out, &errw); err != nil {
			t.Fatal(err)
		}
		if out.String() != `{"cancel":false}`+"\n" {
			t.Fatalf("minimum reply=%q", out.String())
		}
	}
}

func TestClineCombinedFailedStdoutNeverACKs(t *testing.T) {
	t.Setenv("PUNK_MESSAGING", "")
	t.Setenv("PUNK_MESSAGING_FROM", "")
	t.Setenv("PUNK_NAMESPACE", "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var mu sync.Mutex
	ack, released := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/v1/agent/hooks":
			_, _ = io.WriteString(w, `{"status":"stored"}`)
		case "/v1/agent/context":
			_, _ = io.WriteString(w, `{"context":"memory"}`)
		case "/v1/agent/namespace":
			_, _ = io.WriteString(w, `{"namespace":"team"}`)
		case "/v1/namespaces/team/members":
			_, _ = io.WriteString(w, `{"status":"registered"}`)
		case "/v1/namespaces/team/messages":
			_, _ = io.WriteString(w, `{"messages":[{"id":"m","namespace":"team","sender":"lead","recipient":"cline:t","body":"peer","created_at":"now"}]}`)
		case "/v1/namespaces/team/messages/ack":
			ack++
			_, _ = io.WriteString(w, `{"acked":1}`)
		case "/v1/namespaces/team/messages/release":
			released++
			_, _ = io.WriteString(w, `{"released":1}`)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	var errw bytes.Buffer
	if err := RunFromCline(strings.NewReader(`{"taskId":"t","hookName":"TaskStart","workspaceRoots":["/work"]}`), srv.URL, "", true, clineFailWriter{}, &errw); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if ack != 0 || released != 1 {
		t.Fatalf("failed composed stdout: ack=%d released=%d stderr=%s", ack, released, errw.String())
	}
}
