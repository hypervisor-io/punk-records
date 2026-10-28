package hookcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenCodeMessagingInstallDefault runs the emitted plugin under node:
// a plugin installed with --messaging binds the session's inbox with no
// PUNK_MESSAGING in OpenCode's environment, PUNK_MESSAGING=0 still turns
// it off, and the default install stays off without the variable.
func TestOpenCodeMessagingInstallDefault(t *testing.T) {
	const bound = `
  await initHost();
  await start("s1");
  await until(() => calls.some((c) => c.path.endsWith("/members")), "member registration");
`
	const unbound = `
  await initHost();
  await start("s1");
  await new Promise((r) => setTimeout(r, 300));
  must(!calls.some((c) => c.path.endsWith("/members")), "messaging must stay off");
`
	on := func(src string) string {
		return strings.Replace(src, "const punkMessagingDefault = false\n", openCodeMessagingOnLine, 1)
	}
	cases := []struct {
		name, env, driver string
		mutate            func(string) string
	}{
		{"installed on, env unset", "", bound, on},
		{"installed on, env 0", "0", unbound, on},
		{"installed off, env unset", "", unbound, nil},
		{"installed off, env 1", "1", bound, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := execPluginBindingHarness(t, "opencode", "", map[string]string{"PUNK_MESSAGING": tc.env}, tc.driver, tc.mutate)
			if err != nil || !strings.Contains(out, "PASS binding regression") {
				t.Fatalf("%v\n%s", err, out)
			}
		})
	}
}

func TestInstalledOpenCodeMessaging(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.js")
	if InstalledOpenCodeMessaging(path) {
		t.Fatal("missing file reports messaging on")
	}
	if _, err := ConnectOpenCodeWith(path, "http://localhost:9090", OpenCodePluginOptions{Messaging: true}); err != nil {
		t.Fatal(err)
	}
	if !InstalledOpenCodeMessaging(path) {
		t.Fatal("plugin installed with messaging reports off")
	}
	if changed, err := ConnectOpenCodeWith(path, "http://localhost:9090", OpenCodePluginOptions{}); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if InstalledOpenCodeMessaging(path) {
		t.Fatal("plugin reinstalled without messaging reports on")
	}
	if err := os.WriteFile(path, []byte("// hand written\nconst punkMessagingDefault = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if InstalledOpenCodeMessaging(path) {
		t.Fatal("a plugin punk did not write reports messaging on")
	}
}
