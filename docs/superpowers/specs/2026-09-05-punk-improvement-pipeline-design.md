# Punkrecords improvement pipeline design

Date: 2026-09-05; scheduling revised 2026-09-06. Status: implementation underway; consult the live task board for reviewed/integrated work.
Coordination namespace: `punk-punkrecords-improvement`.
Repository: `/mnt/d/Hypervisor_Code/punkrecords`.
Integration branch: `feat/punk-improvement-pipeline` (local; do not assume a remote branch exists).
Baseline: Punkrecords `9ab2b8f`; Cognee `78ff576559a7f75f65884c5bd90b22cdc790016e`.
Compatibility target: installed `codex-cli 0.153.4`; upstream `rust-v0.153.4`, commit `3d2ee51ca2d5db578f328aa75e20aa22c0197c9a`.

## User intent and authorization

Prepare and execute a worker-ready improvement pipeline derived from the Cognee comparison, prioritizing the reported Codex integration problem. The user reports repeated Punkrecords hook/status text near/in the input box and in the transcript. This planning session creates documents, task facts and handoff prompts; it does not deploy code or change the live Codex integration. A worker receiving the pickup prompt is authorized to implement its assigned tasks. No automatic publishing, main merge, release, deployment or live-server restart is authorized by this plan.

## Chosen approach

Extend Punkrecords' current fact store, outbox, retrieval, reflect and skill machinery. Keep Go plus SQLite/PostgreSQL, deterministic default operation, optional model enrichment and inspectable evidence. Alternatives considered: importing Cognee as the core would introduce a second memory model and dependency stack; adding many unrelated retrieval features before evaluation would make quality regressions difficult to attribute. Neither is selected. Optional Cognee ingestion interoperation remains future work, not a dependency.

## Investigation evidence: Codex spam

Observed locally:
- `codex --version` reports 0.153.4; installed `punk --version` reports v1.8.1.
- User's global hooks file has one Punk command for each SessionStart, UserPromptSubmit, PostToolUse and Stop; no project hooks file was found in this checkout. SessionStart matcher is startup|resume. No statusMessage was present on these inspected entries.
- Both ~/.agents/skills and ~/.codex/skills contain independent punk-memory and punk-plan directories. The session skill catalog exposes duplicate entries. This proves duplicated instruction/catalog material, not editable-composer corruption.
- `punk namespace` resolves this checkout to `agent-punk-records-75eeb0` from the remote. This session's MCP whoami resolves `agent-default` with source default. This proves a namespace alignment problem for this connection; do not migrate data automatically.
- The installed Punk hook binary was exercised against a disposable local HTTP fixture, not the live server. SessionStart and UserPromptSubmit returned valid nested additionalContext, exit 0, empty stderr, about 7ms. PostToolUse and Stop returned empty stdout/stderr, exit 0, about 9-10ms. These timings describe only the local synthetic test.
- The native upstream 0.153.4 UserPromptSubmit schema has turn_id and no prompt_id (codex-rs/hooks/src/schema.rs:567). Punk's Codex path forwards that payload unchanged (internal/hookcli/hookcli.go:318), while the API requires prompt_id (internal/api/agent_handlers.go:192). The disposable test confirmed no prompt_id was forwarded. The server code therefore ignores these native prompt captures. This is a confirmed separate compatibility defect.
- Existing synthetic tests pass: `go test ./internal/hookcli ./internal/api -run 'TestConnectCodex|TestRunFromCodex|TestAgentTurnContextDedup' -count=1`. They do not establish native UI behavior.

Version-pinned upstream behavior:
- codex-rs/tui/src/history_cell/hook_cell.rs:33 reveals running-hook activity after 300ms.
- The same file's hook_run_is_quiet_success and has_persistent_output exclude completed context-only output from persistent transcript history. Thus normal additionalContext alone does not explain the reported transcript spam.
- codex-rs/tui/src/chatwidget/hook_lifecycle.rs drives input-adjacent hook status and completion rendering.
- hooks/src/events/user_prompt_submit.rs accepts the nested output envelope; suppress_output is parsed then ignored there. hooks/src/engine/output_parser.rs explicitly rejects suppressOutput for PostToolUse. Do not propose that flag as a generic fix.
- Current Punk startup context records injected fact IDs but does not filter them on repeated SessionStart; per-turn context does filter. This is a repeated-context risk, separate from the renderer.

Remaining uncertainty: the exact user-visible transcript trigger has NOT been reproduced. C02 must distinguish command/wrapper output, non-success runs, latency, duplicate effective registration, upstream host rendering and actual editable input mutation. If a Codex app is used, record the app build and embedded CLI; a terminal test is not proof of that app renderer. No unsupported root-cause claim is allowed.

Public references:
- https://github.com/openai/codex/blob/rust-v0.153.4/codex-rs/tui/src/history_cell/hook_cell.rs
- https://github.com/openai/codex/blob/rust-v0.153.4/codex-rs/hooks/src/schema.rs
- https://github.com/openai/codex/blob/rust-v0.153.4/codex-rs/hooks/src/engine/output_parser.rs
- https://learn.chatgpt.com/docs/hooks (current docs; source tag is authoritative for this version)

## Screenshot update

The user added `codex-spam.png` in the repository root; it was visually inspected. Literal `punkrecords` repeats horizontally after the transcript overlay footer's `q to quit / esc to edit` hints, not as ordinary hook-result entries. The user also reports the symptom near/in the normal input box and describes repeated hook/status messages.

