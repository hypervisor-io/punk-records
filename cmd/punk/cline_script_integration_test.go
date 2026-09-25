package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/hookcli"
)

// Exercises the actual generated native hook and built punk binary, not only
// reply helpers. Unix runtime here; Windows script shape is covered separately.
func TestClineGeneratedHookExecutableRoundtrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix generated hook execution")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "punk binary")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	var mu sync.Mutex
	acks, reads, captures := 0, 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/v1/agent/hooks":
			captures++
			_, _ = io.WriteString(w, `{"status":"stored"}`)
		case "/v1/agent/context":
			_, _ = io.WriteString(w, `{"context":"memory context"}`)
		case "/v1/namespaces/team/members":
			_, _ = io.WriteString(w, `{"status":"registered"}`)
		case "/v1/namespaces/team/messages":
			reads++
			if r.URL.Query().Get("agent") != "cline:t1" {
				t.Errorf("address=%q", r.URL.Query().Get("agent"))
			}
			fmt.Fprintf(w, `{"messages":[{"id":"m%d","namespace":"team","sender":"lead","recipient":"cline:t1","body":"hello worker","created_at":"now"}]}`, reads)
		case "/v1/namespaces/team/messages/ack":
			acks++
			_, _ = io.WriteString(w, `{"acked":1}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	hooks := filepath.Join(dir, "hooks")
	if _, err := hookcli.ConnectCline(hooks, hookcli.ClineConnectOpts{PunkPath: bin, ServerURL: srv.URL, Namespace: "team", Messaging: true}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUNK_MESSAGING", "")
	t.Setenv("PUNK_MESSAGING_FROM", "")
	t.Setenv("PUNK_MESSAGING_RENDER_BYTES", "")
	t.Setenv("PUNK_NAMESPACE", "")
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	for _, event := range []string{"TaskStart", "UserPromptSubmit", "TaskComplete"} {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		cmd := exec.CommandContext(ctx, filepath.Join(hooks, event))
		cmd.Stdin = strings.NewReader(`{"taskId":"t1","hookName":"` + event + `","workspaceRoots":["/work"],"userPromptSubmit":{"prompt":"next"},"taskComplete":{"taskMetadata":{"result":"done"}}}`)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		raw, err := cmd.Output()
		cancel()
		if err != nil {
			t.Fatalf("hook %s: %v stderr=%s", event, err, stderr.String())
		}
		var reply map[string]any
		if err := json.Unmarshal(raw, &reply); err != nil {
			t.Fatalf("not single JSON %s: %q %v", event, raw, err)
		}
		if reply["cancel"] != false || reply["decision"] != nil {
			t.Fatal(reply)
		}
		if event != "TaskComplete" {
			text, _ := reply["contextModification"].(string)
			if !strings.Contains(text, "memory context") || !strings.Contains(text, "hello worker") {
				t.Fatal(reply)
			}
		} else if reply["contextModification"] != nil {
			t.Fatal("terminal inbox injection")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if captures != 3 || reads != 2 || acks != 2 {
		t.Fatalf("capture/read/ACK=%d/%d/%d", captures, reads, acks)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatal(err)
	}
}
