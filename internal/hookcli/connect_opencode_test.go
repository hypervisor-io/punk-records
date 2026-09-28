package hookcli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// openCodeGoldenPlugin returns the EXACT byte-for-byte JS a fresh plugin
// file gets for serverURL "http://localhost:9090": the managed marker as
// the first line, the module export shape, all four hooks (event,
// tool.execute.after, chat.message, experimental.chat.system.transform),
// the shared inbox bridge splice, and the OpenCode-only diagnostics +
// restart-recovery splice. The golden lives in
// testdata/opencode_plugin_golden.js - a checked-in generated-source
// fixture produced by openCodePluginContent itself - and is still compared
// byte-for-byte, so a wrong hook name, a dropped field, or broken JS
// around a substitution point fails exactly as it did when the bytes were
// one inline literal. When the generator changes intentionally,
// regenerate the fixture rather than hand-editing it.
func openCodeGoldenPlugin(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "opencode_plugin_golden.js"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// goldenMismatchWindow prints a small window around the FIRST differing
// byte so a golden regression reads like a diff instead of a 2000-line
// %q dump.
func goldenMismatchWindow(got, want string) string {
	limit := len(got)
	if len(want) < limit {
		limit = len(want)
	}
	at := 0
	for at < limit && got[at] == want[at] {
		at++
	}
	lo := at - 150
	if lo < 0 {
		lo = 0
	}
	hi := at + 150
	clamp := func(s string) string {
		if hi > len(s) {
			return s[lo:]
		}
		return s[lo:hi]
	}
	return "first difference at byte " + itoa(at) + ":\n got: ..." + clamp(got) + "...\nwant: ..." + clamp(want) + "..."
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestConnectOpenCodeGoldenContent pins the exact bytes ConnectOpenCode
// writes for a fresh plugin file.
func TestConnectOpenCodeGoldenContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.js")
	changed, err := ConnectOpenCode(path, "http://localhost:9090")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := openCodeGoldenPlugin(t)
	if string(got) != want {
		t.Fatalf("golden mismatch (generated plugin differs from testdata/opencode_plugin_golden.js; regenerate the fixture if the generator changed intentionally):\n%s", goldenMismatchWindow(string(got), want))
	}
	if !strings.HasPrefix(string(got), openCodePluginMarker) {
		t.Fatalf("marker must be the first line, got: %q", got[:min(len(got), 80)])
	}
}

// TestConnectOpenCodeIdempotent verifies a re-run with identical inputs
// reports changed=false and does not rewrite the file.
func TestConnectOpenCodeIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.js")
	if changed, err := ConnectOpenCode(path, "http://localhost:9090"); err != nil || !changed {
		t.Fatal(changed, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectOpenCode(path, "http://localhost:9090")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("idempotent re-run must report changed=false")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("idempotent re-run must not rewrite the file")
	}
}

// TestConnectOpenCodeRefusesUnmanagedExisting verifies an existing plugin
// file without the managed marker on its first line is left completely
// untouched and refused with an error naming the path - overwriting a
// user's own hand-authored OpenCode plugin would destroy their content
// silently otherwise.
func TestConnectOpenCodeRefusesUnmanagedExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.js")
	original := []byte("// my own hand-authored opencode plugin\nexport const Mine = async () => ({})\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ConnectOpenCode(path, "http://localhost:9090")
	if err == nil {
		t.Fatal("expected error for unmanaged existing plugin file")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("expected error naming the path, got: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatalf("unmanaged file must be left untouched: got %q, want %q", after, original)
	}
}

// TestConnectOpenCodeUpdatesStaleManagedContent verifies a plugin file
// that DOES carry the managed marker (e.g. from a prior punk version, or a
// different serverURL baked into the PUNK_URL fallback) is updated in
// place rather than refused.
func TestConnectOpenCodeUpdatesStaleManagedContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.js")
	stale := openCodePluginMarker + "\nexport const Old = async () => { /* stale, mentions http://old:1 */ return {} }\n"
	if err := os.WriteFile(path, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectOpenCode(path, "http://localhost:9090")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), "http://old:1") {
		t.Fatalf("stale content must be replaced: %s", after)
	}
	if !strings.Contains(string(after), "http://localhost:9090") {
		t.Fatalf("expected new server URL baked in: %s", after)
	}
}

