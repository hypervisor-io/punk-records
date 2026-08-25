package topo

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/store"
)

const catalog = `apiVersion: backstage.io/v1alpha1
kind: Component
metadata:
  name: api-svc
  annotations:
    punk.io/oncall: pagerduty:schedule:web
spec:
  owner: team-web
  tier: 1
  dependsOn:
    - component:payments-db
    - component:auth-svc
---
apiVersion: backstage.io/v1alpha1
kind: Component
metadata:
  name: auth-svc
spec:
  owner: team-identity
  dependsOn:
    - component:payments-db
---
apiVersion: backstage.io/v1alpha1
kind: Component
metadata:
  name: payments-db
spec:
  owner: team-data
  tier: 0
---
kind: API
metadata:
  name: not-a-component
`

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "topo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestImportLookupUpstreamEnrich(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	svcs, edges, err := ImportBackstage(ctx, db, []byte(catalog))
	if err != nil || svcs != 3 || edges != 3 {
		t.Fatalf("import = %d svcs %d edges err=%v", svcs, edges, err)
	}

	svc, ok, err := Lookup(ctx, db, "api-svc")
	if err != nil || !ok || svc.Owner != "team-web" || svc.Tier != 1 || svc.OncallRef != "pagerduty:schedule:web" {
		t.Fatalf("lookup = %+v ok=%v err=%v", svc, ok, err)
	}

	up, err := Upstream(ctx, db, "api-svc", 3)
	if err != nil {
		t.Fatal(err)
	}
	// direct deps first (payments-db, auth-svc), no duplicates
	if len(up) != 2 {
		t.Fatalf("upstream = %v", up)
	}

	labels, chain, err := Enrich(ctx, db, map[string]string{"service": "api-svc", "severity": "critical"})
	if err != nil {
		t.Fatal(err)
	}
	if labels["owner"] != "team-web" || labels["tier"] != "1" || labels["severity"] != "critical" {
		t.Fatalf("enriched = %v", labels)
	}
	if len(chain) != 2 {
		t.Fatalf("chain = %v", chain)
	}

	// unknown service: passthrough
	same, chain2, err := Enrich(ctx, db, map[string]string{"service": "ghost"})
	if err != nil || len(chain2) != 0 || same["owner"] != "" {
		t.Fatalf("ghost enrich = %v %v %v", same, chain2, err)
	}
}
