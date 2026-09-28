package nativewake

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testIdentity = "http://x\nteam\nclaude:s1"

func TestStateLoadMissingIsFresh(t *testing.T) {
	dir := t.TempDir()
	st, err := loadWakeState(dir, testIdentity)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(st.Attempts) != 0 || st.Fingerprint != "" || st.LastWakeAt != 0 {
		t.Fatalf("fresh state = %+v", st)
	}
	if st.Version != wakeStateVersion || st.Identity != testIdentity {
		t.Fatalf("fresh state identity = %+v", st)
	}
}

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := &wakeState{
		Version:     wakeStateVersion,
		Identity:    testIdentity,
		Attempts:    []int64{111, 222},
		Fingerprint: "abc123",
		LastWakeAt:  222,
	}
	if err := saveWakeState(dir, st); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := loadWakeState(dir, testIdentity)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Fingerprint != "abc123" || got.LastWakeAt != 222 || len(got.Attempts) != 2 || got.Attempts[0] != 111 {
		t.Fatalf("round trip = %+v", got)
	}
	info, err := os.Stat(filepath.Join(dir, wakeStateFile))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state file perm = %o, want 600", perm)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestStateCorruptFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, wakeStateFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadWakeState(dir, testIdentity); err == nil {
		t.Fatal("corrupt state must fail closed, not silently reset the wake budget")
	}
}

func TestStateValidationFailsClosed(t *testing.T) {
	now := time.Now()
	cases := map[string]wakeState{
		"wrong version": {Version: 99, Identity: testIdentity},
		"unsorted":      {Version: wakeStateVersion, Identity: testIdentity, Attempts: []int64{200, 100}},
		"future": {Version: wakeStateVersion, Identity: testIdentity,
			Attempts: []int64{now.Add(time.Hour).UnixNano()}},
		"unbounded": {Version: wakeStateVersion, Identity: testIdentity,
			Attempts: make([]int64, maxStoredAttempts+1)},
	}
	for name, st := range cases {
		dir := t.TempDir()
		if err := saveWakeState(dir, &st); err != nil {
			t.Fatalf("%s: save: %v", name, err)
		}
		if _, err := loadWakeState(dir, testIdentity); err == nil {
			t.Fatalf("%s: invalid state accepted", name)
		}
	}
}

func TestStateIdentityMismatchResets(t *testing.T) {
	dir := t.TempDir()
	st := &wakeState{Version: wakeStateVersion, Identity: testIdentity,
		Attempts: []int64{time.Now().UnixNano()}, Fingerprint: "fp", LastWakeAt: 1}
	if err := saveWakeState(dir, st); err != nil {
		t.Fatal(err)
	}
	// Same dir, different endpoint/namespace/address: foreign quota must
	// never be inherited.
	got, err := loadWakeState(dir, "http://y\nother\nclaude:s2")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Attempts) != 0 || got.Fingerprint != "" || got.Identity != "http://y\nother\nclaude:s2" {
		t.Fatalf("foreign identity inherited budget: %+v", got)
	}
	// Matching identity keeps the budget.
	got, err = loadWakeState(dir, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Attempts) != 1 || got.Fingerprint != "fp" {
		t.Fatalf("own identity lost budget: %+v", got)
	}
}

func TestStatePruneAttempts(t *testing.T) {
	now := time.Unix(1000, 0)
	window := 100 * time.Second
	st := &wakeState{Attempts: []int64{
		now.Add(-2 * window).UnixNano(), // expired
		now.Add(-50 * time.Second).UnixNano(),
		now.UnixNano(),
	}}
	pruneAttempts(st, now, window)
	if len(st.Attempts) != 2 {
		t.Fatalf("attempts = %v", st.Attempts)
	}
}

func TestStateReserveRefundPersist(t *testing.T) {
	dir := t.TempDir()
	st, err := loadWakeState(dir, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(500, 0)
	if err := reserveAttempt(dir, st, now); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	got, err := loadWakeState(dir, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Attempts) != 1 || got.Attempts[0] != now.UnixNano() {
		t.Fatalf("reserved = %+v", got)
	}
	if err := refundAttempt(dir, st); err != nil {
		t.Fatalf("refund: %v", err)
	}
	got, err = loadWakeState(dir, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Attempts) != 0 {
		t.Fatalf("refunded = %+v", got)
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg := withDefaults(Config{URL: "http://x/", MaxWakes: -1})
	if cfg.MaxWakes != 5 || cfg.Window != 600*time.Second || cfg.Cooldown != 60*time.Second {
		t.Fatalf("defaults = %+v", cfg)
	}
	if cfg.URL != "http://x" {
		t.Fatalf("url not normalized: %q", cfg.URL)
	}
	cfg = withDefaults(Config{MaxWakes: 3, Window: time.Second, Cooldown: time.Second})
	if cfg.MaxWakes != 3 {
		t.Fatalf("explicit MaxWakes overridden: %+v", cfg)
	}
	// Explicit zero is the disabled sentinel, not "default".
	if cfg := withDefaults(Config{MaxWakes: 0}); cfg.MaxWakes != 0 {
		t.Fatalf("zero MaxWakes must stay disabled, got %d", cfg.MaxWakes)
	}
}

func TestStateNeverHoldsSecrets(t *testing.T) {
	dir := t.TempDir()
	st := &wakeState{Version: wakeStateVersion, Identity: testIdentity,
		Attempts: []int64{1}, Fingerprint: "fp", LastWakeAt: 1}
	if err := saveWakeState(dir, st); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, wakeStateFile))
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	for key := range probe {
		if strings.Contains(strings.ToLower(key), "key") || strings.Contains(strings.ToLower(key), "token") || strings.Contains(strings.ToLower(key), "body") {
			t.Fatalf("state file carries forbidden field %q", key)
		}
	}
}
