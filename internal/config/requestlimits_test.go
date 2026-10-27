package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Request-limit config (task P3-A): body cap, per-IP rate, stream cap.
// The defaults are generous; these tests pin them and the env overrides so
// a silent default change is caught.

func TestRequestLimitDefaults(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if c.HTTP.MaxBodyBytes != 8<<20 {
		t.Errorf("HTTP.MaxBodyBytes = %d, want %d", c.HTTP.MaxBodyBytes, 8<<20)
	}
	if c.HTTP.RateLimitRPS != 50 {
		t.Errorf("HTTP.RateLimitRPS = %d, want 50", c.HTTP.RateLimitRPS)
	}
	if c.HTTP.RateLimitBurst != 100 {
		t.Errorf("HTTP.RateLimitBurst = %d, want 100", c.HTTP.RateLimitBurst)
	}
	if c.Messaging.StreamLimit != 4 {
		t.Errorf("Messaging.StreamLimit = %d, want 4", c.Messaging.StreamLimit)
	}
	if c.Messaging.StreamIPLimit != 256 {
		t.Errorf("Messaging.StreamIPLimit = %d, want 256", c.Messaging.StreamIPLimit)
	}
	if len(c.HTTP.TrustedProxies) != 0 {
		t.Errorf("HTTP.TrustedProxies = %v, want empty (XFF never trusted by default)", c.HTTP.TrustedProxies)
	}
}

func TestRequestLimitFileAndEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := []byte("http:\n  max_body_bytes: 1048576\n  rate_limit_rps: 7\n  trusted_proxies: [10.0.0.0/8]\nmessaging:\n  stream_limit: 3\n  stream_ip_limit: 9\n")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUNK_HTTP_RATE_LIMIT_BURST", "11")
	t.Setenv("PUNK_MESSAGING_STREAM_LIMIT", "4")
	t.Setenv("PUNK_MESSAGING_STREAM_IP_LIMIT", "10")
	t.Setenv("PUNK_HTTP_TRUSTED_PROXIES", "192.0.2.0/24, 127.0.0.1/32")

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.HTTP.MaxBodyBytes != 1048576 {
		t.Errorf("file max_body_bytes = %d, want 1048576", c.HTTP.MaxBodyBytes)
	}
	if c.HTTP.RateLimitRPS != 7 {
		t.Errorf("file rate_limit_rps = %d, want 7", c.HTTP.RateLimitRPS)
	}
	if c.HTTP.RateLimitBurst != 11 {
		t.Errorf("env rate_limit_burst = %d, want 11", c.HTTP.RateLimitBurst)
	}
	if c.Messaging.StreamLimit != 4 {
		t.Errorf("env stream_limit = %d, want 4 (env beats file)", c.Messaging.StreamLimit)
	}
	if c.Messaging.StreamIPLimit != 10 {
		t.Errorf("env stream_ip_limit = %d, want 10 (env beats file)", c.Messaging.StreamIPLimit)
	}
	if got := strings.Join(c.HTTP.TrustedProxies, ","); got != "192.0.2.0/24,127.0.0.1/32" {
		t.Errorf("env trusted_proxies = %q, want the two configured CIDRs (env beats file)", got)
	}
}

func TestRequestLimitValidation(t *testing.T) {
	for name, body := range map[string]string{
		"tiny body cap":  "http:\n  max_body_bytes: 10\n",
		"zero rps":       "http:\n  rate_limit_rps: 0\n",
		"negative burst": "http:\n  rate_limit_burst: -1\n",
		"zero streams":   "messaging:\n  stream_limit: 0\n",
		"zero ip cap":    "messaging:\n  stream_ip_limit: 0\n",
		"bad proxy cidr": "http:\n  trusted_proxies: [10.0.0.0/8, not-a-cidr]\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatalf("want a validation error for %s", name)
			}
		})
	}
}
