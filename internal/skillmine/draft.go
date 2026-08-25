package skillmine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Drafter turns a mined group into SKILL.md prose. The LLM adapter lives in
// cmd; tests use fakes.
type Drafter interface {
	Draft(ctx context.Context, g Group) (name, description, body string, err error)
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	return strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// yamlDescription collapses a (possibly LLM-authored, possibly multi-line)
// description into a single YAML scalar line. Newlines/tabs become spaces
// and repeated whitespace collapses to one space, since a raw embedded
// newline would either break the "key: value" line or silently start a
// new YAML mapping key. The result is then always quoted with
// strconv.Quote: YAML plain scalars treat a long list of leading
// characters as indicators (-, ?, :, ,, [, ], {, }, #, &, *, !, |, >, ',
// ", %, @, `), so any conditional allowlist misses some of them, and a
// review proved several of the missed cases either fail to parse or
// silently parse to a different value (a leading "&" becomes an anchor
// definition, a leading "!!str" a type tag). Always quoting removes the
// character-by-character judgment call entirely. strconv.Quote's escaping
// (backslashes, embedded quotes, control chars) happens to line up with
// YAML's double-quoted scalar escaping for this ASCII-only input, so it
// doubles as a YAML quoter here rather than requiring a second escaper.
func yamlDescription(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strconv.Quote(s)
}

// WriteDrafts renders one agentskills.io-shaped SKILL.md per group into
// outDir/<slug>/SKILL.md. Drafts are proposals for a human to review and
// move into specs/skills/ - this function never touches specs/. Name
// collisions get -2, -3 suffixes. Returns the paths written so far even
// when a later draft call fails, so callers can report partial progress.
// Re-running with the same slugs overwrites earlier drafts.
func WriteDrafts(ctx context.Context, groups []Group, d Drafter, outDir string) ([]string, error) {
	var paths []string
	used := map[string]bool{}
	for _, g := range groups {
		name, desc, body, err := d.Draft(ctx, g)
		if err != nil {
			return paths, fmt.Errorf("draft for agent %s: %w", g.Agent, err)
		}
		slug := slugify(name)
		if slug == "" {
			slug = "skill"
		}
		base := slug
		for i := 2; used[slug]; i++ {
			slug = fmt.Sprintf("%s-%d", base, i)
		}
		used[slug] = true
		dir := filepath.Join(outDir, slug)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return paths, fmt.Errorf("write draft %s: %w", slug, err)
		}
		content := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n%s\n\n## Provenance\n\nMined from %d completed %s tasks with trajectory: %s\n",
			slug, yamlDescription(desc), body, len(g.TaskIDs), g.Agent, strings.Join(g.Trajectory, ", "))
		path := filepath.Join(dir, "SKILL.md")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return paths, fmt.Errorf("write draft %s: %w", slug, err)
		}
		paths = append(paths, path)
	}
	return paths, nil
}
