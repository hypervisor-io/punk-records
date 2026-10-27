package hookcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Credentials is what a machine needs to reach one punk server: the URL
// and, when the server has API keys enabled, a bearer token. Stored at
// CredentialsPath with mode 0600 so no key ever has to sit in a shared
// settings file or a shell profile.
type Credentials struct {
	URL    string `json:"url"`
	APIKey string `json:"api_key,omitempty"`
}

const defaultServerURL = "http://localhost:9090"

const savedKeyMismatchDiagnostic = "saved credentials do not match the selected server; saved key ignored; reconnect or set PUNK_API_KEY explicitly"

// cleartextSavedKeyError is the refusal a SAVED bearer token earns when the
// selected server is plain http on a host that is not loopback: the token
// would cross the network in the clear on every future invocation, silently,
// because a saved key is a stored default rather than a per-invocation
// decision. It is deliberately sanitized - no URL, host or key value appears
// - and it is the one resolution failure a user can clear by switching to
// https or by passing PUNK_API_KEY explicitly (an explicit key is the
// operator's own choice for this run, so it is never refused).
const cleartextSavedKeyError = "refusing to send a saved API key over plain http to a non-loopback host; use https, or set PUNK_API_KEY explicitly for this host"

// isCleartextRemote reports whether canonicalURL is plain http to a host
// that is not loopback. https is never cleartext, and http to localhost /
// 127.0.0.0/8 / ::1 never leaves the machine, so both stay allowed.
func isCleartextRemote(canonicalURL string) bool {
	u, err := url.Parse(canonicalURL)
	if err != nil || u.Scheme != "http" {
		return false
	}
	return !isLoopbackHost(u.Hostname())
}

// isLoopbackHost reports whether host names this machine's loopback
// interface: "localhost" (a single trailing root dot ignored, matching
// canonicalBaseURL's DNS handling), any 127.0.0.0/8 address, or ::1.
// canonicalBaseURL only ever emits a strict dotted-decimal IPv4, a DNS name
// or a bracketed-free IPv6 address, so those three shapes are exhaustive; a
// 4-in-6 mapped address is normalized to its hex IPv6 form and is NOT
// treated as loopback, which errs toward refusing.
func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" {
		return true
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		// A 4-in-6 mapped address is deliberately NOT loopback here.
		// canonicalBaseURL normalizes it to its hex IPv6 form precisely so
		// it stays a distinct identity from the plain IPv4 address (see the
		// "mapped-is-not-ipv4" parity case), and netip's IsLoopback would
		// otherwise unmap it. Treating it as a remote host errs toward
		// refusing the saved key, which is the safe direction.
		if ip.Is4In6() {
			return false
		}
		return ip.IsLoopback()
	}
	return false
}

// ServerResolution is one coherent URL/key pair selected for a command.
// Diagnostic is deliberately sanitized and never contains an input URL or key.
type ServerResolution struct {
	URL        string
	APIKey     string
	Diagnostic string
}

// CredentialsPath is $PUNK_CREDENTIALS or ~/.punk/credentials.json.
func CredentialsPath() string {
	if v := os.Getenv("PUNK_CREDENTIALS"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".punk", "credentials.json")
	}
	return filepath.Join(home, ".punk", "credentials.json")
}

