# Punkrecords improvement pipeline implementation plan

Design brief: [design](../specs/2026-09-05-punk-improvement-pipeline-design.md).
Worker instructions: [worker prompt](2026-09-05-punk-improvement-pipeline-WORKER-PROMPT.md).
Integrator instructions: [review prompt](2026-09-05-punk-improvement-pipeline-REVIEWER-PROMPT.md).
Cognee workstream refinement: [borrowing plan](2026-09-06-cognee-borrowing.md).
Machine-readable task contracts: [manifest](2026-09-05-punk-improvement-pipeline.tasks.json).

Namespace: `punk-punkrecords-improvement`. Integration branch: `feat/punk-improvement-pipeline`.
Local baseline: `9ab2b8f`. Do not assume origin has this branch. The planning commit makes all these files available in local worker worktrees.

## Execution contract

Use the task's contract and acceptance tests as the specification; proposed new filenames are new, not claims they already exist. Read current source before editing. Adapt signatures to the current tree and record material deviations. This plan specifies observable interfaces and red/green proofs rather than speculative full patches across 23 tasks.

For each implementation task:
1. Verify dependency commits are on the integration branch; claim task and affected paths.
2. Add the smallest regression test proving the requested behavior; run it and save the expected failure.
3. Implement the narrow change, preserving all compatibility constraints.
4. Run task tests, gofmt on changed files, and go vet/go build for affected packages. Run the full gate when required by cross-cutting work.
5. Inspect git diff; stage only task-owned files; create one commit with the specified message, no attribution trailers.
6. Submit status review with worker branch, SHA, failing/passing evidence and deviations; release claims.
7. Reviewer integrates under the integration lease, reruns relevant checks and marks done with the integrated SHA. Only then may dependents start.

Testing-only tasks must prove their test detects a reverted/broken boundary in a throwaway checkout; no artificial product regression is committed. Native UI acceptance cannot be replaced by unit assertions. If a task cannot meet its acceptance criterion, record the exact blocker and preserve unrelated progress.

## Task map

| ID | Task | Depends on |
|---|---|---|
| C01 | Normalize Codex 0.153.4 native hook events | none |
| C02 | Diagnose and stop Codex terminal/footer repetition | none |
| C03 | Detect and reconcile duplicate managed Codex integrations | none |
| C04 | Make repeated context delivery idempotent | C01, C02 |
| C05 | Verify hook and MCP namespace alignment | C03 |
| C07 | Bound repeated memory guidance in agent context | C03 |
| C06 | Gate the complete Codex 0.153.4 integration | C01, C02, C03, C04, C05, C07 |
| E01 | Create reproducible retrieval baselines and ablations | none |
| E02 | Evaluate answer quality and citation support | E01 |
| A01 | Add an explicit namespace authorization model | none |
| A02 | Enforce namespace permissions at every external memory boundary | A01 |
| G01 | Add typed entities and optional domain vocabulary | E01, A02 |
| G02 | Resolve aliases with reversible, type-aware merge proposals | G01, E02 |
| I01 | Preserve document and chunk source provenance | E01, A02 |
| I02 | Add optional text and document loader adapters | I01 |
| S01 | Discover procedural skills without loading full procedures | E01, A02 |
| S02 | Record skill outcomes and propose versioned improvements | S01, E02 |
| R01 | Route retrieval with explicit, inspectable strategies | E01, G02, S01 |
| R02 | Add optional budgeted evidence expansion to reflect | R01, E02 |
| P01 | Persist enrichment stage status and retry lineage | E01, A02 |
| P02 | Preview ingestion and enrichment work before execution | P01, I02, G02 |
| H01 | Build opt-in source-linked hierarchical summaries | E02, P01, G02, R01 |
| Z01 | Run cross-feature acceptance and publish the local evidence report | C06, A02, G02, I02, S02, R02, P02, H01 |

Scheduling revision 2026-09-06: E01 and A01 have no dependencies and can start while C02 remains blocked and C05 is reviewed. A02 depends only on A01; selecting a namespace and authorizing it are separate concerns. Keep the C-series dependencies unchanged. Z01 explicitly depends on C06, so combined completion still requires native Codex evidence. E01 and A01 share cmd/punk/main.go: serialize that file using exact claims. Ready tasks may run concurrently in distinct worktrees when files do not overlap.

## Source borrowing

Use Cognee commit 78ff576559a7f75f65884c5bd90b22cdc790016e as the research baseline. Relevant references: cognee/eval_framework; modules/pipelines/layers/resolve_authorized_user_datasets.py; tasks/memify/consolidate_entities.py; infrastructure/loaders; modules/retrieval/skills_retriever.py; modules/memify/skill_improvement.py; api/v1/recall/query_router.py; modules/retrieval/graph_completion_context_extension_retriever.py; modules/cognify/estimator.py; tasks/memify/global_context_index. Borrow mechanisms selectively, preferably reimplement in Go. Preserve attribution/license notices for adapted code; do not copy separately licensed components.

## Task C01: Normalize Codex 0.153.4 native hook events

**Dependencies:** none.
**Files:** `internal/hookcli/normalize.go`, `internal/hookcli/hookcli.go`, `internal/hookcli/connect_codex_test.go`, `internal/api/hookcli_roundtrip_test.go`, `internal/hookcli/testdata/codex-0.153.4/ (new)`.

**Contract and implementation boundary:** Add an explicit Codex normalizer at RunFrom's source boundary. For UserPromptSubmit preserve a supplied prompt_id, otherwise map the native turn_id to the stable capture prompt_id. Preserve session identity and tool IDs. Capture native SessionStart/UserPromptSubmit/PostToolUse/Stop shapes with provenance to upstream rust-v0.153.4; do not change Claude's input contract or hash prompt text as identity.

