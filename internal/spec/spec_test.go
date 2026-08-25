package spec

import (
	"path/filepath"
	"strings"
	"testing"
)

const goodAgent = `---
name: database
version: 0.1.0
description: db specialist
triggers:
  - id: t1
    source: acme
    labels:
      domain: database
autonomy: propose
handoff_contract: results_only
skills: []
---

You are the database agent.
`

func TestParseAgentValid(t *testing.T) {
	a, err := ParseAgent("agents/database.md", []byte(goodAgent))
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "database" || a.Autonomy != "propose" || len(a.Triggers) != 1 {
		t.Fatalf("parsed = %+v", a)
	}
	if !strings.Contains(a.Prompt, "database agent") {
		t.Errorf("prompt body lost: %q", a.Prompt)
	}
}

func TestParseAgentDefaults(t *testing.T) {
	minimal := `---
name: net
version: 0.1.0
description: network agent
triggers:
  - source: acme
---
prompt body
`
	a, err := ParseAgent("x.md", []byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if a.Autonomy != AutonomyAdvise {
		t.Errorf("default autonomy = %q, want advise", a.Autonomy)
	}
	if a.HandoffContract != HandoffResultsOnly {
		t.Errorf("default handoff = %q, want results_only", a.HandoffContract)
	}
}

func TestParseAgentDisposition(t *testing.T) {
	valid := `---
name: database
version: 0.1.0
description: db specialist
triggers:
  - source: acme
disposition:
  skepticism: 4
  literalism: 2
---
prompt body
`
	a, err := ParseAgent("x.md", []byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if a.Disposition == nil || a.Disposition.Skepticism != 4 || a.Disposition.Literalism != 2 || a.Disposition.Empathy != 0 {
		t.Fatalf("parsed disposition = %+v", a.Disposition)
	}

	bad := `---
name: database
version: 0.1.0
description: db specialist
triggers:
  - source: acme
disposition:
  skepticism: 7
---
prompt body
`
	if _, err := ParseAgent("x.md", []byte(bad)); err == nil || !strings.Contains(err.Error(), "disposition.skepticism") {
		t.Fatalf("err = %v, want disposition.skepticism", err)
	}
}

func TestParseAgentInvalid(t *testing.T) {
	cases := map[string]string{
		"missing name":       "---\nversion: 1\ndescription: d\ntriggers: [{source: s}]\n---\nbody",
		"bad name":           "---\nname: Bad_Name\nversion: 1\ndescription: d\ntriggers: [{source: s}]\n---\nbody",
		"missing triggers":   "---\nname: a\nversion: 1\ndescription: d\n---\nbody",
		"empty trigger rule": "---\nname: a\nversion: 1\ndescription: d\ntriggers: [{id: t}]\n---\nbody",
		"bad autonomy":       "---\nname: a\nversion: 1\ndescription: d\ntriggers: [{source: s}]\nautonomy: god-mode\n---\nbody",
		"bad handoff":        "---\nname: a\nversion: 1\ndescription: d\ntriggers: [{source: s}]\nhandoff_contract: telepathy\n---\nbody",
		"full_trace handoff": "---\nname: a\nversion: 1\ndescription: d\ntriggers: [{source: s}]\nhandoff_contract: full_trace\n---\nbody",
		"negative budget":    "---\nname: a\nversion: 1\ndescription: d\ntriggers: [{source: s}]\nbudgets: {tokens: -1}\n---\nbody",
		"empty body":         "---\nname: a\nversion: 1\ndescription: d\ntriggers: [{source: s}]\n---\n",
		"no frontmatter":     "just markdown",
	}
	for name, src := range cases {
		if _, err := ParseAgent("bad.md", []byte(src)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseSkillValidAndInvalid(t *testing.T) {
	good := `---
name: db-connection-triage
description: triage connections
metadata:
  version: 0.1.0
  action_class: read
  approval_required: "false"
allowed-tools: a__x b__y
---
steps here
`
	sk, err := ParseSkill("skills/db-connection-triage/SKILL.md", "db-connection-triage", []byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if sk.Metadata["action_class"] != "read" {
		t.Errorf("metadata lost: %+v", sk.Metadata)
	}

	if _, err := ParseSkill("s", "wrong-dir", []byte(good)); err == nil {
		t.Error("dir mismatch accepted")
	}
	noDesc := "---\nname: wrong-dir\n---\nbody"
	if _, err := ParseSkill("s", "wrong-dir", []byte(noDesc)); err == nil {
		t.Error("missing description accepted")
	}
	badApproval := "---\nname: x\ndescription: d\nmetadata: {approval_required: maybe}\n---\nbody"
	if _, err := ParseSkill("s", "x", []byte(badApproval)); err == nil {
		t.Error("bad approval_required accepted")
	}
}

func TestParsePolicy(t *testing.T) {
	good := `
name: default
action_classes:
  - name: read
    min_autonomy: observe
    tool_patterns: ["a__*"]
`
	p, err := ParsePolicy("p.yaml", []byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if p.ActionClasses[0].MinAutonomy != "observe" {
		t.Fatalf("parsed = %+v", p)
	}

	dup := `
name: default
action_classes:
  - {name: read, min_autonomy: observe, tool_patterns: ["a"]}
  - {name: read, min_autonomy: auto, tool_patterns: ["b"]}
`
	if _, err := ParsePolicy("p.yaml", []byte(dup)); err == nil {
		t.Error("duplicate class accepted")
	}
	badAut := `
name: default
action_classes:
  - {name: read, min_autonomy: sudo, tool_patterns: ["a"]}
`
	if _, err := ParsePolicy("p.yaml", []byte(badAut)); err == nil {
		t.Error("bad min_autonomy accepted")
	}
}

// The shipped specs are executable documentation: they must validate.
// Pointed at specs/ itself rather than a samples subdirectory, so what is
// pinned here is the agent/skill/policy set the product actually loads.
// LoadDir reads only root/agents, root/skills and root/policies and never
// recurses, so any nested directory under specs/ is invisible to it.
func TestLoadDirExamples(t *testing.T) {
	root, err := filepath.Abs("../../specs")
	if err != nil {
		t.Fatal(err)
	}
	b, errs := LoadDir(root)
	for _, e := range errs {
		t.Errorf("examples: %v", e)
	}
	if len(b.Agents) == 0 || len(b.Skills) == 0 || len(b.Policies) == 0 {
		t.Fatalf("examples incomplete: %d agents, %d skills, %d policies",
			len(b.Agents), len(b.Skills), len(b.Policies))
	}
}

func TestLoadDirCrossValidation(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "agents", "a.md"),
		"---\nname: a\nversion: 1\ndescription: d\ntriggers: [{source: s}]\nskills: [ghost-skill]\n---\nbody")
	_, errs := LoadDir(dir)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "ghost-skill") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing-skill reference not caught: %v", errs)
	}
}
