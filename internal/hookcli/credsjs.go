package hookcli

// credentialsJS is shared by the three generated ESM integrations, like
// inboxBridgeJS. Each factory/register call resolves one immutable connection;
// only the installation URL is generated, never a credential file or token.
// Keep this boundary aligned with credentials.go and docs/client-credentials.md.
// codeops:trace repo=punk-records work_item=punk-connect-remote-url-review-20261009 spec=docs/client-credentials.md plan=docs/superpowers/plans/2026-10-09-client-credentials.md test=TestPluginCredentialsRuntime
// codeops:trace repo=punk-records work_item=punk-connect-remote-url-review-20261009 spec=docs/client-credentials.md test=internal/hookcli/credentials_parity_test.go evidence=docs/superpowers/reports/2026-10-09-client-credentials/parity-correction.md
const credentialsJS = `
// Shared startup credentials. Reconnect and restart to refresh an installation
// snapshot; PUNK_URL is an explicit runtime override (native MCP stays static).
import { readFileSync as punkReadCredentials } from "node:fs";
import { homedir as punkHome } from "node:os";
import { join as punkJoinPath } from "node:path";
import { isIP as punkIPVersion } from "node:net";

function punkCanonicalURL(raw) {
  // Supported subset shared with credentials.go: ASCII DNS/punycode, strict
  // IPv4 or bracketed IPv6, numeric ports and URI-encoded paths. Reject parser
  // repairs (including dot segments) before WHATWG can change a key boundary.
  if (typeof raw !== "string" || !/^https?:\/\/[^/?#]+/i.test(raw) ||
      /[^\x21-\x7e]|[\\?#]/.test(raw) || /%(?![0-9a-f]{2})/i.test(raw)) throw new Error("invalid base URL");
  const rest = raw.slice(raw.indexOf("://") + 3);
  const slash = rest.indexOf("/");
  const authority = slash < 0 ? rest : rest.slice(0, slash);
  const path = slash < 0 ? "" : rest.slice(slash);
  if (authority.includes("@") || authority.endsWith(":")) throw new Error("invalid base URL");
  const parts = /^(\[[^\]]+\]|[^:]+)(?::([0-9]+))?$/.exec(authority);
  if (!parts) throw new Error("invalid base URL");
  const host = parts[1].toLowerCase();
  if (host.startsWith("[")) {
    const ip = host.slice(1, -1);
    if (ip.includes("%") || punkIPVersion(ip) !== 6) throw new Error("invalid base URL");
  } else if (punkIPVersion(host) !== 4) {
    const domain = host.replace(/\.$/, "");
    const labels = domain.split(".");
    if (domain.length > 253 || labels.some((label) => !/^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(label)) ||
        /^(?:[0-9]+|0x[0-9a-f]*)$/.test(labels[labels.length - 1])) throw new Error("invalid base URL");
  }
  if (parts[2] !== undefined) {
    const port = Number(parts[2]);
    if (port < 1 || port > 65535) throw new Error("invalid base URL");
  }
  if (!/^[a-zA-Z0-9._~!$&'()*+,;=:@/%-]*$/.test(path) || path.split("/").some((segment) => {
    const dots = segment.replace(/%2e/ig, ".");
    return dots === "." || dots === "..";
  })) throw new Error("invalid base URL");
  const u = new URL(raw);
  return u.origin + path.replace(/\/+$/, "");
}

function punkResolveConnection(installedURL) {
  try {
    const env = (typeof process !== "undefined" && process.env) || {};
    const runtimeURL = env.PUNK_URL || "";
    const explicitKey = env.PUNK_API_KEY || "";
    let url = runtimeURL || installedURL || "";
    if (url) url = punkCanonicalURL(url);
    // Only a fully explicit runtime pair makes the saved file irrelevant.
    if (runtimeURL && explicitKey) return Object.freeze({ url, apiKey: explicitKey, enabled: true });
    const file = env.PUNK_CREDENTIALS || punkJoinPath(punkHome(), ".punk", "credentials.json");
    let raw;
    try {
      raw = punkReadCredentials(file, "utf8");
    } catch (err) {
      if (!err || err.code !== "ENOENT") throw err;
    }
    let savedURL = "", savedKey = "";
    if (raw !== undefined) {
      const saved = JSON.parse(raw);
      if (!saved || typeof saved !== "object" || Array.isArray(saved) ||
          typeof saved.url !== "string" || !saved.url ||
          (Object.prototype.hasOwnProperty.call(saved, "api_key") && typeof saved.api_key !== "string")) {
        throw new Error("invalid credentials");
      }
      savedURL = punkCanonicalURL(saved.url);
      savedKey = saved.api_key || "";
    }
    url = url || savedURL || "http://localhost:9090";
    let apiKey = explicitKey;
    if (!apiKey && savedKey) {
      if (url === savedURL) apiKey = savedKey;
      else console.error("[punk] saved credentials do not match the selected server; saved key ignored. Reconnect and restart, or set PUNK_API_KEY explicitly.");
    }
    return Object.freeze({ url, apiKey, enabled: true });
  } catch (_) {
    // Never print a caught error: parser and filesystem errors can contain
    // tokens, credential-bearing URLs, raw file contents or private paths.
    console.error("[punk] invalid connection credentials or server URL; Punk network activity is disabled. Correct saved credentials or set PUNK_URL and PUNK_API_KEY, then restart the client.");
    return Object.freeze({ url: "", apiKey: "", enabled: false });
  }
}
`
