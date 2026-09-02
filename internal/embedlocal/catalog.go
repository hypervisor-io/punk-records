package embedlocal

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Model is one pinned static embedding model. Revision is a commit SHA so
// the same name always resolves to the same bytes.
type Model struct {
	Name     string
	Repo     string
	Revision string
	Dims     int
	Files    []string
}

// Catalog lists the models punk knows how to fetch and load.
var Catalog = map[string]Model{
	"potion-code-16m-v2": {
		Name:     "potion-code-16m-v2",
		Repo:     "minishlab/potion-code-16M-v2",
		Revision: "e9d2a44ca6a05ac6685f3b23709ea57eb7352d5b",
		Dims:     256,
		Files:    []string{"config.json", "tokenizer.json", "model.safetensors"},
	},
}

// DefaultModel is used when ai.embeddings.provider is local and model is empty.
const DefaultModel = "potion-code-16m-v2"

// Lookup finds a catalog entry by name.
func Lookup(name string) (Model, bool) {
	m, ok := Catalog[name]
	return m, ok
}

// DefaultCacheDir is $PUNK_MODEL_CACHE or $HOME/.punk/models.
func DefaultCacheDir() string {
	if v := os.Getenv("PUNK_MODEL_CACHE"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".punk", "models")
	}
	return filepath.Join(home, ".punk", "models")
}

const defaultHub = "https://huggingface.co"

// Dir is where Ensure places a model's files.
func Dir(cacheDir string, m Model) string {
	return filepath.Join(cacheDir, strings.ReplaceAll(m.Repo, "/", "--"), m.Revision)
}

// Ensure downloads any missing catalog file for name into cacheDir and
// returns the model directory. Files are written to a temporary name and
// renamed into place, so an interrupted download never leaves a partial
// file that a later Load would trust. progress may be nil.
func Ensure(ctx context.Context, cacheDir, name, hub string, progress io.Writer) (string, error) {
	m, ok := Lookup(name)
	if !ok {
		return "", fmt.Errorf("embedlocal: unknown model %q (known: %s)", name, strings.Join(names(), ", "))
	}
	if hub == "" {
		hub = defaultHub
	}
	dir := Dir(cacheDir, m)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	for _, f := range m.Files {
		dst := filepath.Join(dir, f)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		url := hub + "/" + m.Repo + "/resolve/" + m.Revision + "/" + f
		if progress != nil {
			fmt.Fprintf(progress, "downloading %s\n", url)
		}
		if err := download(ctx, client, url, dst); err != nil {
			return "", fmt.Errorf("embedlocal: %s: %w", f, err)
		}
	}
	return dir, nil
}

func download(ctx context.Context, client *http.Client, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	tmp := dst + fmt.Sprintf(".part-%d", os.Getpid())
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func names() []string {
	out := make([]string, 0, len(Catalog))
	for n := range Catalog {
		out = append(out, n)
	}
	return out
}
