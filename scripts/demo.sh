#!/bin/bash
# Deterministic end-to-end demo: a generic incident webhook in,
# routed to the database agent, audited on the ledger. Runs with
# ai.enabled=false, so the investigation parks for a human - proving the
# coordination plane works with no model at all.
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=${PORT:-9391}
DB=$PWD/bin/demo.db
rm -f "$DB"*

export PUNK_DB_DSN=$DB
export PUNK_HTTP_ADDR=:$PORT

make build >/dev/null
./bin/punk migrate up >/dev/null

./bin/punk serve >bin/demo-serve.log 2>&1 &
SPID=$!
trap 'kill -TERM $SPID 2>/dev/null; wait $SPID 2>/dev/null; rm -f "$DB"*' EXIT

for i in $(seq 1 20); do
  curl -sf localhost:$PORT/healthz >/dev/null && break
  sleep 0.5
done

BODY='{"source":"incidents","external_ref":"incident:42","labels":{"domain":"database","severity":"critical"}}'

echo "== task in (generic webhook intake) =="
curl -s -X POST "localhost:$PORT/v1/intake/webhook" -d "$BODY" | head -c 500; echo; echo

TASK_ID=$(curl -s "localhost:$PORT/v1/tasks?status=submitted" | grep -o '"id":"[a-f0-9]*"' | head -1 | cut -d'"' -f4)
echo "== task $TASK_ID routed; waiting for the dispatcher =="
sleep 4

echo "== ledger (deterministic park: ai disabled) =="
curl -s "localhost:$PORT/v1/tasks/$TASK_ID" | python3 -m json.tool | grep -E '"(status|type|agent_name|reason|error)"' | head -12

echo
echo "== retry is a dedup, not a duplicate =="
curl -s -X POST "localhost:$PORT/v1/intake/webhook" -d "$BODY" | grep -o '"created":[a-z]*'
echo "demo ok"
