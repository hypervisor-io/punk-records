// Package spec parses and validates the declarative surface: agent specs
// (markdown + YAML frontmatter), skills (agentskills.io SKILL.md folders),
// and policies (YAML). No agent exists only in code.
package spec

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/goccy/go-yaml"
)

// Autonomy ladder, lowest to highest. An agent's autonomy caps which
// action classes its tool calls may hit (policy engine, P6).
const (
	AutonomyObserve = "observe" // read-only tools
	AutonomyAdvise  = "advise"  // findings only
	AutonomyPropose = "propose" // may create proposals for approval
	AutonomyAuto    = "auto"    // may execute allowlisted actions
)

var autonomyLevels = map[string]int{
	AutonomyObserve: 0, AutonomyAdvise: 1, AutonomyPropose: 2, AutonomyAuto: 3,
}

// AutonomyLevel returns the numeric rank of an autonomy mode (-1 unknown).
func AutonomyLevel(a string) int {
	if l, ok := autonomyLevels[a]; ok {
		return l
	}
	return -1
}

// Handoff contracts: what context transfers on the parent<->child edge.
const (
	HandoffResultsOnly = "results_only"
	HandoffSummary     = "summary"
	HandoffFullTrace   = "full_trace"
)

var nameRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// AgentSpec is one agents/*.md file.
type AgentSpec struct {
	Name            string           `yaml:"name"`
	Version         string           `yaml:"version"`
	Description     string           `yaml:"description"`
	ModelProfile    string           `yaml:"model_profile"`
	Triggers        []Trigger        `yaml:"triggers"`
	Tools           []string         `yaml:"tools"`
	Skills          []string         `yaml:"skills"`
	Regions         []string         `yaml:"regions"` // brain regions this satellite registers to (B1)
	Autonomy        string           `yaml:"autonomy"`
	Budgets         *BudgetSpec      `yaml:"budgets"`
	HandoffContract string           `yaml:"handoff_contract"`
	Disabled        bool             `yaml:"disabled"` // cost kill switch, hot-reloadable (P14.4)
	Disposition     *DispositionSpec `yaml:"disposition"`

	Prompt     string `yaml:"-"` // markdown body
	SourcePath string `yaml:"-"`
}

// DispositionSpec tunes reflect-side reasoning tone: each
// trait is 1..5, 0 = unset (omitted from the prompt).
type DispositionSpec struct {
	Skepticism int `yaml:"skepticism"`
	Literalism int `yaml:"literalism"`
	Empathy    int `yaml:"empathy"`
}

// Trigger is one deterministic routing rule. All set fields must match.
type Trigger struct {
	ID       string            `yaml:"id"`
	Source   string            `yaml:"source"`   // exact, or glob suffix "prefix*"
	Kind     string            `yaml:"kind"`     // task kind, exact
	Labels   map[string]string `yaml:"labels"`   // every entry must match
	Severity []string          `yaml:"severity"` // label "severity" in-list
	Priority int               `yaml:"priority"` // higher wins on ties
}

// BudgetSpec lowers (never raises) the configured defaults per agent.
type BudgetSpec struct {
	Tokens    int     `yaml:"tokens"`
	ToolCalls int     `yaml:"tool_calls"`
	WallMS    int64   `yaml:"wall_ms"`
	Subagents int     `yaml:"subagents"`
	DailyUSD  float64 `yaml:"daily_usd"` // model spend cap per day (P14.2)
}

// SkillSpec is one skills/<name>/SKILL.md folder, agentskills.io-conformant.
// Punk Records extensions ride in metadata: version, action_class,
// approval_required.
type SkillSpec struct {
	Name          string            `yaml:"name"`
	Description   string            `yaml:"description"`
	License       string            `yaml:"license"`
	Compatibility string            `yaml:"compatibility"`
	Metadata      map[string]string `yaml:"metadata"`
	AllowedTools  string            `yaml:"allowed-tools"` // space-separated per spec

	Body       string `yaml:"-"`
	Dir        string `yaml:"-"`
	SourcePath string `yaml:"-"`
}

