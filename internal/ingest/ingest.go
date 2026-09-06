// Package ingest loads external documents into source-aware memory
// documents (task I02). Loaders only normalize bytes into sections;
// every write goes through memory.Store.WriteDocumentSource (task I01),
// so content-addressed chunk identity, delta ingest and the write-time
// defense path apply to loader output exactly as to hand-written source
// documents. There is no second write path.
//
// Built-in loaders are pure Go: plain text, Markdown, HTML and
// structured incident JSON. PDF extraction is delegated to an optional
// external adapter process (PDFLoader); the static binary stays fully
// useful without one. URL fetching is explicit (Spec.URL) and passes an
// SSRF guard that refuses private, loopback and link-local targets.
package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hypervisor-io/punk-records/internal/memory"
)

// DefaultMaxBytes bounds one input document (and one adapter's output).
const DefaultMaxBytes int64 = 16 << 20

// DefaultTimeout caps loading, fetching and external adapter runs.
const DefaultTimeout = 30 * time.Second

// IncidentMediaType is the structured incident JSON format: a single
// JSON object validated strictly by the incident loader.
const IncidentMediaType = "application/vnd.punk.incident+json"

// PDFMediaType selects the external-adapter PDF loader.
const PDFMediaType = "application/pdf"

// Spec describes one ingest request. Path and URL are mutually
// exclusive; URL is an explicit remote fetch and goes through the SSRF
// guard. Format forces a loader (text|markdown|html|incident-json|pdf or
// a media type); empty detects from the name, then content type, then
// bytes. MaxBytes/Timeout of zero use the package defaults.
type Spec struct {
	Path       string
	URL        string
	Format     string
	SourceID   string        // stable source identity owning the chunks (default: the URI)
	Revision   string        // caller's revision label (git SHA, etag)
	MaxBytes   int64         // size cap on input and adapter output
	Timeout    time.Duration // time cap on loading, fetching and adapters
	PDFAdapter []string      // external PDF extractor command; required for application/pdf

	fetcher *Fetcher // in-package test hook; nil builds a guarded fetcher
}

func (s Spec) maxBytes() int64 {
	if s.MaxBytes <= 0 {
		return DefaultMaxBytes
	}
	return s.MaxBytes
}

func (s Spec) timeout() time.Duration {
	if s.Timeout <= 0 {
		return DefaultTimeout
	}
	return s.Timeout
}

// LoadInput is one resolved document handed to a Loader.
type LoadInput struct {
	Name string // basename or URL path hint, for errors
	Body []byte // raw bytes, already size-capped
}

// Result is a loader's normalized output. Revision lets an extractor
// report the revision it read (used only when the caller gave none).
type Result struct {
	Sections []memory.DocumentSection
	Revision string
}

// A Loader normalizes one document format into source-aware sections.
// Loaders never write to memory and never touch the network or
// subprocesses themselves (PDFLoader is the documented exception: it
// runs the configured external adapter).
type Loader interface {
	MediaType() string
	Load(ctx context.Context, in LoadInput) (*Result, error)
}

// extMedia maps lowercase file extensions to loader media types.
var extMedia = map[string]string{
	".txt":      "text/plain",
	".text":     "text/plain",
	".log":      "text/plain",
	".md":       "text/markdown",
	".markdown": "text/markdown",
	".html":     "text/html",
	".htm":      "text/html",
	".json":     IncidentMediaType,
	".pdf":      PDFMediaType,
}

// contentMedia maps served content types to loader media types; anything
// else falls through to byte sniffing.
var contentMedia = map[string]string{
	"text/plain":       "text/plain",
	"text/markdown":    "text/markdown",
	"text/x-markdown":  "text/markdown",
	"text/html":        "text/html",
	"application/json": IncidentMediaType,
	IncidentMediaType:  IncidentMediaType,
	PDFMediaType:       PDFMediaType,
}

var pdfMagic = []byte("%PDF-")

// normalizeFormat resolves an explicit --format value to a media type.
func normalizeFormat(f string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(f)) {
	case "text", "txt", "plain", "text/plain":
		return "text/plain", nil
	case "markdown", "md", "text/markdown":
		return "text/markdown", nil
	case "html", "text/html":
		return "text/html", nil
	case "incident", "incident-json", "json", "application/json", IncidentMediaType:
		return IncidentMediaType, nil
	case "pdf", PDFMediaType:
		return PDFMediaType, nil
	}
	return "", fmt.Errorf("ingest: unknown format %q: supported loaders are text, markdown, html, incident-json and pdf (external adapter)", f)
}

// DetectMediaType picks the loader media type for one input. Priority:
// explicit format, then file extension, then served content type, then
// byte signature ("%PDF-", a JSON object, valid UTF-8 text). Anything
// else is a visible unsupported-format error.
func DetectMediaType(name, contentType string, body []byte, explicit string) (string, error) {
	if explicit != "" {
		return normalizeFormat(explicit)
	}
	if mt, ok := extMedia[strings.ToLower(filepath.Ext(name))]; ok {
		return mt, nil
	}
	if contentType != "" {
		if base, _, err := mime.ParseMediaType(contentType); err == nil {
			if mt, ok := contentMedia[strings.ToLower(base)]; ok {
				return mt, nil
			}
		}
	}
	if bytes.HasPrefix(body, pdfMagic) {
		return PDFMediaType, nil
	}
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '{' {
		return IncidentMediaType, nil
	}
	if utf8.Valid(body) {
		return "text/plain", nil
	}
	return "", fmt.Errorf("ingest: cannot detect the format of %q (no known extension, content type or byte signature): pass --format text|markdown|html|incident-json|pdf", name)
}

