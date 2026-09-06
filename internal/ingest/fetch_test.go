package ingest

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestFetchBlocksPrivateNetworks is the SSRF red proof: an explicit URL
// fetch still refuses loopback, private and link-local targets at dial
// time, so a fetched document can never reach a server-side internal
// address. httptest's own loopback server stands in as the victim.
func TestFetchBlocksPrivateNetworks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("# never\n"))
	}))
	defer srv.Close()

	f := NewFetcher(0, 0)
	_, _, err := f.Fetch(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "ssrf") {
		t.Fatalf("err = %v, want the ssrf guard to refuse loopback", err)
	}
}

func TestFetchRejectsBadSchemesAndUserinfo(t *testing.T) {
	f := NewFetcher(0, 0)
	for _, u := range []string{"file:///etc/passwd", "ftp://example.com/x", "gopher://example.com/"} {
		if _, _, err := f.Fetch(context.Background(), u); err == nil ||
			!strings.Contains(err.Error(), "only http and https") {
			t.Fatalf("%s: err = %v, want a scheme refusal", u, err)
		}
	}
	// userinfo is refused, and the refusal must not echo the credential.
	_, _, err := f.Fetch(context.Background(), "http://user:hunter2secret@example.com/")
	if err == nil || !strings.Contains(err.Error(), "userinfo") {
		t.Fatalf("userinfo: err = %v, want a userinfo refusal", err)
	}
	if strings.Contains(err.Error(), "hunter2secret") {
		t.Fatalf("error leaks the URL credential: %q", err)
	}
}

func TestIsPublicIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"2606:4700:4700::1111", true},
		{"127.0.0.1", false},
		{"::1", false},
		{"10.0.0.4", false},
		{"172.16.0.2", false},
		{"192.168.1.1", false},
		{"169.254.169.254", false}, // cloud metadata (link-local)
		{"fe80::1", false},
		{"100.64.0.1", false}, // CGNAT
		{"100.127.255.254", false},
		{"100.128.0.1", true}, // just past the CGNAT range
		{"0.0.0.0", false},
		{"224.0.0.1", false},
		{"ff02::1", false},
		{"fc00::1", false},
	}
	for _, c := range cases {
		if got := isPublicIP(net.ParseIP(c.ip)); got != c.want {
			t.Fatalf("isPublicIP(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

// TestFetchSuccessPath uses the in-package allowPrivate escape hatch
// (never wired by the CLI) so httptest can stand in for a public origin:
// the body is returned bounded and the content type flows into format
// detection.
func TestFetchSuccessPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Write([]byte("# Hi\n\nbody text\n"))
	}))
	defer srv.Close()

	f := &Fetcher{MaxBytes: DefaultMaxBytes, Timeout: 5 * time.Second, allowPrivate: true}
	doc, err := Load(context.Background(), Spec{URL: srv.URL, fetcher: f})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Source.URI != srv.URL {
		t.Fatalf("source URI = %q, want %q", doc.Source.URI, srv.URL)
	}
	if doc.Source.MediaType != "text/markdown" {
		t.Fatalf("media type = %q, want text/markdown from the served content type", doc.Source.MediaType)
	}
	if len(doc.Sections) != 1 || doc.Sections[0].Name != "Hi" ||
		!strings.Contains(doc.Sections[0].Text, "body text") {
		t.Fatalf("sections = %+v", doc.Sections)
	}
}

func TestFetchEnforcesSizeAndStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.Error(w, "gone", http.StatusNotFound)
			return
		}
		w.Write([]byte(strings.Repeat("x", 64)))
	}))
	defer srv.Close()

	f := &Fetcher{MaxBytes: 8, Timeout: 5 * time.Second, allowPrivate: true}
	if _, _, err := f.Fetch(context.Background(), srv.URL); err == nil ||
		!strings.Contains(err.Error(), "max_bytes") {
		t.Fatalf("oversize body: err = %v, want a max_bytes error", err)
	}
	f.MaxBytes = DefaultMaxBytes
	if _, _, err := f.Fetch(context.Background(), srv.URL+"/missing"); err == nil ||
		!strings.Contains(err.Error(), "404") {
		t.Fatalf("404: err = %v, want the HTTP status in the error", err)
	}
}
