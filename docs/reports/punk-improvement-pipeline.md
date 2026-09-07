# Punkrecords improvement pipeline - acceptance evidence report

Final cross-feature acceptance (task Z01) for the improvement pipeline
defined by `docs/superpowers/specs/2026-09-05-punk-improvement-pipeline-design.md`
and `docs/superpowers/plans/2026-09-05-punk-improvement-pipeline.md`.
Sections 3, 5, 7, 8 and 9 record fresh Z01 runs executed for this report
on 2026-09-07; nothing there is marked passed without a logged run, and
unrun items are labeled UNRUN with the missing prerequisite. Section 4's
per-task matrix additionally cites HISTORICAL accepted-review evidence
(each task's own acceptance dates, e.g. C02's 2026-09-06 investigation);
those rows are reused reviewer evidence, not re-executions by this
report.

## 1. Provenance and scope

- Repository: `/mnt/d/Hypervisor_Code/punkrecords`; verification worktree
  `/mnt/d/Hypervisor_Code/punkrecords-worktrees/punk-improve-Z01-ubuntu`,
  branch `work/punk-improve-Z01-ubuntu`.
- Integration branch: `feat/punk-improvement-pipeline` (local only).
- Verified tip: `9159cb069ec014ddc9667f4877925200435d4d90`
  (`feat(memory): add incremental summary hierarchies`, H01).
- Baseline: `9ab2b8f22b6e89567c3d367d9460a3b3ba4da739` (migration 0021).
- Compatibility target: installed `codex-cli 0.153.4`, upstream
  `rust-v0.153.4` commit `3d2ee51ca2d5db578f328aa75e20aa22c0197c9a`.
- Scope: 23 reviewed task commits (C01-C08, A01, A02, E01, E02, G01,
  G02, I01, I02, P01, P02, R01, R02, S01, S02, H01) plus this acceptance
  task. The verified tree carries one TEST-ONLY addition on top of the
  tip: the PostgreSQL skill-test portability patch (section 5).
- Exact tested-tree hashes (sha256): the two test-only portability
  files, `internal/memory/skills_test.go`
  `f001069ac5ccacc453a5d6d8fd6d2db60a5447c45a0f59865dbc3f8e387eb5fe` and
  `internal/memory/skills_runs_test.go`
  `66035f8ad732dc51454ba3acddaf46db5c7ffe0d2e6b5d760932514ff7fb377f`;
  every other tested file is byte-identical to the verified tip
  `9159cb0` (git-clean except the two patched test files).
- Out of scope and not performed: deployment, release, tagging, merge to
  main, push, live-server (`127.0.0.1:9090`) restart or modification,
  real `~/.codex` modification (one read-only `auth.json` copy into a
  temp CODEX_HOME, per the established C02/C06 boundary).

## 2. Environment and configuration

- Host: WSL2 (`5.15.167.4-microsoft-standard-WSL2`), linux/amd64.
- Toolchain: go1.26.1, node v22.21.0 (npm 11.19.1), tmux 3.2a,
  Docker 28.0.4, image `postgres:16` (pulled for this run).
- Go gates ran in the verification worktree with default flags except
  where noted; race suite ran with `-timeout 1200s` (section 11).
- PostgreSQL runs used one disposable container
  `punk-z01-pg-20260907` on `127.0.0.1:15432` (databases `punk`,
  `punkupg`, `punktest`), removed after the runs; test suite DSN via
  `PUNK_TEST_PG_DSN`, serial `-p 1`.
- SQLite acceptance used temp files under `/tmp/punk-Z01-sqlite/`.
- Native Codex acceptance used a temp Punk server on `127.0.0.1:19399`
  (sqlite, `ai.enabled=false`), temp CODEX_HOME `/tmp/codex-z01/home`,
  fixture project `/tmp/codex-z01/proj` (basename `proj`).
- All acceptance probes used isolated servers bound to loopback high
  ports (19091-19094, 19399); the live `:9090` service was not
  restarted, upgraded or otherwise modified (Punk coordination calls by
  the agents still read/write the shared task plane it serves, as
  usual).

## 3. Full-gate log

All times UTC 2026-09-07. Full logs: `/tmp/punk-Z01-logs/`.

