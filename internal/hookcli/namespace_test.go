package hookcli

import (
	"os/exec"
	"testing"
)

func TestNormalizeRemote(t *testing.T) {
	for _, c := range [][2]string{
		{"git@github.com:Org/Repo.git", "github.com/org/repo"},
		{"https://github.com/org/repo", "github.com/org/repo"},
		{"https://alice@github.com/org/repo.git/", "github.com/org/repo"},
		{"ssh://git@gitlab.example.com:2222/team/proj.git", "gitlab.example.com:2222/team/proj"},
	} {
		if got := NormalizeRemote(c[0]); got != c[1] {
			t.Fatalf("NormalizeRemote(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func TestProjectNamespaceFromRemoteAndFallback(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q")
	ns, src := ProjectNamespace(dir)
	if src != "path" || ns == "" {
		t.Fatalf("no remote: %s %s", ns, src)
	}
	run("remote", "add", "origin", "git@github.com:Acme/Billing.git")
	ns1, src := ProjectNamespace(dir)
	if src != "remote" || len(ns1) != len("agent-billing-")+6 || ns1[:14] != "agent-billing-" {
		t.Fatalf("remote: %s %s", ns1, src)
	}
	run("remote", "set-url", "origin", "https://github.com/acme/billing")
	if ns2, _ := ProjectNamespace(dir); ns2 != ns1 {
		t.Fatalf("same repo via different URL forms must agree: %s vs %s", ns1, ns2)
	}
}
