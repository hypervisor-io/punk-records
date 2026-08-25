package memory

import "testing"

// Two seeds match the query; a third "principle" fact matches nothing
// textually but is linked to both seeds. Bridge discovery must surface
// it (the multi-hop insight).
func TestBridgeDiscovery(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	w := func(key, body string) {
		t.Helper()
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: key, Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	w("/decisions/pg", "chose postgres for the queue backend")
	w("/decisions/kafka", "evaluating postgres vs kafka for queueing")
	w("/principles/boring", "team prefers boring proven technology")
	for _, from := range []string{"/decisions/pg", "/decisions/kafka"} {
		if err := s.AddLink(ctx, "ns", from, "/principles/boring", "exemplifies"); err != nil {
			t.Fatal(err)
		}
	}
	res, err := s.HybridSearchScored(ctx, "ns", "postgres", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sf := range res {
		if sf.Key == "/principles/boring" {
			found = true
			if sf.Components["bridge"] <= 0 {
				t.Fatalf("bridge component = %v, want > 0", sf.Components["bridge"])
			}
		}
	}
	if !found {
		t.Fatalf("bridge fact not in results: %+v", res)
	}
}

func TestInvalidatedDemotion(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	w := func(key, body string) {
		t.Helper()
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: key, Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	w("/runbooks/old", "failover runbook for postgres cluster")
	w("/runbooks/new", "failover runbook for postgres cluster v2")
	if err := s.AddLink(ctx, "ns", "/runbooks/old", "/runbooks/new", "invalidated_by"); err != nil {
		t.Fatal(err)
	}
	res, err := s.HybridSearchScored(ctx, "ns", "failover runbook", 10, 0)
	if err != nil || len(res) < 2 {
		t.Fatalf("res %v err %v", res, err)
	}
	if res[0].Key != "/runbooks/new" {
		t.Fatalf("top = %s, want /runbooks/new (old is demoted)", res[0].Key)
	}
	for _, sf := range res {
		if sf.Key == "/runbooks/old" && sf.Components["invalidated"] != 1 {
			t.Fatalf("old runbook missing invalidated component: %v", sf.Components)
		}
	}
}
