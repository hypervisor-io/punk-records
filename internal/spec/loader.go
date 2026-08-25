package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Bundle is one fully parsed, cross-validated spec tree.
type Bundle struct {
	Agents   map[string]*AgentSpec
	Skills   map[string]*SkillSpec
	Policies map[string]*PolicySpec
}

// LoadDir walks root (agents/*.md, skills/*/SKILL.md, policies/*.yaml)
// and returns the bundle plus every validation error found. A non-empty
// error list means the bundle must not be activated.
func LoadDir(root string) (*Bundle, []error) {
	b := &Bundle{
		Agents:   map[string]*AgentSpec{},
		Skills:   map[string]*SkillSpec{},
		Policies: map[string]*PolicySpec{},
	}
	var errs []error

	agentsDir := filepath.Join(root, "agents")
	if entries, err := os.ReadDir(agentsDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			path := filepath.Join(agentsDir, e.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			a, err := ParseAgent(path, raw)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if prev, dup := b.Agents[a.Name]; dup {
				errs = append(errs, fmt.Errorf("%s: duplicate agent %q (also %s)", path, a.Name, prev.SourcePath))
				continue
			}
			b.Agents[a.Name] = a
		}
	} else if !os.IsNotExist(err) {
		errs = append(errs, err)
	}

	skillsDir := filepath.Join(root, "skills")
	if entries, err := os.ReadDir(skillsDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			path := filepath.Join(skillsDir, e.Name(), "SKILL.md")
			raw, err := os.ReadFile(path)
			if err != nil {
				errs = append(errs, fmt.Errorf("skills/%s: missing SKILL.md", e.Name()))
				continue
			}
			sk, err := ParseSkill(path, e.Name(), raw)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			b.Skills[sk.Name] = sk
		}
	} else if !os.IsNotExist(err) {
		errs = append(errs, err)
	}

	policiesDir := filepath.Join(root, "policies")
	if entries, err := os.ReadDir(policiesDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yaml") && !strings.HasSuffix(e.Name(), ".yml")) {
				continue
			}
			path := filepath.Join(policiesDir, e.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			p, err := ParsePolicy(path, raw)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if prev, dup := b.Policies[p.Name]; dup {
				errs = append(errs, fmt.Errorf("%s: duplicate policy %q (also %s)", path, p.Name, prev.SourcePath))
				continue
			}
			b.Policies[p.Name] = p
		}
	} else if !os.IsNotExist(err) {
		errs = append(errs, err)
	}

	errs = append(errs, b.crossValidate()...)
	return b, errs
}

// crossValidate checks references between spec kinds.
func (b *Bundle) crossValidate() []error {
	var errs []error
	for _, a := range b.Agents {
		for _, s := range a.Skills {
			if _, ok := b.Skills[s]; !ok {
				errs = append(errs, fmt.Errorf("%s: skill %q not found under skills/", a.SourcePath, s))
			}
		}
	}
	if len(b.Policies) > 0 {
		classes := map[string]bool{}
		for _, p := range b.Policies {
			for _, ac := range p.ActionClasses {
				classes[ac.Name] = true
			}
		}
		for _, sk := range b.Skills {
			if ac := sk.Metadata["action_class"]; ac != "" && !classes[ac] {
				errs = append(errs, fmt.Errorf("%s: action_class %q not defined by any policy", sk.SourcePath, ac))
			}
		}
	}
	return errs
}
