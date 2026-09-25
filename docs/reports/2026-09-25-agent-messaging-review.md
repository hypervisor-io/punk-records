# Agent messaging implementation review

## Scope and outcome

Opus, Astra, Kimi and GLM implemented M1-M11 in `punk-agent-messaging`, including the user's expanded multiclient plan. The orchestrator reviewed persistence, API/MCP authorization and enablement, inbox rendering/state, client reply contracts, extension lease/ACK/retry behavior and integration evidence. Implementation and automated gate accepted. Live-client acceptance remains unverified.

No commits, deployment, live coordination-server restart or production database migration performed. Existing unrelated working-tree edits preserved.

## Findings returned to builders and fixed

- Explicit sender and namespace missing from bridge reply guidance; per-turn identity context lost after first transform.
- Registration failure stranded sessions; restored retry/unknown status could prompt busy sessions; asynchronous deletion/disposal races.
- Non-OK SSE response handling reset backoff; stalled connections lacked watchdogs.
- Earlier successful deliveries could miss ACK after a later prompt failure; pending/recent receipt state required bounds and retry reconciliation.
- Shared adapter registration could be overwritten by Go init ordering.
- Render limit omitted header/footer and neutralisation growth; hard whole-envelope UTF-8 bound now enforced, with no delivery below the minimum safe envelope size.
- Older allowlist-denied rows hid eligible messages behind the first fetch page; bounded leased scans and owner release now reach later rows.
- Short stdout writes incorrectly counted as receipt; wait-mode fetches escaped the wait deadline.
- Lease release endpoint missing; added scoped, owner-checked, idempotent release and real hook round-trip tests.
- Generated bridges ignored cap zero, exceeded 100-ID ACK/release batches, leaked leases after partial fetch failures and housekeeping, and failed to wake at the next cap window.
- Tool schemas grew for default users; disabled schemas/budget restored and enabled messaging measured separately.
- Stdio omitted Region/Bus wiring; actual built-process tool discovery now tests both enablement states.

## Independent final verification

Executed by orchestrator against the combined working tree after builder corrections:

```text
go test ./...
go vet ./...
go build -o /dev/null ./cmd/punk
go test -race ./internal/region ./internal/api ./internal/mcpserver ./internal/hookcli -run 'Message|Inbox|Messaging|Cline' -count=1
git diff --check
gofmt -l cmd/punk internal/config internal/hookcli internal/mcpserver internal/region internal/store internal/api
```

All passed. Final formatting/comment-only cleanup followed the semantic gates; subsequent formatting and diff checks were clean. The full suite includes authenticated real-API tests executing installed hooks from `connect --messaging` for seven subprocess clients, actual generated-JavaScript execution for OpenCode/Pi/OpenClaw, reversible SQLite migrations, namespace-decoy controls, cross-handle leases/caps, per-process continuation locks and default/opt-in MCP budget checks. These are automated host simulations, not real-client transcripts.

## Delivery capabilities

- OpenCode and Pi: idle wake through long-lived extensions, with busy deferral and capped retries.
- Claude Code, Codex, Cursor, Copilot CLI and Antigravity: documented context events and bounded turn-end continuation; no universal idle wake claim.
- Hermes and Cline: context catch-up on supported events.
- OpenClaw: context catch-up only; no verified plugin API starts a turn.

See `docs/agent-messaging.md` for exact events, configuration, receipt semantics and client-version evidence.

## Remaining acceptance limits

- Real Claude Code, Codex, Cursor, Pi and other client session acceptance was not performed. No screenshots/transcripts of live clients are claimed.
- Postgres runtime/migration tests require `PUNK_TEST_PG_DSN`; no Postgres execution was available. SQL migration pairs exist for both drivers.
- PowerShell/native Windows runtime and installed OpenClaw manifest compatibility remain unverified.
- Delivery is at-least-once. ACK records host handoff, not proof of model consumption or task completion. Retention also removes idempotency history.
- Sender allowlist scans are bounded. Increasing the server backlog cap beyond the client scan bound can leave eligible rows behind older denied rows until the backlog changes.
- Server MCP enablement and client opt-in are separate. HTTP messaging endpoints retain existing namespace authorization and are not disabled by the MCP tool switch.

Owner-operated upgrade and live acceptance are the next gate before release.
