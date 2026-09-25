package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/hookcli"
)

func TestMessageReleaseHTTPWriteGrant(t *testing.T) {
	g := authzBoundaryServer(t, true)
	for _, path := range []string{"ns-b", "ns-a"} {
		if path == "ns-a" {
			if err := g.az.Revoke(context.Background(), "alice", path, authz.OpWrite); err != nil {
				t.Fatal(err)
			}
		}
		r := g.do(t, http.MethodPost, "/v1/namespaces/"+path+"/messages/release", `{"agent":"bob","ids":["id"],"leased_by":"hook"}`, nil)
		if r.Code != 403 {
			t.Fatalf("release write auth = %d %s", r.Code, r.Body)
		}
	}
	if err := g.az.Grant(context.Background(), "alice", "ns-a", authz.OpWrite); err != nil {
		t.Fatal(err)
	}
	if r := g.do(t, http.MethodPost, "/v1/namespaces/ns-a/messages/release", `{"agent":"bob","ids":["id"],"leased_by":"hook"}`, nil); r.Code != 200 || strings.TrimSpace(r.Body.String()) != `{"released":0}` {
		t.Fatalf("positive control: %d %s", r.Code, r.Body)
	}
}

func TestHookInboxReleasesHeldBackAndUnrenderedImmediately(t *testing.T) {
	for _, held := range []bool{true, false} {
		t.Run(fmt.Sprint("allowlist=", held), func(t *testing.T) {
			g, decoy, m := deliveryHTTPFixture(t)
			srv := httptest.NewServer(g.s.Router())
			defer srv.Close()
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv("PUNK_MESSAGING", "1")
			t.Setenv("PUNK_MESSAGING_FROM", "")
			if held {
				t.Setenv("PUNK_MESSAGING_FROM", "trusted")
			}
			hookcli.RegisterInboxClient(hookcli.InboxClient{Name: "release-test", Parse: func(_ string, raw []byte) (hookcli.InboxPayload, error) {
				return hookcli.InboxPayload{SessionID: "bob", CWD: "/tmp", Event: "TaskStart", Raw: raw}, nil
			}, Reply: func(hookcli.InboxReplyRequest) hookcli.InboxReply { return hookcli.InboxReply{} }})
			// Native inbox prefixes addresses; create same-address decoys first.
			for _, ns := range []string{"other", "team"} {
				g.register(t, ns, "release-test:bob")
				r := g.do(t, http.MethodPost, "/v1/namespaces/"+ns+"/messages", `{"sender":"alice","recipient":"release-test:bob","body":"held"}`)
				if r.Code != 201 {
					t.Fatal(r.Body)
				}
			}
			var out, errw bytes.Buffer
			if err := hookcli.Inbox(hookcli.InboxOpts{Client: "release-test", Mode: "context", Namespace: "team", BaseURL: srv.URL}, strings.NewReader(`{}`), &out, &errw); err != nil {
				t.Fatal(err)
			}
			for _, ns := range []string{"other", "team"} {
				rows := deliveryHTTPMessages(t, g.do(t, http.MethodGet, "/v1/namespaces/"+ns+"/messages?agent=release-test:bob", ""))
				if len(rows) != 1 || rows[0].LeasedBy != "" || rows[0].AckedAt != "" {
					t.Fatalf("%s not immediately readable: %+v", ns, rows)
				}
			}
			// Existing unrelated rows still readable too.
			for ns, id := range map[string]string{"other": decoy.ID, "team": m.ID} {
				rows := deliveryHTTPMessages(t, g.do(t, http.MethodGet, "/v1/namespaces/"+ns+"/messages?agent=bob", ""))
				if len(rows) != 1 || rows[0].ID != id {
					t.Fatal(rows)
				}
			}
		})
	}
}
