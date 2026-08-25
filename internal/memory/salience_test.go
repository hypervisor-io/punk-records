package memory

import (
	"context"
	"testing"
)

// newTestStore wraps newTest (memory_test.go) to drop the *store.DB and
// *fakeClock the salience tests don't need.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, _, _ := newTest(t)
	return s
}

func TestImportanceRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	f, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/svc/db",
		Body: "primary is pg14", Importance: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	if f.Importance != 0.9 {
		t.Fatalf("returned Importance = %v, want 0.9", f.Importance)
	}
	got, err := s.Recall(ctx, "ns", "/svc", 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("recall: %v %v", got, err)
	}
	if got[0].Importance != 0.9 || got[0].AccessCount != 0 {
		t.Fatalf("recalled fact = imp %v acc %d, want 0.9, 0", got[0].Importance, got[0].AccessCount)
	}
}

func TestImportanceClamped(t *testing.T) {
	s := newTestStore(t)
	f, err := s.Write(context.Background(), WriteInput{Namespace: "ns", Key: "/k", Body: "x", Importance: 7})
	if err != nil || f.Importance != 1 {
		t.Fatalf("Importance = %v (err %v), want clamped to 1", f.Importance, err)
	}
}

func TestTouchAccess(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	f, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/k", Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.TouchAccess(ctx, []string{f.ID}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Recall(ctx, "ns", "/k", 1)
	if len(got) != 1 || got[0].AccessCount != 1 {
		t.Fatalf("AccessCount = %v, want 1", got)
	}
}
