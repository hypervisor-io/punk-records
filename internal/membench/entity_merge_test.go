package membench

import (
	"testing"
)

// entityMergeLabeledFixture is the G02 labeled corpus: duplicate-entity
// pairs whose correct resolution is known a priori.
//
// Should-merge (labeled alias pairs):
//   - Alice / Alice Chen: one fact revision evidences both names
//     (shared-source identity evidence); names are prefix-containment
//     similar. The red-proof alias case.
//   - Payments / Payments Platform: the platform's extractor output
//     declared "Payments" as an alias (declared-alias identity evidence).
//
// Must-not-merge (everything else):
//   - Auth API / Billing API: two different services that share the short
//     declared alias "API" AND one source revision (/notes/call evidences
//     both). A shared short name is context, and co-occurrence in one
//     fact is context too - neither is identity evidence.
//   - Mercury (person) / Mercury (service): same display name, different
//     types; types constrain matching.
//   - inc-2026-001 / inc-2026-002: near-identical incident names, but the
//     incident type is protected and there is no identity evidence.
func entityMergeLabeledFixture() EntityMergeFixture {
	return EntityMergeFixture{
		Facts: []Record{
			{Type: "fact", Key: "/notes/n1", Body: "Alice Chen filed the incident report"},
			{Type: "fact", Key: "/notes/n2", Body: "Alice joined the oncall rota"},
			{Type: "fact", Key: "/notes/n3", Body: "Alice Chen, aka Alice, owns the pager"},
			{Type: "fact", Key: "/notes/n4", Body: "Payments Platform settles merchant invoices"},
			{Type: "fact", Key: "/notes/n5", Body: "Payments settles merchant invoices nightly"},
			{Type: "fact", Key: "/notes/n6", Body: "Auth API issues short-lived tokens"},
			{Type: "fact", Key: "/notes/n7", Body: "Billing API generates invoices"},
			{Type: "fact", Key: "/notes/n8", Body: "Mercury joined the oncall rota"},
			{Type: "fact", Key: "/notes/n9", Body: "Mercury deploys the billing API"},
			{Type: "fact", Key: "/notes/n10", Body: "inc-2026-001 disk full on host7"},
			{Type: "fact", Key: "/notes/n11", Body: "inc-2026-002 disk full on host9"},
			{Type: "fact", Key: "/notes/call", Body: "Auth API calls Billing API; these are two separate services owned by different teams."},
		},
		Entities: []EntityMergeEntity{
			{Key: "/entities/person/alice-chen", Name: "Alice Chen", Type: "person", Sources: []string{"/notes/n1", "/notes/n3"}},
			{Key: "/entities/person/alice", Name: "Alice", Type: "person", Sources: []string{"/notes/n2", "/notes/n3"}},
			{Key: "/entities/service/payments-platform", Name: "Payments Platform", Type: "service", Aliases: []string{"Payments"}, Sources: []string{"/notes/n4"}},
			{Key: "/entities/service/payments", Name: "Payments", Type: "service", Sources: []string{"/notes/n5"}},
			{Key: "/entities/service/auth-api", Name: "Auth API", Type: "service", Aliases: []string{"API"}, Sources: []string{"/notes/n6", "/notes/call"}},
			{Key: "/entities/service/billing-api", Name: "Billing API", Type: "service", Aliases: []string{"API"}, Sources: []string{"/notes/n7", "/notes/call"}},
			{Key: "/entities/person/mercury", Name: "Mercury", Type: "person", Sources: []string{"/notes/n8"}},
			{Key: "/entities/service/mercury", Name: "Mercury", Type: "service", Sources: []string{"/notes/n9"}},
			{Key: "/entities/incident/inc-2026-001", Name: "inc-2026-001", Type: "incident", Sources: []string{"/notes/n10"}},
			{Key: "/entities/incident/inc-2026-002", Name: "inc-2026-002", Type: "incident", Sources: []string{"/notes/n11"}},
		},
		AliasPairs: [][2]string{
			{"/entities/person/alice", "/entities/person/alice-chen"},
			{"/entities/service/payments", "/entities/service/payments-platform"},
		},
	}
}