// LoaderForMediaType returns the built-in loader for a detected media
// type. PDF is intentionally absent: it needs the configured external
// adapter, so construct PDFLoader (or use Load, which wires it from
// Spec.PDFAdapter).
func LoaderForMediaType(mt string) (Loader, error) {
	switch mt {
	case "text/plain":
		return textLoader{}, nil
	case "text/markdown":
		return markdownLoader{}, nil
	case "text/html":
		return htmlLoader{}, nil
	case IncidentMediaType:
		return incidentLoader{}, nil
	case PDFMediaType:
		return nil, fmt.Errorf("ingest: application/pdf needs an external adapter: construct PDFLoader with Command, or pass punk ingest --pdf-adapter")
	}
	return nil, fmt.Errorf("ingest: unsupported media type %q: supported loaders are text/plain, text/markdown, text/html, %s and application/pdf (external adapter)", mt, IncidentMediaType)
}

// readLimited reads r fully, erroring when it exceeds max bytes.
func readLimited(r io.Reader, max int64) ([]byte, error) {
	if max <= 0 {
		max = DefaultMaxBytes
	}
	body, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("ingest: input exceeds max_bytes (%d)", max)
	}
	return body, nil
}

// Load resolves the input bytes, detects the format and runs the loader,
// returning the source-aware document ready for
// memory.Store.WriteDocumentSource. A whitespace-only input is refused:
// silently loading zero sections would reconcile the prefix's owned
// chunks down to none, so an empty document is a visible error, never a
// wipe. Load performs no memory writes.
func Load(ctx context.Context, spec Spec) (*memory.SourceDocument, error) {
	if spec.Path == "" && spec.URL == "" {
		return nil, errors.New("ingest: pass a file path or --url")
	}
	if spec.Path != "" && spec.URL != "" {
		return nil, errors.New("ingest: pass a file path or --url, not both")
	}
	ctx, cancel := context.WithTimeout(ctx, spec.timeout())
	defer cancel()

	var body []byte
	var name, contentType, uri string
	if spec.URL != "" {
		f := spec.fetcher
		if f == nil {
			f = NewFetcher(spec.maxBytes(), spec.timeout())
		}
		b, ct, err := f.Fetch(ctx, spec.URL)
		if err != nil {
			return nil, err
		}
		body, contentType, uri = b, ct, spec.URL
		name = "document"
		if u, err := url.Parse(spec.URL); err == nil {
			if base := path.Base(u.Path); base != "." && base != "/" && base != "" {
				name = base
			} else if u.Host != "" {
				name = u.Host
			}
		}
	} else {
		abs, err := filepath.Abs(spec.Path)
		if err != nil {
			return nil, fmt.Errorf("ingest: resolve %s: %w", spec.Path, err)
		}
		f, err := os.Open(spec.Path)
		if err != nil {
			return nil, fmt.Errorf("ingest: %w", err)
		}
		defer f.Close()
		if fi, err := f.Stat(); err == nil && fi.Size() > spec.maxBytes() {
			return nil, fmt.Errorf("ingest: %s exceeds max_bytes (%d)", spec.Path, spec.maxBytes())
		}
		body, err = readLimited(f, spec.maxBytes())
		if err != nil {
			return nil, err
		}
		name, uri = filepath.Base(abs), "file://"+abs
	}

	if len(bytes.TrimSpace(body)) == 0 {
		return nil, fmt.Errorf("ingest: %s: input is empty", name)
	}
	mt, err := DetectMediaType(name, contentType, body, spec.Format)
	if err != nil {
		return nil, err
	}
	var loader Loader
	if mt == PDFMediaType {
		loader = PDFLoader{Command: spec.PDFAdapter, Timeout: spec.timeout(), MaxBytes: spec.maxBytes()}
	} else {
		loader, err = LoaderForMediaType(mt)
		if err != nil {
			return nil, err
		}
	}
	res, err := loader.Load(ctx, LoadInput{Name: name, Body: body})
	if err != nil {
		return nil, err
	}
	if len(res.Sections) == 0 {
		return nil, fmt.Errorf("ingest: %s: loader %s produced no sections", name, mt)
	}
	rev := spec.Revision
	if rev == "" {
		rev = res.Revision
	}
	return &memory.SourceDocument{
		Source:   memory.DocumentSource{ID: spec.SourceID, URI: uri, Revision: rev, MediaType: mt},
		Sections: res.Sections,
	}, nil
}

// Ingest loads spec and writes it under prefix through I01's
// WriteDocumentSource: the single source-aware write path, with its
// delta ingest (unchanged chunks are never rewritten) and write-time
// defense. A failed load returns before any write, so an unloadable or
// unsupported document never leaves a partially ingested prefix.
func Ingest(ctx context.Context, s *memory.Store, ns, prefix, author string, spec Spec) (written, unchanged, removed, blocked int, err error) {
	doc, err := Load(ctx, spec)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return s.WriteDocumentSource(ctx, ns, prefix, *doc, author)
}