// TestConnectOpenCodeSymlinkedPluginStaysSymlink verifies that when the
// plugin path is a symlink (e.g. into a dotfiles repo), connecting updates
// the content the symlink points at in place rather than replacing the
// symlink with a plain file.
func TestConnectOpenCodeSymlinkedPluginStaysSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-punk-memory.js")
	if err := os.WriteFile(real, []byte(openCodePluginMarker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "punk-memory.js")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectOpenCode(link, "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("plugin symlink was replaced by a regular file")
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if target != real {
		t.Fatalf("symlink now points elsewhere: %s", target)
	}
	raw, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "PunkMemoryPlugin") {
		t.Fatalf("symlink target missing plugin content: %s", raw)
	}
}

// TestConnectOpenCodePreservesExistingFileMode verifies connecting against
// an existing plugin file does not widen its permissions.
func TestConnectOpenCodePreservesExistingFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.js")
	if _, err := ConnectOpenCode(path, "http://a:1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectOpenCode(path, "http://b:2"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("expected mode 0600 preserved, got %o", fi.Mode().Perm())
	}
}

// TestConnectOpenCodeCreatesParentDirs verifies ConnectOpenCode creates
// the plugin's parent directory tree (e.g. ~/.config/opencode/plugins/ or
// ./.opencode/plugins/, neither of which typically pre-exists) rather than
// requiring the caller to mkdir first.
func TestConnectOpenCodeCreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode", "plugins", "punk-memory.js")
	changed, err := ConnectOpenCode(path, "http://localhost:9090")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected plugin file to exist after parent dir creation: %v", err)
	}
}

// TestConnectOpenCodeEscapesHostileServerURL verifies a serverURL
// containing characters that are meaningful inside a JS string literal
// (a double quote, a backslash) is escaped rather than breaking out of the
// generated string literal - per .claude/rules/ai.md's "always
// unconditionally escape" rule. If node or bun is on PATH, the emitted
// file is additionally syntax-checked so a broken escape would fail loudly
// instead of merely "looking escaped" to a substring check.
func TestConnectOpenCodeEscapesHostileServerURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.js")
	hostile := `http://evil","x":"pwned`
	if _, err := ConnectOpenCode(path, hostile); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, `\"x\":\"pwned`) {
		t.Fatalf("expected hostile quotes to be escaped, got: %s", s)
	}
	// The fallback expression must still open with exactly one unescaped
	// quote before the hostile payload - i.e. the payload landed INSIDE
	// the string literal, not appended after it as extra JS tokens.
	if !strings.Contains(s, `(fromEnv || "http://evil\"`) {
		t.Fatalf("hostile URL did not stay inside the intended string literal: %s", s)
	}
	runJSSyntaxCheck(t, path, got)
}

// TestOpenCodePluginPassesJSSyntaxCheck is a lightweight sanity check that
// the emitted plugin is syntactically valid JavaScript, using whichever of
// node/bun is available in the test environment; skipped when neither is
// on PATH.
func TestOpenCodePluginPassesJSSyntaxCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.js")
	if _, err := ConnectOpenCode(path, "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	runJSSyntaxCheck(t, path, got)
}

