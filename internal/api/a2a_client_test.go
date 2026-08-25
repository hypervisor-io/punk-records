package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/a2a"
)

// TestA2AClientRoundTrip drives the outbound A2A client against our own
// inbound A2A server over real HTTP - card fetch, send, get, cancel and a
// streamed exchange - proving both halves interoperate with no mocks.
func TestA2AClientRoundTrip(t *testing.T) {
	s := a2aServer(t)
	ts := httptest.NewServer(s.Router())
	defer ts.Close()
	ctx := context.Background()
	cl := a2a.NewClient(ts.URL+"/v1/a2a", "")

	// discovery
	card, err := cl.FetchCard(ctx, ts.URL)
	if err != nil {
		t.Fatalf("fetch card: %v", err)
	}
	if card.ProtocolVersion != a2a.ProtocolVersion {
		t.Fatalf("bad protocolVersion %q", card.ProtocolVersion)
	}
	hasDB := false
	for _, sk := range card.Skills {
		if sk.ID == "database" {
			hasDB = true
		}
	}
	if !hasDB {
		t.Fatalf("card missing database skill: %+v", card.Skills)
	}

	// delegate a message
	tk, err := cl.SendMessage(ctx, a2a.TextMessage("hello from the client"), nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if tk.ID == "" || tk.Kind != a2a.KindTask {
		t.Fatalf("bad task: %+v", tk)
	}

	// fetch it back
	got, err := cl.GetTask(ctx, tk.ID, 0)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != tk.ID || len(got.History) != 1 {
		t.Fatalf("get mismatch: %+v", got)
	}

	// cancel it
	canceled, err := cl.CancelTask(ctx, tk.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if canceled.Status.State != a2a.StateCanceled {
		t.Fatalf("want canceled, got %q", canceled.Status.State)
	}

	// streamed exchange: on the first Task frame, cancel it from another
	// goroutine so the stream terminates with a final status-update.
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var gotTask, gotFinal bool
	err = cl.StreamMessage(sctx, a2a.TextMessage("stream me"), nil, func(kind string, raw json.RawMessage) error {
		switch kind {
		case a2a.KindTask:
			gotTask = true
			var streamed a2a.Task
			_ = json.Unmarshal(raw, &streamed)
			go func() { _, _ = cl.CancelTask(context.Background(), streamed.ID) }()
		case a2a.KindStatusUpdate:
			gotFinal = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if !gotTask || !gotFinal {
		t.Fatalf("stream incomplete: gotTask=%v gotFinal=%v", gotTask, gotFinal)
	}
}
