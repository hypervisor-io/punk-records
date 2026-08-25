package skillmine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type fakeDrafter struct{}

func (fakeDrafter) Draft(_ context.Context, g Group) (string, string, string, error) {
	return "Db Triage!", "triage db incidents", "1. look at logs\n2. check metrics", nil
}

func TestWriteDrafts(t *testing.T) {
	dir := t.TempDir()
	groups := []Group{
		{Agent: "db", Trajectory: []string{"get_logs"}, TaskIDs: []string{"t1"}, Summaries: []string{"s"}},
		{Agent: "db", Trajectory: []string{"get_logs"}, TaskIDs: []string{"t2"}, Summaries: []string{"s"}},
	}
	paths, err := WriteDrafts(context.Background(), groups, fakeDrafter{}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("paths: %v", paths)
	}
	// slugging + collision suffix
	if filepath.Base(filepath.Dir(paths[0])) != "db-triage" || filepath.Base(filepath.Dir(paths[1])) != "db-triage-2" {
		t.Fatalf("dirs: %v", paths)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	for _, want := range []string{"---", "name: db-triage", `description: "triage db incidents"`, "look at logs"} {
		if !strings.Contains(content, want) {
			t.Fatalf("draft missing %q:\n%s", want, content)
		}
	}
}

// errAtDrafter fails on the Nth call (1-indexed), succeeding with a
// distinct name derived from the group's first task id on every other
// call so that successful drafts before the failure are distinguishable.
type errAtDrafter struct {
	failAt int
	calls  int
}

func (d *errAtDrafter) Draft(_ context.Context, g Group) (string, string, string, error) {
	d.calls++
	if d.calls == d.failAt {
		return "", "", "", errors.New("boom")
	}
	name := "ok-" + g.TaskIDs[0]
	return name, "desc " + name, "body " + name, nil
}

func TestWriteDraftsErrorPropagatesWithPartialPaths(t *testing.T) {
	dir := t.TempDir()
	groups := []Group{
		{Agent: "db", Trajectory: []string{"a"}, TaskIDs: []string{"t1"}, Summaries: []string{"s"}},
		{Agent: "db", Trajectory: []string{"b"}, TaskIDs: []string{"t2"}, Summaries: []string{"s"}},
		{Agent: "db", Trajectory: []string{"c"}, TaskIDs: []string{"t3"}, Summaries: []string{"s"}},
	}
	d := &errAtDrafter{failAt: 2}
	paths, err := WriteDrafts(context.Background(), groups, d, dir)
	if err == nil {
		t.Fatal("want error from second draft call")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error should wrap underlying cause: %v", err)
	}
	// Only the first (successful) group's draft was written before the
	// failure; the third group's Draft must never have been attempted.
	if len(paths) != 1 {
		t.Fatalf("paths: %v, want 1 (only the pre-failure success)", paths)
	}
	if _, statErr := os.Stat(paths[0]); statErr != nil {
		t.Fatalf("returned path does not exist: %v", statErr)
	}
	if d.calls != 2 {
		t.Fatalf("calls = %d, want 2 (stop at first failure, never reach group 3)", d.calls)
	}
}

// emptyNameDrafter always returns an empty name, forcing the slug
// fallback path.
type emptyNameDrafter struct{}

func (emptyNameDrafter) Draft(_ context.Context, g Group) (string, string, string, error) {
	return "", "empty name case", "body text", nil
}

func TestWriteDraftsEmptyNameFallsBackToSkill(t *testing.T) {
	dir := t.TempDir()
	groups := []Group{
		{Agent: "db", Trajectory: []string{"a"}, TaskIDs: []string{"t1"}, Summaries: []string{"s"}},
	}
	paths, err := WriteDrafts(context.Background(), groups, emptyNameDrafter{}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("paths: %v", paths)
	}
	if filepath.Base(filepath.Dir(paths[0])) != "skill" {
		t.Fatalf("dir: %v, want fallback slug \"skill\"", paths[0])
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "name: skill") {
		t.Fatalf("draft missing fallback name:\n%s", raw)
	}
}

// fixedDrafter always returns the given name/description/body, for tests
// that need to control the exact description text fed through
// yamlDescription.
type fixedDrafter struct {
	name, desc, body string
}

func (d fixedDrafter) Draft(_ context.Context, g Group) (string, string, string, error) {
	return d.name, d.desc, d.body, nil
}

// extractFrontmatter pulls the YAML body between the two "---" fences a
// draft's SKILL.md opens with, so it can be parsed as a standalone YAML
// document.
func extractFrontmatter(t *testing.T, content string) string {
	t.Helper()
	parts := strings.SplitN(content, "---\n", 3)
	if len(parts) < 3 {
		t.Fatalf("content missing frontmatter delimiters:\n%s", content)
	}
	return parts[1]
}

// TestYamlDescriptionAdversarial covers the round 1 review's adversarial
// descriptions: characters that are YAML indicators (sequence/mapping/
// anchor/alias/tag/directive/flow markers) which either break parsing
// entirely or silently change the parsed value when left unquoted. Every
// case is rendered through the real WriteDrafts path and verified two
// ways: the emitted line must be exactly `description: ` +
// strconv.Quote(collapsed), and gopkg.in/yaml.v3 (already a direct
// dependency, see internal/itbench/scenario.go) must parse the
// frontmatter back to the same collapsed description.
func TestYamlDescriptionAdversarial(t *testing.T) {
	cases := []struct {
		name string
		desc string
	}{
		{"bracket-flow-prefix", "[draft] a proposal for review"},
		{"seq-item-prefix", "- seq item description"},
		{"folded-scalar-prefix", "> folded description"},
		{"literal-scalar-prefix", "| literal description"},
		{"explicit-key-prefix", "? key description"},
		{"comma-prefix", ", comma leading description"},
		{"alias-prefix", "*anchor reference description"},
		{"directive-prefix", "%YAML directive-looking description"},
		{"reserved-at-prefix", "@ mention style description"},
		{"reserved-backtick-prefix", "`code` styled description"},
		{"embedded-nul", "before\x00after control char"},
		{"anchor-prefix-value-change", "&anchor labeled description"},
		{"tag-prefix-value-change", "!!str typed description"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			groups := []Group{{Agent: "a", Trajectory: []string{"x"}, TaskIDs: []string{"t1"}, Summaries: []string{"s"}}}
			d := fixedDrafter{name: "skill-" + tc.name, desc: tc.desc, body: "body"}
			paths, err := WriteDrafts(context.Background(), groups, d, dir)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(paths[0])
			if err != nil {
				t.Fatal(err)
			}
			content := string(raw)

			collapsed := strings.Join(strings.Fields(tc.desc), " ")

			// The emitted description line must be exactly the quoted form.
			wantLine := "description: " + strconv.Quote(collapsed)
			if !strings.Contains(content, wantLine) {
				t.Fatalf("frontmatter missing exact quoted line %q:\n%s", wantLine, content)
			}

			// strconv.Quote's output must round-trip via strconv.Unquote.
			got, err := strconv.Unquote(strconv.Quote(collapsed))
			if err != nil || got != collapsed {
				t.Fatalf("strconv round-trip failed: got=%q err=%v", got, err)
			}

			// End-to-end proof: a real YAML parser must recover the same
			// description from the rendered frontmatter.
			fm := extractFrontmatter(t, content)
			var doc struct {
				Name        string `yaml:"name"`
				Description string `yaml:"description"`
			}
			if err := yaml.Unmarshal([]byte(fm), &doc); err != nil {
				t.Fatalf("yaml.v3 failed to parse frontmatter: %v\nfrontmatter:\n%s", err, fm)
			}
			if doc.Description != collapsed {
				t.Fatalf("yaml.v3 parsed description = %q, want %q (value changed under parsing)", doc.Description, collapsed)
			}
		})
	}
}

func TestWriteDraftsProvenance(t *testing.T) {
	dir := t.TempDir()
	groups := []Group{
		{Agent: "net", Trajectory: []string{"ping", "traceroute"},
			TaskIDs:   []string{"t1", "t2", "t3"},
			Summaries: []string{"s1", "s2", "s3"}},
	}
	paths, err := WriteDrafts(context.Background(), groups, fakeDrafter{}, dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	for _, want := range []string{"## Provenance", "3", "net", "ping, traceroute"} {
		if !strings.Contains(content, want) {
			t.Fatalf("provenance missing %q:\n%s", want, content)
		}
	}
}
