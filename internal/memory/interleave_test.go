package memory

import "testing"

func TestInterleaveSearchAlternatesArms(t *testing.T) {
	s := newTestStore(t)
	// FTS matches only "/f" (its body contains the query word "alpha").
	// Vector arm: query embeds identical to "/v"'s vector and orthogonal
	// to "/f"'s, so the two arms genuinely disagree on rank 1.
	s.SetEmbedder(&fakeEmbedder{m: map[string][]float32{
		"alpha alpha alpha": {1, 0, 0},
		"unrelated body":    {0, 0, 1},
		"alpha":             {0, 0, 1},
	}})
	ctx := t.Context()
	for k, b := range map[string]string{"/f": "alpha alpha alpha", "/v": "unrelated body"} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: b}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := s.InterleaveSearch(ctx, "ns", "alpha", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("got %d results, want 2", len(res))
	}
	// both arms' #1 hits occupy the first two slots, whatever the order
	keys := map[string]bool{res[0].Key: true, res[1].Key: true}
	if !keys["/f"] || !keys["/v"] {
		t.Fatalf("first two = %v, want /f and /v", keys)
	}
	if res[0].Components["arm_rank"] != 1 || res[1].Components["arm_rank"] != 1 {
		t.Fatalf("expected both arm-rank-1 hits first: %+v", res)
	}
}

func TestInterleaveSearchDegradesWithoutEmbedder(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "alpha alpha alpha"}); err != nil {
		t.Fatal(err)
	}
	res, err := s.InterleaveSearch(ctx, "ns", "alpha", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Key != "/f" {
		t.Fatalf("got %+v, want single FTS hit", res)
	}
	if res[0].Components["arm"] != 0 || res[0].Components["arm_rank"] != 1 {
		t.Fatalf("expected fts arm/rank 1: %+v", res[0].Components)
	}
}
