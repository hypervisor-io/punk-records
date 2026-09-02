package embedlocal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCatalogHasPinnedDefault(t *testing.T) {
	m, ok := Lookup("potion-code-16m-v2")
	if !ok {
		t.Fatal("default model missing from catalog")
	}
	if m.Repo != "minishlab/potion-code-16M-v2" || m.Revision != "e9d2a44ca6a05ac6685f3b23709ea57eb7352d5b" || m.Dims != 256 {
		t.Fatalf("catalog entry = %+v", m)
	}
	if strings.Join(m.Files, ",") != "config.json,tokenizer.json,model.safetensors" {
		t.Fatalf("files = %v", m.Files)
	}
}

func TestEnsureDownloadsOnceAndIsResumable(t *testing.T) {
	hits := 0
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if !strings.HasPrefix(r.URL.Path, "/minishlab/potion-code-16M-v2/resolve/e9d2a44ca6a05ac6685f3b23709ea57eb7352d5b/") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("payload:" + filepath.Base(r.URL.Path)))
	}))
	defer hub.Close()
	cache := t.TempDir()
	dir, err := Ensure(context.Background(), cache, "potion-code-16m-v2", hub.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hits != 3 {
		t.Fatalf("expected 3 downloads, got %d", hits)
	}
	want := filepath.Join(cache, "minishlab--potion-code-16M-v2", "e9d2a44ca6a05ac6685f3b23709ea57eb7352d5b")
	if dir != want {
		t.Fatalf("dir = %s, want %s", dir, want)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "tokenizer.json")); string(b) != "payload:tokenizer.json" {
		t.Fatalf("tokenizer.json content = %q", b)
	}
	// Second call: nothing to fetch.
	if _, err := Ensure(context.Background(), cache, "potion-code-16m-v2", hub.URL, nil); err != nil || hits != 3 {
		t.Fatalf("second ensure: err=%v hits=%d", err, hits)
	}
	// Remove one file: only it is re-fetched.
	_ = os.Remove(filepath.Join(dir, "config.json"))
	if _, err := Ensure(context.Background(), cache, "potion-code-16m-v2", hub.URL, nil); err != nil || hits != 4 {
		t.Fatalf("partial ensure: err=%v hits=%d", err, hits)
	}
	if _, err := Ensure(context.Background(), cache, "nope", hub.URL, nil); err == nil {
		t.Fatal("unknown model must error")
	}
}
