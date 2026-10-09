package hookcli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/api"
	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/hookcli"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/region"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// registeredInboxSnapshot reads the state written by the real hook without
// duplicating its filename/hash algorithm. Tests retain these bytes across
// remote member removal rather than forcing a first-registration path.
func registeredInboxSnapshot(t *testing.T, stateHome, client, namespace, address string) (string, []byte) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(stateHome, "punk", "inbox", client, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var state struct {
			Namespace    string `json:"namespace"`
			Address      string `json:"address"`
			RegisteredAt int64  `json:"registered_at"`
		}
		if err := json.Unmarshal(raw, &state); err != nil {
			t.Fatal(err)
		}
		if state.Namespace == namespace && state.Address == address && state.RegisteredAt != 0 {
			return path, raw
		}
	}
	t.Fatalf("hook did not persist a confirmed registration for %s in %s", address, namespace)
	return "", nil
}

// codeops:trace repo=punk-records work_item=punk-agent-refresh-20261009 spec=docs/agent-messaging.md plan=hook-registration test=TestInboxRegistrationExpiryRestoresBoundMembership,TestInstalledInboxHookMatrix
func TestInboxRegistrationExpiryRestoresBoundMembership(t *testing.T) {
	for _, removal := range []string{"expiry", "removal"} {
		t.Run(removal, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			stateHome := filepath.Join(dir, "state")
			t.Setenv("XDG_STATE_HOME", stateHome)
			t.Setenv("PUNK_NAMESPACE", "")
			t.Setenv("PUNK_MESSAGING", "1")
			t.Setenv("PUNK_MESSAGING_FROM", "")
			t.Setenv("PUNK_MESSAGING_RENDER_BYTES", "")
			hookcli.SetNamespaceOverride("")
			db, err := store.Open("sqlite", filepath.Join(dir, "inbox.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if _, err := db.MigrateUp(ctx); err != nil {
				t.Fatal(err)
			}
			var clock atomic.Int64
			clock.Store(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC).Unix())
			now := func() time.Time { return time.Unix(clock.Load(), 0).UTC() }
			reg := region.New(db, now)
			const addr = "claude-code:native-session"
			for _, agent := range []string{"lead", addr} {
				if err := reg.Register(ctx, "team", agent, ""); err != nil {
					t.Fatal(err)
				}
			}
			if err := reg.SetInboxBinding(ctx, "team", addr); err != nil {
				t.Fatal(err)
			}
			keys := api.NewKeys(db, nil)
			az := authz.New(db, nil)
			keys.SetAuthorizer(az)
			token, err := keys.Create(ctx, "hook", "hook")
			if err != nil {
				t.Fatal(err)
			}
			for _, ns := range []string{"team", "pinned"} {
				for _, op := range []authz.Op{authz.OpRead, authz.OpWrite} {
					if err := az.Grant(ctx, "hook", ns, op); err != nil {
						t.Fatal(err)
					}
				}
			}
			srv := httptest.NewServer(api.New(slog.New(slog.DiscardHandler), api.Deps{
				Region: reg, Memory: memory.New(db, now), Bus: bus.New(), Keys: keys,
			}).Router())
			defer srv.Close()
			// No namespace pin: /work/inbox derives agent-inbox, so only the
			// surviving explicit binding can route this hook back to team.
			opts := hookcli.InboxOpts{Client: "claude-code", Mode: "context", BaseURL: srv.URL, APIKey: token}
			const payload = `{"session_id":"native-session","cwd":"/work/inbox","hook_event_name":"SessionStart"}`
			run := func() string {
				t.Helper()
				var out, errw bytes.Buffer
				if err := hookcli.Inbox(opts, strings.NewReader(payload), &out, &errw); err != nil {
					t.Fatal(err)
				}
				if errw.Len() != 0 {
					t.Errorf("hook stderr: %s", errw.String())
				}
				return out.String()
			}
			if got, want := run(), nativeExpectedEmptyReply(opts.Client, addr); got != want {
				t.Fatalf("initial hook=%q want=%q", got, want)
			}
			statePath, stateBefore := registeredInboxSnapshot(t, stateHome, opts.Client, "team", addr)
			pending, err := reg.SendMessage(ctx, region.MessageInput{Namespace: "team", Sender: "lead", Recipient: addr, Body: "queued before expiry"})
			if err != nil {
				t.Fatal(err)
			}

			if removal == "expiry" {
				clock.Add(int64((8 * 24 * time.Hour) / time.Second))
				if err := reg.Touch(ctx, "team", "lead"); err != nil {
					t.Fatal(err)
				}
				if n, err := reg.ExpireMembers(ctx, 7*24*time.Hour); err != nil || n != 1 {
					t.Fatalf("member expiry=%d err=%v, want only the hook member expired", n, err)
				}
			} else if removed, err := reg.RemoveMember(ctx, "team", addr); err != nil || !removed {
				t.Fatalf("member removal=%v err=%v", removed, err)
			}
			assertBinding := func() {
				t.Helper()
				if ns, ok, err := reg.InboxBinding(ctx, addr); err != nil || !ok || ns != "team" {
					t.Fatalf("explicit binding changed: namespace=%q bound=%v err=%v", ns, ok, err)
				}
			}
			assertBinding()
			for _, in := range []region.MessageInput{
				{Namespace: "team", Sender: "lead", Recipient: addr, Body: "unregistered recipient"},
				{Namespace: "team", Sender: addr, Recipient: "lead", Body: "unregistered sender"},
			} {
				if _, err := reg.SendMessage(ctx, in); !errors.Is(err, region.ErrMessageNotFound) {
					t.Fatalf("send while member is absent: %v, want ErrMessageNotFound", err)
				}
			}
			if raw, err := os.ReadFile(statePath); err != nil || !bytes.Equal(raw, stateBefore) {
				t.Fatalf("remote %s must leave persisted hook registration intact: %v", removal, err)
			}

			if got, want := run(), nativeExpectedReply(opts.Client, nativeExpectedEnvelope(addr, pending), false); got != want {
				t.Fatalf("resumed hook=%q want=%q", got, want)
			}
			members, err := reg.Regions(ctx, addr)
			if err != nil || len(members) != 1 || members[0].Namespace != "team" {
				t.Fatalf("resumed hook did not recreate bound membership with cached registration: members=%+v err=%v", members, err)
			}
			assertBinding()
			fresh, err := reg.SendMessage(ctx, region.MessageInput{Namespace: "team", Sender: "lead", Recipient: addr, Body: "addressed after resume"})
			if err != nil {
				t.Fatalf("resumed address cannot receive: %v", err)
			}
			if got, want := run(), nativeExpectedReply(opts.Client, nativeExpectedEnvelope(addr, fresh), false); got != want {
				t.Fatalf("addressed delivery=%q want=%q", got, want)
			}
			if n, err := reg.CountUnreadMessages(ctx, "team", addr); err != nil || n != 0 {
				t.Fatalf("resumed messages not ACKed: unread=%d err=%v", n, err)
			}
			reply, err := reg.SendMessage(ctx, region.MessageInput{Namespace: "team", Sender: addr, Recipient: "lead", ReplyTo: fresh.ID, Body: "resumed reply"})
			if err != nil {
				t.Fatalf("resumed address cannot send: %v", err)
			}
			if rows, err := reg.ReadMessages(ctx, "team", "lead", 10); err != nil || len(rows) != 1 || rows[0].ID != reply.ID {
				t.Fatalf("resumed reply not delivered to peer: messages=%+v err=%v", rows, err)
			}

			// An eligible poll pinned elsewhere must register there without
			// taking over the durable binding used by subsequent unpinned polls.
			opts.Namespace = "pinned"
			run()
			assertBinding()
			opts.Namespace = ""
			if got, want := run(), nativeExpectedEmptyReply(opts.Client, addr); got != want {
				t.Fatalf("binding after pinned poll=%q want=%q", got, want)
			}
			assertBinding()
		})
	}
}
