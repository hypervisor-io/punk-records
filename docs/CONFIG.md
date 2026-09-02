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
punk region branch --ns repo-main --dir /tmp/exp --branch exp-1
punk replay --task <id> --k 3    # golden-ledger eval, pass^k
punk embed-backfill --ns <ns>    # embed pre-existing facts
punk topo import --file catalog.yaml   # import a Backstage catalog
kill -HUP <pid>                       # force spec reload (watcher also does this)
```

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