**Red proof:** A native UserPromptSubmit fixture containing turn_id but no prompt_id must create exactly one prompt fact through a temporary API server; replaying it is idempotent and a distinct turn with identical text remains a distinct capture.

**Acceptance:** All four fixtures exercise the real hook-to-API boundary. Submit capture is retained; capture-only hooks have empty stdout; valid nested additionalContext is produced only for supported injection events. Malformed input and unavailable server remain fail-open.

**Checks:** `go test ./internal/hookcli ./internal/api -run 'Codex|HookCLI' -count=1`. Use existing package test helpers and temporary DBs. PostgreSQL coverage is required whenever SQL/schema changes.

**Commit:** `fix(codex): normalize native hook event identifiers`.

**Handoff:** status review with exact worker branch/SHA and check results. Reviewer marks done only after integration; all dependencies refer to integrated done states.

## Task C02: Diagnose and stop Codex terminal/footer repetition

**Dependencies:** none.
**Files:** `internal/hookcli/connect_codex.go`, `internal/hookcli/connect_codex_test.go`, `internal/hookcli/hookcli.go`, `docs/investigations/codex-0.153.4-terminal-spam.md (new)`.

**Contract and implementation boundary:** Use the user screenshot codex-spam.png: repeated literal punkrecords appears after the transcript overlay's static q-to-quit/esc-to-edit footer. Upstream that footer has no memory/cwd text. Leading hypothesis is terminal title/OSC or other out-of-band output corruption; this is not a proven Punk hook defect. Trace emitted bytes and effective terminal/shell configuration, compare terminal-title enabled/disabled and Punk hooks enabled/disabled independently in temporary config. Verify the exact terminal emulator and whether this is CLI or embedded app. Apply a narrow Punk fix only if the producer is Punk; otherwise provide a tested supported configuration mitigation and a version-pinned upstream reproduction without modifying Codex source inside this repo.

**Red proof:** Record the visible native 0.153.4 footer/composer corruption plus PTY/output evidence. Run a four-cell A/B matrix (title on/off x Punk hooks on/off) with fixed actions and no secrets. Try one-shot codex -c 'tui.terminal_title=[]' using a temporary fixture/config. Rename the fixture directory to a different basename to test whether the repeated word follows the project title. Preserve the original screenshot as user-owned input; do not commit it by default.

**Acceptance:** Show the selected remediation stops footer/input contamination in the user's host while preserving useful memory capture/context, and separately classify any actual repeated hook messages. Do not claim fixed from Go mocks or duplicate skill cleanup. Upstream context-only successful hooks are hidden and the installed config has no custom statusMessage. If the environment cannot reproduce the symptom or validate a mitigation, keep blocked with exact evidence; no blind suppressOutput flags (PostToolUse rejects them).

**Checks:** `go test ./internal/hookcli ./internal/api -count=1 if Punk code changes; native Codex 0.153.4 terminal/title/hooks A/B matrix with sanitized output capture`. Use existing package test helpers and temporary DBs. PostgreSQL coverage is required whenever SQL/schema changes.

**Commit:** `fix(codex): address terminal footer repetition`.

**Handoff:** status review with exact worker branch/SHA and check results. Reviewer marks done only after integration; all dependencies refer to integrated done states.

## Task C03: Detect and reconcile duplicate managed Codex integrations

**Dependencies:** none.
**Files:** `internal/hookcli/skillpaths.go`, `internal/hookcli/skillpaths_test.go`, `internal/hookcli/connect_codex.go`, `internal/hookcli/connect_codex_test.go`, `internal/hookcli/skill.go`, `internal/hookcli/planskill.go`.

**Contract and implementation boundary:** Detect both shared ~/.agents/skills and CODEX_HOME/skills installations plus global/project hook registrations and duplicate Punk MCP aliases. Resolve only byte-equivalent, Punk-managed duplicate skills/hooks; preserve customized and foreign content and present actionable collision diagnostics. Respect different client tool-prefix needs instead of deleting another client's required skill. Prefer a common discoverable neutral skill only when compatible with all affected clients.

**Red proof:** Temporary-home fixtures with both managed skill roots and global/project hooks produce duplicate effective entries before repair; a mixed custom/managed fixture must not lose custom bytes.

**Acceptance:** Repeated connect is idempotent; the effective Codex catalog has one intended entry per skill and one execution per intended event, or a precise unresolved-collision diagnostic. Custom CODEX_HOME, symlinks, CRLF and project scopes are covered. No automatic changes to the current user's installed files during tests.

**Checks:** `go test ./internal/hookcli -run 'Skill|ConnectCodex|MCP' -count=1`. Use existing package test helpers and temporary DBs. PostgreSQL coverage is required whenever SQL/schema changes.

**Commit:** `fix(connect): reconcile duplicate managed Codex entries`.

**Handoff:** status review with exact worker branch/SHA and check results. Reviewer marks done only after integration; all dependencies refer to integrated done states.

## Task C04: Make repeated context delivery idempotent

**Dependencies:** C01, C02.
**Files:** `internal/api/agent_handlers.go`, `internal/api/agent_handlers_test.go`, `internal/hookcli/hookcli.go`, `internal/hookcli/connect_codex_test.go`, `internal/memory/session.go`, `internal/store/migrations/{sqlite,postgres}/ (only if persistence is needed)`.

**Contract and implementation boundary:** Define delivery identity using namespace, session, event/turn and context revision, plus an explicit issued/acknowledged/retry state boundary. Suppress duplicate concurrent processing of an event, including profile, summary and entities. Do not promise end-to-end exactly-once rendering without a Codex host acknowledgement. Define behavior if the connection fails after a response is produced or the hook exits before stdout is consumed. Deliberate resume refreshes once per identified resume event; distinguish duplicate transport delivery from a fresh resume, or document a bounded policy if the host lacks a delivery ID.