| Command | Result | Duration | Log |
|---|---|---|---|
| `go test ./... -count=1` | PASS (exit 0) | 17:36:31-17:38:31 (120s wall) | `go-test-full.log` |
| `go test -race -timeout 1200s -count=1 ./internal/hookcli ./internal/api ./internal/mcpserver ./internal/memory ./internal/reflect` | PASS (exit 0) | 17:38:52-17:52:23 (13m31s wall) | `go-test-race.log` |
| `go vet ./...` | PASS (exit 0) | 10.9s | `vet-build-ui.log` |
| `go build ./...` | PASS (exit 0) | 9.6s | `vet-build-ui.log` |
| `GOOS=windows GOARCH=amd64 go build ./...` | PASS (exit 0) | 14.0s | `vet-build-ui.log` |
| `make test-ui` | PASS (exit 0) | 4.1s | `vet-build-ui.log` |
| `gofmt -l` on changed Go files | PASS (no output) | - | - |

Race per-package: hookcli 6.858s, api 75.997s, mcpserver 98.499s,
memory 800.516s, reflect 22.072s. Full-suite per-package times are in
`go-test-full.log` (memory 114.059s, mcpserver 71.926s, api 41.268s,
membench 21.366s; all others under 13s).

`make test-ui` exists in the Makefile and ran for real: node executed
`node --test internal/api/ui/brain-core.test.mjs` (14 tests, all
`ok`) and `node --check internal/api/ui/brain.js`. No npm install was
required, so this is a PASS, not an assumed one.

Focused acceptance suites (all PASS, `focused-suites.log`):

| Focus | Command | Result |
|---|---|---|
| Authorization inventory (A01/A02) | `go test ./internal/authz ./internal/api ./internal/mcpserver -run 'Inventory\|Authz\|Grant\|Boundary' -count=1` | ok: authz 1.005s, api 7.546s, mcpserver 6.851s |
| Retrieval modes, deterministic + opt-in (R01/R02/H01) | `go test ./internal/memory ./internal/reflect ./internal/mcpserver -run 'Route\|Strategy\|Expand\|Summar\|OptIn' -count=1` | ok: memory 21.469s, reflect 5.450s, mcpserver 0.760s |
| Restart/retry scenarios (P01) | `go test ./internal/memory -run 'TestPipelineRunCrashBeforeAckRetryIdempotent\|TestRequeuedPendingWorkDrainedByRecovery\|TestDrainedOutboxEventPersistsDurableWorkBeforeAck\|TestRecoverPipelineRuns\|TestPipelineRunRetryBudget' -count=1 -v` | 5/5 `--- PASS` |

## 4. Per-task acceptance matrix

SHAs are INTEGRATED commits on `feat/punk-improvement-pipeline` (worker
SHAs are recorded in the coordination namespace `/reviews/<ID>/accepted`
and are not repeated here). "Gate" names each task's contract checks.
Results are quoted from accepted review records, which distinguish
reviewer tests, worker evidence and user observations such as the
original-host title mitigation. The combined tip re-ran the full gates
in section 3.

