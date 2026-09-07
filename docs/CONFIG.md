# Configuration reference

One YAML file (default `config.yaml`, `--config` to override) plus
`PUNK_*` environment overrides. Env wins over file; file wins over
defaults. Validation runs at load: bad values refuse to boot.

| Key | Env override | Default | Notes |
|---|---|---|---|
| `http.addr` | `PUNK_HTTP_ADDR` | `:9090` | REST + /mcp listener |
| `log.format` | `PUNK_LOG_FORMAT` | `text` | `text` or `json` |
| `log.level` | `PUNK_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `db.driver` | `PUNK_DB_DRIVER` | `sqlite` | `sqlite` or `postgres` |
| `db.dsn` | `PUNK_DB_DSN` | `punk.db` | pgx URL for postgres |
| `specs.dir` | `PUNK_SPECS_DIR` | `./specs` | hot-reloaded (fsnotify + SIGHUP) |
| `ai.enabled` | `PUNK_AI_ENABLED` | `false` | requires at least one profile when true |
| `ai.profiles.<name>` | - | - | `{base_url, api_key_env, model}`; any OpenAI-compatible endpoint |
| `budgets.tokens` | `PUNK_BUDGET_TOKENS` | `200000` | per-task default cap |
| `budgets.tool_calls` | `PUNK_BUDGET_TOOL_CALLS` | `50` | |
| `budgets.wall_ms` | `PUNK_BUDGET_WALL_MS` | `600000` | |
| `budgets.subagents` | `PUNK_BUDGET_SUBAGENTS` | `3` | children never spawn grandchildren |
| `memory.retention_days` | `PUNK_MEMORY_RETENTION_DAYS` | `0` | 0 disables the hourly sweep |
| `memory.consolidate_days` | - | `0` | horizon for region compaction during consolidation; 0 disables (also gates `memory.contradictions` and the observation/reconcile passes) |
| `memory.contradictions` | - | `false` | during consolidation, embedding-similar fact pairs are judged by the model; contradicting pairs get `contradicts` + `invalidated_by` links (ranking halves the older one's score); needs embeddings + `ai.enabled`; runs only when `memory.consolidate_days` > 0 |
| `otel.endpoint` | `PUNK_OTEL_ENDPOINT` | empty | OTLP/HTTP; empty = noop tracer |
| `route.epsilon` | - | `0.05` | routing-tie exploration; keeps off-policy eval possible |
| `route.fallback` | - | empty | agent for unmatched tasks; empty parks them |
| `proposals.expire_after_hours` | - | `72` | stale-proposal sweep; 0 disables |
| `ai.embeddings.model` | - | empty | write-time vectors for hybrid search; empty = FTS only |
| `ai.embeddings.provider` | - | `ollama` | `ollama` (or empty): use base_url + model; `local`: in-process static model, no service needed (downloads about 31 MB once from huggingface.co) |
| `ai.embeddings.base_url` | - | `http://localhost:11434` | Ollama-compatible root URL; punk appends `/api/embed` |
| `ai.embeddings.model_cache` | - | `~/.punk/models` | local provider only; directory for downloaded static models (or set `PUNK_MODEL_CACHE`) |
| `mcp.default_namespace` | `PUNK_NAMESPACE` | `agent-default` | namespace used when a client advertises no root and omits namespace |
| `mcp.toolset` | `PUNK_MCP_TOOLSET` | `full` | stdio server toolset: `agent` (lean session set) or `full` |
| credentials | `PUNK_CREDENTIALS` | `~/.punk/credentials.json` | file `punk login` writes (mode 0600); override the path, not the file's role |
| server URL | `PUNK_URL` | - | server base URL; flag > this env > credentials file > `http://localhost:9090` |
| API key | `PUNK_API_KEY` | - | bearer token; flag/env wins over the credentials file |
| PDF extractor adapter | `PUNK_INGEST_PDF_ADAPTER` | empty | command `punk ingest` runs for `application/pdf` (flag `--pdf-adapter` wins); empty = PDF unsupported, all other loaders work |
| request headers | - | - | the MCP server reads `X-Punk-Namespace` and `X-Punk-Agent` from clients; `X-Punk-Subject` is set by the auth middleware from the verified API key and any client-supplied value is deleted |
| `ai.embeddings.max_input_tokens` | - | `0` | model input window in tokens; 0 = unknown (diagnose skips oversize accounting) |
| `budgets.global_daily_usd` | - | `0` | burn-rate projection alerts; 0 disables |
| `budgets.price_table_path` | - | empty | override the shipped model price table |
| `mcp_client.servers[]` | - | - | `{name, command+args}` or `{name, url, token_env}` |