**Red proof:** Concurrent requests for the same delivery share a stable logical delivery identity and do not amplify bookkeeping. Inject disconnect/crash failures before response, after response and before client output; tests assert the documented replay/dedup boundary rather than unobservable host consumption. A distinct turn/revision/session remains eligible.

**Acceptance:** Repeated healthy event delivery is deduplicated at the documented boundary; changed facts are eligible again; namespaces and sessions are isolated. Failed fetch is not recorded as confirmed delivery. Issued versus acknowledged state and any unavoidable loss/duplication window are explicit in code and documentation. Existing per-turn fact dedup remains effective.

**Checks:** `go test -race ./internal/api ./internal/hookcli ./internal/memory -count=1`. Use existing package test helpers and temporary DBs. PostgreSQL coverage is required whenever SQL/schema changes.

**Commit:** `fix(memory): deduplicate repeated context delivery`.

**Handoff:** status review with exact worker branch/SHA and check results. Reviewer marks done only after integration; all dependencies refer to integrated done states.

## Task C05: Verify hook and MCP namespace alignment

**Dependencies:** C03.
**Files:** `internal/hookcli/verify.go`, `internal/hookcli/verify_test.go`, `internal/hookcli/connect_codex.go`, `internal/hookcli/namespace.go`, `internal/mcpserver/namespace.go`, `internal/mcpserver/namespace_test.go`, `cmd/punk/main.go`.

**Contract and implementation boundary:** Extend connect/verify diagnostics to compare effective capture namespace with MCP whoami using the same project and credentials. Project connections must pin the remote-derived namespace when roots are unavailable. A global connection spanning several repos must not pin all repos to the planner's cwd. Distinguish explicit namespace overrides from accidental fallback. Report existing data locations without copying or deleting data.

**Red proof:** No-roots MCP fixture yields agent-default while the same repository hook resolves a remote namespace; verify must detect this mismatch and a project connection must resolve both sides identically.

**Acceptance:** Cover roots/no roots, explicit headers, two clones, two unrelated repos, custom CODEX_HOME and remote/global connections. whoami reports source. The observed agent-default versus agent-punk-records-75eeb0 mismatch is represented by sanitized fixtures.

**Checks:** `go test ./internal/hookcli ./internal/mcpserver ./internal/api -run 'Namespace|Verify|Codex' -count=1`. Use existing package test helpers and temporary DBs. PostgreSQL coverage is required whenever SQL/schema changes.

**Commit:** `fix(connect): verify memory namespace alignment`.

**Handoff:** status review with exact worker branch/SHA and check results. Reviewer marks done only after integration; all dependencies refer to integrated done states.

## Task C07: Bound repeated memory guidance in agent context

**Dependencies:** C03.
**Files:** `internal/mcpserver/instructions.go`, `internal/mcpserver/server_test.go`, `internal/hookcli/skill.go`, `internal/hookcli/skill_test.go`, `internal/hookcli/planskill.go`, `internal/mcpserver/toolset.go`.

**Contract and implementation boundary:** Measure initialization instructions, actual tools/list descriptions and installed skill text separately. Determine whether this host prepends initialize instructions to each exposed tool before changing server descriptions. Keep concise tool-specific descriptions, one canonical routing explanation and discoverable skill references. Remove only verified redundant server-owned content; preserve standalone MCP use without installed skills.

**Red proof:** Snapshot byte/token estimates for the effective agent toolset and initialization payload; pin an upper bound derived from the measured baseline and test that all retrieval and coordination routing guidance remains discoverable.

**Acceptance:** Report before/after payload sizes using the same estimator and actual MCP responses. No lost tool capabilities or ambiguous namespace instructions. Distinguish model-context duplication from the user's UI spam; this task cannot close C02.

**Checks:** `go test ./internal/mcpserver ./internal/hookcli -run 'Instruction|Skill|Toolset|Budget' -count=1`. Use existing package test helpers and temporary DBs. PostgreSQL coverage is required whenever SQL/schema changes.

**Commit:** `perf(mcp): bound repeated memory guidance`.

**Handoff:** status review with exact worker branch/SHA and check results. Reviewer marks done only after integration; all dependencies refer to integrated done states.

## Task C06: Gate the complete Codex 0.153.4 integration

**Dependencies:** C01, C02, C03, C04, C05, C07.
**Files:** `internal/api/codex_roundtrip_test.go (new)`, `docs/investigations/codex-0.153.4-terminal-spam.md`, `docs/CONFIG.md`, `README.md`.

**Contract and implementation boundary:** Create a repeatable integration acceptance procedure with an isolated config directory and temporary Punk DB. Exercise connect twice, startup, submit, tool burst, stop, resume, duplicate delivery, slow/offline server and foreign configuration. Preserve source-version references and separate native observations from simulated tests.

**Red proof:** The end-to-end acceptance scenario must detect the original spam or documented boundary regression when the responsible fix is reverted, and detect the missing prompt_id capture on the old adapter.

**Acceptance:** All Codex acceptance criteria in the brief pass at 0.153.4. Exact transcript spam trigger and remediation are documented, or task remains blocked; no claim that a CLI test proves a different app's renderer. Installed live Codex/Punk files remain untouched.

**Checks:** `go test ./internal/hookcli ./internal/api ./internal/mcpserver -count=1; run documented native 0.153.4 acceptance; go vet ./...`. Use existing package test helpers and temporary DBs. PostgreSQL coverage is required whenever SQL/schema changes.

**Commit:** `test(codex): cover native memory integration lifecycle`.