| Task | Integrated SHA | Gate (scoped) | Result at acceptance |
|---|---|---|---|
| C01 hook event normalization | `444d1bd436b3904fa3a172e03db448630e1084ca` | `go test ./internal/hookcli ./internal/api -count=1` + vet/build | PASS; native fixture normalization, supplied prompt_id, replay identity, capture-only stdout covered |
| C02 terminal-title diagnosis/mitigation | `9ab3464b845e29447bd87e8347d97e2a9afbf35f` | docs-only; original-host A/B + four-cell tmux matrix | PASS; user-confirmed 2026-09-06 12:20:41 UTC that `tui.terminal_title=[]` removed the repetition; renderer itself unisolated |
| C03 managed-install reconciliation | `08b0afee2540441faf32a6a8a652a4154c2221ec` | `go test ./internal/hookcli ./cmd/punk -count=1` + vet/build | PASS incl. reviewer across-shared-state and same-file/symlink/custom-root regressions |
| C04 context-delivery dedup | `6862f41ae27d5d81bb237d92f5a044943f97a2d5` | `go test` memory/api/hookcli + race | PASS; race api 66.318s/hookcli 6.290s/memory 232.938s; server-issued dedup boundary, no host ack |
| C05 namespace diagnostics | `3d6e95732d1cbe86afda32bf4d7807a429e5c532` | `go test ./internal/hookcli ./internal/mcpserver ./cmd/punk -count=1` | PASS; conservative TOML/config inspection, duplicate supported keys detected |
| C06 native Codex acceptance | `17f2c0191613e11948f888166b0db42a7855c15c` | simulated `codex_roundtrip_test.go` + native 0.153.4 run | PASS; hookcli 3.861s/api 33.206s/mcpserver 69.009s, race TestCodex 7.687s; native OSC on=128/off=0; resume-after-turn proven. Re-executed at tip, section 7 |
| C07 guidance budget | `c3b935b1b28ed8ae3f9732fc8abe1e22ca066574` | `go test ./internal/mcpserver ./internal/hookcli -count=1` | PASS; initialize 2130->1533B (~28%), combined init+tools ~2.47% smaller (bytes/4 token estimates) |
| C08 safe task waits | `5e65d7fe8d25952f41678eb71652f5cbd528a54f` | mcpserver/taskboard/hookcli tests | PASS; omitted await defaults to 45s, bounded-duration test 45.00s measured |
| A01 namespace grants | `071d8e3320c07f64a7897ddca7a71c639104aebc` | `go test ./internal/authz ./internal/api ./internal/config ./internal/store ./cmd/punk` + race + disposable PG16 migration | PASS; real binary returned 403 under `deny`; PG up/down/re-up preserved baseline key |
| A02 interface enforcement | `efd18e0107463ad4b0305a64f07a48cd60b52f28` | race authz/api/mcpserver/region + combined integration tests | PASS; encoded-namespace, registry-flood, revocation repros permanent |
| E01 retrieval baselines | `10da2d3ae58572304d2c813cf06479cc975c9fd6` | `go test ./internal/membench ./internal/memory ./cmd/punk -count=1` | PASS; committed artifact `scenarios/membench/baseline-report.json` with honest `+modified` provenance |
| E02 answer/citation eval | `0b60177abae789647be46fbeb21c2734fe9b426f` | `go test ./internal/membench ./internal/reflect ./internal/mcpserver ./cmd/punk -count=1` | PASS; offline CLI report: 4 retrieval runs, 3 answer cases, 2 answered + 1 correct abstention, 0 model calls, exact/support/existence 1 on the tiny fixture |
| G01 typed entities | `316194f5556ec71626161160c2c68aa25f49aa02` | memory/config/membench/cmd tests + vet/build | PASS; exact revision IDs persist, invalid citations no longer expand to batch keys; extraction default-off |
| G02 alias merges | `5dcc5ecffe381cafbf7fff27c3e18bb1bae29585` | memory/membench/api/mcpserver + PG16 race | PASS; memory 94.839s integrated; PG16 merge/enrichment/rollback race 14.345s on a disposable container |
| I01 delta ingest | `35d1b2f0922dc0bd05d456b0f90481f4349754d9` | `go test ./internal/memory ./internal/api ./internal/mcpserver -count=1` | PASS (65.967/31.422/65.872s combined); destination ownership + paginated reconciliation |
| I02 loader adapters | `bfa8c2029454d32c1e05e43d18afcfb69a667b0b` | `go test -race ./internal/ingest` + Windows amd64 build | PASS; race 15.249s; Windows build compile-verified (not native-runtime-tested); bounded process lifecycle |
| P01 durable pipeline | `92fafb7eeb0b13110a65b6e1fc6f71d69a0473ac` | memory/api/mcpserver/store/cmd tests + PG16 fence validation | PASS; integrated memory 68.962s; race TestPipeline/TestReviewerP01 8.681s; migration 0023 |
| P02 ingest dry-run | `9a7ed012216c05d69529ccf2c49d9c9ff99eed1c` | ingest/memory/cost/cmd suites + focused race | PASS; 4 reviewer red-to-green proofs permanent; CLI-only `--dry-run`/`--allow-adapter` |
| R01 retrieval strategies | `ff47cef19426a8716671e44bdc7e736d938cb5b4` | memory/api/mcpserver/membench suites + focused race | PASS; explicit + deterministic `auto`, no classifier call, legacy defaults byte-preserved |
| R02 bounded expansion | `7afd590ffa64677723f8042996a7993b0d9815ad` | memory/reflect/membench + reflect race 24.209s + PG16 neighbor race 10.400s | PASS; Go-API opt-in only; synthetic evaluation measured neutral |
| S01 skill discovery | `497386e34ab7670fb29d97d1343f3ed663fa7b8c` | skillmine/spec/memory/mcpserver/cmd suites | PASS; digest-only identity survives unpublish+retention; version immutability |
| S02 skill improvement | `9b65f9a572c723a791b5a0e8ffff7aa3544eaf50` | skillmine/task/policy + PG16 race (8 concurrent ledger writers) | PASS; contains/absent fixtures check procedure CONTENT, not runtime execution (section 10) |
| H01 summary hierarchies | `9159cb069ec014ddc9667f4877925200435d4d90` | memory 130.856s/reflect 2.774s/membench 14.975s + boundary race | PASS; Go-API opt-in only; Recall checks both observations and summaries |
| Z01 this acceptance | (committed by orchestrator after verification) | this entire report | see sections 3, 5-9 |

