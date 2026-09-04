# Changelog

## v1.7.0 (unreleased)

### Changed
- Brain view redrawn around an anatomical brain mesh (BodyParts3D, CC BY-SA 2.1 JP, attribution at `/brain/mesh/NOTICE`): a translucent glass shell with visible gyri, a lattice of neurons inside, signals that run along axons when memory is written, magenta haze on active regions, floating region labels, and a plain-language activity log that coalesces bursts and has a thin scrollbar.

## v1.6.0 (2026-09-04)

### Added
- The server root `/` (alias `/brain`) is now a live brain visualization of every memory namespace; regions glow with write, claim and task activity, sparks mark each event, and a side log narrates them. `punk brain --open` opens it; `punk brain serve` starts the server and prints the URL.
- `GET /v1/brain/snapshot` (per-namespace counts, members, claims, task tallies) and `GET /v1/brain/events` (server-wide SSE of bus events).
- `memory.TaskCounts`: done/blocked/pending digest of the `/tasks` convention.

## v1.5.3 (2026-09-04)

### Fixed
- MCP read tools always return at least the first matching fact, so a single document larger than the token budget is readable instead of coming back as an empty, truncated result.

## v1.5.2 (2026-09-04)

### Fixed
- Default read-tool budget lowered from 16000 to 8000 tokens: 16000 body tokens plus JSON overhead was 76 KB, still past OpenCode's 50 KB tool-output limit.

## v1.5.1 (2026-09-04)

### Fixed
- MCP read tools (`recall`, `recall_as_of`, `search`) cap their payload at 16000 tokens when `max_tokens` is unset and report `truncated`, `total` and a `note` when the cap cuts the result; pass `max_tokens: -1` for the old unbounded behaviour. An uncapped recall of a busy prefix (254 task bodies, 1 MB) exceeded OpenCode's 50 KB tool-output limit and reached the model as an empty truncation notice.
- `list_keys` no longer advertises a `max_tokens` argument it ignored.
- `punk-memory` skill: poll `/tasks` with `list_keys` and recall single keys instead of recalling the whole prefix.

## v1.5.0 (2026-09-03)

### Added
- `punk-memory` skill installed by every `punk connect` target (`punk skill install|print|paths`); MCP instructions are generated from the same text.

## v1.4.0 (2026-09-03)

### Fixed
- `punk connect opencode` wrote the MCP entry to `~/.config/opencode.json`; it now goes to `~/.config/opencode/opencode.json` where OpenCode reads it.
- `punk connect verify` advertises the working directory as a root, so the namespace it reports is the one a session gets.
- Codex `config.toml` files with CRLF line endings keep them.

### Added
- `punk connect codex`: capture hooks (`hooks.json`, passthrough payload) and the punk MCP entry in `config.toml`; `--project` derives the namespace from the git remote.

## v1.3.1 (2026-09-03)

### Fixed
- MCP server crashed on any method called without params (for example `tools/list` from a real client): the trace middleware dereferenced a typed-nil params pointer. Regression test added over HTTP.

## v1.3.0 (2026-09-03)

### Added
- `format: compact` on `search` and `unified_search` (MCP) and `?format=compact` (HTTP).
- MCP initialize instructions describing retrieval routing.
- `anchors` on hybrid scored search: exact phrase routes fused by rank.
- `repo_revision` / `?revision=` flags code-map hits from another revision as stale; `punk seed rinnegan` records the revision.
- `ai.embeddings.provider: local` with a pinned static model and `punk models pull`.
- `diagnose.oversize_embeddings` and `ai.embeddings.max_input_tokens`.
- `punk embed-backfill --force`.
- MCP calls join the caller's W3C trace context from `_meta`.
- `punk connect claude-code|cursor|opencode` register the MCP server (and the Claude Code permission rule); `--verify`, `--no-mcp`, `--force`.
- MCP: `whoami`; `namespace` optional on every memory and region tool, resolved from client roots; `remember_many`; `remember_document {path}` on the stdio server; subscribable `punk://memory/{ns}{prefix}` resources; `agent` and `full` toolsets (`punk mcp --toolset`, `/mcp?toolset=agent`).
- `punk login` / `punk logout`; hooks and connect read `~/.punk/credentials.json`.
- MCP entries carry `Authorization` (literal or `${VAR}` with `--api-key-env`), `X-Punk-Namespace`, `X-Punk-Agent`; `connect --verify` authenticates.
- `punk connect --project` derives the namespace from the git remote; `punk namespace`; `punk hook --ns`; `/v1/agent/hooks?ns=`.
- `claim_work`, `release_work`, `register` default holder/agent to the session identity; `whoami` reports `agent`.
- `docker compose --profile central` adds a Caddy TLS front; `deploy/Caddyfile`.
- `punk connect` registers the punk MCP server for Copilot CLI, Antigravity, Hermes and OpenClaw; the pi extension registers `punk_whoami`, `punk_recall`, `punk_search`, `punk_remember`.
- `GET /v1/agent/namespace?cwd=` returns the namespace a directory maps to.

