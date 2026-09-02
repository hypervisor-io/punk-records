package hookcli

import (
	"os"
	"path/filepath"
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

func TestResolveServerPrecedence(t *testing.T) {
	p := filepath.Join(t.TempDir(), "credentials.json")
	if err := SaveCredentials(p, Credentials{URL: "https://file.example", APIKey: "prk_file"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUNK_CREDENTIALS", p)
	t.Setenv("PUNK_URL", "")
	t.Setenv("PUNK_API_KEY", "")
	if u, k := ResolveServer(""); u != "https://file.example" || k != "prk_file" {
		t.Fatalf("file: %s %s", u, k)
	}
	t.Setenv("PUNK_URL", "http://env.example/")
	t.Setenv("PUNK_API_KEY", "prk_env")
	if u, k := ResolveServer(""); u != "http://env.example" || k != "prk_env" {
		t.Fatalf("env: %s %s", u, k)
	}
	if u, _ := ResolveServer("http://flag.example"); u != "http://flag.example" {
		t.Fatalf("flag: %s", u)
	}
	t.Setenv("PUNK_CREDENTIALS", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("PUNK_URL", "")
	t.Setenv("PUNK_API_KEY", "")
	if u, k := ResolveServer(""); u != "http://localhost:9090" || k != "" {
		t.Fatalf("default: %s %q", u, k)
	}
}