Integration also carries reviewer/planning commits between the task
commits (hardening fixes `e861d9c`, `5f3ffaf`, `ad9f377`, `12daa60`,
`9ab3464`, and docs/planning commits); the full list is
`git log --oneline 9ab2b8f..feat/punk-improvement-pipeline`.

## 5. Storage acceptance (both dialects)

Migrations at baseline `9ab2b8f`: through `0021_member_seen`. Tip adds
`0022_namespace_grants` (A01) and `0023_pipeline_runs` (P01), paired per
dialect. Binaries: `punk-tip` built from the verified worktree,
`punk-baseline` built from a detached `9ab2b8f` checkout (discarded
after). `punk migrate` supports `up` (all pending), `down` (one step),
`status`; flags precede the action word.

### PostgreSQL 16 (disposable container `punk-z01-pg-20260907`, `127.0.0.1:15432`)

| Step | Command / evidence | Result |
|---|---|---|
| Fresh install | `punk-tip migrate --config /tmp/punk-Z01-pg/fresh.yaml up` | PASS: `applied 23 migration(s)`; status shows 0022+0023 applied (`pg-fresh.log`) |
| Fresh smoke | serve on `127.0.0.1:19091`; POST remember, GET prefix recall, `memories/search?q=`, `memories/search?q=&strategy=exact` | PASS: fact stored/read; strategy=exact returned the routed envelope `{"mode":"exact","requested_mode":"exact","reasons":["explicit-mode"],...}` |
| Baseline upgrade | `punk-baseline migrate up` -> 21 applied (through 0021); `punk-tip migrate up` -> `applied 2 migration(s)` (0022, 0023) | PASS (`pg-upgrade.log`) |
| Post-upgrade smoke | serve on `127.0.0.1:19092`; write + read in namespace `z01-pg-upgrade` | PASS |
| Rollback | `punk-tip migrate down` x2 -> 0022/0023 pending; `up` -> both re-applied | PASS (`pg-down-up.log`) |
| Test suite on PG | `PUNK_TEST_PG_DSN=... go test -count=1 -p 1 -timeout 900s ./internal/store ./internal/task ./internal/route ./internal/memory` | PASS: store 4.271s, task 6.298s, route 5.363s, memory 376.640s (`pg-suite.log`) |

