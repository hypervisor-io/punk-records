package hookcli

import (
	"fmt"
	"hash/fnv"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var nsSlug = regexp.MustCompile(`[^\p{L}\p{N}]+`)

// NormalizeRemote reduces a git remote URL to host/path so every clone
// of one repository, however its URL is written, maps to the same value.
func NormalizeRemote(raw string) string {
	s := strings.TrimSpace(strings.ToLower(raw))
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	} else if at := strings.Index(s, "@"); at >= 0 && strings.Contains(s[at:], ":") && !strings.Contains(s[:at], "/") {
		// scp-like git@host:org/repo
		s = s[at+1:]
		s = strings.Replace(s, ":", "/", 1)
	}
	if at := strings.Index(s, "@"); at >= 0 && !strings.Contains(s[:at], "/") {
		s = s[at+1:]
	}
	s = strings.TrimRight(s, "/")
	s = strings.TrimSuffix(s, ".git")
	return strings.TrimRight(s, "/")
}

// ProjectNamespace derives the namespace for a checkout: from the origin
// remote when there is one (stable across machines and clones), else
// from the path the way hooks always have.
func ProjectNamespace(dir string) (ns, source string) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	out, err := exec.Command("git", "-C", abs, "config", "--get", "remote.origin.url").Output()
	if err == nil {
		if remote := NormalizeRemote(string(out)); remote != "" {
			base := remote[strings.LastIndex(remote, "/")+1:]
			slug := strings.Trim(nsSlug.ReplaceAllString(base, "-"), "-")
			if slug == "" {
				slug = "repo"
			}
			h := fnv.New32a()
			_, _ = h.Write([]byte(remote))
			return fmt.Sprintf("agent-%s-%06x", slug, h.Sum32()&0xffffff), "remote"
		}
	}
	return agentNamespaceForPath(abs), "path"
}

// agentNamespaceForPath mirrors internal/api's AgentNamespace byte for
// byte (same slug regex, same fnv32a fallback): hookcli cannot import
// that package (its tests import hookcli), so the derivation lives here
// too and the two must move together.
func agentNamespaceForPath(cwd string) string {
	if cwd == "" {
		return "agent-default"
	}
	base := filepath.Base(cwd)
	slug := strings.Trim(nsSlug.ReplaceAllString(strings.ToLower(base), "-"), "-")
	if slug == "" {
		return "agent-" + fnv32aHex(cwd)
	}
	return "agent-" + slug
}
