package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaults(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if c.HTTP.Addr != ":9090" {
		t.Errorf("HTTP.Addr = %q, want :9090", c.HTTP.Addr)
	}
	if c.AI.Enabled {
		t.Error("AI.Enabled = true by default, want false (deterministic-first)")
	}
	if c.DB.Driver != "sqlite" {
		t.Errorf("DB.Driver = %q, want sqlite", c.DB.Driver)
	}
}

func TestLoadFileAndEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := []byte("http:\n  addr: \":7777\"\ndb:\n  driver: postgres\n  dsn: postgres://x\nbudgets:\n  tokens: 1000\nai:\n  profiles:\n    default:\n      base_url: http://localhost:11434/v1\n      model: llama3\n")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PUNK_DB_DSN", "postgres://from-env")
	t.Setenv("PUNK_AI_ENABLED", "true")

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.HTTP.Addr != ":7777" {
		t.Errorf("file value lost: HTTP.Addr = %q", c.HTTP.Addr)
	}
	if c.DB.DSN != "postgres://from-env" {
		t.Errorf("env override lost: DB.DSN = %q", c.DB.DSN)
	}
	if !c.AI.Enabled {
		t.Error("env override lost: AI.Enabled = false")
	}
	if c.Budgets.Tokens != 1000 {
		t.Errorf("Budgets.Tokens = %d, want 1000", c.Budgets.Tokens)
	}
	// unset keys keep defaults
	if c.Budgets.ToolCalls != 50 {
		t.Errorf("Budgets.ToolCalls = %d, want default 50", c.Budgets.ToolCalls)
	}
}

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if c.Specs.Dir != "./specs" {
		t.Errorf("Specs.Dir = %q", c.Specs.Dir)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	t.Setenv("PUNK_DB_DRIVER", "mysql")
	if _, err := Load(""); err == nil {
		t.Fatal("want error for db.driver=mysql, got nil")
	}
	t.Setenv("PUNK_DB_DRIVER", "sqlite")
	t.Setenv("PUNK_AI_ENABLED", "not-a-bool")
	if _, err := Load(""); err == nil {
		t.Fatal("want error for bad bool, got nil")
	}
}

func TestEmbeddingsMaxInputTokensValidation(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("ai:\n  embeddings:\n    model: x\n    max_input_tokens: -1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil {
		t.Fatal("want error for max_input_tokens=-1, got nil")
	}
	okPath := filepath.Join(dir, "ok.yaml")
	if err := os.WriteFile(okPath, []byte("ai:\n  embeddings:\n    model: x\n    max_input_tokens: 2048\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(okPath)
	if err != nil {
		t.Fatalf("max_input_tokens=2048 should validate: %v", err)
	}
	if c.AI.Embeddings.MaxInputTokens != 2048 {
		t.Fatalf("MaxInputTokens = %d, want 2048", c.AI.Embeddings.MaxInputTokens)
	}
}