Upstream `codex-rs/tui/src/pager_overlay.rs:870-895` builds that footer from static shortcut hints only. It has no project-name, memory or hook context insertion. `terminal_title.rs:56-90` emits an OSC 0 window title to terminal stdout; default title items are activity and project-name (`chatwidget/status_surfaces.rs:27`), with the project name derived from the repository directory. This provides a plausible source for the exact repeated word, but escape-sequence handling/cursor contamination has NOT been proven in the user's terminal.

Noninteractive bash -c and bash -lc with true emitted no stdout/stderr in this environment. Inspected shell startup files show conventional interactive PS1 title handling, not a confirmed noninteractive producer. Do not blame shell initialization without further evidence.

C02 now prioritizes a four-way title on/off versus Punk hooks on/off experiment plus raw terminal output classification. A one-shot `codex -c 'tui.terminal_title=[]'` is a diagnostic candidate, not a verified fix. Use temporary config; do not change the current live session. A fixture directory rename can establish whether the repeated word follows the project basename. A supported, actually validated upstream-host mitigation is an acceptable outcome if Punk is not responsible.

The screenshot remains user-owned and untracked; the planning commit records the visual observation rather than adding the image.

## Scheduling update: 2026-09-06

The user requested continued Cognee borrowing work. Independent evaluation and authorization work may proceed while the native terminal issue awaits host evidence. E01 and A01 have no task dependencies; A02 depends on A01, not C05. Existing C-series dependencies and acceptance remain intact; Z01 explicitly depends on C06 as well as all memory feature gates. This changes scheduling, not the definition of a fixed Codex integration or a completed pipeline. Details: [Cognee borrowing plan](../plans/2026-09-06-cognee-borrowing.md).

## Scope and phases

The workstreams below describe scope, not a global sequential barrier.

1. C01-C07: native Codex compatibility, spam diagnosis/fix, duplicate managed installations, delivery deduplication with explicit acknowledgement semantics, namespace alignment and measured guidance duplication. C06 gates the complete integration.
2. E01-E02: reproducible retrieval and answer/citation evaluation.
3. A01-A02: opt-in namespace authorization with a complete boundary inventory.
4. G01-G02, I01-I02, S01-S02, P01-P02: typed entities, provenance/loaders, skill lifecycle and enrichment observability.
5. R01-R02 and H01: measured retrieval routing/expansion and opt-in summary hierarchy.
6. Z01: full integrated acceptance and evidence report.

The task manifest is authoritative for dependencies. Independent ready tasks may run in separate worktrees after claims; overlapping file claims serialize editing. Readiness requires dependencies to be integrated and reviewer-marked done, not merely committed on a worker branch.

## Core contracts

- Legacy calls and deterministic defaults remain compatible. New auth enforcement, expansion and hierarchy are opt-in.
- Authorization is separate from namespace selection. HTTP credential identity is verified; local stdio trust is explicit.
- Entity identity is separate from display name; aliases and merges preserve old references and source lineage.
- Document ingestion produces stable source/revision/chunk provenance and uses the existing defense/write path.
- Pipeline runs are keyed by source revision and stage version; retries are idempotent.
- Generated summaries never acquire curated mental-model authority automatically.
- Skill discovery returns metadata; procedure bodies are loaded on demand. Improvement is a proposal, not automatic activation.
- Benchmarks distinguish evidence retrieval, answer quality, citation existence and claim support.
- Schema changes need paired SQLite/PostgreSQL migrations using the next free version at integration time.
- No blanket claims of immutable eternal history, exact token budgeting or proof of answer truth. Current retention can delete revisions and token estimates are approximate.

## Acceptance

Codex: 0.153.4 native event fixtures; all four captures; idempotent connect; no successful repeated persistent hook/status spam; no editable-composer mutation; documented resume semantics; preserved useful context; no-roots namespace alignment; diagnostic failure behavior; foreign configuration preservation.

Quality: deterministic baseline and ablation manifests, no hidden LLM calls, answer/citation/abstention fixtures, corpus/model/config hashes and per-query results. No invented performance thresholds or Cognee comparisons. Preserve exact-match/correction/namespace cases; any measured tradeoff is explicitly documented for review.

Security: valid credentials for namespace A cannot read/write/enumerate/subscribe to B when enforcement is enabled. Cover REST, MCP, hooks, exports/imports, board/claims and UI snapshots. Disabled enforcement keeps trusted deployment behavior.

Storage: fresh install and baseline upgrade on both database dialects; retry/crash idempotence; stable provenance; non-destructive entity aliases; summary invalidation; retained references consistent with the configured retention policy.

Full gate: gofmt on changed Go files; `go test ./...`; `go vet ./...`; `go build ./...`; `make test-ui`; race tests for changed concurrent packages; PostgreSQL tests following repository CI; native Codex acceptance. Missing external prerequisites are reported as unrun, never passed.

## Exclusions

No mandatory Cognee dependency, broad database-adapter matrix, full RDF/OWL reasoner, built-in OCR stack, unbounded autonomous graph agent, live namespace cleanup or secret/config migration. No benchmark billing without explicit model/budget settings. No live server changes.

## Coordination and review

Workers implement in isolated task worktrees and submit one commit with status review. A reviewer/integrator cherry-picks accepted commits under /coordination/integration, reruns gates, then marks done using the integrated SHA. This deliberately keeps unreviewed worker commits from unlocking dependencies. Workers never merge to main, deploy, tag or release. A final reviewer writes /plan/status only after Z01 is integrated and every task is done.