// ActionClass groups tool patterns under a minimum autonomy requirement.
type ActionClass struct {
	Name         string   `yaml:"name"`
	MinAutonomy  string   `yaml:"min_autonomy"`
	ToolPatterns []string `yaml:"tool_patterns"`
}

// MemoryACLRule scopes memory-plane writes by key prefix (P12.4).
// Longest matching prefix wins; unmatched keys are writable by anyone.
type MemoryACLRule struct {
	Prefix   string   `yaml:"prefix"`
	Writers  []string `yaml:"writers"`   // agent names; ["*"] allows all
	ReadOnly bool     `yaml:"read_only"` // reference data: nobody writes
}

// PolicySpec is one policies/*.yaml file.
type PolicySpec struct {
	Name          string          `yaml:"name"`
	ActionClasses []ActionClass   `yaml:"action_classes"`
	MemoryACL     []MemoryACLRule `yaml:"memory_acl"`

	SourcePath string `yaml:"-"`
}

// splitFrontmatter returns (yaml, body) for a ----delimited markdown file.
func splitFrontmatter(raw []byte) (string, string, error) {
	const delim = "---"
	s := string(bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n")))
	if !strings.HasPrefix(s, delim+"\n") {
		return "", "", fmt.Errorf("missing frontmatter: file must start with ---")
	}
	rest := s[len(delim)+1:]
	end := strings.Index(rest, "\n"+delim)
	if end < 0 {
		return "", "", fmt.Errorf("unterminated frontmatter: closing --- not found")
	}
	body := rest[end+1+len(delim):]
	body = strings.TrimPrefix(body, "\n")
	return rest[:end], strings.TrimSpace(body), nil
}

// ParseAgent parses and validates one agent spec file.
func ParseAgent(path string, raw []byte) (*AgentSpec, error) {
	fm, body, err := splitFrontmatter(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var a AgentSpec
	if err := yaml.Unmarshal([]byte(fm), &a); err != nil {
		return nil, fmt.Errorf("%s: frontmatter: %w", path, err)
	}
	a.Prompt = body
	a.SourcePath = path

	var errs []string
	if a.Name == "" {
		errs = append(errs, "name is required")
	} else if !nameRE.MatchString(a.Name) {
		errs = append(errs, fmt.Sprintf("name %q: lowercase alphanumerics and hyphens only", a.Name))
	}
	if a.Version == "" {
		errs = append(errs, "version is required")
	}
	if a.Description == "" {
		errs = append(errs, "description is required")
	}
	if len(a.Triggers) == 0 {
		errs = append(errs, "at least one trigger is required")
	}
	for i, tr := range a.Triggers {
		if tr.Source == "" && tr.Kind == "" && len(tr.Labels) == 0 && len(tr.Severity) == 0 {
			errs = append(errs, fmt.Sprintf("triggers[%d]: empty rule would match everything; set source, kind, labels, or severity", i))
		}
	}
	if a.Autonomy == "" {
		a.Autonomy = AutonomyAdvise
	}
	if AutonomyLevel(a.Autonomy) < 0 {
		errs = append(errs, fmt.Sprintf("autonomy %q: want observe|advise|propose|auto", a.Autonomy))
	}
	if d := a.Disposition; d != nil {
		if d.Skepticism < 0 || d.Skepticism > 5 {
			errs = append(errs, fmt.Sprintf("disposition.skepticism %d: want 1-5", d.Skepticism))
		}
		if d.Literalism < 0 || d.Literalism > 5 {
			errs = append(errs, fmt.Sprintf("disposition.literalism %d: want 1-5", d.Literalism))
		}
		if d.Empathy < 0 || d.Empathy > 5 {
			errs = append(errs, fmt.Sprintf("disposition.empathy %d: want 1-5", d.Empathy))
		}
	}
	switch a.HandoffContract {
	case "":
		a.HandoffContract = HandoffResultsOnly
	case HandoffResultsOnly, HandoffSummary:
	case HandoffFullTrace:
		errs = append(errs, "handoff_contract full_trace is not implemented yet; use results_only or summary")
	default:
		errs = append(errs, fmt.Sprintf("handoff_contract %q: want results_only|summary|full_trace", a.HandoffContract))
	}
	if b := a.Budgets; b != nil {
		if b.Tokens < 0 || b.ToolCalls < 0 || b.WallMS < 0 || b.Subagents < 0 || b.DailyUSD < 0 {
			errs = append(errs, "budgets: negative values")
		}
	}
	if a.Prompt == "" {
		errs = append(errs, "markdown body (system prompt) is empty")
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s: %s", path, strings.Join(errs, "; "))
	}
	return &a, nil
}

// ParseSkill parses and validates one SKILL.md. dirName is the containing
// folder name, which the spec requires to equal the skill name.
func ParseSkill(path, dirName string, raw []byte) (*SkillSpec, error) {
	fm, body, err := splitFrontmatter(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var sk SkillSpec
	if err := yaml.Unmarshal([]byte(fm), &sk); err != nil {
		return nil, fmt.Errorf("%s: frontmatter: %w", path, err)
	}
	sk.Body = body
	sk.Dir = dirName
	sk.SourcePath = path

	var errs []string
	if sk.Name == "" {
		errs = append(errs, "name is required")
	} else {
		if len(sk.Name) > 64 || !nameRE.MatchString(sk.Name) {
			errs = append(errs, fmt.Sprintf("name %q: 1-64 chars, lowercase alphanumerics and hyphens", sk.Name))
		}
		if sk.Name != dirName {
			errs = append(errs, fmt.Sprintf("name %q must match directory %q", sk.Name, dirName))
		}
	}
	if sk.Description == "" {
		errs = append(errs, "description is required")
	} else if len(sk.Description) > 1024 {
		errs = append(errs, "description exceeds 1024 chars")
	}
	if ac := sk.Metadata["action_class"]; ac != "" && !nameRE.MatchString(ac) {
		errs = append(errs, fmt.Sprintf("metadata.action_class %q: lowercase alphanumerics and hyphens", ac))
	}
	if ar := sk.Metadata["approval_required"]; ar != "" && ar != "true" && ar != "false" {
		errs = append(errs, fmt.Sprintf("metadata.approval_required %q: want true or false", ar))
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s: %s", path, strings.Join(errs, "; "))
	}
	return &sk, nil
}

// ParsePolicy parses and validates one policies/*.yaml file.
func ParsePolicy(path string, raw []byte) (*PolicySpec, error) {
	var p PolicySpec
	if err := yaml.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	p.SourcePath = path
	var errs []string
	if p.Name == "" {
		errs = append(errs, "name is required")
	}
	if len(p.ActionClasses) == 0 {
		errs = append(errs, "at least one action class is required")
	}
	seen := map[string]bool{}
	for i, ac := range p.ActionClasses {
		if ac.Name == "" || !nameRE.MatchString(ac.Name) {
			errs = append(errs, fmt.Sprintf("action_classes[%d]: bad name %q", i, ac.Name))
		}
		if seen[ac.Name] {
			errs = append(errs, fmt.Sprintf("action_classes[%d]: duplicate name %q", i, ac.Name))
		}
		seen[ac.Name] = true
		if AutonomyLevel(ac.MinAutonomy) < 0 {
			errs = append(errs, fmt.Sprintf("action_classes[%d]: min_autonomy %q invalid", i, ac.MinAutonomy))
		}
		if len(ac.ToolPatterns) == 0 {
			errs = append(errs, fmt.Sprintf("action_classes[%d]: tool_patterns required", i))
		}
	}
	for i, r := range p.MemoryACL {
		if !strings.HasPrefix(r.Prefix, "/") {
			errs = append(errs, fmt.Sprintf("memory_acl[%d]: prefix %q must start with /", i, r.Prefix))
		}
		if !r.ReadOnly && len(r.Writers) == 0 {
			errs = append(errs, fmt.Sprintf("memory_acl[%d]: writers required unless read_only", i))
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s: %s", path, strings.Join(errs, "; "))
	}
	return &p, nil
}
