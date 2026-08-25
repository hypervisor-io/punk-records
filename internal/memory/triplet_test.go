package memory

import "testing"

func TestAddLinkDescribedEmbeds(t *testing.T) {
	s, db, _ := newTest(t)
	ctx := t.Context()
	s.SetEmbedder(&fakeEmbedder{m: map[string][]float32{
		"alpha leads to beta": {1, 0, 0},
	}})
	for k, b := range map[string]string{"/a": "fact alpha", "/b": "fact beta"} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: b}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddLinkDescribed(ctx, "ns", "/a", "/b", "leads_to", 1.0, "alpha leads to beta"); err != nil {
		t.Fatal(err)
	}
	links, err := s.Neighbors(ctx, "ns", "/a", "out")
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].Description != "alpha leads to beta" || links[0].LinkType != "leads_to" {
		t.Fatalf("links = %+v, want one leads_to edge with description", links)
	}
	var raw []byte
	if err := db.QueryRowContext(ctx, db.Rebind(
		`SELECT embedding FROM memory_links WHERE namespace = $1 AND from_key = $2 AND to_key = $3`),
		"ns", "/a", "/b").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("embedding not stored for described link")
	}
}

// TestTripletSearchRanksByRelation proves the ranking is driven by the
// EDGE description's query-relevance, not the endpoints. Both candidate
// triplets share endpoint bodies embedded to the SAME vectors pairwise
// (alpha/gamma == {3,4,0}, beta/delta == {4,3,0}), so fromCos and toCos
// are bit-identical between the two triplets and cancel in the score
// difference. Only the edge descriptions differ: one on-topic (matches
// the query exactly), one off-topic (orthogonal). Neither edge vector
// equals any endpoint vector, so a bug that scored the edge off an
// endpoint's embedding (the aliasing hole in the old version of this
// test) would change the score and fail the assertions below.
func TestTripletSearchRanksByRelation(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := t.Context()
	s.SetEmbedder(&fakeEmbedder{m: map[string][]float32{
		"fact alpha":                  {3, 4, 0}, // cos(query) = 0.6
		"fact beta":                   {4, 3, 0}, // cos(query) = 0.8
		"fact gamma":                  {3, 4, 0}, // == alpha's vector
		"fact delta":                  {4, 3, 0}, // == beta's vector
		"alpha leads to beta":         {1, 0, 0}, // edge on-topic: cos(query) = 1
		"gamma leads to delta, noise": {0, 0, 1}, // edge off-topic: cos(query) = 0
		"query about alpha and beta":  {1, 0, 0},
	}})
	for k, b := range map[string]string{
		"/a": "fact alpha", "/b": "fact beta",
		"/c": "fact gamma", "/d": "fact delta",
	} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: b}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddLinkDescribed(ctx, "ns", "/a", "/b", "leads_to", 1.0, "alpha leads to beta"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddLinkDescribed(ctx, "ns", "/c", "/d", "leads_to", 1.0, "gamma leads to delta, noise"); err != nil {
		t.Fatal(err)
	}

	got, err := s.TripletSearch(ctx, "ns", "query about alpha and beta", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d triplets, want 2: %+v", len(got), got)
	}
	first, second := got[0], got[1]
	if first.From.Key != "/a" || first.To.Key != "/b" {
		t.Fatalf("top triplet = %+v, want a->b first (on-topic edge)", first)
	}
	if second.From.Key != "/c" || second.To.Key != "/d" {
		t.Fatalf("second triplet = %+v, want c->d (off-topic edge)", second)
	}
	// Endpoint contributions are identical between the two triplets
	// (alpha/gamma share a vector, beta/delta share a vector), so the
	// entire score gap must equal the edge cosine gap: 1.0 - 0.0 = 1.0.
	// If the edge term were dropped (or scored off the wrong, aliased
	// embedding) from the Score computation, this gap would collapse to
	// ~0 and the assertion below would fail.
	const wantGap = 1.0
	gap := first.Score - second.Score
	if diff := gap - wantGap; diff < -1e-9 || diff > 1e-9 {
		t.Fatalf("score gap = %v, want %v (edge cosine gap; endpoint terms are equal and must cancel)", gap, wantGap)
	}
}

func TestTripletSearchNoEmbedder(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	for k, b := range map[string]string{"/a": "fact alpha", "/b": "fact beta"} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: b}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddLink(ctx, "ns", "/a", "/b", "leads_to"); err != nil {
		t.Fatal(err)
	}
	got, err := s.TripletSearch(ctx, "ns", "anything", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d triplets with no embedder, want 0", len(got))
	}
}
