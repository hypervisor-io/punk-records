package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/region"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// brainTestServer is testServer plus a region store and a clock the test
// can read, so members and claims show up in the snapshot.
func brainTestServer(t *testing.T) (*Server, *memory.Store, *region.Store) {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The clock starts at the real now (not a fixed date) because the
	// snapshot's writes_5m window is measured from time.Now(); writes
	// stamped hours in the past would fall outside it.
	var mu sync.Mutex
	clk := time.Now().UTC()
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		clk = clk.Add(time.Millisecond)
		return clk
	}
	mem := memory.New(db, now)
	reg := region.New(db, nil)
	s := New(testLogger(), Deps{Memory: mem, Region: reg})
	s.version = "vtest"
	return s, mem, reg
}

func TestBrainSnapshot(t *testing.T) {
	s, mem, reg := brainTestServer(t)
	ctx := context.Background()

	for _, w := range []struct{ ns, key, body string }{
		{"quiet", "/notes/a", "one"},
		{"busy", "/tasks/T1", "task"},
		{"busy", "/tasks/T2", "task"},
		{"busy", "/tasks/T1/status", "done: x"},
	} {
		if _, err := mem.Write(ctx, memory.WriteInput{Namespace: w.ns, Key: w.key, Body: w.body}); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.Ensure(ctx, "busy", "busy"); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(ctx, "busy", "worker-1", "worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.ClaimWork(ctx, "busy", "/tasks/T2", "worker-1", time.Hour); err != nil {
		t.Fatal(err)
	}

	rec := do(t, s, http.MethodGet, "/v1/brain/snapshot", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var snap brainSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Version != "vtest" || snap.Now == "" {
		t.Fatalf("header: %+v", snap)
	}
	if len(snap.Namespaces) != 2 || snap.Namespaces[0].Name != "busy" {
		t.Fatalf("order: %+v", snap.Namespaces)
	}
	b := snap.Namespaces[0]
	if b.Facts != 3 || b.Tasks != (memory.TaskCounts{Total: 2, Done: 1, Pending: 1}) {
		t.Fatalf("busy counts: %+v", b)
	}
	if len(b.Members) != 1 || b.Members[0].Agent != "worker-1" {
		t.Fatalf("members: %+v", b.Members)
	}
	if len(b.Claims) != 1 || b.Claims[0].Key != "/tasks/T2" {
		t.Fatalf("claims: %+v", b.Claims)
	}
	if b.Writes5m != 3 || b.LastWriteAt == "" {
		t.Fatalf("activity: %+v", b)
	}
	q := snap.Namespaces[1]
	if q.Members == nil || q.Claims == nil {
		t.Fatalf("quiet lists must be empty, not null: %+v", q)
	}
}

func TestBrainSnapshotWithoutRegionStore(t *testing.T) {
	s := testServer(t)
	rec := do(t, s, http.MethodGet, "/v1/brain/snapshot", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var snap brainSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Namespaces == nil {
		t.Fatal("namespaces must be [] not null")
	}
}

func TestSplitBusKey(t *testing.T) {
	for _, tc := range []struct{ in, ns, key string }{
		{"agent-x:/tasks/T1", "agent-x", "/tasks/T1"},
		{"ns:/a:b", "ns", "/a:b"},
		{"task-42", "", "task-42"},
		{"", "", ""},
	} {
		ns, key := splitBusKey(tc.in)
		if ns != tc.ns || key != tc.key {
			t.Fatalf("%q: got (%q,%q) want (%q,%q)", tc.in, ns, key, tc.ns, tc.key)
		}
	}
}

func TestBrainEventsStream(t *testing.T) {
	s, _, _ := brainTestServer(t)
	b := bus.New()
	s.bus = b

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/brain/events", nil).WithContext(ctx)
	pr, pw := io.Pipe()
	rec := &streamRecorder{ResponseRecorder: httptest.NewRecorder(), w: pw}
	done := make(chan struct{})
	go func() {
		s.Router().ServeHTTP(rec, req)
		pw.Close()
		close(done)
	}()

	r := bufio.NewReader(pr)
	readFrame := func() (string, string) {
		t.Helper()
		var ev, data string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case strings.HasPrefix(line, "event: "):
				ev = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			case line == "" && ev != "":
				return ev, data
			}
		}
	}

	ev, data := readFrame()
	if ev != "hello" || !strings.Contains(data, `"version":"vtest"`) {
		t.Fatalf("hello frame: %q %q", ev, data)
	}

	// Subscribe happens before the hello frame is flushed, so a publish
	// after reading hello is guaranteed to be seen.
	b.Publish(bus.Event{Kind: "memory", Key: "agent-x:/tasks/T1/status",
		Data: map[string]string{"action": "add", "writer": "worker-1"}})
	ev, data = readFrame()
	if ev != "memory" {
		t.Fatalf("event name %q", ev)
	}
	var got brainEvent
	if err := json.Unmarshal([]byte(data), &got); err != nil {
		t.Fatal(err)
	}
	if got.Namespace != "agent-x" || got.Key != "/tasks/T1/status" || got.Data["writer"] != "worker-1" || got.TS == "" {
		t.Fatalf("envelope: %+v", got)
	}

	b.Publish(bus.Event{Kind: "task_status", Key: "task-9", Data: map[string]string{"status": "completed"}})
	ev, data = readFrame()
	if ev != "task_status" || !strings.Contains(data, `"namespace":""`) || !strings.Contains(data, `"key":"task-9"`) {
		t.Fatalf("ledger event: %q %q", ev, data)
	}

	cancel()
	<-done
}

func TestBrainEventsWithoutBus(t *testing.T) {
	s := testServer(t)
	rec := do(t, s, http.MethodGet, "/v1/brain/events", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
}

// streamRecorder forwards every Write to a pipe so the test can read the
// stream while the handler is still running (ResponseRecorder alone only
// exposes the body after the handler returns).
type streamRecorder struct {
	*httptest.ResponseRecorder
	w *io.PipeWriter
}

func (s *streamRecorder) Write(p []byte) (int, error) {
	if _, err := s.w.Write(p); err != nil {
		return 0, err
	}
	return s.ResponseRecorder.Write(p)
}

func (s *streamRecorder) Flush() { s.ResponseRecorder.Flush() }