### Changed
- Embedding input is `key: <key>` + body; re-embed with `embed-backfill --force`.
- Expanded search keeps each reformulation's top hit before filling by score.
- Document chunks are bounded (4000 chars); oversize paragraphs split at sentence or line breaks.

### Fixed
- Example config embeddings `base_url` no longer carries a `/v1` suffix.

- P0.1: repo skeleton - go module, cmd/amaterasu, internal packages, Makefile, lint config
- P0.2: CLI subcommands serve/migrate/validate stubs + --version
- P0.3: config package - YAML + PUNK_* env overrides, validation
- P0.4: slog logger, chi server, /healthz + /readyz, graceful shutdown
- P0.5: GitHub Actions CI - build, race tests, lint
- P1.1+P1.2: migration runner (up/down/status, embedded SQL) + core schema both dialects
- P1.3: store.Open (sqlite WAL / pgx simple protocol), WithTx, Rebind, time codec
- P1.4: CI postgres job; store suite verified on Postgres 16 locally
- P2.1-2.3: memory plane - remember/recall/list_keys/forget, quarantine, FTS both engines
- P2.4: REST /v1/namespaces/{ns}/memories + keys + search wired into serve
- P2.5: JSONL export/import CLI, idempotent restore, rollback on bad line
- P2.6: retention sweep config + hourly job in serve
- P3.1-3.4: spec parsers (agent frontmatter, SKILL.md, policy YAML), examples, validate CLI
- P3.5-3.6: registry with versioned atomic snapshots, agent_versions persistence, fsnotify+SIGHUP hot-reload
- P4: task ledger (state machine, events, dedup, leases) + deterministic router with recorded decisions + /v1/tasks API
- P5.1: LLM profiles (openai-go, any OpenAI-compatible base_url), noop fail-closed, llm_calls accounting
- P5.2: MCP client pool, namespaced tools, failed server skipped
- P5.3-5.7: budget enforcement, agent runtime loop, evidence-linked findings, subagents (results-only), serve dispatcher
- P6: policy engine (autonomy ladder, action classes), proposals + PATCH API, OTel spans, audit completeness
- P7: API keys (bootstrap-then-enforced bearer auth), MCP server (stdio + /mcp HTTP), generic webhook intake
- P8: Susanoo consumer (HMAC intake, finding comments, proposals -> pending_approval), database agent pack, demo
- P10.1+10.2: wall-clock budget enforced; live loops extend their lease
- P10.3: agents can remember/recall/list_keys (namespace = task source, attributed)
- P10.4: skill allowed-tools cap the agent tool surface
- P10.5-10.8: proposal expiry sweep, LLM tie-breaker, OPE propensity logging + epsilon exploration, handoff summary contract
- P11: MCP SDK v1.7.0-pre (2026-07-28 RC), task resources subscribable via bus
- P12: memory v2 - provenance, as-of reads, outbox subscriptions (SSE), ACLs, embeddings + hybrid RRF search, per-fact expiry, recency boost
- P16 deferred to backlog per owner
- P13: golden-ledger replay evals - snapshots, frozen-world rerun, trajectory scoring, label harvest, pass^k, replay CLI
- P14: cost governance - local price rating, per-agent daily budgets, burn table /v1/costs, projection alerts, kill switch
- P15: operator console at /ui - approval inbox, parked-task requeue/cancel, ledger timeline, spec browser, cost view
- P17: service topology (Backstage import, blast-radius routing enrichment), untrusted-telemetry fencing, auto-autonomy warning
- P18: A2A Agent Card discovery endpoints, API-key subject claim (Okta/Entra), evals doc; ITBench + full A2A transport to backlog
- B1: brain regions - registry, agent registration, spec regions field, MCP register/list tools
- B2: work-claim leases - conflict-free work partitioning (claim/release/list, single-winner races), MCP tools
- B3: content-hash dedup (idempotent concurrent identical writes)
- B4: brain-first MCP - search/hybrid, recall_as_of, forget tools
- B5: git branch/merge/federation over the JSONL substrate (fork a region, experiment, conflict-free merge back)
- B6: consolidation (Vegapunk daily-sync) - deterministic region compaction + optional LLM summarization
- B7: typed links / graph (link, neighbors); MCP link/neighbors tools; serve consolidation job
- B8: shared-brain demo (two satellites, one region, conflict-free concurrent writes + dedup + bi-temporal); docs + site repositioned to Punk Records model
- standalone: removed the Susanoo consumer bridge, config block, and /v1/intake/susanoo; generalized the example pack tool namespace to incidents__; generic webhook demo
- H1: per-turn prompt-scoped context injection (Claude Code + Hermes) with per-session dedup; configurable session-start inject components incl. directives; dream-scheduled per-namespace consolidation (write/idle/spacing triggers, memory-namespace iteration fix, `punk consolidate`, bus event + diagnose observability); reasoning-kind labels on observations (inductive needs two sources); cross-project user profile card (`punk card`); rolling recursive session summaries + "Last session" injection; reflect `level` + `schema` (structured answers); batched entity extraction; `punk skills insights` (memory -> proposed CLAUDE.md additions)