// TestEntityMergeFixtureMeasuresFalseMergesAndMissedAliases is the G02
// acceptance measurement: on the labeled fixture the proposer must find
// every labeled alias pair (zero missed aliases) and propose nothing else
// (zero false merges), deterministically across fresh stores.
func TestEntityMergeFixtureMeasuresFalseMergesAndMissedAliases(t *testing.T) {
	eval, err := EvalEntityMerges(t.Context(), newBenchStore(t), "bench", entityMergeLabeledFixture())
	if err != nil {
		t.Fatal(err)
	}
	if eval.Entities != 10 {
		t.Fatalf("Entities = %d, want 10", eval.Entities)
	}
	if eval.LabeledAliases != 2 {
		t.Fatalf("LabeledAliases = %d, want 2", eval.LabeledAliases)
	}
	if eval.FalseMerges != 0 {
		t.Fatalf("FalseMerges = %d (%v), want 0: no unlabeled pair may be proposed", eval.FalseMerges, eval.FalsePairs)
	}
	if eval.MissedAliases != 0 {
		t.Fatalf("MissedAliases = %d (%v), want 0: every labeled alias pair must be proposed", eval.MissedAliases, eval.Missed)
	}
	if eval.Proposals != 2 {
		t.Fatalf("Proposals = %d, want exactly the 2 labeled pairs: %v", eval.Proposals, eval.Proposed)
	}
	// canonical direction is deterministic: the fuller display name is
	// the merge target.
	want := map[string]string{
		"/entities/person/alice":     "/entities/person/alice-chen",
		"/entities/service/payments": "/entities/service/payments-platform",
	}
	for _, p := range eval.Proposed {
		if want[p.AliasKey] != p.CanonicalKey {
			t.Fatalf("proposal = %+v, want canonical %q", p, want[p.AliasKey])
		}
		delete(want, p.AliasKey)
	}
	if len(want) != 0 {
		t.Fatalf("labeled pairs never proposed: %v", want)
	}

	// deterministic across fresh stores.
	again, err := EvalEntityMerges(t.Context(), newBenchStore(t), "bench", entityMergeLabeledFixture())
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Proposed) != len(eval.Proposed) {
		t.Fatalf("re-run proposals = %v, want %v", again.Proposed, eval.Proposed)
	}
	for i := range eval.Proposed {
		if again.Proposed[i] != eval.Proposed[i] {
			t.Fatalf("re-run proposal %d = %+v, want %+v (deterministic)", i, again.Proposed[i], eval.Proposed[i])
		}
	}
}

// TestEntityMergeCooccurrenceAloneIsNotIdentity: two separately named
// services (Auth API / Billing API) evidenced by ONE shared fact revision
// with no declared aliases - co-occurrence is contextual evidence, NOT
// identity evidence. A shared source revision may corroborate a pair that
// carries independent identity evidence, but must never suffice alone, so
// the proposer emits no merge for the co-occurring pair.
func TestEntityMergeCooccurrenceAloneIsNotIdentity(t *testing.T) {
	fixture := EntityMergeFixture{
		Facts: []Record{{Type: "fact", Key: "/notes/call", Body: "Auth API calls Billing API; these are two separate services owned by different teams."}},
		Entities: []EntityMergeEntity{
			{Key: "/entities/service/auth-api", Name: "Auth API", Type: "service", Sources: []string{"/notes/call"}},
			{Key: "/entities/service/billing-api", Name: "Billing API", Type: "service", Sources: []string{"/notes/call"}},
		},
	}
	report, err := EvalEntityMerges(t.Context(), newBenchStore(t), "bench", fixture)
	if err != nil {
		t.Fatal(err)
	}
	if report.FalseMerges != 0 {
		t.Fatalf("co-occurring distinct services proposed as aliases: false_merges=%d pairs=%+v", report.FalseMerges, report.Proposed)
	}
}
