// Package registry holds the active spec snapshot: loaded at boot,
// swapped atomically on hot-reload, versions persisted to agent_versions
// so tasks pin the exact spec they ran under.
package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/hypervisor-io/punk-records/internal/spec"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// Snapshot is one immutable, validated spec tree. In-flight tasks keep
// the snapshot (and version ids) they started with; reload never mutates.
type Snapshot struct {
	Bundle          *spec.Bundle
	Version         int64
	AgentVersionIDs map[string]int64 // agent name -> agent_versions.id
	LoadedAt        time.Time
}

type Registry struct {
	Dir string
	// Debounce collapses editor save bursts into one reload. Tests lower it.
	Debounce time.Duration

	db  *store.DB // nil disables version persistence (validate, some tests)
	log *slog.Logger
	cur atomic.Pointer[Snapshot]
	seq atomic.Int64
	now func() time.Time
}

func New(dir string, db *store.DB, log *slog.Logger) *Registry {
	return &Registry{Dir: dir, Debounce: 2 * time.Second, db: db, log: log, now: time.Now}
}

// Current returns the active snapshot (nil before the first Load).
func (r *Registry) Current() *Snapshot { return r.cur.Load() }

// Get returns an agent spec plus its persisted version id.
func (r *Registry) Get(name string) (*spec.AgentSpec, int64, bool) {
	s := r.Current()
	if s == nil {
		return nil, 0, false
	}
	a, ok := s.Bundle.Agents[name]
	if !ok {
		return nil, 0, false
	}
	return a, s.AgentVersionIDs[name], true
}

// Load parses and validates the tree. On any validation error the old
// snapshot stays active and the error is returned (validate-before-apply).
func (r *Registry) Load(ctx context.Context) error {
	bundle, errs := spec.LoadDir(r.Dir)
	if len(errs) > 0 {
		return fmt.Errorf("spec validation: %w", errors.Join(errs...))
	}
	ids := map[string]int64{}
	if r.db != nil {
		for name, a := range bundle.Agents {
			id, err := r.persistAgentVersion(ctx, a)
			if err != nil {
				return fmt.Errorf("persist agent version %s: %w", name, err)
			}
			ids[name] = id
		}
	}
	snap := &Snapshot{
		Bundle:          bundle,
		Version:         r.seq.Add(1),
		AgentVersionIDs: ids,
		LoadedAt:        r.now().UTC(),
	}
	r.cur.Store(snap)
	for _, a := range bundle.Agents {
		if a.Autonomy == "auto" {
			r.log.Warn("agent declares auto autonomy without an earned-autonomy scorecard gate; keep allowlists tight",
				"agent", a.Name)
		}
	}
	r.log.Info("spec snapshot active",
		"version", snap.Version,
		"agents", len(bundle.Agents),
		"skills", len(bundle.Skills),
		"policies", len(bundle.Policies))
	return nil
}

// hashAgent fingerprints the spec content; identical content reuses the
// persisted version row.
func hashAgent(a *spec.AgentSpec) string {
	raw, _ := json.Marshal(a)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (r *Registry) persistAgentVersion(ctx context.Context, a *spec.AgentSpec) (int64, error) {
	hash := hashAgent(a)
	var id int64
	err := r.db.QueryRowContext(ctx, r.db.Rebind(`
		SELECT id FROM agent_versions WHERE name = $1 AND content_hash = $2
		ORDER BY loaded_at DESC LIMIT 1`), a.Name, hash).Scan(&id)
	if err == nil {
		return id, nil
	}
	err = r.db.QueryRowContext(ctx, r.db.Rebind(`
		INSERT INTO agent_versions (name, version, content_hash, source_path, loaded_at, active)
		VALUES ($1, $2, $3, $4, $5, TRUE) RETURNING id`),
		a.Name, a.Version, hash, a.SourcePath, store.TimeToDB(r.now())).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// watchDirs lists every directory under the spec root that must be
// watched (fsnotify is not recursive; K8s ConfigMaps swap symlinked
// directories, so we watch directories, never single files).
func (r *Registry) watchDirs() []string {
	dirs := []string{r.Dir}
	for _, sub := range []string{"agents", "skills", "policies"} {
		p := filepath.Join(r.Dir, sub)
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			dirs = append(dirs, p)
		}
	}
	if entries, err := os.ReadDir(filepath.Join(r.Dir, "skills")); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, filepath.Join(r.Dir, "skills", e.Name()))
			}
		}
	}
	return dirs
}

// Watch hot-reloads on filesystem changes (debounced) and on demand via
// the reload channel (SIGHUP is wired in cmd). Bad edits keep the old
// snapshot active; the error is logged, never fatal. Blocks until ctx
// is done.
func (r *Registry) Watch(ctx context.Context, reload <-chan struct{}) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer func() { _ = w.Close() }()
	addAll := func() {
		for _, d := range r.watchDirs() {
			_ = w.Add(d) // re-adding an existing watch is a no-op error we ignore
		}
	}
	addAll()

	var timer *time.Timer
	fire := make(chan struct{}, 1)
	bump := func() {
		if timer == nil {
			timer = time.AfterFunc(r.Debounce, func() {
				select {
				case fire <- struct{}{}:
				default:
				}
			})
			return
		}
		timer.Reset(r.Debounce)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-w.Events:
			if !ok {
				return errors.New("watcher closed")
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) != 0 {
				bump()
			}
		case err, ok := <-w.Errors:
			if !ok {
				return errors.New("watcher closed")
			}
			r.log.Warn("spec watcher error", "err", err)
		case <-fire:
			if err := r.Load(ctx); err != nil {
				r.log.Error("spec reload rejected, keeping previous snapshot", "err", err)
			}
			addAll() // new skill folders need their own watch
		case <-reload:
			if err := r.Load(ctx); err != nil {
				r.log.Error("spec reload (signal) rejected, keeping previous snapshot", "err", err)
			}
			addAll()
		}
	}
}