Secrets are never placed in the file: `*_env` keys name the environment
variable that holds the value.

## Operational commands

```sh
punk migrate up|down|status      # schema lifecycle
punk validate ./specs            # CI-friendly spec check (exit 1 on errors)
punk apikey create --name ci     # prints token once; store it
punk apikey revoke --name ci
punk backup --out snap.db        # sqlite VACUUM INTO snapshot
punk export --ns repo-main > m.jsonl  # region memory, derived data
punk import --ns repo-main < m.jsonl  # idempotent restore
punk ingest --ns repo-main --prefix /docs/rb runbook.md  # document with source provenance; repeat runs rewrite only changed chunks
punk region branch --ns repo-main --dir /tmp/exp --branch exp-1
punk replay --task <id> --k 3    # golden-ledger eval, pass^k
punk embed-backfill --ns <ns>    # embed pre-existing facts
punk topo import --file catalog.yaml   # import a Backstage catalog
punk skill install --agent <name>      # punk-memory and punk-plan skills for an agent (install|print|paths; --name picks one)
kill -HUP <pid>                       # force spec reload (watcher also does this)
```

## Document ingest

`punk ingest --ns NS --prefix P [flags] <file>` loads one document into
memory with source provenance: every chunk records the source id/URI,
revision, media type, section name/page and byte offsets, and repeated
ingestion rewrites only changed chunks (I01 delta ingest). There is no
REST surface for this; MCP `remember_document` remains the agent path.

Loaders: `text`, `markdown` (sections split at ATX headings), `html`
(document-order text, sections at h1/h2), `incident-json` (strictly
validated incident object: `id`, `title`, `status`, `severity`,
`summary`, `impact`, `root_cause`, `started_at`, `resolved_at`,
`timeline: [{time, event}]`, `action_items: [{description, owner}]` -
each populated field becomes a section named after its JSON field).
Detection order: `--format`, file extension, served content type, then
byte signature; undetectable input is an error, not a guess. Flags:
`--url URL` (explicit remote fetch; private/loopback/link-local targets
are refused by the SSRF guard, proxy env vars are ignored, redirects
re-validate), `--source-id`, `--revision`, `--author`, `--max-bytes`
(16 MiB default), `--timeout` (30s default). A whitespace-only input is
refused so an empty file can never wipe a prefix's chunks.

PDF is delegated to an optional external adapter - punk does not bundle
a PDF/OCR stack. Adapter contract: punk writes the PDF to a temp file
and runs `<adapter...> <file>` under the timeout; on exit 0 stdout must
be one JSON object
`{"revision":"...", "sections":[{"name":"...", "page":1, "text":"..."}]}`
(`revision`, `name`, `page` optional). stdout is capped at
`--max-bytes`, stderr (bounded) and the exit status appear in errors,
and the timeout kills the process. No adapter, a nonzero exit or
unparsable/empty output fails the ingest before any write.

`punk ingest --dry-run` previews the delta instead of executing it: how
many chunks the write path would add/change/remove, the exact input byte
counts per configured enrichment stage, and an estimated token/cost
figure per stage. A dry run writes nothing, calls no model and spawns
no adapter subprocess; a PDF input is reported as `unestimated` unless
`--allow-adapter` explicitly lets the preview run the external extractor
(`--allow-adapter` without `--dry-run` is a usage error). Byte counts
are exact measurements at the declared stage boundaries; token figures
are `bytes/4` estimates; stage completion is heuristic; a model without
a known price is reported as unknown, and a positive price that rounds
below micro-USD is labeled `priced_rounded_zero`, never "free". The
forecast excludes retries, control prompts and work outside the
disclosed stages - it is a preview, not an invoice. Note the ordinary
CLI writer has no embedder wired, so it performs zero synchronous
write-time embedding calls; normal writes still emit durable outbox
rows, and the configured background `embed_link` stage (a separate
processor) is forecast with body-only input, while the wired library
writer embeds the keyed input at write time.

