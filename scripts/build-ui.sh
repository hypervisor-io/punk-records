#!/usr/bin/env bash
# Builds the operator console's Tailwind CSS from its source into the
# embedded, committed output. Run from the repo root:
#   scripts/build-ui.sh
#
# Requires Node 22+ and npm. This repo has no package.json (the Go
# module is the only build unit), so `npx --yes @tailwindcss/cli@4.3.3`
# alone cannot resolve the `tailwindcss` package: Tailwind's `@import
# "tailwindcss"` resolves node_modules by walking up from the CSS file,
# and npx's own package cache is not an ancestor of this tree. We
# install @tailwindcss/cli into a scratch prefix, link it in as this
# repo's node_modules just long enough for the same npx command to
# resolve it, then remove the link so the working tree is left clean.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

scratch="$(mktemp -d)"
cleanup() {
  [ -L node_modules ] && rm -f node_modules
  rm -rf "$scratch"
}
trap cleanup EXIT

npm install --no-save --silent --prefix "$scratch" @tailwindcss/cli@4.3.3

if [ -e node_modules ] && [ ! -L node_modules ]; then
  echo "build-ui.sh: a real ./node_modules already exists; refusing to touch it" >&2
  exit 1
fi
ln -sfn "$scratch/node_modules" node_modules

npx --yes @tailwindcss/cli@4.3.3 \
  -i internal/api/ui/console.src.css \
  -o internal/api/ui/console.css \
  --minify

echo "built internal/api/ui/console.css"
