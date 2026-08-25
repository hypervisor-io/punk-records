// Package itbench runs Punk Records' investigation loop against IBM
// ITBench-style SRE scenarios and scores the diagnosis. ITBench (an ICML
// 2025 benchmark, github.com/itbench-hub/ITBench) evaluates agents on
// real-world IT automation; its SRE track asks the agent to identify the
// faulty entity behind an incident from observability data.
//
// The live benchmark runs against IBM-hosted Kubernetes sandboxes. This
// package runs the OFFLINE, snapshot form (as shipped in the ITBench-Lite
// dataset): each scenario is a directory of frozen observability files plus
// a ground_truth.yaml. The frozen files back a set of SRE tools the agent
// investigates with - the same frozen-world technique as internal/replay.
package itbench

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Scenario is one ITBench SRE incident: an alert plus frozen observability
// snapshots, and the ground-truth faulty entity(ies) to be identified.
type Scenario struct {
	ID     string
	Domain string // "sre"
	Title  string

	// Frozen observability snapshots (raw file contents). Empty is allowed;
	// a tool simply returns "no data".
	Alerts      string // alerts/*.json, concatenated
	Metrics     string // metrics (Prometheus text exposition)
	K8sEvents   string // k8s_events_raw.tsv
	K8sObjects  string // k8s_objects_raw.tsv
	Logs        string // otel_logs_raw.tsv
	Traces      string // otel_traces_raw.tsv
	GroundTruth GroundTruth
}

// GroundTruth is the scored answer: the faulty entities and metadata. The
// upstream ground_truth.yaml schema is not fully published; these are the
// fields the SRE task is scored on (faulty entity identification).
type GroundTruth struct {
	FaultyEntities []string `yaml:"faulty_entities"`
	FaultType      string   `yaml:"fault_type"`
	Description    string   `yaml:"description"`
}

// Load reads one scenario directory in the ITBench-Lite layout. Missing
// optional files are tolerated; a missing or empty ground_truth is an error
// because it cannot be scored.
func Load(dir string) (*Scenario, error) {
	s := &Scenario{ID: filepath.Base(dir), Domain: "sre"}

	s.Alerts = readGlob(dir, "alerts", ".json")
	s.Metrics = readGlob(dir, "metrics", "")
	s.K8sEvents = readFile(filepath.Join(dir, "k8s_events_raw.tsv"))
	s.K8sObjects = readFile(filepath.Join(dir, "k8s_objects_raw.tsv"))
	s.Logs = readFile(filepath.Join(dir, "otel_logs_raw.tsv"))
	s.Traces = readFile(filepath.Join(dir, "otel_traces_raw.tsv"))

	gtRaw, err := os.ReadFile(filepath.Join(dir, "ground_truth.yaml"))
	if err != nil {
		return nil, fmt.Errorf("itbench: %s: ground_truth.yaml: %w", s.ID, err)
	}
	if err := yaml.Unmarshal(gtRaw, &s.GroundTruth); err != nil {
		return nil, fmt.Errorf("itbench: %s: parse ground_truth: %w", s.ID, err)
	}
	if len(s.GroundTruth.FaultyEntities) == 0 {
		return nil, fmt.Errorf("itbench: %s: ground_truth has no faulty_entities", s.ID)
	}
	s.Title = s.GroundTruth.Description
	if s.Title == "" {
		s.Title = s.ID
	}
	return s, nil
}

// LoadDir loads every immediate subdirectory of root that contains a
// ground_truth.yaml, sorted by directory name.
func LoadDir(root string) ([]*Scenario, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []*Scenario
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "ground_truth.yaml")); err != nil {
			continue
		}
		s, err := Load(dir)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("itbench: no scenarios (dirs with ground_truth.yaml) under %s", root)
	}
	return out, nil
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// readGlob concatenates every file under dir/sub with the given suffix
// (suffix "" matches all). Also accepts a single file named sub<suffix>.
func readGlob(dir, sub, suffix string) string {
	// single-file form: alerts.json / metrics
	for _, name := range []string{sub + suffix, sub} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			return string(b)
		}
	}
	entries, err := os.ReadDir(filepath.Join(dir, sub))
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, e := range entries {
		if e.IsDir() || (suffix != "" && !strings.HasSuffix(e.Name(), suffix)) {
			continue
		}
		if data, err := os.ReadFile(filepath.Join(dir, sub, e.Name())); err == nil {
			b.Write(data)
			b.WriteByte('\n')
		}
	}
	return b.String()
}
