# Cognee borrowing implementation plan

> **For agentic workers:** Use `superpowers:executing-plans` to execute one claimed task at a time. The master plan supplies each task's files, contract, red proof, checks and commit message. Read both documents before editing.

**Goal:** Bring useful Cognee mechanisms into Punk's existing memory system, with reproducible evidence of benefit and preserved deterministic defaults.

**Architecture:** Extend the Go fact store, retrieval, reflect, outbox and skill machinery. Evaluation and authorization are independent foundations. Feature work uses those foundations and stays compatible with existing APIs; the Codex workstream joins at combined acceptance.

**Tech stack:** Go, existing SQLite/PostgreSQL stores, existing REST/MCP interfaces, optional configured model clients.

**Spec:** [pipeline design](../specs/2026-09-05-punk-improvement-pipeline-design.md).
**Task contracts:** [master plan](2026-09-05-punk-improvement-pipeline.md) and [manifest](2026-09-05-punk-improvement-pipeline.tasks.json).
**Worker entry point:** [Cognee worker prompt](2026-09-06-cognee-borrowing-WORKER-PROMPT.md).

Namespace: `punk-punkrecords-improvement`. Branch: `feat/punk-improvement-pipeline`.
Research baseline: Cognee `78ff576559a7f75f65884c5bd90b22cdc790016e`; inspected Punk integration base `c3b935b1b28ed8ae3f9732fc8abe1e22ca066574`.
These SHAs identify evidence, not a requirement to start workers from an obsolete commit. Always branch from the current local integration tip.

## Global constraints

- Keep Go plus SQLite/PostgreSQL. No mandatory Cognee/Python dependency or additional service.
- Preserve existing calls and deterministic defaults; model work requires explicit configuration and budget.
- No live server changes. `127.0.0.1:9090` is coordination-only: use temporary databases and isolated test servers for experiments.
- Namespace selection never grants access. Enabled authorization uses verified identity and deny-by-default; trusted disabled mode remains compatible.
- Source revisions, aliases, citations and retry lineage remain inspectable under the retention policy. No destructive fuzzy merges or silent summary promotion.
- Adapt mechanisms to Punk. Any adapted upstream code must retain applicable source/license notices; record exact source paths and commit in the submission. Do not copy upstream corpora without checking their terms.
- Workers submit review; reviewers integrate and mark done. No main merge, push, deployment, tag or release is authorized.

## Scheduling decision

The old E01 -> C06 dependency serialized all memory work behind a terminal reproduction requiring the user's host. It was a scheduling preference, not a code dependency. This revision makes E01 and A01 independently ready. A01's authorization model does not need benchmark output; A02 enforcement does not need C05's connection diagnostics.

| Changed task | Previous dependencies | Revised dependencies | Reason |
|---|---|---|---|
| E01 | C06 | none | Offline fixture evaluation does not require native terminal evidence. |
| A01 | E01 | none | Permission decisions can be implemented and tested independently. |
| A02 | A01, C05 | A01 | Authorize the actual resolved namespace; diagnostics are not the authority. |
| Z01 | A02, G02, I02, S02, R02, P02, H01 | C06 plus previous dependencies | Preserve complete Codex acceptance at final convergence. |

All other task dependencies and existing status/submission records are preserved. C02 remains blocked until host evidence meets its original acceptance. C05 remains a separate review task. No worker should reopen completed C01/C03/C07 simply to start this workstream.

## What to borrow and how to adapt it

The links below are pinned research references. Punk changes are proposed adaptations; they are not claims that Cognee guarantees all of Punk's planned acceptance conditions.

