package hookcli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const paritySavedKey = "synthetic-parity-saved-key"
const parityExplicitKey = "synthetic-parity-explicit-key"

type credentialParityResult struct {
	Enabled    bool   `json:"enabled"`
	Error      bool   `json:"error"`
	URL        string `json:"url"`
	HasKey     bool   `json:"hasKey"`
	KeySource  string `json:"keySource"`
	Mismatch   bool   `json:"mismatch"`
	RequestURL string `json:"requestURL"`
}

func parityConnection(url, keySource string, mismatch bool) credentialParityResult {
	if url == "" {
		return credentialParityResult{Error: true, KeySource: "none"}
	}
	return credentialParityResult{
		Enabled: true, URL: url, HasKey: keySource != "none", KeySource: keySource,
		Mismatch: mismatch, RequestURL: url + "/v1/agent/hooks",
	}
}

// CREDS-2/3/4 regression: compare the real Go resolver with the unmodified
// credentialsJS fragment emitted into all three plugins, executed as Node ESM.
// Each side reads the SAME synthetic file under the SAME isolated home. Expected
// results are independent contract examples, not outputs derived from either
// implementation. Request constructors additionally catch WHATWG repairs after
// resolution; no network is needed for these reserved-domain/IP examples.
// codeops:trace repo=punk-records work_item=punk-connect-remote-url-review-20261009 spec=docs/client-credentials.md test=TestCredentialsGoJSParity evidence=docs/superpowers/reports/2026-10-09-client-credentials/parity-correction.md
func TestCredentialsGoJSParity(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; Go/emitted-JS credential parity requires Node ESM")
	}
	urls := []struct{ name, raw, canonical string }{
		// The independent QA's exact 35-input sweep, including its nine mismatches.
		{"qa01-host-case", "http://Example.TEST:9090/Base", "http://example.test:9090/Base"},
		{"qa02-http-default", "http://example.test:80/", "http://example.test"},
		{"qa03-https-default", "https://example.test:443/x/", "https://example.test/x"},
		{"qa04-zero-default", "http://example.test:080/", "http://example.test"},
		{"qa05-ipv6-compression", "http://[::0001]:9090", "http://[::1]:9090"},
		{"qa06-ipv6", "http://[::1]", "http://[::1]"},
		{"qa07-ipv4-shorthand", "http://127.1", ""},
		{"qa08-ipv4-hex", "http://0x7f.0.0.1", ""},
		{"qa09-unicode-host", "http://exämple.test", ""},
		{"qa10-parent-segment", "http://example.test/a/../b", ""},
		{"qa11-dot-segment", "http://example.test/a/./b", ""},
		{"qa12-upper-escape", "http://example.test/%7Ea", "http://example.test/%7Ea"},
		{"qa13-lower-escape", "http://example.test/%7ea", "http://example.test/%7ea"},
		{"qa14-escaped-slash", "http://example.test/a%2Fb", "http://example.test/a%2Fb"},
		{"qa15-path-space", "http://example.test/a b", ""},
		{"qa16-userinfo", "http://user@example.test", ""},
		{"qa17-password", "http://:pw@example.test", ""},
		{"qa18-port-overflow", "http://example.test:99999", ""},
		{"qa19-empty-port", "http://example.test:", ""},
		{"qa20-zero-port", "http://example.test:0", ""},
		{"qa21-query", "http://example.test?x=1", ""},
		{"qa22-fragment", "http://example.test#f", ""},
		{"qa23-empty-query", "http://example.test?", ""},
		{"qa24-empty-fragment", "http://example.test#", ""},
		{"qa25-empty-authority", "http:///example.test", ""},
		{"qa26-scheme-case", "HTTP://EXAMPLE.TEST", "http://example.test"},
		{"qa27-backslash", `http://example.test\x`, ""},
		{"qa28-invalid-escape", "http://example.test/%zz", ""},
		{"qa29-ftp", "ftp://example.test", ""},
		{"qa30-trailing-slashes", "http://example.test////", "http://example.test"},
		{"qa31-relative", "//example.test", ""},
		{"qa32-host-space", "http://exam ple.test", ""},
		{"qa33-ipv4-mapped", "http://[::ffff:127.0.0.1]", "http://[::ffff:7f00:1]"},
		{"qa34-mounted-base", "http://example.test:9090/base/", "http://example.test:9090/base"},
		{"qa35-root-dot", "http://example.test./x", "http://example.test./x"},
		{"double-zero-default", "http://example.test:0080", "http://example.test"},
		{"zero-https-default", "https://example.test:00443/base", "https://example.test/base"},
		{"zero-nondefault", "http://example.test:00081/base", "http://example.test:81/base"},
		{"many-zeroes-port", "http://example.test:00000000000000000000000000000000080", "http://example.test"},
		{"maximum-port", "https://example.test:065535", "https://example.test:65535"},
		{"minimum-port", "http://example.test:00001", "http://example.test:1"},
		{"all-zero-port", "http://example.test:000", ""},
		{"huge-port", "http://example.test:99999999999999999999999999999999999", ""},
		{"signed-port", "http://example.test:+80", ""},
		{"ipv4", "http://127.0.0.1:080/base", "http://127.0.0.1/base"},
		{"ipv4-empty-port", "http://127.0.0.1:", ""},
		{"ipv6-empty-port", "http://[::1]:", ""},
		{"mapped-empty-port", "http://[::ffff:127.0.0.1]:", ""},
		{"ipv6-expanded", "HTTPS://[2001:0DB8:0000:0000:0000:0000:0000:0001]:00443/base/", "https://[2001:db8::1]/base"},
		{"ipv6-zero-tie", "http://[1:0:0:2:0:0:3:4]", "http://[1::2:0:0:3:4]"},
		{"ipv6-all-zero", "http://[0:0:0:0:0:0:0:0]", "http://[::]"},
		{"mapped-expanded", "http://[0:0:0:0:0:ffff:127.0.0.1]", "http://[::ffff:7f00:1]"},
		{"mapped-zero", "http://[::ffff:0.0.0.1]", "http://[::ffff:0:1]"},
		{"embedded-ipv4", "http://[2001:db8::192.0.2.1]", "http://[2001:db8::c000:201]"},
		{"unbracketed-ipv6", "http://::1", ""},
		{"bracketed-ipv4", "http://[127.0.0.1]", ""},
		{"ipv6-zone", "http://[fe80::1%25eth0]", ""},
		{"ipv4-integer", "http://2130706433", ""},
		{"ipv4-octal", "http://0177.0.0.1", ""},
		{"ipv4-leading-zero", "http://127.0.00.1", ""},
		{"ipv4-root-dot", "http://127.0.0.1.", ""},
		{"numeric-final-label", "http://example.123", ""},
		{"hex-final-label", "http://example.0xabc", ""},
		{"empty-hex-host", "http://0x", ""},
		{"nonnumeric-label", "http://example.123abc", "http://example.123abc"},
		{"nonhex-label", "http://example.0xg", "http://example.0xg"},
		{"punycode", "HTTPS://XN--EXMPLE-CUA.TEST/base", "https://xn--exmple-cua.test/base"},
		{"double-hyphen-label", "http://ab--cd.test", "http://ab--cd.test"},
		{"localhost", "http://LOCALHOST:9090", "http://localhost:9090"},
		{"escaped-host", "http://%65xample.test", ""},
		{"empty-label", "http://example..test", ""},
		{"underscore-label", "http://my_host.test", ""},
		{"leading-hyphen", "http://-example.test", ""},
		{"trailing-hyphen", "http://example-.test", ""},
		{"nbsp-host", "http://exam\u00a0ple.test", ""},
		{"unicode-dot", "http://example\u3002test", ""},
		{"leading-space", " http://example.test", ""},
		{"trailing-space", "http://example.test ", ""},
		{"interior-tab", "http://example.test/a\tb", ""},
		{"interior-lf", "http://example.test/a\nb", ""},
		{"interior-cr", "http://example.test/a\rb", ""},
		{"interior-nbsp", "http://example.test/a\u00a0b", ""},
		{"interior-nel", "http://example.test/a\u0085b", ""},
		{"control", "http://example.test/a\x00b", ""},
		{"delete-control", "http://example.test/a\x7fb", ""},
		{"unicode-path", "http://example.test/café", ""},
		{"encoded-unicode-path", "http://example.test/caf%C3%A9", "http://example.test/caf%C3%A9"},
		{"encoded-space", "http://example.test/a%20b", "http://example.test/a%20b"},
		{"encoded-dot", "http://example.test/a/%2e/b", ""},
		{"encoded-parent", "http://example.test/a/%2e%2E/b", ""},
		{"mixed-parent-left", "http://example.test/a/.%2e/b", ""},
		{"mixed-parent-right", "http://example.test/a/%2E./b", ""},
		{"terminal-dot", "http://example.test/a/.", ""},
		{"terminal-parent", "http://example.test/a/..", ""},
		{"dot-containing-segment", "http://example.test/a/.../b.v1", "http://example.test/a/.../b.v1"},
		{"encoded-dot-in-name", "http://example.test/a%2eb", "http://example.test/a%2eb"},
		{"double-escaped-dot", "http://example.test/%252e", "http://example.test/%252e"},
		{"interior-slashes", "http://example.test/a//b///", "http://example.test/a//b"},
		{"authority-shaped-path", "http://example.test//other.test/base", "http://example.test//other.test/base"},
		{"encoded-authority-path", "http://example.test/%2F%2Fother.test", "http://example.test/%2F%2Fother.test"},
		{"rfc3986-path", "http://example.test/a-._~!$&'()*+,;=:@/b", "http://example.test/a-._~!$&'()*+,;=:@/b"},
		{"path-brackets", "http://example.test/a[b]", ""},
		{"path-pipe", "http://example.test/a|b", ""},
		{"path-caret", "http://example.test/a^b", ""},
		{"path-quote", "http://example.test/a\"b", ""},
		{"empty-userinfo", "http://@example.test", ""},
	}
	type parityCase struct {
		name, selected, installed, envKey, file string
		defaultFile, directory                  bool
		want                                    credentialParityResult
	}
	savedFile := func(rawURL string) string {
		b, err := json.Marshal(Credentials{URL: rawURL, APIKey: paritySavedKey})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	var cases []parityCase
	for _, u := range urls {
		selected := parityCase{name: u.name + "/selected", selected: u.raw, want: parityConnection(u.canonical, "none", false)}
		if strings.ContainsRune(u.raw, 0) {
			// NUL cannot be represented in an OS environment; exercise the
			// explicit argument on both real resolvers instead.
			selected.selected, selected.installed = "", u.raw
		}
		cases = append(cases,
			selected,
			parityCase{name: u.name + "/saved", file: savedFile(u.raw), want: parityConnection(u.canonical, "saved", false)},
		)
	}
	for _, pair := range []struct {
		name, saved, selected, canonical string
		match                            bool
	}{
		{"http-zero-port", "http://example.test:0080/base/", "http://example.test/base", "http://example.test/base", true},
		{"http-zero-port-reverse", "http://example.test/base", "http://example.test:080/base/", "http://example.test/base", true},
		{"https-zero-port", "https://example.test:00443", "HTTPS://EXAMPLE.TEST/", "https://example.test", true},
		{"nondefault-zero-port", "http://example.test:00081", "http://example.test:81", "http://example.test:81", true},
		{"different-port", "http://example.test:81", "http://example.test:82", "http://example.test:82", false},
		{"different-scheme", "https://example.test/base", "http://example.test/base", "http://example.test/base", false},
		{"ipv6-compression", "http://[0:0:0:0:0:0:0:1]:0080/base", "http://[::1]/base", "http://[::1]/base", true},
		{"mapped-ipv6", "http://[::ffff:127.0.0.1]/base", "http://[::ffff:7f00:1]/base", "http://[::ffff:7f00:1]/base", true},
		{"mapped-is-not-ipv4", "http://[::ffff:127.0.0.1]", "http://127.0.0.1", "http://127.0.0.1", false},
		{"punycode-case", "https://XN--EXMPLE-CUA.TEST/base/", "https://xn--exmple-cua.test/base", "https://xn--exmple-cua.test/base", true},
		{"root-dot-distinct", "http://example.test./base", "http://example.test/base", "http://example.test/base", false},
		{"different-host", "http://first.test/base", "http://second.test/base", "http://second.test/base", false},
		{"path-trailing-slashes", "http://example.test/a/b///", "http://example.test/a/b", "http://example.test/a/b", true},
		{"path-prefix-boundary", "http://example.test/a", "http://example.test/ab", "http://example.test/ab", false},
		{"path-case", "http://example.test/Base", "http://example.test/base", "http://example.test/base", false},
		{"encoded-slash-boundary", "http://example.test/a%2Fb", "http://example.test/a/b", "http://example.test/a/b", false},
		{"escape-spelling", "http://example.test/%7Ea", "http://example.test/%7ea", "http://example.test/%7ea", false},
		{"encoded-vs-literal", "http://example.test/%7Ea", "http://example.test/~a", "http://example.test/~a", false},
		{"interior-slashes-distinct", "http://example.test/a//b", "http://example.test/a/b", "http://example.test/a/b", false},
		{"authority-path-distinct", "http://example.test//other.test", "http://other.test", "http://other.test", false},
		{"dot-selected", "http://example.test/b", "http://example.test/a/../b", "", false},
		{"dot-saved", "http://example.test/a/../b", "http://example.test/b", "", false},
		{"unicode-selected", "http://xn--exmple-cua.test", "http://exämple.test", "", false},
		{"unicode-saved", "http://exämple.test", "http://xn--exmple-cua.test", "", false},
		{"shorthand-selected", "http://127.0.0.1", "http://127.1", "", false},
		{"shorthand-saved", "http://127.1", "http://127.0.0.1", "", false},
	} {
		source := "none"
		if pair.match {
			source = "saved"
		}
		want := parityConnection(pair.canonical, source, !pair.match)
		cases = append(cases, parityCase{name: "pair/" + pair.name, file: savedFile(pair.saved), selected: pair.selected, want: want})
		// An installation snapshot and a CLI hook's explicit URL share the same
		// key boundary when there is no runtime environment override.
		cases = append(cases, parityCase{name: "installed/" + pair.name, file: savedFile(pair.saved), installed: pair.selected, want: want})
	}
	invalid := parityConnection("", "none", false)
	for name, raw := range map[string]string{
		"json": "{", "null": "null", "array": "[]", "scalar": `"synthetic"`, "empty": " ",
		"trailing-json": `{"url":"http://example.test"}{}`, "missing-url": `{"api_key":"synthetic"}`,
		"null-url": `{"url":null}`, "number-url": `{"url":7}`, "empty-url": `{"url":""}`,
		"null-key":    `{"url":"http://example.test","api_key":null}`,
		"number-key":  `{"url":"http://example.test","api_key":7}`,
		"array-key":   `{"url":"http://example.test","api_key":[]}`,
		"object-key":  `{"url":"http://example.test","api_key":{}}`,
		"boolean-key": `{"url":"http://example.test","api_key":false}`,
	} {
		cases = append(cases, parityCase{name: "file/" + name, file: raw, want: invalid})
	}
	cases = append(cases,
		parityCase{name: "file/absent", want: parityConnection("http://localhost:9090", "none", false)},
		parityCase{name: "file/unreadable-directory", directory: true, want: invalid},
		parityCase{name: "file/default-home", defaultFile: true, file: savedFile("https://example.test/base/"), want: parityConnection("https://example.test/base", "saved", false)},
		parityCase{name: "file/empty-key", file: `{"url":"http://example.test","api_key":""}`, want: parityConnection("http://example.test", "none", false)},
		parityCase{name: "file/omitted-key", file: `{"url":"http://example.test"}`, want: parityConnection("http://example.test", "none", false)},
		parityCase{name: "explicit/bypass-invalid-file", selected: "http://example.test:0080/base", envKey: parityExplicitKey, file: "invalid-synthetic-file", want: parityConnection("http://example.test/base", "explicit", false)},
		parityCase{name: "explicit/bypass-null-key", selected: "http://example.test", envKey: parityExplicitKey, file: `{"url":"http://example.test","api_key":null}`, want: parityConnection("http://example.test", "explicit", false)},
		parityCase{name: "explicit/key-only-invalid-file", envKey: parityExplicitKey, file: "invalid-synthetic-file", want: invalid},
		parityCase{name: "explicit/invalid-url-with-key", selected: "http://127.0.0.1:", envKey: parityExplicitKey, file: "unused-synthetic-file", want: invalid},
		parityCase{name: "explicit/key-wins", envKey: parityExplicitKey, file: savedFile("https://example.test/base"), want: parityConnection("https://example.test/base", "explicit", false)},
	)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			defaultPath := filepath.Join(home, ".punk", "credentials.json")
			if err := os.MkdirAll(filepath.Dir(defaultPath), 0o700); err != nil {
				t.Fatal(err)
			}
			// A custom selection must not read the invalid default decoy.
			if err := os.WriteFile(defaultPath, []byte("default-file-must-not-be-read"), 0o600); err != nil {
				t.Fatal(err)
			}
			file, envFile := filepath.Join(home, "selected.json"), filepath.Join(home, "selected.json")
			if tc.defaultFile {
				file, envFile = defaultPath, ""
			}
			if tc.directory {
				if err := os.Mkdir(file, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if tc.file != "" {
				if err := os.WriteFile(file, []byte(tc.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("PUNK_CREDENTIALS", envFile)
			t.Setenv("PUNK_URL", tc.selected)
			t.Setenv("PUNK_API_KEY", tc.envKey)
			resolved, resolveErr := ResolveServer(tc.installed)
			goResult := credentialParityResult{
				Enabled: resolveErr == nil, Error: resolveErr != nil, URL: resolved.URL,
				HasKey: resolved.APIKey != "", KeySource: "none", Mismatch: resolved.Diagnostic != "",
			}
			switch resolved.APIKey {
			case "":
			case paritySavedKey:
				goResult.KeySource = "saved"
			case parityExplicitKey:
				goResult.KeySource = "explicit"
			default:
				t.Fatal("Go returned an unexpected key (value suppressed)")
			}
			if resolveErr != nil {
				switch resolveErr.Error() {
				case "invalid server URL", "invalid credentials file", "cannot read credentials file":
				default:
					t.Fatal("Go returned an unexpected unsanitized error (value suppressed)")
				}
			} else {
				req, err := http.NewRequest(http.MethodPost, resolved.URL+"/v1/agent/hooks", nil)
				if err != nil {
					t.Error("Go canonical URL cannot construct an HTTP request")
				} else {
					goResult.RequestURL = req.URL.String()
				}
			}
			if resolved.Diagnostic != "" && resolved.Diagnostic != savedKeyMismatchDiagnostic {
				t.Fatal("Go returned an unexpected mismatch diagnostic (value suppressed)")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, node, "--input-type=module", "-e", credentialsJS+credentialParityJS)
			cmd.Dir = home
			installedJSON, err := json.Marshal(tc.installed)
			if err != nil {
				t.Fatal(err)
			}
			cmd.Env = pluginNodeEnv(home, map[string]string{
				"PUNK_CREDENTIALS": envFile, "PUNK_URL": tc.selected, "PUNK_API_KEY": tc.envKey,
				"PUNK_PARITY_INSTALLED": string(installedJSON), "PUNK_PARITY_FILE": file,
			})
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("Node ESM parity runner failed: %v\n%s", err, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatal("Node emitted an uncaptured diagnostic (content suppressed)")
			}
			var jsResult credentialParityResult
			if err := json.Unmarshal(stdout.Bytes(), &jsResult); err != nil {
				t.Fatalf("invalid Node observation: %v", err)
			}
			if goResult != jsResult {
				t.Errorf("Go != actual emitted JS: Go=%+v JS=%+v", goResult, jsResult)
			}
			if goResult != tc.want {
				t.Errorf("Go violates shared contract: got=%+v want=%+v", goResult, tc.want)
			}
			if jsResult != tc.want {
				t.Errorf("JS violates shared contract: got=%+v want=%+v", jsResult, tc.want)
			}
		})
	}
	t.Logf("corpus: %d URL inputs x selected/saved, 26 saved-vs-selected pairs x runtime/installed, 25 file/override cases = %d real Go/Node comparisons", len(urls), len(cases))
}

// This runner does not copy either canonicalizer. It imports no external
// packages, guards credential reads, calls the emitted resolver, and observes
// the exact request serialization without opening a socket. Only key presence
// and synthetic key identity labels ever leave the child process.
const credentialParityJS = `
import assert from "node:assert/strict";
import fs from "node:fs";
import { syncBuiltinESMExports } from "node:module";
assert.equal(process.env.HOME, process.env.USERPROFILE);
const originalRead = fs.readFileSync;
const readViolations = [];
fs.readFileSync = (file, ...args) => {
  if (String(file) !== process.env.PUNK_PARITY_FILE) {
    readViolations.push("read escaped selected synthetic credential file");
    throw new Error("test read guard");
  }
  return originalRead(file, ...args);
};
syncBuiltinESMExports();
let fetchCalls = 0;
globalThis.fetch = () => { fetchCalls++; throw new Error("parity test must not use network"); };
const diagnostics = [];
console.error = (...args) => diagnostics.push(args.map(String).join(" "));
const connection = punkResolveConnection(JSON.parse(process.env.PUNK_PARITY_INSTALLED));
assert.deepEqual(readViolations, []);
assert.equal(fetchCalls, 0);
const mismatch = connection.enabled && diagnostics.length !== 0;
const expectedDiagnostics = !connection.enabled
  ? ["[punk] invalid connection credentials or server URL; Punk network activity is disabled. Correct saved credentials or set PUNK_URL and PUNK_API_KEY, then restart the client."]
  : mismatch
    ? ["[punk] saved credentials do not match the selected server; saved key ignored. Reconnect and restart, or set PUNK_API_KEY explicitly."] : [];
assert.deepEqual(diagnostics, expectedDiagnostics, "exact sanitized diagnostic");
let keySource = "none";
if (connection.apiKey === "synthetic-parity-saved-key") keySource = "saved";
else if (connection.apiKey === "synthetic-parity-explicit-key") keySource = "explicit";
else assert.equal(Boolean(connection.apiKey), false, "unexpected key (value suppressed)");
process.stdout.write(JSON.stringify({
  enabled: connection.enabled, error: !connection.enabled, url: connection.url,
  hasKey: Boolean(connection.apiKey), keySource, mismatch,
  requestURL: connection.enabled ? new Request(connection.url + "/v1/agent/hooks").url : "",
}));
`
