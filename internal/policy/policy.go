// Package policy enforces the autonomy ladder: every tool call an agent
// makes is classified (action classes from policy specs) and checked
// against the agent's declared autonomy. Refusals are decisions, not
// crashes; actions above the agent's level become proposals for an
// external approver.
package policy

import (
	"path"
	"sort"
	"strings"

	"github.com/hypervisor-io/punk-records/internal/spec"
)

// Verdict is the outcome of a policy check.
type Verdict int

const (
	// Allow: execute the tool call directly.
	Allow Verdict = iota
	// Propose: do not execute; record a proposal for approval.
	Propose
	// Deny: refuse; the agent lacks the autonomy for this class.
	Deny
)

func (v Verdict) String() string {
	switch v {
	case Allow:
		return "allow"
	case Propose:
		return "propose"
	case Deny:
		return "deny"
	}
	return "unknown"
}

// Engine is an immutable view compiled from one spec snapshot.
type Engine struct {
	classes []compiledClass
	acl     []spec.MemoryACLRule // sorted longest-prefix-first
	// permissive is true when no policies are loaded at all: a
	// policy-less deployment runs unrestricted (documented stance).
	permissive bool
}

type compiledClass struct {
	name        string
	minAutonomy string
	patterns    []string
}

// FromBundle compiles every policy's action classes, in policy-name then
// declaration order. First matching pattern wins.
func FromBundle(b *spec.Bundle) *Engine {
	e := &Engine{permissive: len(b.Policies) == 0}
	names := make([]string, 0, len(b.Policies))
	for n := range b.Policies {
		names = append(names, n)
	}
	// deterministic order
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	for _, n := range names {
		for _, ac := range b.Policies[n].ActionClasses {
			e.classes = append(e.classes, compiledClass{
				name: ac.Name, minAutonomy: ac.MinAutonomy, patterns: ac.ToolPatterns,
			})
		}
		e.acl = append(e.acl, b.Policies[n].MemoryACL...)
	}
	sort.Slice(e.acl, func(i, j int) bool { return len(e.acl[i].Prefix) > len(e.acl[j].Prefix) })
	return e
}

// CheckMemoryWrite gates an agent's memory-plane write by key prefix.
// Longest matching rule wins; keys with no rule are writable (P12.4).
func (e *Engine) CheckMemoryWrite(agent, key string) Decision {
	for _, r := range e.acl {
		if !strings.HasPrefix(key, r.Prefix) {
			continue
		}
		if r.ReadOnly {
			return Decision{Verdict: Deny, ActionClass: "memory",
				Reason: "prefix " + r.Prefix + " is read-only reference data"}
		}
		for _, w := range r.Writers {
			if w == "*" || w == agent {
				return Decision{Verdict: Allow, ActionClass: "memory", Reason: "acl writer match"}
			}
		}
		return Decision{Verdict: Deny, ActionClass: "memory",
			Reason: "agent " + agent + " not in writers for prefix " + r.Prefix}
	}
	return Decision{Verdict: Allow, ActionClass: "memory", Reason: "no acl rule"}
}

func matchPattern(pattern, tool string) bool {
	if ok, _ := path.Match(pattern, tool); ok {
		return true
	}
	if pre, isGlob := strings.CutSuffix(pattern, "*"); isGlob && strings.HasPrefix(tool, pre) {
		return true
	}
	return pattern == tool
}

// ClassFor returns the first action class matching the tool. Unmatched
// tools in a policied deployment are unclassified: conservative callers
// treat them as requiring proposal.
func (e *Engine) ClassFor(tool string) (name, minAutonomy string, found bool) {
	for _, c := range e.classes {
		for _, p := range c.patterns {
			if matchPattern(p, tool) {
				return c.name, c.minAutonomy, true
			}
		}
	}
	return "", "", false
}

// Decision carries the verdict plus the reasoning for the audit trail.
type Decision struct {
	Verdict     Verdict
	ActionClass string
	Reason      string
}

// Check applies the ladder:
//   - no policies loaded: allow (permissive deployment)
//   - unclassified tool: propose (conservative default)
//   - agent autonomy >= class minimum: allow
//   - class requires auto, agent can propose: propose
//   - otherwise: deny
func (e *Engine) Check(agentAutonomy, tool string) Decision {
	if e.permissive {
		return Decision{Verdict: Allow, ActionClass: "unpolicied", Reason: "no policies loaded"}
	}
	class, minAut, found := e.ClassFor(tool)
	if !found {
		return Decision{Verdict: Propose, ActionClass: "unclassified",
			Reason: "tool matches no action class; conservative default is proposal"}
	}
	agentLvl := spec.AutonomyLevel(agentAutonomy)
	needLvl := spec.AutonomyLevel(minAut)
	switch {
	case agentLvl >= needLvl:
		return Decision{Verdict: Allow, ActionClass: class, Reason: "autonomy sufficient"}
	case needLvl == spec.AutonomyLevel(spec.AutonomyAuto) && agentLvl >= spec.AutonomyLevel(spec.AutonomyPropose):
		return Decision{Verdict: Propose, ActionClass: class,
			Reason: "action requires auto; agent may propose"}
	default:
		return Decision{Verdict: Deny, ActionClass: class,
			Reason: "agent autonomy " + agentAutonomy + " below required " + minAut}
	}
}
