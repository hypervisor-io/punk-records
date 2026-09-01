#!/bin/sh
# Ensures the SQLite/Postgres schema exists before serving. `punk migrate
# up` is idempotent (golang-migrate no-ops when already at head), so this
# is safe on every start, not just the first. Fixes first-run on an empty
# volume, where serve would otherwise die with:
#   punk: ping sqlite: unable to open database file (14)
set -e
if [ "$1" = "serve" ]; then
    punk migrate up --config /app/config.yaml
fi
exec punk "$@"