Test-portability fix (applied, part of this task's commit): the tip's
`internal/memory/skills_runs_test.go` used literal `?` placeholders in
two `db.Rebind` queries and `internal/memory/skills_test.go` used
SQLite-only trigger SQL in four failure-injection tests - six failures
on real PostgreSQL, pre-existing on the clean tip (reproduced by the
reviewer on accepted `9a7ed01`: first query FAIL 0.783s, four trigger
tests FAIL 2.319s with SQLSTATE 42601, retention query FAIL 0.658s).
The reviewer-prepared TEST-ONLY patch
(`/tmp/punk-Z01-pg-skill-test-portability.patch`) was applied verbatim;
post-patch file SHA256 match the prepared manifest
(`skills_test.go` f001069a..., `skills_runs_test.go` 66035f8a...).
No production code changed; SQLite trigger branch retained via a shared
engine-aware helper following the existing G02 PG-trigger pattern; all
fault assertions preserved. Verified here on disposable PG16:

```
PUNK_TEST_PG_DSN=... go test -count=1 -v -p 1 -run 'TestFailedBodyWriteLeavesNoDiscoverableSkill|TestFailedUnpublishKeepsSkillCoherent|TestFailedBodyTombstoneLeavesInvisibleOrphan|TestSkillIdentityHealsFromLiveRows|TestRecordSkillRunRequiresPublishedIdentity|TestSkillRunOutcomeSurvivesRetentionSweep' ./internal/memory
--- PASS x6, ok internal/memory 4.378s
```

The same six tests pass under SQLite in the full suite (section 3).

### SQLite

| Step | Evidence | Result |
|---|---|---|
| Fresh install | `punk-tip migrate --config /tmp/punk-Z01-sqlite/fresh.yaml up` | PASS: 23 applied |
| Fresh smoke | serve on `127.0.0.1:19093`; write + search in `z01-sqlite` | PASS |
| Baseline upgrade | baseline binary 21 applied -> tip binary +2 (0022, 0023) | PASS |
| Rollback | down x2 -> pending; re-up -> applied | PASS |

Log: `sqlite.log`.

The full Go suite (section 3) runs against SQLite by default; the PG
suite above is the second-dialect proof.

## 6. Security inventory (authorization)

Enforcement is opt-in (`authz.enforcement: deny`); disabled keeps the
trusted-deployment behavior. Grant model: exact subject x namespace x
{read, write, admin}, revocable, keyed to verified API-key identity;
local CLI and trusted stdio MCP use local OS authority by design; the
global non-namespaced task ledger keeps its existing semantics; MCP
credential rotation requires reconnect.

The boundary is codified as tested inventories, not prose:

- REST: `internal/api/authz_inventory_test.go` requires every registered
  route in `routePermissionInventory` with exactly one class -
  `classOpen` (probes/chrome), `classGlobal` (ledger, proposals, specs,
  costs, A2A - no namespace memory), `classDiagnostic`
  (namespace-selection derivation only), `classNSPath` (`{ns}` path,
  read for GET/HEAD, write otherwise; SSE/long-poll reauthorizes before
  every delivery), `classNSResolved` (query/cwd-derived namespace,
  enforced at the handler against the FINAL resolved namespace),
  `classAggregate` (brain snapshot/events filtered per source
  namespace), `classMCP` (enforced inside the MCP protocol). An
  unclassified new route fails the inventory test.
- MCP: `internal/mcpserver/authz_inventory_test.go` pins the per-tool
  and per-resource permission inventory and proves enforcement over
  HTTP; per-delivery grant + safe-key-ID revalidation, changed-subject
  session reuse denied, close-before-forget subscription lifecycle.
- Boundary behaviors: `internal/api/authz_boundary_test.go` -
  cross-namespace 403s (read/write/forget/search), forged
  `X-Punk-Subject`/`X-Punk-Namespace` headers never grant, encoded
  namespace variants (`%2F`, `%6E`) denied, brain snapshot/event
  filtering, revocation closing streams and mid-wait task-board polls,
  profile-card gating, read-only callers skip injected bookkeeping and
  cannot write delivery markers, zero-key bootstrap denied on resolved
  namespaces, no namespace-colon leak.

Z01 re-ran the inventory/boundary/grant suites at the tip: PASS (section
3). Red proof 2 (section 9) demonstrates the harness detects a removed
grant check.

## 7. Codex 0.153.4 native acceptance (re-executed at tip)

Procedure: `docs/investigations/codex-0.153.4-terminal-spam.md`, "C06:
repeatable integration acceptance procedure", followed exactly. Temp
Punk server `127.0.0.1:19399` (sqlite, ai off) serving a binary built
from the verified tree; temp CODEX_HOME wired with `punk connect codex`;
one read-only copy of `~/.codex/auth.json` into the temp home (never
printed or written back; temporary copy removed after verification); TUI under tmux 200x50 with `pipe-pane`
raw-byte capture. Fixture directory basename `proj` (any contamination
would carry a word distinct from the user's `punkrecords` report).

| Check | Observation (all times UTC 2026-09-07) |
|---|---|
| Connect twice | Second `punk connect codex` reported every artifact "already up to date" (idempotent) |
| Hook trust gate | First launch showed "Hooks need review"; "Trust all and continue" once activated all four wired events |
| Startup capture | `/agent-sessions/01a07cf8-c766-7302-967f-bcbf2b729a67/start` at 17:44:50.393680566Z (`cwd=/tmp/codex-z01/proj source=codex`) |
| Prompt capture | `/prompt-01a07cf9-0528-...` 17:44:50.414 and `/prompt-01a07cf9-8a2d-...` 17:45:23.852 - native turn UUID verbatim as the capture identity (C01) |
| Tool burst | `/tool-exec-e184ea0f-f236-4d9c-937c-d106b5fc3c63` 17:45:28.854 (`Bash: {"command":"echo z01-tool-probe"}` + output) |
| Stop capture | `/stop` 17:45:33.605 (assistant message body) |
| Resume semantics | `codex resume 01a07cf8-...` fired NO hook at reopen (pinned design: `SessionStartSource::Resume` queues at session.rs:1600,1623 and dispatches at the post-resume turn, turn.rs:264). With one durable fact seeded first, the post-resume turn produced `/delivery` body `resume c4e8b2dcc07057108412d44884940846cb0969e2e3b166028fb83fa44f86d965 issued` at 17:46:37.593763650Z and `/injected` naming the seeded fact at 17:46:37.598; turn's own prompt captured 17:46:37.617, `/stop` 17:46:40.561 |
| Duplicate delivery replay | Identical native SessionStart payloads replayed through the installed hook command (`punk hook --from codex`). Startup replay 1 issued a block (195 bytes - the original startup block was EMPTY because the namespace held no durable facts, and an empty block records no marker by design, so this was the first non-empty startup issue); startup replay 2 silent. Resume replays after the marker read `resume c4e8b2dc...`: silent, exit 0, `/delivery` marker byte-unchanged (`created_at` pinned at 17:47:22.059405378Z across consecutive replays). Once-per-(event, revision) suppression holds at the server-issued boundary |
| Title A/B | Title on (default): 211 OSC 0 sequences in the raw stream (`ESC ] 0 ; proj BEL` and `ESC ] 0 ; <spinner> proj BEL` - payload is the directory basename, matching the C02 rename test). Title off (`-c 'tui.terminal_title=[]'`): 0 sequences; captures unchanged (session `01a07cfc-20d3-73a1-9fff-9b1d558370c2`: `/start` 17:48:29.968, `/prompt-`, `/stop` 17:48:32.384) |
| Grid contamination | None under tmux in either cell (tmux consumes OSC 0); the original host's renderer remains unisolated - the user-confirmed mitigation stands on the original-host A/B, not on this grid |

No secrets and no raw prompt dumps are included; session IDs, timestamps
and OSC counts above are the evidence. The simulated half
(`internal/api/codex_roundtrip_test.go`, hookcli Codex tests) ran in the
section-3 gates; the two halves are never cited as evidence for each
other.

## 8. Benchmark evidence (E01/E02)

Committed fixture: `scenarios/membench/` (`baseline.jsonl` 9-query
corpus, `answers.jsonl`, `sample.jsonl`) and the E01 artifact
`baseline-report.json` (`source_revision`
`736f5ae175a8fe6ba5b267cb24a30e28c0d84042+modified` - honest
modified-tree provenance, not a clean-source claim).

Methodology: deterministic, offline. FTS-only (no embeddings
configured), no LLM judge, no model calls; the answers stage used the
default extractive composer. Preview/context estimates use `bytes/4`;
scripted usage is labeled synthetic, and model-reported usage is a
separate measurement when an actual endpoint is used. The offline
benchmark does not establish billed-token savings.

Fresh run at the verified tip:

```
punk-tip membench --file scenarios/membench/baseline.jsonl \
  --report /tmp/punk-Z01-logs/membench-tip-report.json --answers \
  --commit 9159cb069ec014ddc9667f4877925200435d4d90 \
  --config /tmp/punk-Z01-sqlite/fresh.yaml
```

Result (2026-09-07 17:58): the manifest carries 4 run entries, of which
TWO configurations are available in this offline setup (baseline and
top1 executed; rerank and embed-hybrid are `available: false` - no
reranker/embedder configured - and produce no summaries; that
unavailability is expected offline, not a failed or unrun gate). The
executed baseline arm: `hit_at_k=0.8750`, `evidence_recall_at_k=0.8125`,
`mrr=0.8750` over 9 query entries: 8 have expected evidence, and one
of those 8 is the whitespace query rejected with `memory: empty search
query`. The ninth query is the valid, deliberately unanswerable
'quantum...' case; it is an abstention fixture and did NOT error. Both available arms' summaries are IDENTICAL
to the committed E01 artifact (verified field-by-field); the retrieval
numbers reproduce at the tip. Answers stage: 9 cases, 7 answered, 1
abstained, 1 failed (the same blank-query error); `citation_existence=1.0000`, `support=1.0000`; 0 model calls,
0 judge tokens. The fresh binary reports `source_revision: unknown`
(an unstamped worktree `dev` build - the E01 runtime check that an
unstamped binary must not claim a clean revision is working as
accepted).

E02 reviewer-executed offline report (`/tmp/punk-review-E02-r4-report.json`
at acceptance): 4 retrieval runs, 3 answer cases, 2 answered plus 1
correct abstention, 0 failed, 0 model calls, exact/support/existence = 1
on the tiny fixture.

No controlled comparative run against Cognee (or any external system)
was executed anywhere in this pipeline. This report makes NO Cognee
superiority/inferiority claim; the fixtures establish determinism,
provenance and grading separation on this corpus only. The R02 fixture
is synthetic, shared-callback and altered-evidence controlled; the H01
fixture is a separate synthetic equal-budget off/on coverage comparison;
both measured NEUTRAL - they are not evidence of real answer quality
gains.

## 9. Red proofs (acceptance harness detects reverted boundaries)

Disposable worktree `/tmp/punk-Z01-redproof` at `9159cb0` (created with
`git worktree add --detach`, discarded after; the shared integration
branch was never touched). One surgical production-code revert per
boundary, tests left intact, scoped suite run, then the file restored.
Logs: `red1-c01.log`, `red2-a02.log`, `red3-p01.log`.

| # | Boundary reverted | Detection (all FAILed as required) |
|---|---|---|
| 1 | C01: removed the `turn_id` -> `prompt_id` mapping in `internal/hookcli/normalize.go` (restores the pre-C01 passthrough that silently dropped every native Codex prompt capture) | `go test ./internal/hookcli ./internal/api -run 'Codex\|HookCLI' -count=1`: `TestRunFromCodexUserPromptSubmitMapsTurnID` (`forwarded prompt_id = ""`), `TestRunFromCodexForwardsDeliveryIdentity`, `TestNormalizeCodexUserPromptSubmitTurnID`, `TestCodexIntegrationLifecycle/submit_turn_captures_and_injects_once_per_turn` (`native turn_id capture not stored`), `TestCodexHookRoundTripsThroughServerContract/userPromptSubmit` and `TestCodexUserPromptSubmitReplayIsIdempotent` (`status = "ignored", want stored`) - 6 failures |
| 2 | A02: `authorizeResolved` in `internal/api/authz_boundary.go` made an unconditional pass-through (cross-namespace grant check removed) | `go test ./internal/api -run 'TestAuthzBoundary' -count=1`: `TestAuthzBoundaryRESTDenials` (recall/remember/forget/search/hook/context subtests all `200, want 403`), `TestAuthzBoundaryHeadersNeverGrant` (forged subject `200, want 403`), `TestAuthzBoundaryTaskBoardWaitRevocation`, `TestAuthzBoundaryZeroKeyBootstrapDeniedResolvedNS` |
| 3 | P01: `fencedStageTx` in `internal/memory/pipeline_runs.go` stripped of its run-claim and live-revision fence checks (stage function always commits) | `go test ./internal/memory -run 'TestStaleEmbedWorkerFencedAtWriteBoundary\|TestStaleEntityWorkerFencedAtApplyBoundary\|...' -count=1 -v`: both fence tests FAIL - "stale worker wrote old-revision derived state after the newer revision's run finished" (entity attrs and a `similar_to` link, i.e. duplicate/stale stage output committed). The crash-retry idempotence and status-clobber tests still passed because their guards (ON CONFLICT run rows, attempt-token finish guard) were not reverted - recorded for precision |

3/3 reverted boundaries detected. Files restored and the disposable
worktree removed (`git worktree list` verified).

**Reviewer-provenance canonical duplicate-output proof** (recorded in
punk at `/reviews/Z01/duplicate-output-red-proof`, executed by the
reviewer on production-equivalent `9159cb0` source in isolated
`/tmp/punk-review-R02-H01-integration`): existing
`TestPipelineEntityCrashBeforeAckRetry` is GREEN unmutated (exit 0,
memory 0.186s, `/tmp/punk-Z01-review-duplicate-green.log`); the
disposable mutation `/tmp/punk-Z01-review-duplicate-mutation.patch`
disables ONLY `planEntityApply`'s `existing[e.key]` skip in
`internal/memory/enrich.go`; the same exact test then goes RED (exit 1,
assertion `pipeline_runs_test.go:226` "/entities/acme mention_count = 2,
want 1 (retry must not double-count)",
`/tmp/punk-Z01-review-duplicate-red.log`). This directly detects
duplicate derived accounting after crash-before-ack recovery, not merely
stale-fence rejection; the mutated file was restored byte-for-byte and
never applied to shared/final source.

## 10. Limitations (accurate by design)

- Retention is real deletion: `memory.retention_days` sweeps can remove
  revisions, and raw session capture stops being retrievable after its
  window regardless; no component of this pipeline provides immutable
  eternal history. Derived state pins exact source revision IDs, so a
  swept source reads as stale/unverifiable rather than silently current.
- Token budgets are approximate: every token figure in ingest previews,
  guidance budgets and benchmark manifests is a `bytes/4` estimate (or a
  model-reported usage when a real endpoint is configured); none are
  billed-token measurements. Unknown model prices are reported as
  unknown; positive prices rounded below micro-USD are labeled
  `priced_rounded_zero`, not "free".
- Citation existence is not claim support: citation validation proves
  the cited evidence was retrieved/shown; it does not prove the
  generated claim is true. The benchmark grades the two separately.
- Synthetic fixtures: the E02 answer cases and the R02/H01 evaluation
  harness are small synthetic fixtures with scripted or extractive
  responders; the S02 acceptance fixtures are deterministic
  contains/absent assertions on procedure CONTENT and do not execute a
  procedure or prove runtime task success. None of these measure
  real-world answer quality.
- Codex integration: dedup is a server-issued boundary; there is no host
  acknowledgment and no end-to-end exactly-once guarantee; hosts without
  distinct delivery IDs use a documented bounded resume policy. A resume
  `/start` capture is byte-identical to the startup one (the C01
  envelope hardcodes `source=codex`); the `/delivery` marker's event
  field is the startup/resume discriminator. The original terminal
  renderer was never identified; tmux never reproduced the visible
  contamination, and the mitigation's persistence across future launches
  was not separately verified.
- I02 Windows process lifecycle is compile-verified (`GOOS=windows`
  build PASS) but not native-Windows-runtime-tested here.
- R02/H01 ship as Go-library opt-ins only: no CLI flag, MCP argument or
  config key exists for evidence expansion or summary trees in this
  build (the MCP `reflect` tool exposes only `level` and `schema`).
- The pipeline was accepted on the local integration branch; the live
  `:9090` installation was not upgraded and released artifacts do not
  contain this work.

## 11. Known flakes and policy notes

- `TestIntakeRateLimit` (`internal/api/auth_test.go`) is load-flaky
  under parallel full suites. In this run it PASSED in both the full
  suite (api 41.268s) and the race suite (api 75.997s), so no isolated
  rerun was needed. Policy: if it fails, rerun it isolated with `-race`,
  record both results, and label it known-load-flaky - never silently
  omit the original failure.
- The `internal/memory` race suite exceeds Go's default 600s test
  timeout (800.516s measured here; ~822s measured at H01 acceptance; the
  runtime cause is unproven - H01's accepted review notes the known
  added work of Recall checking BOTH observations and summaries, without
  claiming calibrated timing attribution). Policy: run it with an
  explicit `-timeout 1200s` (this report) and record the duration.
- The six PostgreSQL skill-test failures fixed here were pre-existing on
  the clean tip (section 5); they were test-portability defects
  (SQLite-only SQL in tests), not product defects, and no production
  query was broadened.

## 12. Rollout and rollback

Rollout posture: the NEW retrieval/config surfaces are opt-in or
preserve the legacy path when their parameter is omitted (fixes and
durable bookkeeping - C01/C03/C04/P01/A02 - deliberately change normal
behavior and are NOT byte-identical; that is their purpose):

- `authz.enforcement` defaults to the trusted behavior; `deny` is
  opt-in (A01/A02).
- Entity extraction, contradiction passes, reranker, vector
  quantization, IVF index: all pre-existing or new opt-ins, unchanged
  defaults (G01; `memory.entities`, `memory.contradictions` off).
- Retrieval strategies require an explicit `strategy` parameter;
  omitting it keeps the legacy fused listing (R01). Evidence expansion
  and summary trees require explicit Go-API opt-ins (R02/H01).
- `punk ingest --dry-run` only previews; normal ingest behavior is
  unchanged (P02). Skill improvement proposals require explicit approval
  and application (S02). Enrichment durability adds bookkeeping and
  fences without changing stage semantics (P01).
- Codex hooks remain fail-open (a dead server never breaks the coding
  session); the title mitigation is a user-side Codex config setting,
  not a Punk behavior change (C02/C06).

Upgrade steps for an operator choosing to deploy (NOT performed here):
build the branch, run `punk migrate --config <cfg> up` (applies 0022,
0023), restart the service, then opt into features individually.

Feature opt-out and schema rollback are different operations. Removing
an optional retrieval parameter or disabling a supported feature flag
does not require dropping its data. Disabling `authz.enforcement: deny`
relaxes namespace authorization; existing grant rows alone do not keep
enforcement active.

For a schema downgrade, stop the service and workers first and take a
restorable database backup, including namespace grants and pipeline-run
history/state. `punk migrate --config <cfg> down` twice drops
`memory_pipeline_runs` (0023) and `namespace_grants` (0022). This destroys
those records; the tested down/re-up sequence proves schema reversibility,
not a lossless recovery of grants or retry history. Re-applying the up
migrations recreates empty tables and does not restore the dropped rows.
Use the compatible baseline binary and configuration only after the
schema downgrade; restore a compatible backup when state preservation
is required. The pre-authorization baseline cannot preserve the newer
namespace-enforcement guarantees merely by retaining configuration.

No deployment, release, tag, push or merge to main is part of this task;
the orchestrator owns the Z01 commit and any integration decision after
independent verification.
