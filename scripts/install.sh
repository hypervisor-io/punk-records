#!/usr/bin/env bash
# install.sh - install punk-records from a GitHub release and bootstrap it.
#
# Needs nothing but curl and tar. An authenticated `gh` CLI or a GITHUB_TOKEN
# is used automatically when present (higher API rate limits, and it is what
# makes this work against a private fork), but neither is required.
#
# Usage:
#   ./scripts/install.sh                 # install latest release binary
#   ./scripts/install.sh --init         # ...and run first-time setup (migrate)
#   PUNK_VERSION=v1.1.0 ./scripts/install.sh
#
# Environment overrides:
#   PUNK_VERSION      release tag to install (default: latest release)
#   PUNK_INSTALL_DIR  binary destination (default: /usr/local/bin if writable,
#                     else ~/.local/bin)
#   PUNK_DIR          data directory used by --init (default: ~/.punk)
#   GITHUB_TOKEN      optional token: raises API rate limits, and is what
#                     lets this reach a private fork of the repo
set -euo pipefail

REPO="hypervisor-io/punk-records"
API="https://api.github.com/repos/${REPO}"

say()  { printf '\033[1;35mpunk:\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31mpunk: error:\033[0m %s\n' "$*" >&2; exit 1; }

INIT=0
for arg in "$@"; do
  case "$arg" in
    --init) INIT=1 ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) fail "unknown flag: $arg (see --help)" ;;
  esac
done

# ---- platform detection ----------------------------------------------------
OS="$(uname -s)"
case "$OS" in
  Linux)  GOOS=linux ;;
  Darwin) GOOS=darwin ;;
  *) fail "unsupported OS: $OS (Windows: download the punk_*_windows_amd64.zip release asset manually)" ;;
esac

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64)  GOARCH=amd64 ;;
  aarch64|arm64) GOARCH=arm64 ;;
  *) fail "unsupported architecture: $ARCH" ;;
esac

# ---- auth strategy ---------------------------------------------------------
# The repo is public, so the default path is unauthenticated: plain curl
# against the public API and the public asset URLs. gh (when authenticated)
# and GITHUB_TOKEN are both honored when available - they raise GitHub's
# rate limit for the anonymous case, and they are what would let this script
# install from a private fork - but their absence is never an error.
HAVE_GH=0
if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
  HAVE_GH=1
fi

command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v tar  >/dev/null 2>&1 || fail "tar is required"

api() { # api <path> - GET an API path as JSON
  if [ "$HAVE_GH" = 1 ]; then
    gh api "repos/${REPO}${1}"
  elif [ -n "${GITHUB_TOKEN:-}" ]; then
    curl -fsSL -H "Authorization: Bearer ${GITHUB_TOKEN}" \
      -H "Accept: application/vnd.github+json" "${API}${1}"
  else
    curl -fsSL -H "Accept: application/vnd.github+json" "${API}${1}"
  fi
}

# ---- resolve version -------------------------------------------------------
VERSION="${PUNK_VERSION:-}"
if [ -z "$VERSION" ]; then
  # sed extraction works on both compact (gh api) and pretty-printed (curl) JSON
  VERSION="$(api /releases/latest \
    | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
  [ -n "$VERSION" ] || fail "could not resolve the latest release tag"
fi
PKG="punk_${VERSION}_${GOOS}_${GOARCH}.tar.gz"
say "installing punk ${VERSION} (${GOOS}/${GOARCH})"

# ---- download + verify -----------------------------------------------------
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

if [ "$HAVE_GH" = 1 ]; then
  gh release download "$VERSION" --repo "$REPO" --dir "$TMP" \
    --pattern "$PKG" --pattern "SHA256SUMS.txt" \
    || fail "release download failed (does ${VERSION} have a ${PKG} asset?)"
elif [ -n "${GITHUB_TOKEN:-}" ]; then
  # Token path: resolve each asset's numeric id and fetch it through the API
  # with an octet-stream Accept. This is the only shape that works for a
  # private repo, where the public browser_download_url 404s.
  assets_json="$(api "/releases/tags/${VERSION}")"
  for name in "$PKG" "SHA256SUMS.txt"; do
    # find the asset id that precedes the matching "name" in the release JSON
    asset_id="$(printf '%s' "$assets_json" | awk -v want="\"${name}\"" '
      /"id":/     { gsub(/[^0-9]/, "", $2); id=$2 }
      /"name":/   { if (index($0, want)) { print id; exit } }')"
    [ -n "$asset_id" ] || fail "asset ${name} not found on release ${VERSION}"
    curl -fsSL -H "Authorization: Bearer ${GITHUB_TOKEN}" \
      -H "Accept: application/octet-stream" \
      -o "${TMP}/${name}" "${API}/releases/assets/${asset_id}" \
      || fail "download of ${name} failed"
  done
else
  # Anonymous path: the public release download URL, no API call and no
  # rate-limited metadata request per asset.
  for name in "$PKG" "SHA256SUMS.txt"; do
    curl -fsSL -o "${TMP}/${name}" \
      "https://github.com/${REPO}/releases/download/${VERSION}/${name}" \
      || fail "download of ${name} failed (does release ${VERSION} have that asset?)"
  done
fi

( cd "$TMP"
  if command -v sha256sum >/dev/null 2>&1; then
    grep " ${PKG}\$" SHA256SUMS.txt | sha256sum -c - >/dev/null
  else
    grep " ${PKG}\$" SHA256SUMS.txt | shasum -a 256 -c - >/dev/null
  fi
) || fail "checksum verification failed for ${PKG}"
say "checksum verified"

tar -xzf "${TMP}/${PKG}" -C "$TMP" punk

# ---- install ---------------------------------------------------------------
DEST="${PUNK_INSTALL_DIR:-}"
if [ -z "$DEST" ]; then
  if [ -w /usr/local/bin ]; then DEST=/usr/local/bin; else DEST="$HOME/.local/bin"; fi
fi
mkdir -p "$DEST"
install -m 0755 "${TMP}/punk" "${DEST}/punk"
say "installed ${DEST}/punk"
"${DEST}/punk" --version

case ":$PATH:" in
  *":${DEST}:"*) ;;
  *) say "note: ${DEST} is not on your PATH" ;;
esac

# ---- optional first-time setup --------------------------------------------
if [ "$INIT" = 1 ]; then
  DATA_DIR="${PUNK_DIR:-$HOME/.punk}"
  mkdir -p "$DATA_DIR"
  say "running migrations in ${DATA_DIR} (SQLite by default, zero external services)"
  ( cd "$DATA_DIR" && "${DEST}/punk" migrate up )
  say "setup complete"
fi

cat <<NEXT

Next steps:
  cd ${PUNK_DIR:-\$HOME/.punk} && punk serve      # API + /mcp on :9090
  punk apikey create --name my-agent              # mint an agent token
  punk connect claude-code                        # wire your coding agent
  # also: punk connect cursor | opencode | pi | antigravity | copilot | hermes | openclaw

Postgres instead of SQLite, model config, and the full agent matrix:
  https://github.com/${REPO}#readme
NEXT
