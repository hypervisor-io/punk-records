# Coding-agent connection credentials

This contract covers `punk connect` and the generated integrations for Claude
Code, Cursor, Antigravity, Copilot CLI, Hermes, Codex, Cline, OpenCode, Pi, and
OpenClaw. A saved login must work without separately exporting its token to
plugins. Connecting one client must not modify another client's configuration.

## Connection selection

**CREDS-1.** At connect time, select the server from explicit `--url`, then
`PUNK_URL`, then the selected credential file's `url`, then
`http://localhost:9090`. `PUNK_CREDENTIALS` selects the credential file;
otherwise use `~/.punk/credentials.json`. An empty flag/environment value is
unset. Do not read the default file in addition to a selected custom file.

**CREDS-2.** `PUNK_API_KEY` is an explicit authentication override. Otherwise,
use the saved `api_key` only when the selected server and the saved server have
the same canonical base URL. Compare scheme, hostname, effective port, and
path; ignore hostname case, explicit default ports, and trailing slashes.
Different path-mounted services on the same host are different servers.
Never send a saved key to a different server. Report a sanitized mismatch
diagnostic without printing a token or unvalidated URL. An explicit environment
key remains usable with an explicitly selected server.

**CREDS-3.** Missing credentials are valid for an anonymous/local installation.
A present but unreadable or malformed credential file is not equivalent to a
missing file. Reject invalid JSON, non-object data, wrong-typed fields, missing
or empty `url`, and invalid base URLs before a dependent write or request.
Diagnostics must not contain file contents, tokens, or credential-bearing URLs.
A fully explicit URL and environment key can bypass the unused credential file.

**CREDS-4.** CLI hooks and generated plugins must use the same credential
boundary. OpenCode, Pi, and OpenClaw must read saved credentials, including a
custom `PUNK_CREDENTIALS` path, without embedding the saved token in generated
JavaScript/TypeScript. Capture, context, inbox, SSE, and Pi's native HTTP tools
must use one coherent URL/key pair for that plugin instance. On invalid
credentials, disable Punk network activity and surface a sanitized diagnostic;
do not interrupt the surrounding coding session.

### Supported base URLs (CREDS-2/3/4)

<!-- codeops:trace repo=punk-records work_item=punk-connect-remote-url-review-20261009 spec=docs/client-credentials.md impl=internal/hookcli/credentials.go,internal/hookcli/credsjs.go test=internal/hookcli/credentials_parity_test.go evidence=docs/superpowers/reports/2026-10-09-client-credentials/parity-correction.md -->

Go hooks and generated plugins accept the same explicit HTTP(S) URL subset.
These rules apply to both the selected URL and the saved credential URL, so a
browser-style repair cannot change which server is eligible for a saved key:

- Use ASCII DNS names (including already-encoded punycode), strict four-part
  decimal IPv4, or bracketed IPv6. DNS labels contain 1–63 ASCII letters, digits
  or hyphens, start and end with a letter/digit, and total at most 253 characters
  excluding an optional final root dot. That dot is preserved and remains a
  distinct credential boundary. Unicode/IDN hostnames must be supplied in their
  ASCII form. Host percent-escapes, IPv6 zone identifiers, IPv4 shorthand,
  integer/hex/octal spellings, leading-zero IPv4 components, and numeric or
  hexadecimal final DNS labels are rejected. For this rule a numeric/hex label
  is all decimal digits or `0x` followed by zero or more hexadecimal digits.
- Ports are decimal integers from 1 through 65535. Leading zeroes are accepted
  and removed, including default ports: HTTP `:080` and `:0080` disappear;
  HTTPS `:00443` disappears; `:00081` becomes `:81`. An explicit empty port
  (`host:`, `127.0.0.1:`, or `[::1]:`) is invalid.
- Scheme and hostname case are normalized. IPv6 uses compressed lowercase hex,
  including mapped addresses: `[::ffff:127.0.0.1]` becomes `[::ffff:7f00:1]`.
  An IPv4-mapped IPv6 address remains distinct from an IPv4 authority.
- Ordinary base paths such as `/team/punk` are supported. Use RFC 3986 path
  characters (letters, digits, `-._~!$&'()*+,;=:@/`) or valid `%HH` escapes;
  percent-encode other path characters and UTF-8. Raw whitespace, controls,
  non-ASCII characters, backslashes, userinfo, query and fragment delimiters
  (even empty ones) are invalid anywhere in the URL.
