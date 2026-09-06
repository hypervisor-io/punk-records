package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/config"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// TestNewKeysWiresLogger pins that newKeys always wires the server
// logger onto api.Keys.Log (task A02 review, item 1d) - visibility for
// KeyActive's fail-closed deny path - regardless of whether namespace
// authorization is enforced.
func TestNewKeysWiresLogger(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "keys-wiring-off.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	log := slog.Default()

	cfg := &config.Config{Authz: config.Authz{Enforcement: "off"}}
	keys := newKeys(cfg, db, log)
	if keys.Log != log {
		t.Fatal("newKeys must wire the server logger onto Keys.Log even with enforcement off")
	}
}

// TestNewKeysWiresLoggerEnforced pins the same wiring with enforcement
// on, where newKeys also constructs the namespace authorizer.
func TestNewKeysWiresLoggerEnforced(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "keys-wiring-deny.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	log := slog.Default()

	cfg := &config.Config{Authz: config.Authz{Enforcement: "deny"}}
	keys := newKeys(cfg, db, log)
	if keys.Log != log {
		t.Fatal("newKeys must wire the server logger onto Keys.Log with enforcement on")
	}
}
