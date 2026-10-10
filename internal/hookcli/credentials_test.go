package hookcli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialsRoundTripAndMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nested", "credentials.json")
	if _, ok, err := LoadCredentials(p); err != nil || ok {
		t.Fatalf("absent file: ok=%v err=%v", ok, err)
	}
	if err := SaveCredentials(p, Credentials{URL: "https://punk.example.com", APIKey: "prk_abc"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v err=%v", info.Mode(), err)
	}
	c, ok, err := LoadCredentials(p)
	if err != nil || !ok || c.URL != "https://punk.example.com" || c.APIKey != "prk_abc" {
		t.Fatalf("load = %+v %v %v", c, ok, err)
	}
}

func mustResolveServer(t *testing.T, flagURL string) ServerResolution {
	t.Helper()
	r, err := ResolveServer(flagURL)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestResolveServerPrecedence(t *testing.T) {
	p := filepath.Join(t.TempDir(), "credentials.json")
	if err := SaveCredentials(p, Credentials{URL: "https://file.example", APIKey: "prk_file"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUNK_CREDENTIALS", p)
	t.Setenv("PUNK_URL", "")
	t.Setenv("PUNK_API_KEY", "")
	if r := mustResolveServer(t, ""); r.URL != "https://file.example" || r.APIKey != "prk_file" {
		t.Fatalf("file: %+v", r)
	}
	t.Setenv("PUNK_URL", "http://env.example/")
	t.Setenv("PUNK_API_KEY", "prk_env")
	if r := mustResolveServer(t, ""); r.URL != "http://env.example" || r.APIKey != "prk_env" {
		t.Fatalf("env: %+v", r)
	}
	if r := mustResolveServer(t, "http://flag.example"); r.URL != "http://flag.example" {
		t.Fatalf("flag: %+v", r)
	}
	t.Setenv("PUNK_CREDENTIALS", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("PUNK_URL", "")
	t.Setenv("PUNK_API_KEY", "")
	if r := mustResolveServer(t, ""); r.URL != "http://localhost:9090" || r.APIKey != "" {
		t.Fatalf("default: %+v", r)
	}
}

// codeops:trace repo=punk-records work_item=punk-connect-remote-url-review-20261009 spec=docs/client-credentials.md plan=phase-0/task-0-A test=TestResolveServerSavedKeyBoundary evidence=docs/superpowers/reports/2026-10-09-client-credentials/builder-a.md
func TestResolveServerSavedKeyBoundary(t *testing.T) {
	p := filepath.Join(t.TempDir(), "credentials.json")
	if err := SaveCredentials(p, Credentials{URL: "HTTPS://Example.COM:443/punk/", APIKey: "synthetic-saved-key"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUNK_CREDENTIALS", p)
	t.Setenv("PUNK_URL", "")
	t.Setenv("PUNK_API_KEY", "")

	if r := mustResolveServer(t, "https://example.com/punk"); r.URL != "https://example.com/punk" || r.APIKey != "synthetic-saved-key" {
		t.Fatalf("canonical match: %+v", r)
	}
	if r := mustResolveServer(t, "https://example.com/other"); r.URL != "https://example.com/other" || r.APIKey != "" || r.Diagnostic != savedKeyMismatchDiagnostic {
		t.Fatalf("path mismatch must suppress saved key with sanitized diagnostic: %+v", r)
	}
	if r := mustResolveServer(t, "http://example.com/punk"); r.URL != "http://example.com/punk" || r.APIKey != "" || strings.Contains(r.Diagnostic, "synthetic") || strings.Contains(r.Diagnostic, "example.com") {
		t.Fatalf("scheme mismatch must suppress saved key safely: %+v", r)
	}
}

func TestLoadCredentialsRejectsInvalidPresentFiles(t *testing.T) {
	cases := map[string]string{
		"malformed":          `{`,
		"array":              `[]`,
		"missing url":        `{"api_key":"synthetic"}`,
		"empty url":          `{"url":""}`,
		"wrong url type":     `{"url":7}`,
		"wrong key type":     `{"url":"https://example.test","api_key":7}`,
		"null key":           `{"url":"https://example.test","api_key":null}`,
		"userinfo":           `{"url":"https://user:pass@example.test"}`,
		"query":              `{"url":"https://example.test/base?token=synthetic"}`,
		"fragment":           `{"url":"https://example.test/base#fragment"}`,
		"unsupported scheme": `{"url":"file:///tmp/punk"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "credentials.json")
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := LoadCredentials(p); err == nil {
				t.Fatal("present invalid credentials must be rejected")
			}
		})
	}
}

func TestResolveServerFullyExplicitPairBypassesUnusedMalformedFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(p, []byte(`{"url":`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUNK_CREDENTIALS", p)
	t.Setenv("PUNK_URL", "")
	t.Setenv("PUNK_API_KEY", "synthetic-explicit-key")
	r, err := ResolveServer("https://explicit.example.test/base")
	if err != nil || r.URL != "https://explicit.example.test/base" || r.APIKey != "synthetic-explicit-key" {
		t.Fatalf("explicit pair: result=%+v err=%v", r, err)
	}
}

func TestResolveServerUsesOnlySelectedCustomCredentialsPath(t *testing.T) {
	home := t.TempDir()
	defaultPath := filepath.Join(home, ".punk", "credentials.json")
	if err := os.MkdirAll(filepath.Dir(defaultPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(defaultPath, []byte(`{"url":`), 0o600); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(t.TempDir(), "selected.json")
	if err := SaveCredentials(custom, Credentials{URL: "https://selected.example.test/base", APIKey: "synthetic-selected-key"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PUNK_CREDENTIALS", custom)
	t.Setenv("PUNK_URL", "")
	t.Setenv("PUNK_API_KEY", "")
	r := mustResolveServer(t, "")
	if r.URL != "https://selected.example.test/base" || r.APIKey != "synthetic-selected-key" {
		t.Fatalf("selected custom credentials not used exclusively: %+v", r)
	}
}

func TestResolveServerExplicitEnvironmentKeyWinsOnDifferentServer(t *testing.T) {
	p := filepath.Join(t.TempDir(), "credentials.json")
	if err := SaveCredentials(p, Credentials{URL: "https://saved.example.test/base", APIKey: "synthetic-saved-key"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUNK_CREDENTIALS", p)
	t.Setenv("PUNK_URL", "https://selected.example.test/base")
	t.Setenv("PUNK_API_KEY", "synthetic-explicit-key")
	r := mustResolveServer(t, "")
	if r.URL != "https://selected.example.test/base" || r.APIKey != "synthetic-explicit-key" || r.Diagnostic != "" {
		t.Fatalf("explicit environment key must win: %+v", r)
	}
}

func TestResolveServerDiagnosticsNeverEchoSensitiveInputs(t *testing.T) {
	p := filepath.Join(t.TempDir(), "credentials.json")
	const sensitive = "synthetic-secret-material"
	if err := os.WriteFile(p, []byte(`{"url":"https://user:`+sensitive+`@private.example.test","api_key":"`+sensitive+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUNK_CREDENTIALS", p)
	t.Setenv("PUNK_URL", "")
	t.Setenv("PUNK_API_KEY", "")
	_, err := ResolveServer("")
	if err == nil {
		t.Fatal("invalid credential-bearing URL must fail")
	}
	if strings.Contains(err.Error(), sensitive) || strings.Contains(err.Error(), "private.example.test") || strings.Contains(err.Error(), p) {
		t.Fatalf("error leaked sensitive credential input: %q", err)
	}
}

func TestResolveServerRejectsMalformedExplicitBaseURLs(t *testing.T) {
	t.Setenv("PUNK_API_KEY", "synthetic-explicit-key")
	t.Setenv("PUNK_CREDENTIALS", filepath.Join(t.TempDir(), "unused-malformed.json"))
	for _, raw := range []string{
		"http:///missing-host",
		"http://example.test:0",
		"http://example.test:bad",
		"http://example.test:65536",
		" https://example.test",
		"https://example.test/path?",
		"https://example.test/path#",
		`https:\\example.test`,
	} {
		if _, err := ResolveServer(raw); err == nil {
			t.Fatalf("malformed explicit URL accepted: %q", raw)
		}
	}
}

func TestResolvedSavedCredentialsDriveActualHookRequest(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"status":"stored"}`)
	}))
	defer srv.Close()

	p := filepath.Join(t.TempDir(), "credentials.json")
	if err := SaveCredentials(p, Credentials{URL: srv.URL, APIKey: "synthetic-hook-key"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUNK_CREDENTIALS", p)
	t.Setenv("PUNK_URL", "")
	t.Setenv("PUNK_API_KEY", "")
	resolved := mustResolveServer(t, "")
	var out, errOut bytes.Buffer
	if err := Run(strings.NewReader(`{"hook_event_name":"Stop","session_id":"synthetic","cwd":"/tmp","last_assistant_message":"done"}`), resolved.URL, resolved.APIKey, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/agent/hooks" || gotAuth != "Bearer synthetic-hook-key" {
		t.Fatalf("actual hook request: path=%q auth=%q stderr=%q", gotPath, gotAuth, errOut.String())
	}
}
