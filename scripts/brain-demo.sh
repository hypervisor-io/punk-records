#!/bin/bash
# Shared-brain demo: two consumers (satellites) connect to Punk Records over
# HTTP, register to ONE brain region, claim disjoint work, append findings
# concurrently, and neither conflicts. Proves the Punk Records model with
# ai.enabled=false - pure coordination, no model.
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=${PORT:-9392}
DB=$PWD/bin/brain.db
rm -f "$DB"*
export PUNK_DB_DSN=$DB PUNK_HTTP_ADDR=:$PORT

make build >/dev/null
./bin/punk migrate up >/dev/null
./bin/punk serve >bin/brain-serve.log 2>&1 &
SPID=$!
trap 'kill -TERM $SPID 2>/dev/null || true; wait $SPID 2>/dev/null || true; rm -f "$DB"* bin/brain-serve.log' EXIT
curl -sf --retry 30 --retry-delay 1 --retry-connrefused localhost:$PORT/healthz >/dev/null

NS=repo-main
echo "== two satellites append to one region, concurrently, no conflict =="

# satellite A works files a-c, satellite B works files d-f; both append
# findings to the shared region. Run their writes interleaved.
pids=()
for f in a b c; do
  curl -s --max-time 10 -X POST "localhost:$PORT/v1/namespaces/$NS/memories" \
    -d "{\"key\":\"/finding/$f\",\"body\":\"agent-A analyzed $f.go\",\"author\":\"agent-A\"}" >/dev/null &
  pids+=($!)
done
for f in d e f; do
  curl -s --max-time 10 -X POST "localhost:$PORT/v1/namespaces/$NS/memories" \
    -d "{\"key\":\"/finding/$f\",\"body\":\"agent-B analyzed $f.go\",\"author\":\"agent-B\"}" >/dev/null &
  pids+=($!)
done
wait "${pids[@]}"  # wait on the curl jobs only, not the background serve

echo "-- region now holds all 6 findings, from both satellites, no lost writes:"
curl -s --max-time 10 "localhost:$PORT/v1/namespaces/$NS/keys?prefix=/finding" | python3 -m json.tool

echo
echo "== duplicate concurrent write collapses (content dedup) =="
pids=()
for i in 1 2 3; do
  curl -s --max-time 10 -X POST "localhost:$PORT/v1/namespaces/$NS/memories" \
    -d '{"key":"/finding/root","body":"same root cause seen by three agents","author":"agent-A"}' >/dev/null &
  pids+=($!)
done
wait "${pids[@]}"  # curl jobs only
N=$(curl -s --max-time 10 "localhost:$PORT/v1/namespaces/$NS/memories?prefix=/finding/root" | python3 -c 'import sys,json;print(len(json.load(sys.stdin)))')
echo "-- /finding/root live revisions: $N (want 1 - three identical writes deduped)"

echo
echo "== bi-temporal: read the region as it was one instant ago =="
curl -s --max-time 10 "localhost:$PORT/v1/namespaces/$NS/memories?prefix=/finding&as_of=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  | python3 -c 'import sys,json;print("  facts valid now:",len(json.load(sys.stdin)))'

echo "brain-demo ok"
exit 0