**Handoff:** status review with exact worker branch/SHA and check results. Reviewer marks done only after integration; all dependencies refer to integrated done states.

## Task E01: Create reproducible retrieval baselines and ablations

**Dependencies:** none.
**Files:** `internal/membench/membench.go`, `internal/membench/membench_test.go`, `internal/membench/locomo.go`, `internal/membench/report.go (new)`, `scenarios/membench/`, `cmd/punk/main.go`.

**Contract and implementation boundary:** Extend the existing benchmark with a versioned run manifest: corpus hash, seed, commit, embedding/reranker IDs, retrieval settings and warm/cold mode. Persist per-query rankings, latency and aggregate recall/MRR. Add deterministic cases for corrections, exact identifiers, multi-hop evidence, aliases, time windows and missing answers. Keep LoCoMo evidence recall explicitly distinct from answer accuracy. The legacy RecallAtK currently means any-hit rate; preserve compatibility explicitly and expose correctly named hit_at_k, evidence recall_at_k and mrr in a versioned report. Evidence recall is retrieved unique expected IDs divided by all unique expected IDs. Empty-gold/unanswerable cases have a separate denominator; report retrieval errors and unavailable ablations explicitly. Exclude run IDs, wall-clock timestamps, measured cost and timing from stable-manifest comparisons.

**Red proof:** A fixed fixture and seed must yield reproducible rankings and manifest content excluding documented volatile fields; changing corpus or strategy must change the recorded manifest. Include two required facts with only one retrieved: hit_at_k=1 and evidence recall_at_k=0.5; duplicate results cannot inflate recall. Unanswerable cases must not cause division by zero or disappear from the report.

**Acceptance:** One command writes machine-readable results for baseline and ablations. No default paid model calls. Fixtures avoid cross-namespace leakage; failed queries are counted rather than silently dropped. Use k=5 and fixed seed 1 for the initial offline fixture report; fixtures include every category named in the contract. Legacy output semantics remain documented, and no feature is called an improvement from the legacy any-hit metric alone.

**Checks:** `go test ./internal/membench ./internal/memory ./cmd/punk -count=1`. Use existing package test helpers and temporary DBs. PostgreSQL coverage is required whenever SQL/schema changes.

**Commit:** `feat(membench): record reproducible retrieval baselines`.

**Handoff:** status review with exact worker branch/SHA and check results. Reviewer marks done only after integration; all dependencies refer to integrated done states.

