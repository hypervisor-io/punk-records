package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConnectCodexLiteralKeyIsGuiReadable pins the Desktop fix for the
// GLOBAL connect: a literal key selected at connect time is embedded as a
// static Authorization header inside the punk-managed block, NOT as a
// PUNK_API_KEY env-var reference. Codex Desktop inherits no shell
// environment, so an env-var reference resolves to nothing there and the
// Desktop session would connect unauthenticated. The embedding is safe
// because ConnectCodexConfig writes config.toml 0600. The --project scope
// deliberately does the opposite; see
// TestConnectProjectScopeNeverEmbedsLiteralKey.
func TestConnectCodexLiteralKeyIsGuiReadable(t *testing.T) {
	_, codexHome := codexRepoFixture(t, "git@github.com:acme/punk-records.git")
	t.Setenv("PUNK_API_KEY", "synthetic-literal-key")
	ts := verifyWhoamiServer(t)

	out, err := captureStdout(t, func() error {
		return cmdConnectCodex([]string{"--no-skill", "--verify", "--url", ts.URL})
	})
	if err != nil {
		t.Fatalf("connect codex: %v\n%s", err, out)
	}

	raw, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(raw)
	if !strings.Contains(cfg, `"Authorization" = "Bearer synthetic-literal-key"`) {
		t.Fatalf("the literal key must be embedded in the punk block for a GUI launch:\n%s", cfg)
	}
	if strings.Contains(cfg, "bearer_token_env_var") {
		t.Fatalf("a literal key must not be an env-var reference:\n%s", cfg)
	}
	if !strings.Contains(out, "the bearer token is written into the punk block") {
		t.Fatalf("connect must say where the token went:\n%s", out)
	}
	// verify cannot compare a header credential safely, and must say so
	// rather than claim an alignment it did not establish.
	if !strings.Contains(out, "namespace alignment not verified") {
		t.Fatalf("verify must report the header credential honestly:\n%s", out)
	}
	if !strings.Contains(out, "Authorization header") {
		t.Fatalf("verify must name the header credential:\n%s", out)
	}
	if strings.Contains(out, "synthetic-literal-key") {
		t.Fatalf("verify must never print the token:\n%s", out)
	}
}

// TestConnectCodexExplicitKeyEnvKeepsEnvReference pins the opt-in half: an
// explicit --api-key-env NAME still writes the env-var reference the
// operator asked for, and the key itself is never embedded.
func TestConnectCodexExplicitKeyEnvKeepsEnvReference(t *testing.T) {
	repo, _ := codexRepoFixture(t, "git@github.com:acme/punk-records.git")
	t.Setenv("PUNK_API_KEY", "synthetic-literal-key")

	out, err := captureStdout(t, func() error {
		return cmdConnectCodex([]string{"--project", "--no-skill", "--no-mcp"})
	})
	if err != nil {
		t.Fatalf("connect codex hooks: %v\n%s", err, out)
	}
	// With MCP disabled nothing is written; then the MCP half with the env
	// reference explicitly selected.
	out, err = captureStdout(t, func() error {
		return cmdConnectCodex([]string{"--project", "--no-skill", "--no-hooks", "--api-key-env", "PUNK_CUSTOM_TOKEN", "--url", "https://punk.example.test"})
	})
	if err != nil {
		t.Fatalf("connect codex mcp: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(repo, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(raw)
	if !strings.Contains(cfg, `bearer_token_env_var = "PUNK_CUSTOM_TOKEN"`) {
		t.Fatalf("an explicit --api-key-env must stay an env reference:\n%s", cfg)
	}
	if strings.Contains(cfg, "synthetic-literal-key") {
		t.Fatalf("an explicit --api-key-env must not embed the key:\n%s", cfg)
	}
	if !strings.Contains(out, "Codex reads the bearer token from $PUNK_CUSTOM_TOKEN") {
		t.Fatalf("connect must name the env var:\n%s", out)
	}
}