// runJSSyntaxCheck syntax-checks content with node (preferred) or bun,
// t.Skip-ing when neither is on PATH. node's CommonJS-by-default loader
// rejects top-level "export" outside a module context even under
// --check, so content is copied to a sibling .mjs file (unambiguously ESM
// by extension) rather than checked at its original .js path; bun infers
// ESM from syntax regardless of extension, so it checks the original path
// directly via "bun build".
func runJSSyntaxCheck(t *testing.T, path string, content []byte) {
	t.Helper()
	if nodePath, err := exec.LookPath("node"); err == nil {
		mjs := path + ".syntax-check.mjs"
		if err := os.WriteFile(mjs, content, 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(nodePath, "--check", mjs).CombinedOutput()
		if err != nil {
			t.Fatalf("node --check %s failed: %v\n%s", mjs, err, out)
		}
		return
	}
	if bunPath, err := exec.LookPath("bun"); err == nil {
		outDir := path + ".syntax-check-out"
		out, err := exec.Command(bunPath, "build", path, "--outdir", outDir).CombinedOutput()
		if err != nil {
			t.Fatalf("bun build %s failed: %v\n%s", path, err, out)
		}
		return
	}
	t.Skip("neither node nor bun on PATH; skipping JS syntax sanity check")
}

// TestConnectOpenCodeEscapesLineAndParagraphSeparators is finding #6's
// pinning test: jsStringLiteral's U+2028/U+2029 re-escape branch
// (opencode_plugin.go) had no test exercising an ACTUAL literal separator
// character reaching it - the hostile-quote test above never touches that
// code path. A serverURL containing a raw U+2028/U+2029 must come out of
// jsStringLiteral escaped (as the six-character  /  sequence),
// never as the raw three-byte UTF-8 separator, since older bundlers/linters
// still choke on an unescaped one inside a JS string literal.
func TestConnectOpenCodeEscapesLineAndParagraphSeparators(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.js")
	hostile := "http://evil.example/ mid end"
	if _, err := ConnectOpenCode(path, hostile); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if strings.ContainsRune(s, ' ') || strings.ContainsRune(s, ' ') {
		t.Fatalf("raw U+2028/U+2029 must never reach the emitted JS unescaped: %q", s)
	}
	if !strings.Contains(s, "\\u2028mid\\u2029end") {
		t.Fatalf("expected the escaped \\u2028/\\u2029 sequences in the fallback string literal, got: %s", s)
	}
	runJSSyntaxCheck(t, path, got)
}

// TestOpenCodePunkFetchAbortsStalledResponseBody is finding #1's pinning
// test: before the fix, punkFetch cleared its AbortController's timer in
// "finally" right after fetch() itself resolved (i.e. once response
// headers arrive), leaving the subsequent `await res.json()` body read
// completely unbounded. A server that sends 200 headers and then never
// writes/closes the body would hang the awaited
// "experimental.chat.system.transform" hook forever.
//
// This drives the ACTUAL rendered plugin (not a hand-copied reimplementation
// of punkFetch) through a small node harness: a local httptest.Server
// answers /v1/agent/context with headers-then-silence (flushed 200, then
// blocks on the request context so it releases the connection the moment
// the client aborts), and the harness invokes the plugin's
// "experimental.chat.system.transform" hook and times how long the
// returned promise takes to settle. t.Skip when node is not on PATH - bun's
// AbortController/fetch timing behavior isn't pinned here, only node's.
func TestOpenCodePunkFetchAbortsStalledResponseBody(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping punkFetch abort runtime test")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agent/context", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Stall the body indefinitely - never write another byte - until
		// the client disconnects (the plugin's AbortController fires) or
		// the test server shuts down, whichever happens first. Selecting
		// on the request's own context (canceled the moment the client
		// aborts/closes the connection) is what lets httptest.Server.Close
		// return promptly instead of blocking on this handler forever.
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.js")
	if _, err := ConnectOpenCode(path, srv.URL); err != nil {
		t.Fatal(err)
	}
	pluginSrc, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// The driver is appended to the actual rendered plugin source in one
	// .mjs file (unambiguously ESM by extension, see runJSSyntaxCheck's own
	// doc comment for why that matters to node), so this exercises the
	// exact bytes ConnectOpenCode writes, not a paraphrase of them.
	driver := `
async function main() {
  const start = Date.now()
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-abort-test-project" })
  const output = { system: [] }
  await hooks["experimental.chat.system.transform"]({ sessionID: "abort-test-session" }, output)
  const elapsedMs = Date.now() - start
  console.log("elapsed_ms=" + elapsedMs)
  if (elapsedMs > 3500) {
    console.error("FAIL: transform hook took " + elapsedMs + "ms to settle, expected the 2s abort to fire well under 4000ms")
    process.exit(1)
  }
  process.exit(0)
}
main().catch((err) => {
  console.error("FAIL: transform hook rejected instead of resolving:", err)
  process.exit(1)
})
`
	harnessPath := path + ".abort-harness.mjs"
	if err := os.WriteFile(harnessPath, append(pluginSrc, []byte(driver)...), 0o644); err != nil {
		t.Fatal(err)
	}

	// A hard outer deadline well above the expected ~2s abort: if the fix
	// regresses back to an unbounded body read, the harness would hang
	// past this deadline and the killed process fails the test with a
	// clear "did not complete" signal rather than blocking `go test`
	// forever.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	start := time.Now()
	out, err := exec.CommandContext(ctx, nodePath, harnessPath).CombinedOutput()
	elapsed := time.Since(start)

	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("node harness did not complete within the 8s hard deadline (abort not honored - unbounded body read): elapsed=%s\n%s", elapsed, out)
	}
	if err != nil {
		t.Fatalf("node harness failed: %v (elapsed=%s)\n%s", err, elapsed, out)
	}
	if elapsed >= 4*time.Second {
		t.Fatalf("transform hook took %s wall-clock to settle from the Go side too, expected well under 4s (abort fired around 2s): %s", elapsed, out)
	}
}