**Borrowing evidence:** [cognee/eval_framework/run_eval.py](https://github.com/topoteretes/cognee/blob/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/eval_framework/run_eval.py), [cognee/eval_framework/evaluation/metrics/context_coverage.py](https://github.com/topoteretes/cognee/blob/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/eval_framework/evaluation/metrics/context_coverage.py). Priority P0; see the borrowing plan for rollout boundaries.

## Task E02: Evaluate answer quality and citation support

**Dependencies:** E01.
**Files:** `internal/membench/answers.go (new)`, `internal/membench/answers_test.go (new)`, `internal/reflect/reflect.go`, `internal/membench/report.go`, `cmd/punk/main.go`.

**Contract and implementation boundary:** Add an optional answer stage with exact/structured grading where possible and a pluggable judge for semantic correctness. Save answers, retrieved IDs, cited IDs, abstention decisions, token usage and judge configuration. Score citation existence separately from claim support. Distinguish unanswerable examples from retrieval misses; fixed test doubles must cover CI.

**Red proof:** An answer citing a real but unrelated fact must pass ID-existence and fail support in a labeled fixture; an unanswerable question answered confidently must fail abstention.

**Acceptance:** Report answer accuracy, evidence metrics and cost separately with per-case artifacts. Model-backed runs require explicit endpoint/model/budget configuration and never use the live memory namespace. No unsupported cross-product winner claim.

**Checks:** `go test ./internal/membench ./internal/reflect -count=1`. Use existing package test helpers and temporary DBs. PostgreSQL coverage is required whenever SQL/schema changes.

**Commit:** `feat(membench): evaluate answers and citation support`.

**Handoff:** status review with exact worker branch/SHA and check results. Reviewer marks done only after integration; all dependencies refer to integrated done states.

**Borrowing evidence:** [cognee/eval_framework/evaluation/evaluator_adapters.py](https://github.com/topoteretes/cognee/blob/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/eval_framework/evaluation/evaluator_adapters.py), [cognee/eval_framework/evaluation/metrics/exact_match.py](https://github.com/topoteretes/cognee/blob/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/eval_framework/evaluation/metrics/exact_match.py). Priority P0; see the borrowing plan for rollout boundaries.

## Task A01: Add an explicit namespace authorization model

**Dependencies:** none.
**Files:** `internal/authz/ (new)`, `internal/api/auth.go`, `internal/api/auth_test.go`, `internal/config/config.go`, `internal/config/config_test.go`, `internal/store/migrations/{sqlite,postgres}/`, `cmd/punk/main.go`.

**Contract and implementation boundary:** Introduce subject-to-namespace read/write/admin grants with an explicit enforcement mode. Existing trusted single-user deployments remain compatible when enforcement is off; when enabled use deny-by-default. Derive identity from verified credentials, never a user-controlled namespace/header or agent label. Specify local stdio trust separately. Allocate the next free matching migration number on both dialects. First version uses exact namespace grants only (no wildcard/prefix matching). Read, write and admin are distinct operations; no implicit inheritance. Enabled mode denies an empty verified subject and zero-key bootstrap; disabled mode keeps existing trusted bootstrap behavior. Administrative grant provisioning must be usable through an explicit local CLI/config path without deploying it to the live server; document recovery and local stdio trust.

**Red proof:** An authenticated subject with only namespace A read permission is denied namespace B and denied A writes when enforcement is enabled; compatibility mode preserves current bootstrap behavior. Test zero-key bootstrap with enforcement enabled, an empty subject, exact-name confusion, explicit admin grants, and revocation.

**Acceptance:** Policy decisions cover exact namespace grants, rejection of wildcard/prefix patterns, grant revocation, empty subject and distinct admin operations. Migration up/down and default behavior are tested in SQLite and PostgreSQL. No live grant or configuration changes.

**Checks:** `go test ./internal/authz ./internal/api ./internal/config ./internal/store ./cmd/punk -count=1`. Use existing package test helpers and temporary DBs. PostgreSQL coverage is required whenever SQL/schema changes.

**Commit:** `feat(authz): add namespace permission grants`.

**Handoff:** status review with exact worker branch/SHA and check results. Reviewer marks done only after integration; all dependencies refer to integrated done states.

**Borrowing evidence:** [cognee/modules/pipelines/layers/resolve_authorized_user_datasets.py](https://github.com/topoteretes/cognee/blob/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/modules/pipelines/layers/resolve_authorized_user_datasets.py). Priority P0; see the borrowing plan for rollout boundaries.

## Task A02: Enforce namespace permissions at every external memory boundary

**Dependencies:** A01.
**Files:** `internal/api/server.go`, `internal/api/memory_handlers.go`, `internal/api/agent_handlers.go`, `internal/api/brain_handlers.go`, `internal/api/task_board_handlers.go`, `internal/mcpserver/server.go`, `internal/mcpserver/tasks.go`, `internal/mcpserver/namespace.go`.

**Contract and implementation boundary:** Apply the common authorizer before reads/writes across REST, HTTP MCP, subscriptions/SSE, hook context/capture, task board, claims, brain snapshots, exports/imports and cross-region operations. Produce an endpoint/tool permission inventory. Do not let roots, ns headers or explicit tool arguments grant access; prevent unauthorized resource enumeration and subscription delivery. Namespace selection diagnostics in C05 are not an implementation dependency: authorize the final resolved namespace using verified identity at each actual request boundary. Include inherited MCP session identity, case-insensitive headers and subscription reauthorization in the inventory.

**Red proof:** Table-driven REST/MCP tests use a valid A-only credential to attempt B access on every inventory entry, including query overrides and resource subscriptions.

**Acceptance:** All external surfaces are inventoried and enforced in enabled mode; trusted disabled mode is compatible. Revocation affects subsequent subscription deliveries according to documented policy. Cross-region calls check source and destination separately.

**Checks:** `go test -race ./internal/authz ./internal/api ./internal/mcpserver ./internal/region -count=1`. Use existing package test helpers and temporary DBs. PostgreSQL coverage is required whenever SQL/schema changes.

**Commit:** `feat(authz): enforce memory permissions across interfaces`.

**Handoff:** status review with exact worker branch/SHA and check results. Reviewer marks done only after integration; all dependencies refer to integrated done states.

**Borrowing evidence:** [cognee/modules/pipelines/layers/resolve_authorized_user_datasets.py](https://github.com/topoteretes/cognee/blob/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/modules/pipelines/layers/resolve_authorized_user_datasets.py). Priority P0; see the borrowing plan for rollout boundaries.

## Task G01: Add typed entities and optional domain vocabulary

**Dependencies:** E01, A02.
**Files:** `internal/memory/entity.go`, `internal/memory/entity_test.go`, `internal/memory/enrich.go`, `internal/config/config.go`, `cmd/punk/main.go`.

**Contract and implementation boundary:** Add an optional structured extractor interface returning name, type, aliases and source fact IDs while adapting the legacy name-only interface. Initial types: service, repository, database, host, incident, person and unknown. Canonical IDs are distinct from display names; existing /entities keys remain addressable. Use a small optional vocabulary, not a general OWL engine.

**Red proof:** Same display name for a person and service remains distinct; legacy string extractor remains supported; malformed structured results do not write partial invalid entities.

**Acceptance:** Disabled mode is unchanged. New entities retain provenance and declared types, unknown types have an explicit policy, batching and fallback remain correct. Compare fixture retrieval against E01.

**Checks:** `go test ./internal/memory ./internal/config ./cmd/punk -count=1`. Use existing package test helpers and temporary DBs. PostgreSQL coverage is required whenever SQL/schema changes.

**Commit:** `feat(memory): add typed entity extraction`.

**Handoff:** status review with exact worker branch/SHA and check results. Reviewer marks done only after integration; all dependencies refer to integrated done states.

**Borrowing evidence:** [cognee/tasks/memify/consolidate_entities.py](https://github.com/topoteretes/cognee/blob/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/tasks/memify/consolidate_entities.py). Priority P1; see the borrowing plan for rollout boundaries.

## Task G02: Resolve aliases with reversible, type-aware merge proposals

**Dependencies:** G01, E02.
**Files:** `internal/memory/entity.go`, `internal/memory/entity_test.go`, `internal/memory/entity_resolve.go (new)`, `internal/memory/entity_resolve_test.go (new)`, `internal/memory/links.go`.

**Contract and implementation boundary:** Resolve exact aliases first, then type-compatible contextual/embedding candidates. Add dry-run merge proposals with evidence and protected types. Applying a proposal records canonical aliases and lineage while preserving old IDs and historical facts; no destructive node deletion or broad fuzzy-name auto-merge. Remove bounded-scan blind spots through paginated candidate discovery.

**Red proof:** Alice/Alice Chen may resolve with supporting identity evidence; two different services sharing a short name do not merge; proposal replay is idempotent; rejected proposals leave data untouched.

**Acceptance:** Measure false merges and missed aliases on labeled fixtures. Types constrain matching, source provenance survives, old references resolve, undo or supersession is defined and tested.

**Checks:** `go test ./internal/memory ./internal/membench -count=1`. Use existing package test helpers and temporary DBs. PostgreSQL coverage is required whenever SQL/schema changes.

**Commit:** `feat(memory): propose reversible entity alias merges`.

**Handoff:** status review with exact worker branch/SHA and check results. Reviewer marks done only after integration; all dependencies refer to integrated done states.

**Borrowing evidence:** [cognee/tasks/memify/consolidate_entities.py](https://github.com/topoteretes/cognee/blob/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/tasks/memify/consolidate_entities.py). Priority P1; see the borrowing plan for rollout boundaries.

## Task I01: Preserve document and chunk source provenance

**Dependencies:** E01, A02.
**Files:** `internal/memory/document.go`, `internal/memory/document_test.go`, `internal/memory/document_source.go (new)`, `internal/api/memory_handlers.go`, `internal/mcpserver/server.go`.

**Contract and implementation boundary:** Add source-aware document input containing stable source ID/URI, revision/content hash, media type, section/page and offsets. Retain the existing text-only API. Track stable chunk identity so inserting an early paragraph does not rewrite every unaffected later paragraph; define deterministic handling of repeated identical paragraphs. Keep source metadata secret-scrubbed under the same namespace policy.

**Red proof:** Insert a paragraph at the beginning of a document and assert unchanged later chunks preserve IDs/provenance; deleting a section tombstones only owned chunks; legacy input still behaves compatibly.

**Acceptance:** Reingestion is idempotent; provenance points to exact retained source revisions, old fact citations remain resolvable within retention, and export/import round-trips source metadata.

**Checks:** `go test ./internal/memory ./internal/api ./internal/mcpserver -count=1`. Use existing package test helpers and temporary DBs. PostgreSQL coverage is required whenever SQL/schema changes.

**Commit:** `feat(ingest): preserve stable chunk provenance`.

**Handoff:** status review with exact worker branch/SHA and check results. Reviewer marks done only after integration; all dependencies refer to integrated done states.

**Borrowing evidence:** [cognee/infrastructure/loaders](https://github.com/topoteretes/cognee/tree/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/infrastructure/loaders). Priority P1; see the borrowing plan for rollout boundaries.

## Task I02: Add optional text and document loader adapters

**Dependencies:** I01.
**Files:** `internal/ingest/ (new)`, `cmd/punk/main.go`, `internal/memory/document.go`, `docs/CONFIG.md`.

**Contract and implementation boundary:** Define a Loader interface producing source-aware document sections and implement plain text, Markdown, HTML and structured incident JSON. Provide an optional external-process adapter contract for PDF extraction rather than embedding a Python/OCR stack. Limit file size and process time; make URL fetching explicit and protect server-side private-network access if supported. Normalize into I01 instead of creating a second write path.

**Red proof:** Fixtures preserve Markdown headings, HTML text order and incident fields with source locations; unavailable PDF adapter yields an actionable error without a partial successful ingest.

**Acceptance:** Static binary remains useful without external loaders. Repeated ingestion writes no unchanged chunks. Unsupported formats and extraction errors are visible, and all writes pass defense/provenance handling.

**Checks:** `go test ./internal/ingest ./internal/memory ./cmd/punk -count=1`. Use existing package test helpers and temporary DBs. PostgreSQL coverage is required whenever SQL/schema changes.

**Commit:** `feat(ingest): add optional source loader adapters`.

**Handoff:** status review with exact worker branch/SHA and check results. Reviewer marks done only after integration; all dependencies refer to integrated done states.

**Borrowing evidence:** [cognee/infrastructure/loaders](https://github.com/topoteretes/cognee/tree/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/infrastructure/loaders). Priority P1; see the borrowing plan for rollout boundaries.

## Task S01: Discover procedural skills without loading full procedures

**Dependencies:** E01, A02.
**Files:** `internal/skillmine/`, `internal/spec/`, `internal/memory/skills.go (new)`, `internal/mcpserver/server.go`, `internal/mcpserver/toolset.go`.

**Contract and implementation boundary:** Index existing SKILL.md metadata (name, description, version, declared tools, scope) for lexical/semantic discovery. Return metadata-only results and load the exact versioned body on demand. Reuse existing spec loading and generated skill formats; do not duplicate entire procedures into every prompt. Enforce namespace authorization when enabled.

**Red proof:** Searching for a procedure returns metadata without procedure text; loading its ID/version returns the matching body; inactive/unauthorized skills are excluded and no-embedder search works.

**Acceptance:** Existing authored and mined skills are discoverable. Missing skill collection is a normal empty result, permission checks hold, and discovery payload stays within an explicit measured size bound.

**Checks:** `go test ./internal/skillmine ./internal/spec ./internal/memory ./internal/mcpserver -count=1`. Use existing package test helpers and temporary DBs. PostgreSQL coverage is required whenever SQL/schema changes.

**Commit:** `feat(skills): add scoped procedural discovery`.

**Handoff:** status review with exact worker branch/SHA and check results. Reviewer marks done only after integration; all dependencies refer to integrated done states.

**Borrowing evidence:** [cognee/modules/retrieval/skills_retriever.py](https://github.com/topoteretes/cognee/blob/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/modules/retrieval/skills_retriever.py). Priority P1; see the borrowing plan for rollout boundaries.

## Task S02: Record skill outcomes and propose versioned improvements

**Dependencies:** S01, E02.
**Files:** `internal/skillmine/`, `internal/task/ledger.go`, `internal/memory/skills.go`, `internal/policy/proposals.go`.

**Contract and implementation boundary:** Record run outcome against the exact skill version and originating ledger/task evidence. Generate explicit improvement proposals from failed or successful trajectories; separate proposal generation from approval/application. Reuse the existing proposal mechanism where its semantics fit, without giving a skill draft automatic authority to edit active procedures.

**Red proof:** A run references the original version after a new version is proposed; an unapproved draft cannot replace the active procedure; applying the same approved proposal twice has one effect.

**Acceptance:** Versions and run lineage are queryable; regressions can revert to a prior version; proposals cite actual runs and pass E02-style outcome checks before activation.

**Checks:** `go test ./internal/skillmine ./internal/memory ./internal/task ./internal/policy -count=1`. Use existing package test helpers and temporary DBs. PostgreSQL coverage is required whenever SQL/schema changes.

**Commit:** `feat(skills): propose improvements from run outcomes`.

**Handoff:** status review with exact worker branch/SHA and check results. Reviewer marks done only after integration; all dependencies refer to integrated done states.

**Borrowing evidence:** [cognee/modules/memify/skill_improvement.py](https://github.com/topoteretes/cognee/blob/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/modules/memify/skill_improvement.py). Priority P1; see the borrowing plan for rollout boundaries.

## Task R01: Route retrieval with explicit, inspectable strategies

**Dependencies:** E01, G02, S01.
**Files:** `internal/memory/unified.go`, `internal/memory/retrieval_route.go (new)`, `internal/memory/retrieval_route_test.go (new)`, `internal/api/memory_handlers.go`, `internal/mcpserver/server.go`.

**Contract and implementation boundary:** Add caller-selectable exact, semantic, historical, relationship and procedural modes with an optional deterministic auto router. Return selected mode, reasons and fallback metadata. Reuse current search functions; preserve legacy default behavior until evaluation supports changing it. Do not interpret arbitrary years in identifiers as time filters or execute raw query text.

**Red proof:** Version/error strings containing years route as identifiers; explicit mode wins; ambiguous language falls back predictably; missing embeddings degrade to lexical retrieval.

**Acceptance:** All strategies preserve namespace and token budgets. E01 reports routing mistakes and quality/cost per strategy. Auto routing adds no model call.

**Checks:** `go test ./internal/memory ./internal/api ./internal/mcpserver ./internal/membench -count=1`. Use existing package test helpers and temporary DBs. PostgreSQL coverage is required whenever SQL/schema changes.

**Commit:** `feat(search): add inspectable retrieval strategies`.

**Handoff:** status review with exact worker branch/SHA and check results. Reviewer marks done only after integration; all dependencies refer to integrated done states.

**Borrowing evidence:** [cognee/api/v1/recall/query_router.py](https://github.com/topoteretes/cognee/blob/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/api/v1/recall/query_router.py). Priority P1; see the borrowing plan for rollout boundaries.

## Task R02: Add optional budgeted evidence expansion to reflect

**Dependencies:** R01, E02.
**Files:** `internal/reflect/reflect.go`, `internal/reflect/reflect_test.go`, `internal/memory/triplet.go`, `internal/membench/answers.go`.

**Contract and implementation boundary:** Extend the existing reflect loop with bounded relationship/evidence expansion, deduplicated evidence IDs, per-round cost and latency, and a no-new-evidence stop rule. Model-generated search suggestions never become facts by themselves. Enforce caller token/tool/time caps and validate citations against actual retrieved IDs.

**Red proof:** A multi-hop fixture needs a second evidence hop; repeated identical retrieval stops early; fabricated relation IDs are rejected; budget exhaustion returns a partial/abstained result with reason.

**Acceptance:** Compare against existing reflect on E02 under equal model/context budgets. Feature stays opt-in unless the final gate demonstrates a justified default change. No unlimited recursion or new agent framework.

**Checks:** `go test ./internal/reflect ./internal/memory ./internal/membench -count=1`. Use existing package test helpers and temporary DBs. PostgreSQL coverage is required whenever SQL/schema changes.

**Commit:** `feat(reflect): bound iterative evidence expansion`.

**Handoff:** status review with exact worker branch/SHA and check results. Reviewer marks done only after integration; all dependencies refer to integrated done states.

**Borrowing evidence:** [cognee/modules/retrieval/graph_completion_context_extension_retriever.py](https://github.com/topoteretes/cognee/blob/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/modules/retrieval/graph_completion_context_extension_retriever.py). Priority P2; see the borrowing plan for rollout boundaries.

## Task P01: Persist enrichment stage status and retry lineage

**Dependencies:** E01, A02.
**Files:** `internal/memory/outbox.go`, `internal/memory/enrich.go`, `internal/memory/pipeline_runs.go (new)`, `internal/api/memory_handlers.go`, `internal/store/migrations/{sqlite,postgres}/`, `cmd/punk/main.go`.

**Contract and implementation boundary:** Extend the existing outbox/enricher with source-revision and stage-version run records: pending/running/succeeded/failed, attempts, timestamps, counts and sanitized error classification. Stages remain in-process workers; do not introduce Kafka or a new orchestration service. Work keys must make at-least-once delivery idempotent and prevent stale revisions overwriting newer derived state.

**Red proof:** Crash after stage output but before acknowledgment, then retry: no duplicate derived entities/links. Stage failure remains visible and a changed source revision gets a new run.

**Acceptance:** Restart recovery and stale-worker handling are tested. Status endpoints honor authorization. Stage errors and durations are inspectable without exposing secrets. Paired SQLite/Postgres migrations preserve old data.

**Checks:** `go test -race ./internal/memory ./internal/api ./internal/store ./cmd/punk -count=1`. Use existing package test helpers and temporary DBs. PostgreSQL coverage is required whenever SQL/schema changes.

**Commit:** `feat(memory): track enrichment stages and retries`.

**Handoff:** status review with exact worker branch/SHA and check results. Reviewer marks done only after integration; all dependencies refer to integrated done states.

**Borrowing evidence:** [cognee/modules/pipelines/operations/log_pipeline_run_progress.py](https://github.com/topoteretes/cognee/blob/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/modules/pipelines/operations/log_pipeline_run_progress.py), [cognee/modules/pipelines/operations/get_pipeline_status.py](https://github.com/topoteretes/cognee/blob/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/modules/pipelines/operations/get_pipeline_status.py). Priority P1; see the borrowing plan for rollout boundaries.

## Task P02: Preview ingestion and enrichment work before execution

**Dependencies:** P01, I02, G02.
**Files:** `internal/ingest/estimate.go (new)`, `internal/ingest/estimate_test.go (new)`, `internal/memory/pipeline_runs.go`, `internal/cost/`, `cmd/punk/main.go`.

**Contract and implementation boundary:** Add a dry-run command reusing real loader/chunker/change-detection rules. Return changed/unchanged chunks, expected stage calls, token estimates, configured model IDs and explicitly approximate cost. No memory writes, model calls or loader subprocesses with hidden network side effects in default dry-run. Unsupported expensive formats must be labeled unestimated.

**Red proof:** A spy store/client sees zero writes and model calls during preview; an unchanged document estimates zero changed-chunk enrichment; unsupported input is never counted as a cheap URL/string.

**Acceptance:** Estimates distinguish measured input from heuristic output, include exclusions, and compare predicted versus recorded P01 work on fixtures. Missing prices return unknown, never zero-cost certainty.

**Checks:** `go test ./internal/ingest ./internal/memory ./internal/cost ./cmd/punk -count=1`. Use existing package test helpers and temporary DBs. PostgreSQL coverage is required whenever SQL/schema changes.

**Commit:** `feat(ingest): preview enrichment work and cost`.

**Handoff:** status review with exact worker branch/SHA and check results. Reviewer marks done only after integration; all dependencies refer to integrated done states.

**Borrowing evidence:** [cognee/modules/cognify/estimator.py](https://github.com/topoteretes/cognee/blob/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/modules/cognify/estimator.py). Priority P1; see the borrowing plan for rollout boundaries.

## Task H01: Build opt-in source-linked hierarchical summaries

**Dependencies:** E02, P01, G02, R01.
**Files:** `internal/memory/consolidate.go`, `internal/memory/observe.go`, `internal/memory/summary_tree.go (new)`, `internal/memory/summary_tree_test.go (new)`, `internal/reflect/reflect.go`.

**Contract and implementation boundary:** Build bounded topic summaries above source-linked observations using deterministic initial grouping by domain/key prefix. Record child revision IDs, stage version and stale state; regenerate only affected ancestors. Generated summaries stay separate from curated /mental-models. Support paginated scans, not a silent first-1000-facts ceiling.

**Red proof:** Editing/deleting one leaf invalidates only its ancestors; untouched branches do no model work; a corpus beyond the old cap retains full coverage and an empty subtree disappears.

**Acceptance:** Feature stays off by default. E02 broad-question cases measure benefit and extra cost. Sources remain traceable and compaction cannot silently present an unsupported summary as current.

**Checks:** `go test ./internal/memory ./internal/reflect ./internal/membench -count=1`. Use existing package test helpers and temporary DBs. PostgreSQL coverage is required whenever SQL/schema changes.

**Commit:** `feat(memory): add incremental summary hierarchies`.

**Handoff:** status review with exact worker branch/SHA and check results. Reviewer marks done only after integration; all dependencies refer to integrated done states.

**Borrowing evidence:** [cognee/tasks/memify/global_context_index](https://github.com/topoteretes/cognee/tree/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/tasks/memify/global_context_index). Priority P2; see the borrowing plan for rollout boundaries.

## Task Z01: Run cross-feature acceptance and publish the local evidence report

**Dependencies:** C06, A02, G02, I02, S02, R02, P02, H01.
**Files:** `docs/CONFIG.md`, `README.md`, `CHANGELOG.md`, `docs/reports/punk-improvement-pipeline.md (new)`, `scenarios/membench/`.

**Contract and implementation boundary:** Review the integrated branch against the brief. Run fresh migrations and upgrades from the baseline on both databases, native Codex 0.153.4 acceptance, authorization inventory, deterministic and opt-in retrieval modes, restart/retry scenarios and benchmark ablations. Document historical retention, approximate token budgets and citation-existence limits accurately. Report actual results and remaining limitations; do not claim Cognee superiority without a controlled comparative run. C06 is an explicit combined-acceptance dependency even though independent borrowing work can now proceed. If C02/C06 remains blocked, publish only clearly scoped component evidence; do not mark Z01 or the whole pipeline complete.

**Red proof:** Revert representative boundary fixes in a disposable test checkout to verify the acceptance harness detects missing prompt capture, cross-namespace access and duplicate stage output; never revert the shared integration branch.

**Acceptance:** All task commits integrated and reviewed; full gates pass with actual logs. The report records SHAs, commands, configurations, results, no unexplained regressions and rollout/rollback steps. No deployment, release, tagging or merge to main is part of this task.

**Checks:** `go test ./...; go test -race ./internal/hookcli ./internal/api ./internal/mcpserver ./internal/memory ./internal/reflect; go vet ./...; go build ./...; make test-ui; PostgreSQL and native Codex acceptance from brief`. Use existing package test helpers and temporary DBs. PostgreSQL coverage is required whenever SQL/schema changes.

**Commit:** `docs: record improvement pipeline acceptance`.

**Handoff:** status review with exact worker branch/SHA and check results. Reviewer marks done only after integration; all dependencies refer to integrated done states.