- Reject entire `.` or `..` path segments, including `%2e`, `%2e%2e`, `.%2e`
  and `%2e.` variants, case-insensitively. Paths are never dot-normalized.
  Preserve path case, escape spelling, encoded separators and interior slashes;
  remove only trailing slashes. Thus `/a%2Fb` and `/a/b`, or `/a//b` and `/a/b`,
  remain distinct credential boundaries.

Credential `api_key` is optional, but when present must be a JSON string.
`null` is invalid; an omitted or empty-string key permits anonymous operation.

## Reconnect and restart

**CREDS-5.** Generated hooks and native MCP entries are installation snapshots.
Keep their selected URL stable until an explicit reconnect, and require a
client restart/reload afterward. A login change must not send a new server's
key to an old hook's URL. Plugins likewise must not switch servers during a
session. Preserve the existing runtime `PUNK_URL` override, with the saved-key
boundary enforced and its possible divergence from static MCP clearly stated.

**CREDS-6.** Reconnecting a selected client updates all its already-enabled
Punk-managed capture, inbox, and wake commands to the selected URL, even when
the reconnect omits the original opt-in flags. Do not accidentally enable
messaging/wake or remove those capabilities. Preserve foreign entries and the
client's existing config semantics. First-time installation of the explicitly
selected client remains supported. Hermes continues using its existing YAML
merger, not a new Python integration.

**CREDS-7.** Direct native MCP clients still read static URL/auth configuration.
This change does not add a relay, replace HTTP with `punk mcp` (which serves a
local database), or promise automatic credential-file following inside those
hosts. Preserve existing host-specific environment-reference syntax,
`--api-key-env`, foreign-entry ownership checks, and owner-only permissions
for literal-token configuration. Codex's env-only MCP auth requires an
actionable diagnostic when the named environment key is unavailable; a probe
with a different saved key must not masquerade as host-auth verification.

## OpenCode configuration ownership

**CREDS-8.** At the selected global/project scope, adopt an existing
`opencode.jsonc` or `opencode.json`, rather than creating a competing file.
If both exist, fail with a clear ambiguity diagnostic before writing the
plugin or configuration. If neither exists, create `opencode.json` as before.
Support JSONC comments/trailing commas and preserve comments, unrelated
settings, foreign MCP entries, symlinks, and file-permission protections.
Reject malformed/foreign configuration before changing the generated plugin.

## Verification contract

Tests use synthetic tokens, isolated HOME/USERPROFILE and `PUNK_CREDENTIALS`,
and loopback endpoints only. Do not inspect developer credentials or contact
a deployed Punk server. Generated-source substring tests alone are not enough:
execute generated plugins or installed hook commands and observe their actual
request URL and authentication.

| Surface | Required behavioral evidence |
| --- | --- |
| Shared Go resolver | Precedence, missing/malformed/wrong-shape credentials, custom path, URL canonicalization, mismatched-key suppression, fully explicit bypass |
| Claude Code, Cursor, Antigravity, Copilot CLI, Hermes, Codex, Cline | Saved-only connect; reconnect to a second URL; preserved messaging/wake opt-in and foreign entries; no stale managed command |
| OpenCode, Pi, OpenClaw | Saved-only authenticated capture/context; inbox/SSE/native tools where supported; same connection pair; credential changes do not retarget an existing instance |
| Native MCP writers | Updated snapshots, correct host syntax, existing private-token permissions and ownership refusal; explicit native-host limitations |
| OpenCode path/merge | JSON and JSONC; comments/trailing commas; both-present refusal; invalid/foreign config leaves plugin and config untouched |

Every fixed defect needs a failing-before/passing-after regression. Preserve
the repository's existing Go, Node, and connector test suites. Verification
does not claim real Hermes/OpenCode/etc. application sessions unless those
hosts were actually exercised.

## Compatibility and rollback

Malformed credentials and cross-server saved-token reuse are deliberate
security tightenings. Correct the saved file or explicitly provide the intended
server and environment key; do not add an unsafe fallback. Reconnect and
restart after changing the saved server or credentials. Users who choose
`--api-key-env` must make that variable available to the client process.

No credential file is rewritten by resolution. No existing client config is
automatically regenerated by a software upgrade. Rollback is to the previous
binary and an operator-held config backup; never restore an exposed token.

Related: [configuration](CONFIG.md#client-authentication-and-verification),
[agent messaging](agent-messaging.md).

<!-- codeops:trace repo=punk-records work_item=punk-connect-remote-url-review-20261009 spec=docs/client-credentials.md plan=docs/superpowers/plans/2026-10-09-client-credentials.md impl=internal/hookcli/credentials.go test=internal/hookcli/credentials_test.go doc=docs/CONFIG.md -->