## Retrieval strategies

`GET /v1/namespaces/{ns}/memories/search?strategy=...` and the MCP
`search`/`unified_search` `strategy` argument accept an explicit
retrieval route: `exact`, `semantic`, `historical`, `relationship`,
`procedural`, or `auto` (deterministic selection from the query shape -
no classifier model call). An explicit strategy returns the routed
envelope: compact hits plus the mode, the router's reasons and any
capability fallback (for example semantic requested without embeddings).
An unknown strategy is an error, not a silent fallback. Omitting the
parameter preserves the legacy fused listing exactly. A `temporal`
window that a windowed strategy cannot honor fails 400.

Two further retrieval opt-ins exist only at the Go library layer
(`internal/reflect`): `reflect.Opts.ExpandEvidence` bounds a
relationship/evidence expansion loop (per-call and per-run caps,
context deadline, staged shown-ID citations, per-round accounting), and
`reflect.Opts.Summaries` adds the `list_summaries` tool backed by
`Store.BuildSummaryTree`'s source-linked hierarchical summaries. Neither
has a CLI flag, MCP argument or config key in this build; the MCP
`reflect` tool exposes only `level` and `schema`. Both default off.

## Codex 0.153.4 integration

`punk connect codex` wires four capture hooks (SessionStart
`startup|resume`, UserPromptSubmit, PostToolUse, Stop) into
`$CODEX_HOME/hooks.json` and the punk MCP entry plus `[features]
hooks = true` into `$CODEX_HOME/config.toml` inside a punk-managed
marker block (foreign config and user hook groups are preserved; a
`[mcp_servers.punk]` table punk did not write is refused without
`--force`). Repeated connects are idempotent. Capture and context
injection derive the namespace from the hook payload's cwd unless the
hook command carries `--ns`.

Codex emits an OSC 0 terminal-title sequence on every spinner frame
(upstream `terminal_title.rs`, project-directory basename plus activity
prefix). The original user confirmed that disabling titles stopped
repeated text in the input and transcript (2026-09-06). The C02 matrix
and C06 acceptance run (2026-09-07) confirmed that this setting
suppresses title bytes while hooks remain functional. Those tmux runs
did not reproduce visible contamination; the original renderer behavior
remains unisolated. Apply the mitigation with:

```toml
[tui]
terminal_title = []
```

or one-shot: `codex -c 'tui.terminal_title=[]'`. Punk hooks and context
injection are unaffected by the setting. Trigger, evidence and the full
repeatable native acceptance procedure (temp `CODEX_HOME`, temporary
Punk server and DB, TUI session, duplicate-delivery replay, title A/B)
live in
`docs/investigations/codex-0.153.4-terminal-spam.md`; the simulated
lifecycle half is `internal/api/codex_roundtrip_test.go`. Native
observations never substitute for simulated boundary tests, and
simulated tests never claim anything about the TUI renderer.

## Deployment shapes

- **Single binary + SQLite**: default; `punk backup` covers DR
  (specs live in git).
- **Postgres**: set `db.driver`/`db.dsn`; use `pg_dump` for backups; CI
  runs the full suite against Postgres 16.
- **Docker**: `docker compose up` - specs bind-mounted read-only so
  editing them on the host hot-reloads inside the container. Keep the
  in-container bind at `:9090`: setting `http.addr: 127.0.0.1:9090` for
  hardening makes the published port unreachable (loopback inside the
  container is not the host's loopback). Restrict reachability at the
  host-side mapping instead - `"127.0.0.1:9090:9090"` in compose - or
  override the bind with `PUNK_HTTP_ADDR`.