// LoadCredentials reads path; a missing file is (zero, false, nil). A present
// file is strict: it must be one JSON object with a non-empty valid base URL
// and an optional string api_key. Errors never include file contents or URLs.
func LoadCredentials(path string) (Credentials, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Credentials{}, false, nil
	}
	if err != nil {
		return Credentials{}, false, errors.New("cannot read credentials file")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var fields map[string]json.RawMessage
	if err := dec.Decode(&fields); err != nil || fields == nil {
		return Credentials{}, false, errors.New("invalid credentials file")
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Credentials{}, false, errors.New("invalid credentials file")
	}
	urlRaw, ok := fields["url"]
	if !ok {
		return Credentials{}, false, errors.New("invalid credentials file")
	}
	var c Credentials
	if err := json.Unmarshal(urlRaw, &c.URL); err != nil || c.URL == "" {
		return Credentials{}, false, errors.New("invalid credentials file")
	}
	if keyRaw, ok := fields["api_key"]; ok {
		var key *string
		if err := json.Unmarshal(keyRaw, &key); err != nil || key == nil {
			return Credentials{}, false, errors.New("invalid credentials file")
		}
		c.APIKey = *key
	}
	canonical, err := canonicalBaseURL(c.URL)
	if err != nil {
		return Credentials{}, false, errors.New("invalid credentials file")
	}
	c.URL = canonical
	return c, true, nil
}

// SaveCredentials writes path atomically with mode 0600 (parent 0700).
func SaveCredentials(path string, c Credentials) error {
	canonical, err := canonicalBaseURL(c.URL)
	if err != nil {
		return errors.New("invalid server URL")
	}
	c.URL = canonical
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(raw, '\n'), 0o600)
}

var (
	baseURLPathChars = regexp.MustCompile(`^[a-zA-Z0-9._~!$&'()*+,;=:@/%-]*$`)
	baseURLDNSLabel  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	baseURLNumeric   = regexp.MustCompile(`^(?:[0-9]+|0x[0-9a-f]*)$`)
)

