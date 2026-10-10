package hookcli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// codeops:trace repo=punk-records work_item=punk-connect-remote-url-review-20261009 spec=docs/client-credentials.md plan=docs/superpowers/plans/2026-10-09-client-credentials.md test=TestPluginCredentialsRuntime

// pluginNodeEnv isolates every emitted-plugin runner from the operator's
// credentials, runtime overrides and persistent inbox state. A scenario may
// override these defaults only with its own synthetic fixtures.
func pluginNodeEnv(home string, extra map[string]string) []string {
	values := map[string]string{
		"HOME": home, "USERPROFILE": home,
		"XDG_STATE_HOME":   filepath.Join(home, "state"),
		"PUNK_CREDENTIALS": filepath.Join(home, "missing-credentials.json"),
	}
	for k, v := range extra {
		values[k] = v
	}
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if _, replaced := values[name]; replaced || strings.HasPrefix(name, "PUNK_") || name == "NODE_OPTIONS" {
			continue
		}
		env = append(env, kv)
	}
	for k, v := range values {
		env = append(env, k+"="+v)
	}
	return env
}

func TestPluginNodeEnvIsHermetic(t *testing.T) {
	home := t.TempDir()
	for _, name := range []string{"HOME", "USERPROFILE", "PUNK_CREDENTIALS", "PUNK_URL", "PUNK_API_KEY", "NODE_OPTIONS"} {
		t.Setenv(name, "inherited-sentinel")
	}
	got := map[string]string{}
	for _, kv := range pluginNodeEnv(home, nil) {
		name, value, _ := strings.Cut(kv, "=")
		if value == "inherited-sentinel" {
			t.Fatalf("inherited %s escaped the isolation boundary", name)
		}
		got[name] = value
	}
	if got["HOME"] != home || got["USERPROFILE"] != home || got["PUNK_CREDENTIALS"] != filepath.Join(home, "missing-credentials.json") {
		t.Fatal("Node home and selected credentials must belong to this test")
	}
}