| Tasks / priority | Cognee reference and mechanism | Punk adaptation and acceptance evidence |
|---|---|---|
| E01–E02 / P0 | [Evaluation framework](https://github.com/topoteretes/cognee/tree/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/eval_framework): separated corpus construction, retrieval and grading. | Extend `internal/membench`; save per-query evidence and versioned manifests. Separate retrieval, answer accuracy, citation existence, claim support and abstention. |
| A01–A02 / P0 | [Authorized dataset resolution](https://github.com/topoteretes/cognee/blob/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/modules/pipelines/layers/resolve_authorized_user_datasets.py). | Map permissions to namespaces and verified subjects. Inventory REST, MCP, hooks, board/claims, subscriptions, import/export and region operations. A-only credentials must not access B in enabled mode. |
| G01–G02 / P1 | [Entity consolidation](https://github.com/topoteretes/cognee/blob/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/tasks/memify/consolidate_entities.py): normalized names, typed candidates and canonicalization. | Structured entity types, alias proposals and preserved old IDs. Measure false merges; applying/undoing a proposal preserves source lineage. Use reversible proposals instead of adopting upstream node-deletion semantics. |
| I01–I02 / P1 | [Loader abstractions](https://github.com/topoteretes/cognee/tree/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/infrastructure/loaders). | Stable source/revision/chunk provenance followed by text, Markdown, HTML and incident JSON adapters. An early insertion must not rewrite unaffected chunks. PDF remains an optional external adapter. |
| S01–S02 / P1 | [Metadata-only skill retrieval](https://github.com/topoteretes/cognee/blob/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/modules/retrieval/skills_retriever.py) and [improvement proposals](https://github.com/topoteretes/cognee/blob/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/modules/memify/skill_improvement.py). | Search metadata, load an exact procedure version only when selected, and connect improvement proposals to actual run outcomes. No automatic activation or whole-procedure prompt repetition. |
| P01–P02 / P1 | [Pipeline progress](https://github.com/topoteretes/cognee/blob/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/modules/pipelines/operations/log_pipeline_run_progress.py) and [work estimator](https://github.com/topoteretes/cognee/blob/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/modules/cognify/estimator.py). | Extend existing outbox/enricher with revision/stage run records and safe retries. Preview reuses ingestion rules and distinguishes measured input from approximate cost; default preview makes zero writes/model calls. |
| R01 / P1 | [Query router](https://github.com/topoteretes/cognee/blob/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/api/v1/recall/query_router.py). | Explicit retrieval strategies plus optional deterministic routing. Report reasons/fallbacks, protect exact identifiers, preserve legacy default search. |
| R02 / P2 | [Graph context extension](https://github.com/topoteretes/cognee/blob/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/modules/retrieval/graph_completion_context_extension_retriever.py). | Bounded evidence expansion in existing reflect. Stop on no new evidence and enforce equal-budget comparisons against existing reflect. |
| H01 / P2 | [Global context index](https://github.com/topoteretes/cognee/tree/78ff576559a7f75f65884c5bd90b22cdc790016e/cognee/tasks/memify/global_context_index). | Opt-in source-linked summary hierarchy with incremental ancestor invalidation. Measure broad-question benefit and additional model work before any default change. |

P0 means foundations first; P1 means the next ready feature; P2 means higher uncertainty and opt-in rollout. Priority never overrides dependencies or file claims. These are existing task IDs, not a duplicate queue.

## First worker assignments

**Worker A: E01.** Reuse `internal/membench/membench.go`, its tests, `locomo.go` and `scenarios/membench/sample.jsonl`. Add a versioned report in the existing package. The current `Run` stops at the first expected hit and divides successful queries by all queries, so the `RecallAtK` field is any-hit rate. Preserve/document legacy output; new reports must separate hit rate from evidence recall and MRR. Required proof: one of two expected facts retrieved yields hit rate 1 and recall 0.5. Cover duplicate IDs, query errors and empty expected sets. Keep all query outcomes in the artifact.

- [ ] Add the failing metric and stable-manifest tests specified in E01.
- [ ] Implement versioned manifests/per-query reports using existing store/test fixtures.
- [ ] Add deterministic exact-ID, correction, multi-hop, alias, temporal, namespace-isolation and no-answer cases. Use k=5 and seed 1 initially.
- [ ] Compare the same fixture/config twice excluding volatile fields. A changed corpus/strategy must alter the stable manifest.
- [ ] Run E01 checks and submit one commit with a small reproducible offline report. Explicitly mark model-based ablations unavailable if no model/budget is configured.

**Worker B: A01.** Add `internal/authz` around the existing verified API-key subject from `internal/api/auth.go`; do not introduce a second authentication scheme. Use exact namespace grants and distinct read/write/admin operations with no implicit inheritance or pattern matching. Enabled mode denies empty subjects and zero-key bootstrap. Disabled mode keeps trusted behavior. Include a usable local grant-provisioning/recovery path, isolated migration tests for both dialects and explicit stdio trust semantics.

- [ ] Test A-only read access, B denial, A-write denial, revocation, empty subjects and bootstrap behavior.
- [ ] Implement grants and checked identity plumbing, preserving existing disabled-mode behavior.
- [ ] Allocate paired migrations under `/coordination/migrations`; test up/down and existing-key upgrade behavior.
- [ ] Document local provisioning and recovery without editing live keys/config.
- [ ] Run A01 checks and submit one commit. A02 then audits/enforces every external memory boundary before dependent feature interfaces are accepted.

Both tasks may need `cmd/punk/main.go`. Claim exact files before editing and serialize that overlap; do not hold a conflicting partial claim set. Avoid changing Codex connect functions for these tasks. Workers can prepare isolated package work independently, then rebase before CLI integration.

## Subsequent pickup order

After E01 integrates, E02 is ready. After A01 integrates, A02 is ready independently of E02. When E01 and A02 are done, G01, I01, S01 and P01 become eligible; claim conflicts may serialize shared memory/API/CLI edits. Each second-stage task follows its explicit dependencies in the manifest. R01 follows G02/S01; R02 and H01 remain opt-in and require evaluation evidence.

Do not silently change retrieval defaults, grant inheritance or public API shapes because a benchmark score looks better. Record measured tradeoffs and preserve old behavior until the relevant acceptance gate supports a change.

## Evidence required from every borrowing task

Submission at `/reviews/<id>/submission` must be written before status `review` and include:

- Original/integration base, worker branch and full commit SHA.
- Mechanism borrowed, exact upstream commit/paths, and whether code was adapted or independently implemented.
- Observable before/after behavior, failing proof, passing checks and any unrun prerequisites.
- Fixture/config hashes and baseline/candidate report locations for retrieval or model-related work. Same corpus, k and model/context budget; report per-category outcomes and added latency/calls.
- Compatibility/default behavior, migration/recovery implications and remaining limitations.

No invented performance percentage or cross-product superiority claim. Fixed fixtures enforce exact-ID, correction and namespace invariants; broader tradeoffs require review of actual measurements. A real citation ID alone does not establish that its content supports an answer.

## Completion boundary

This workstream can deliver reviewed memory improvements while C02 is blocked. Combined pipeline completion still requires C06 and all Z01 prerequisites, followed by the existing full acceptance checks. Report component completion precisely; never close the Codex incident because a memory feature passed its tests.