// canonicalBaseURL implements the supported base-URL subset in
// docs/client-credentials.md. Keep it aligned with punkCanonicalURL in
// credsjs.go: accepted URLs must survive both Go and WHATWG request parsing
// without changing the credential authority or path. In particular, reject
// ambiguous input instead of relying on the parsers' different repairs.
// codeops:trace repo=punk-records work_item=punk-connect-remote-url-review-20261009 spec=docs/client-credentials.md test=internal/hookcli/credentials_parity_test.go evidence=docs/superpowers/reports/2026-10-09-client-credentials/parity-correction.md
func canonicalBaseURL(raw string) (string, error) {
	if raw == "" || strings.ContainsAny(raw, `\?#`) {
		return "", errors.New("invalid base URL")
	}
	for i := range len(raw) {
		// DNS names must already be ASCII (including punycode). Paths may
		// use percent-encoded UTF-8, but never raw whitespace or controls.
		if raw[i] <= 0x20 || raw[i] >= 0x7f {
			return "", errors.New("invalid base URL")
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" || u.Opaque != "" || strings.HasSuffix(u.Host, ":") {
		return "", errors.New("invalid base URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("invalid base URL")
	}
	port := u.Port()
	if port != "" {
		portNumber, err := strconv.ParseUint(port, 10, 16)
		if err != nil || portNumber == 0 {
			return "", errors.New("invalid base URL")
		}
		// Compare the numeric port, not its spelling (:080 and :0080 are
		// HTTP's default too); normalize non-default ports as well.
		port = strconv.FormatUint(portNumber, 10)
		if (scheme == "http" && portNumber == 80) || (scheme == "https" && portNumber == 443) {
			port = ""
		}
	}
	hostname := strings.ToLower(u.Hostname())
	host := hostname
	if strings.HasPrefix(u.Host, "[") {
		ip, err := netip.ParseAddr(hostname)
		if err != nil || !ip.Is6() || ip.Zone() != "" {
			return "", errors.New("invalid base URL")
		}
		hostname = ip.String()
		if ip.Is4In6() {
			// netip prints mapped addresses with dotted IPv4; WHATWG uses
			// two hex groups. Keep the address IPv6, rather than unmapping it.
			b := ip.As16()
			hostname = fmt.Sprintf("::ffff:%x:%x", uint16(b[12])<<8|uint16(b[13]), uint16(b[14])<<8|uint16(b[15]))
		}
		host = "[" + hostname + "]"
	} else if ip, err := netip.ParseAddr(hostname); err == nil && ip.Is4() {
		host = ip.String()
	} else {
		// ASCII DNS labels, including already-encoded punycode. A single
		// terminal root dot is preserved. Numeric final labels are reserved
		// by WHATWG's IPv4 parser: only strict dotted-decimal IPv4 above is
		// accepted, never shorthand, octal, hex or integer spellings.
		domain := strings.TrimSuffix(hostname, ".")
		if len(domain) > 253 {
			return "", errors.New("invalid base URL")
		}
		labels := strings.Split(domain, ".")
		for _, label := range labels {
			if !baseURLDNSLabel.MatchString(label) {
				return "", errors.New("invalid base URL")
			}
		}
		if baseURLNumeric.MatchString(labels[len(labels)-1]) {
			return "", errors.New("invalid base URL")
		}
	}
	if port != "" {
		host += ":" + port
	}
	// Validate the ORIGINAL path, before either parser escapes or resolves it.
	_, authorityAndPath, _ := strings.Cut(raw, "://")
	_, path, hasPath := strings.Cut(authorityAndPath, "/")
	if hasPath {
		path = "/" + path
	}
	if !baseURLPathChars.MatchString(path) {
		return "", errors.New("invalid base URL")
	}
	for _, segment := range strings.Split(path, "/") {
		dots := strings.ReplaceAll(strings.ToLower(segment), "%2e", ".")
		if dots == "." || dots == ".." {
			return "", errors.New("invalid base URL")
		}
	}
	// url.Parse already validated percent escapes. Preserve their spelling,
	// path case, encoded separators and interior slashes as distinct bases.
	return scheme + "://" + host + strings.TrimRight(path, "/"), nil
}

// ResolveServer returns a checked, coherent connection. Selection precedence
// is flag URL, PUNK_URL, saved URL, localhost. PUNK_API_KEY always wins. A
// saved key is usable only when the selected and saved canonical base URLs
// match. A fully explicit URL+key pair bypasses an unused credentials file.
// codeops:trace repo=punk-records work_item=punk-connect-remote-url-review-20261009 spec=docs/client-credentials.md plan=phase-1/task-1-A test=internal/hookcli/credentials_test.go evidence=docs/superpowers/reports/2026-10-09-client-credentials/builder-a.md
func ResolveServer(flagURL string) (ServerResolution, error) {
	selectedRaw := flagURL
	if selectedRaw == "" {
		selectedRaw = os.Getenv("PUNK_URL")
	}
	explicitURL := selectedRaw != ""
	explicitKey := os.Getenv("PUNK_API_KEY")
	if explicitURL && explicitKey != "" {
		selected, err := canonicalBaseURL(selectedRaw)
		if err != nil {
			return ServerResolution{}, errors.New("invalid server URL")
		}
		return ServerResolution{URL: selected, APIKey: explicitKey}, nil
	}

	creds, found, err := LoadCredentials(CredentialsPath())
	if err != nil {
		return ServerResolution{}, err
	}
	if !explicitURL {
		if found {
			selectedRaw = creds.URL
		} else {
			selectedRaw = defaultServerURL
		}
	}
	selected, err := canonicalBaseURL(selectedRaw)
	if err != nil {
		return ServerResolution{}, errors.New("invalid server URL")
	}
	result := ServerResolution{URL: selected, APIKey: explicitKey}
	if explicitKey != "" || !found || creds.APIKey == "" {
		return result, nil
	}
	if selected == creds.URL {
		// The saved key is the right one for this server - but a saved key
		// must never ride plain http to another host. Refuse instead of
		// attaching it: an unauthenticated cleartext connection to a remote
		// server is not a safe downgrade either (the server may be in
		// bootstrap pass-through).
		if isCleartextRemote(selected) {
			return ServerResolution{}, errors.New(cleartextSavedKeyError)
		}
		result.APIKey = creds.APIKey
	} else {
		result.Diagnostic = savedKeyMismatchDiagnostic
	}
	return result, nil
}