// CREDS-4: generators see synthetic saved/env tokens while rendering, but only
// runtime code, never those values or the selected file path, may be emitted.
func TestGeneratedPluginCredentialsStayRuntimeOnly(t *testing.T) {
	home := t.TempDir()
	file := filepath.Join(home, "private-fixture.json")
	const savedToken = "synthetic-generator-saved-token"
	const envToken = "synthetic-generator-env-token"
	if err := os.WriteFile(file, []byte(`{"url":"http://localhost:9090","api_key":"`+savedToken+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PUNK_CREDENTIALS", file)
	t.Setenv("PUNK_API_KEY", envToken)
	for kind, source := range map[string]string{
		"opencode": openCodePluginContent("http://localhost:9090"),
		"pi":       piExtensionContentNS("http://localhost:9090", ""),
		"openclaw": openClawPluginSource("http://localhost:9090"),
	} {
		for _, secret := range []string{savedToken, envToken, file} {
			if strings.Contains(source, secret) {
				t.Errorf("%s embedded private connection material", kind)
			}
		}
	}
}

// CREDS-1..5: import the unmodified emitted ESM module and drive its public
// host hooks/tools. Requests cross a real loopback listener; a fetch guard
// rejects every other destination before dialing. Auth values stay out of
// evidence logs. No dependency on ResolveServer's concurrently changing API.
func TestPluginCredentialsRuntime(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping emitted-plugin credential runtime tests")
	}
	driver, err := os.ReadFile(filepath.Join("testdata", "plugin_credentials.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []string{
		"saved-only", "custom-file", "saved-url-fallback", "missing-installed", "missing-local", "empty-env",
		"env-key", "runtime-url-match", "runtime-url-mismatch", "installed-url-mismatch", "custom-missing",
		"explicit-bypass", "env-key-invalid-file", "snapshot", "saved-snapshot", "two-instances",
		"canonical-host", "canonical-http-port", "canonical-https-port", "different-scheme", "different-port",
		"invalid-json", "invalid-null", "invalid-array", "invalid-scalar", "invalid-url-missing", "invalid-url-empty",
		"invalid-url-type", "invalid-key-type", "invalid-key-null", "invalid-unreadable",
		"invalid-userinfo", "invalid-query", "invalid-fragment", "invalid-empty-query", "invalid-empty-fragment",
		"invalid-relative", "invalid-scheme", "invalid-host", "invalid-port", "invalid-backslash", "invalid-whitespace",
		"invalid-escape", "invalid-selected-url",
	}
	for _, scenario := range cases {
		t.Run(scenario, func(t *testing.T) {
			for _, kind := range []string{"opencode", "pi", "openclaw"} {
				t.Run(kind, func(t *testing.T) {
					var mu sync.Mutex
					var requests []string
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						auth := "missing"
						switch r.Header.Get("Authorization") {
						case "Bearer synthetic-saved-key":
							auth = "saved-fixture"
						case "Bearer synthetic-env-key":
							auth = "env-fixture"
						case "":
						default:
							auth = "other-fixture"
						}
						mu.Lock()
						requests = append(requests, r.Method+" "+r.URL.Path+" auth="+auth)
						mu.Unlock()
						w.Header().Set("Content-Type", "application/json")
						switch {
						case strings.HasSuffix(r.URL.Path, "/events"):
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprint(w, ": synthetic heartbeat\n\n")
							w.(http.Flusher).Flush()
							<-r.Context().Done()
						case strings.HasSuffix(r.URL.Path, "/context"):
							fmt.Fprint(w, `{"context":"fixture memory context"}`)
						case strings.HasSuffix(r.URL.Path, "/namespace"):
							fmt.Fprint(w, `{"namespace":"fixture-ns"}`)
						case strings.HasSuffix(r.URL.Path, "/members"):
							fmt.Fprint(w, `{"status":"registered","namespace":"fixture-ns"}`)
						case strings.HasSuffix(r.URL.Path, "/messages"):
							fmt.Fprint(w, `{"messages":[]}`)
						default:
							fmt.Fprint(w, `{"status":"stored","key":"/fixture","id":"fixture-id","memories":[]}`)
						}
					}))
					defer srv.Close()
					home := t.TempDir()
					fallback := srv.URL + "/installed"
					if scenario == "saved-url-fallback" || scenario == "missing-local" {
						fallback = ""
					}
					var source string
					switch kind {
					case "opencode":
						source = openCodePluginContent(fallback)
					case "pi":
						source = piExtensionContentNS(fallback, "")
					case "openclaw":
						source = openClawPluginSource(fallback)
					}
					plugin := filepath.Join(home, "plugin.mjs")
					harness := filepath.Join(home, "driver.mjs")
					if err := os.WriteFile(plugin, []byte(source), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(harness, driver, 0o600); err != nil {
						t.Fatal(err)
					}
					config, err := json.Marshal(map[string]string{"kind": kind, "scenario": scenario, "server": srv.URL, "plugin": plugin})
					if err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, node, harness)
					cmd.Dir = home
					cmd.Env = pluginNodeEnv(home, map[string]string{"PUNK_TEST_CASE": string(config)})
					out, err := cmd.CombinedOutput()
					mu.Lock()
					observed := strings.Join(requests, "\n")
					mu.Unlock()
					if err != nil {
						t.Fatalf("emitted %s/%s: %v\n%s\nloopback observations:\n%s", kind, scenario, err, out, observed)
					}
					if !strings.Contains(string(out), "PASS credentials") {
						t.Fatalf("missing runtime assertion evidence: %s", out)
					}
					t.Log(strings.TrimSpace(string(out)))
					if scenario == "saved-only" || scenario == "snapshot" || scenario == "invalid-json" {
						t.Logf("loopback observations:\n%s", observed)
					}
				})
			}
		})
	}
}
