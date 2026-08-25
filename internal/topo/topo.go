// Package topo is the minimal service topology: service -> owner ->
// on-call, plus dependency edges (declared from catalogs, observed from
// telemetry later). Routing walks it to rank probable-cause owners.
// Deliberately not a CMDB.
package topo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// Service is the 5-element model (GAPS2 C2#7).
type Service struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Owner     string `json:"owner"`
	OncallRef string `json:"oncall_ref"`
	Tier      int    `json:"tier"`
}

// Upsert writes one service row.
func Upsert(ctx context.Context, db *store.DB, s Service) error {
	if s.Namespace == "" {
		s.Namespace = "default"
	}
	_, err := db.ExecContext(ctx, db.Rebind(`
		INSERT INTO services (name, namespace, owner, oncall_ref, tier, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (name, namespace) DO UPDATE SET
		  owner = excluded.owner, oncall_ref = excluded.oncall_ref,
		  tier = excluded.tier, updated_at = excluded.updated_at`),
		s.Name, s.Namespace, s.Owner, s.OncallRef, s.Tier, store.TimeToDB(time.Now()))
	return err
}

// AddEdge records from -> to (from depends on to).
func AddEdge(ctx context.Context, db *store.DB, from, to, kind string) error {
	if kind != "declared" && kind != "observed" {
		return fmt.Errorf("topo: edge kind %q", kind)
	}
	_, err := db.ExecContext(ctx, db.Rebind(`
		INSERT INTO service_edges (from_service, to_service, kind, updated_at)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (from_service, to_service, kind) DO UPDATE SET updated_at = excluded.updated_at`),
		from, to, kind, store.TimeToDB(time.Now()))
	return err
}

// Lookup fetches one service.
func Lookup(ctx context.Context, db *store.DB, name string) (*Service, bool, error) {
	var s Service
	err := db.QueryRowContext(ctx, db.Rebind(`
		SELECT name, namespace, owner, oncall_ref, tier FROM services WHERE name = $1
		ORDER BY namespace LIMIT 1`), name).
		Scan(&s.Name, &s.Namespace, &s.Owner, &s.OncallRef, &s.Tier)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &s, true, nil
}

// Upstream walks dependencies breadth-first from a service: the things
// it depends on, ranked by distance - the probable-cause order.
func Upstream(ctx context.Context, db *store.DB, name string, maxDepth int) ([]string, error) {
	if maxDepth <= 0 {
		maxDepth = 3
	}
	seen := map[string]bool{name: true}
	frontier := []string{name}
	out := []string{}
	for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
		next := []string{}
		for _, svc := range frontier {
			rows, err := db.QueryContext(ctx, db.Rebind(`
				SELECT DISTINCT to_service FROM service_edges WHERE from_service = $1`), svc)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var to string
				if err := rows.Scan(&to); err != nil {
					_ = rows.Close()
					return nil, err
				}
				if !seen[to] {
					seen[to] = true
					out = append(out, to)
					next = append(next, to)
				}
			}
			_ = rows.Close()
			if err := rows.Err(); err != nil {
				return nil, err
			}
		}
		frontier = next
	}
	return out, nil
}

// backstageDoc is the subset of Backstage's catalog format we import.
type backstageDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name        string            `yaml:"name"`
		Namespace   string            `yaml:"namespace"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Spec struct {
		Owner     string   `yaml:"owner"`
		DependsOn []string `yaml:"dependsOn"`
		Tier      int      `yaml:"tier"`
	} `yaml:"spec"`
}

// ImportBackstage reads a multi-document Backstage catalog YAML and
// upserts Components + declared edges. Returns services and edges written.
func ImportBackstage(ctx context.Context, db *store.DB, raw []byte) (int, int, error) {
	docs := strings.Split(string(raw), "\n---")
	services, edges := 0, 0
	for _, doc := range docs {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}
		var b backstageDoc
		if err := yaml.Unmarshal([]byte(doc), &b); err != nil {
			return services, edges, fmt.Errorf("catalog parse: %w", err)
		}
		if !strings.EqualFold(b.Kind, "Component") || b.Metadata.Name == "" {
			continue
		}
		tier := b.Spec.Tier
		if tier == 0 {
			tier = 3
		}
		svc := Service{
			Name: b.Metadata.Name, Namespace: b.Metadata.Namespace,
			Owner: b.Spec.Owner, Tier: tier,
			OncallRef: b.Metadata.Annotations["punk.io/oncall"],
		}
		if err := Upsert(ctx, db, svc); err != nil {
			return services, edges, err
		}
		services++
		for _, dep := range b.Spec.DependsOn {
			target := dep
			if i := strings.LastIndex(dep, ":"); i >= 0 {
				target = dep[i+1:]
			}
			if err := AddEdge(ctx, db, b.Metadata.Name, target, "declared"); err != nil {
				return services, edges, err
			}
			edges++
		}
	}
	return services, edges, nil
}

// Enrich decorates routing labels with topology: owner, tier, and the
// ranked upstream chain for the labeled service (P17.2). Unknown
// services change nothing - topology is an amplifier, not a gate.
func Enrich(ctx context.Context, db *store.DB, labels map[string]string) (map[string]string, []string, error) {
	name := labels["service"]
	if name == "" {
		return labels, nil, nil
	}
	svc, ok, err := Lookup(ctx, db, name)
	if err != nil || !ok {
		return labels, nil, err
	}
	out := map[string]string{}
	for k, v := range labels {
		out[k] = v
	}
	if _, has := out["owner"]; !has && svc.Owner != "" {
		out["owner"] = svc.Owner
	}
	if _, has := out["tier"]; !has {
		out["tier"] = fmt.Sprintf("%d", svc.Tier)
	}
	up, err := Upstream(ctx, db, name, 3)
	if err != nil {
		return out, nil, err
	}
	return out, up, nil
}
