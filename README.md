<p align="center">
  <img src="docs/assets/banner.webp" alt="Punk Records - the shared brain for AI agents" width="100%">
</p>

# Punk Records

**Where intent becomes coordination.**

Punk Records is a self-hosted, Go-native **shared brain** for domain AI
agents - a Punk Records: memory partitioned into **regions (namespaces)**
that specialist agents register to and coordinate through, reached over
**MCP**. Agents append to a region concurrently and never conflict
(append-only is the coordination substrate); they claim work so they
don't overlap; they read the region as of any past instant; they can fork
a region to a git branch, experiment, and merge back. Around that brain
it also routes tasks to agents, holds them to budgets and policy, and
returns **evidence-linked findings - never unreviewed actions**.

Free software (LGPL-2.1 or LGPL-3.0), from ServerGurus. Reached over MCP by
any agent you build.

## Mental model

One brain, many satellite agents. Agents are **files on disk**, not code.
The brain never touches production; it proposes, a human approves, the
consumer executes.

```mermaid
flowchart TB
    subgraph INTENT["INTENT arrives"]
        W["webhook intake<br/>(any consumer)"]
        R["REST /v1/tasks"]
        M["MCP submit_task"]
    end

    subgraph BRAIN["PUNK RECORDS / CORE-01"]
        direction TB
        ROUTE["Router<br/>declarative triggers, rules first,<br/>every decision recorded"]
        LEDGER["Task Ledger<br/>event-sourced, append-only,<br/>the audit log IS the system"]
        POLICY["Policy Engine<br/>observe - advise - propose - auto<br/>checked per tool call"]
        BUDGET["Budgets<br/>tokens, tool calls, time, subagents<br/>exhaustion parks, never truncates"]
    end

    subgraph AGENTS["DOMAIN AGENTS - specs/ on disk, hot-reloaded"]
        DB["database.md<br/>+ 4 SKILL.md procedures"]
        NET["network.md<br/>(next pack)"]
        APP["application.md<br/>(next pack)"]
    end

    subgraph WORLD["TOOLS and CONSUMER"]
        MCPT["MCP tool servers<br/>(metrics, logs, incidents)"]
        SUS["consumer<br/>the only executor"]
    end

    W --> ROUTE
    R --> ROUTE
    M --> ROUTE
    ROUTE --> LEDGER
    LEDGER --> DB & NET & APP
    DB & NET & APP -->|"tool calls"| POLICY
    POLICY -->|"allow (read)"| MCPT
    POLICY -->|"propose (write)"| SUS
    DB -->|"findings + evidence"| LEDGER
    LEDGER -->|"comment on timeline"| SUS
    BUDGET -.enforced on every step.- DB

    style BRAIN stroke:#ffb64d,stroke-width:2px
    style AGENTS stroke:#3fe0ff
    style WORLD stroke:#ff4fd8
```

The loop, in one line:

> **intake → route → investigate → findings (with evidence) → propose → learn**

## Why it is safe by construction

| Principle | Mechanism |
|---|---|
| Deterministic-first | Routing, dedup, ledger, budgets, policy all run with `ai.enabled=false`. The model only adds judgment. |
| Diagnosis-first | Agents hold read tools; every mutation becomes a **proposal** into a consumer's approval gate (approver != requester). |
| Evidence or it did not happen | Findings must cite the exact `tool_call_seq` events behind them; the runtime rejects unsupported findings and demands correction. |
| Budgets are hard | Token / tool-call / wall-time / subagent caps per task. Exhaustion parks the task `input_required` with the trail on the ledger. |
| Everything audited | Every route, tool call, refusal, proposal, finding and status change is an append-only ledger event. OTel spans mirror it. |

## The memory engine

The brain's memory is not a vector store with a REST hat. It is an
append-only, bi-temporal fact store with a scoring pipeline that explains
itself — every recall hit carries `score_components` showing exactly why
it surfaced (the search-side analogue of the findings evidence contract).

```mermaid
flowchart LR
    subgraph WRITE["WRITE PATH"]
        W["remember / remember_document"] --> DEF["defense<br/>off | redact | block"]
        DEF --> HASH["content-hash dedup"]
        HASH --> DB[("append-only region<br/>bi-temporal facts + typed links")]
        DB --> OUTBOX["outbox → bus"]
        OUTBOX --> ENR["async enricher<br/>embeds + similar_to links"]
        ENR --> ENT["entity extraction<br/>(opt-in) /entities/ + mentions/co_occurs"]
    end

    subgraph SLEEP["CONSOLIDATION — the Vegapunk sync"]
        C1["compaction<br/>superseded revisions fold"]
        C2["staleness-gated rollup<br/>no new facts → no LLM call"]
        C3["creative pass<br/>REM-like similar_to discovery"]
        C4["observations<br/>beliefs + source_ids + proof_count"]
        C5["reconcile<br/>merge near-duplicate beliefs"]
    end

    subgraph LEARN["LEARNING LAYER"]
        MM["mental models<br/>curated top-tier syntheses"]
        RF["reflect<br/>models → observations → raw<br/>verify · validated citations"]
    end

    subgraph RECALL["RECALL PATH"]
        Q["query"] --> TMP["temporal parse<br/>'errors last month' → window"]
        TMP --> FTS["FTS arm"]
        Q --> VEC["vector arm<br/>int8 quant · IVF index (opt-in)"]
        Q --> ENT["entity arm<br/>fuzzy name match (opt-in)"]
        FTS --> CAP["per-source caps"]
        CAP --> FUSE["RRF k=60<br/>or interleave"]
        VEC --> FUSE
        ENT --> FUSE
        FUSE --> BRIDGE["bridge discovery<br/>weighted graph, tanh-saturated"]
        BRIDGE --> BOOST["bounded boosts<br/>recency · importance · access · feedback · reinforce · proof · model"]
        BOOST --> PREF["models &gt; observations &gt; raw<br/>invalidated demoted · stale flagged"]
        PREF --> RERANK["cross-encoder rerank<br/>optional, opt-in"]
        RERANK --> BUDGET["token budget<br/>agents ask in tokens, not top-k"]
        BUDGET --> OUT["scored facts<br/>+ score_components"]
    end

    DB -. "ticker" .-> SLEEP
    SLEEP -. "writes back" .-> DB
    SLEEP --> LEARN
    LEARN -. "reflect reads" .-> RECALL
    DB --> RECALL
```

| Capability | What it does |
|---|---|
| Typed link taxonomy | 14 validated edge types with weights: 11 authorable (`relates_to`, `leads_to`, `occurred_before`, `prefers_over`, `exemplifies`, `contradicts`, `reinforces`, `invalidated_by`, `evolved_into`, `derived_from`, `part_of`) plus 3 the machine passes add (`similar_to`, `mentions`, `co_occurs`); recall demotes invalidated facts and walks the graph |
| Bridge discovery | A fact linked to ≥2 search hits surfaces even when it matches the query textually not at all — the principle behind two decisions gets found |
| Salience | `importance` (author-declared) + `access_count` (bumped on hits) fold into ranking as bounded multiplicative boosts; absent signal = exactly ×1.0 |
| Observations | Consolidation folds raw facts into beliefs carrying `source_ids` + `proof_count`, each labeled with its reasoning `kind` (explicit, deductive, inductive, abductive - inductive requires two sources or the belief is dropped); recall prefers the belief and drops its cited evidence; near-duplicate beliefs reconcile into one; surfaced beliefs are flagged `stale` when newer evidence exists |
| Mental models | Curated top-tier syntheses under `/mental-models/` outrank observations and are never superseded — the layer `reflect` checks first |
| Reflect | An agentic loop that gathers evidence hierarchically (mental models → observations → raw recall to verify), then answers with citations validated against what it actually retrieved — the evidence contract, applied to reasoning. Tunable per call: `level` (minimal→max scales the tool-round budget) and `schema` (a JSON Schema the answer conforms to, returned as a parsed `structured` value) |
| Secret scrubbing | `defense: redact\|block`, per-namespace, scrubs API keys, DSNs, JWTs at every write path (incl. JSONL import + region merge); a trigger emits an audit event with length-aware fingerprints — never the raw secret |
| Token-budget recall | `max_tokens` on recall/search fills the context budget with ranked facts instead of returning an arbitrary top-k |
| Time travel + language windows | `recall_as_of` any past instant; a query like "errors last month" parses to a time window, and windowed search spreads results across 8 time buckets so "what happened in June" spans June |
| Delta document ingest | `remember_document` chunks by paragraph and rewrites only changed chunks on re-ingest |
| Entity graph (opt-in) | With `memory.entities` on, an LLM extracts named entities per fact into `/entities/` nodes, links each mention, and counts co-occurrences — a knowledge graph over the region. Extraction batches under bursts (8 pending facts or 30s, one model call per batch, per-fact fallback on a malformed answer). Off by default; no model, no entities |
| Cross-encoder rerank (opt-in) | Set `reranker_url` to a TEI-style endpoint and scored search re-ranks its top candidates through it; `score_components` gains a `rerank` term. Empty URL = byte-identical, single-binary preserved |
| Vector quantization (opt-in) | `quantize_vectors` stores embeddings as int8 — 4× smaller, integer-dot-product scan, ~2% recall cost. Tagged encoding coexists with existing float32 vectors, no migration |
| Approximate index (opt-in) | `ivf_nprobe` enables an in-memory IVF index (deterministic k-means clusters, probed per query) for sublinear vector search past the brute-force ceiling — recall@10 ≈ 0.97 vs brute force, with a current-liveness guard so tombstones and updates are always honored |
| Benchmark | `punk membench` scores recall@k + MRR against a JSONL dataset — deterministic, no LLM judge; `--rerank` measures the cross-encoder delta; `--locomo` runs the LoCoMo retrieval benchmark (evidence-in-top-k over the gold annotations) |

## Install

```sh
# 0. Install script (linux/macOS, needs only curl and tar): downloads the
#    latest release, verifies the checksum, installs the binary, and --init
#    runs first-time migrations
curl -fsSL https://hypervisor-io.github.io/punk-records/install | bash -s -- --init

# 1. Prebuilt binary (tagged releases; linux/macOS/windows, amd64/arm64)
#    grab the archive for your platform from the Releases page, then:
tar xzf punk_*_linux_amd64.tar.gz && ./punk --version

# 2. From source (Go 1.26+)
go install github.com/hypervisor-io/punk-records/cmd/punk@latest

# 3. Container
docker compose up            # or: docker build -t punk .
```

One static binary, no CGO, no external services (SQLite by default).

## Quickstart

```sh
make build                       # -> bin/punk
./bin/punk migrate up       # SQLite by default; Postgres via config
./bin/punk validate specs   # the shipped database pack must pass
./bin/punk serve            # API :9090, dispatcher, hot-reload watcher

make demo                        # end-to-end: signed webhook -> routed ->
                                 # audited park (no model required)
```

Enable the model when ready (any OpenAI-compatible endpoint, Ollama
included): copy `punk.example.yaml` to `config.yaml`, set
`ai.enabled: true` and one profile.

## Central server

One punk, many machines.

On the server:

    docker compose --profile central up -d          # PUNK_DOMAIN=punk.example.com in .env
    docker compose exec punk punk apikey create --name alice --subject alice

On each developer machine (needs the punk binary; see Install):

    punk login --url https://punk.example.com --api-key prk_...
    cd ~/src/billing
    punk connect claude-code --project --verify

What that does: hooks and the MCP entry carry the bearer token from `~/.punk/credentials.json` (0600); the namespace is derived from the git remote, so every clone of `billing` on every machine shares `agent-billing-<hash>`; claims and registrations default to `<user>@<host>`; `--verify` proves the round trip from that machine. `punk namespace` prints what a directory maps to. Keep `/mcp` and `/v1` behind TLS: the token is a bearer credential.

## Using Postgres

SQLite is the default and needs nothing. To run on Postgres instead —
same schema, same migrations, same tests — point the `db` block of your
config at it. The Postgres driver is pure Go (no CGO), so the binary stays
one static artifact.

**1. Provision a database and a role:**

```sql
CREATE ROLE punk WITH LOGIN PASSWORD 'secret';
CREATE DATABASE punk OWNER punk;
```

Or spin one up with Docker:

```sh
docker run -d --name punk-pg -p 5432:5432 \
  -e POSTGRES_USER=punk -e POSTGRES_PASSWORD=secret -e POSTGRES_DB=punk \
  postgres:16
```

**2. Point the config at it** (`config.yaml`) — set the driver to
`postgres` and give a connection string:

```yaml
db:
  driver: postgres
  dsn: postgres://punk:secret@localhost:5432/punk?sslmode=disable
```

(For a managed Postgres, drop `sslmode=disable` and use the provider's
`sslmode=require` URL. Never put the password in a shared file — use an
env-interpolated DSN or a secret store.)

**3. Migrate and serve** — every `punk` subcommand reads the same `db`
config, so migrations run against Postgres and the coordinator serves from
it:

```sh
punk migrate up      --config config.yaml   # runs the Postgres migrations
punk validate specs  --config config.yaml
punk serve           --config config.yaml   # API + /mcp on :9090
```

`punk migrate status --config config.yaml` shows the applied version;
`punk migrate down --config config.yaml` rolls back one step. Everything
else (memory, regions, MCP, agents, export/import) behaves identically —
only the storage engine changes. Move an existing SQLite brain over with
`punk export` → `punk import` against the Postgres-configured binary.

## Use as agent memory

Punk Records doubles as persistent memory for the agents you already run: a
hook or plugin captures what the agent does, and the next session starts with
the project's memory injected as context. Six of the eight targets below are
coding agents; Hermes Agent and OpenClaw are general assistants with their own
hook and plugin systems, wired the same way.

```sh
punk serve                # memory server on :9090
punk connect claude-code  # or cursor, opencode, pi, antigravity, copilot, hermes, openclaw - see the matrix below
```

`punk connect claude-code` (also `cursor`, `opencode`) now wires three things: the capture and injection hooks, the punk MCP server entry (`/mcp?toolset=agent`, the lean session toolset), and, for Claude Code, the `mcp__punk` permission rule so calls never prompt. Add `--verify` to open a real MCP session and call `whoami` before you trust it. Use `--no-mcp` to keep the old hooks-only behaviour and `--force` to replace an `mcpServers.punk` entry punk did not write.

Inside a session the agent can omit `namespace` on every tool: it resolves from the workspace root the client advertises, exactly as the hooks derive it. `whoami` shows the result. `remember_many` writes up to 200 facts in one call; the stdio server (`punk mcp`) also accepts `remember_document {path}`. Subscribe to `punk://memory/<namespace>/<prefix>` to be notified of changes without polling.

| Target | Capture hooks | In-session tools | Where the tools come from |
| --- | --- | --- | --- |
| claude-code | settings.json | MCP | `~/.claude.json` or `.mcp.json`, `mcp__punk` permission |
| cursor | hooks.json | MCP | `~/.cursor/mcp.json` or `.cursor/mcp.json` |
| opencode | plugin | MCP | `opencode.json` `mcp.punk` |
| copilot | hooks.json | MCP | `~/.copilot/mcp-config.json` |
| antigravity | hooks.json | MCP | `~/.gemini/config/mcp_config.json` or `.agents/mcp_config.json` |
| hermes | config.yaml hooks | MCP | `~/.hermes/config.yaml` `mcp_servers.punk` |
| openclaw | plugin | MCP | `config.json` `mcp.servers.punk` |
| codex | hooks.json (`[features] hooks = true`) | MCP | `$CODEX_HOME/config.toml` `[mcp_servers.punk]` inside a punk-managed block |
| pi | extension | extension tools | `punk_whoami`, `punk_recall`, `punk_search`, `punk_remember` in the same extension file |

Every target accepts `--verify` (real round trip), `--no-mcp` (hooks only), `--force` (replace a foreign `punk` entry), `--api-key-env NAME`, `--agent NAME`, and `--no-skill`. All but `openclaw` and `hermes` also accept `--project`, which writes project-local files and derives the namespace from the git remote.

Every `punk connect <agent>` also installs a `punk-memory` skill where that agent loads skills from (`~/.claude/skills`, `$CODEX_HOME/skills`, `~/.agents/skills` for OpenCode, Cursor, Copilot and OpenClaw, `~/.gemini/config/skills`, `~/.hermes/skills/memory`, `~/.pi/agent/skills`; `--project` writes the project-local equivalent). It teaches namespaces, read routing, key conventions, claims and the `/tasks` coordination convention, feedback and compact output. A file you edited yourself is never overwritten. `punk skill print --agent <name>` shows the text; `--no-skill` skips it.

Codex asks once to trust the punk hook; with API keys enabled, export `PUNK_API_KEY` in the shell that starts Codex because Codex reads bearer tokens from the environment, not from a file.

| Agent | Wire up | Capture | Context injection |
|---|---|---|---|
| Claude Code | `punk connect claude-code` | hooks (SessionStart, UserPromptSubmit, PostToolUse, Stop) | native: SessionStart's `additionalContext` |
| Cursor | `punk connect cursor` | a `hooks.json` merge (six events; `beforeSubmitPrompt` answers `{"continue":true}`) | a project rules file via `--project`, otherwise paste into Cursor Settings -> Rules; also needs the punk MCP server registered in Cursor for `recall`/`remember` (once API keys exist, add a `headers: { "Authorization": "Bearer <key>" }` entry alongside `url` in `mcp.json`) |
| OpenCode | `punk connect opencode` | a JS plugin written to `.opencode/plugins/` (or `~/.config/opencode/plugins/` without `--project`) | system-prompt injection once per session, via OpenCode's `experimental.chat.system.transform` hook (marked experimental upstream) |
| pi | `punk connect pi` | a TS extension written to `.pi/extensions/` (or `~/.pi/agent/extensions/` without `--project`; project-local extensions only load once pi has trusted the project) | system-prompt injection once per session, via pi's `before_agent_start` hook (fires once per submitted prompt upstream; gated here to the session's first prompt) |
| Antigravity CLI | `punk connect antigravity` | a `hooks.json` merge under a `punk` hook-name key (`.agents/hooks.json`, or `~/.gemini/config/hooks.json` without `--project`): PostToolUse, PreInvocation (session start), and Stop (Stop answers `{"decision":"allow"}`, never `"continue"`, and PostToolUse always answers the required empty `{}`); no UserPromptSubmit capture - Antigravity's hook payloads carry no prompt text at all | via PreInvocation, gated to the conversation's first model call (`invocationNum==0`) since Antigravity has no session-start hook of its own: `{"injectSteps":[{"ephemeralMessage":...}]}` |
| GitHub Copilot CLI | `punk connect copilot` | punk's own `.github/hooks/punk.json` (or `~/.copilot/hooks/punk.json` without `--project`): SessionStart, UserPromptSubmit, PostToolUse, Stop, and SessionEnd, registered under their PascalCase names to select Copilot's snake_case "VS Code compatible" payload shape | additionalContext on SessionStart (documented in the hooks reference; fail-safe if ignored) |
| Hermes Agent | `punk connect hermes` | a `~/.hermes/config.yaml` merge of shell-hook entries (`on_session_start`, `pre_llm_call`, `post_tool_call`, `post_llm_call`); `on_session_end` is left unwired on purpose so its bare completed/interrupted pair cannot overwrite `post_llm_call`'s real assistant text on the same capture key. Hermes prompts for hook consent on first run unless `hooks_auto_accept` is set | native: `pre_llm_call` answers `{"context": ...}`, gated to the session's first turn via the payload's own `is_first_turn` |
| OpenClaw | `punk connect openclaw` | a JS plugin written to `~/.openclaw/plugins/punk-memory/` plus a `config.json` entry enabling it (`session_start`, `before_prompt_build`, `after_tool_call`, `agent_end`). Connecting also sets that entry's `hooks.allowConversationAccess` and `hooks.allowPromptInjection`, without which OpenClaw blocks the capture/injection hook | native: `before_prompt_build` returns `prependContext`, once per session |
| Codex CLI | no `punk connect` target yet | Codex CLI has its own stable, on-by-default `hooks.json` lifecycle hooks (SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, Stop), but punk does not wire into it yet | none via hooks today; memory is reachable via MCP only - `punk mcp` (stdio) or `/mcp` (HTTP), see the Interfaces section |

### Seeding an existing codebase

Hook capture is forward-looking: it learns what happens after you connect,
so a repository with years of history starts empty. `punk seed` closes that
gap from a deterministic code-knowledge tool:

```sh
rinnegan map --json | punk seed rinnegan     # one fact per architecture domain
```

[Rinnegan](https://github.com/hypervisor-io/rinnegan) derives the domain
map from the AST, so the seed is reproducible rather than model-generated:
the same corpus yields a byte-identical index, every edge is tagged with
its provenance (`ast_exact` through `unresolved`), and nothing leaves the
machine. Run both as MCP servers and an agent gets `understand`/`lookup`
for what the code is alongside `recall`/`remember` for why it got that
way; `rinnegan install <agent>` prints its own registration.
Facts land under `/code-map/` in the same `agent-<dir>` namespace hook
capture uses, so a freshly connected agent opens already knowing the shape
of the tree. Re-seeding is delta-based: an unchanged domain writes nothing,
a changed one supersedes itself, and a domain that no longer exists is
tombstoned so a deleted package stops being injected. Facts you wrote by
hand under the same prefix are never touched.

`/code-map/` is deliberately the ordinary raw tier, not `/mental-models/`:
a derived map goes stale on the next commit, and the tier that is never
superseded is the wrong place for anything a machine regenerates.

What happens per session, whichever agent you wired up:

- session starts, prompts, tool calls, and final answers land as facts
  under `/agent-sessions/<id>/` in a per-project namespace (`agent-<dir>`),
  replay-deduped per key, secret-scrubbed when `memory.defense` is set, and
  picked up by the async enricher for embedding (if `ai.embeddings.model`
  is set) and entity extraction (if `memory.entities` and `ai.enabled` are
  on)
- on the next session start, context injection (see the matrix above)
  delivers a memory block: your profile card, the last session's rolling
  summary, known entities, then important and recent facts,
  token-budgeted (`memory.inject` selects the components; add
  `directives` for a fixed memory-usage note)
- with `ai.enabled` and `memory.session_summary_tokens` set, a rolling
  recursive summary per session (`/agent-sessions/<sid>/summary`) folds
  prior summary + new events every time enough new capture accumulates,
  so long sessions stay compressed mid-flight
- on every prompt after that, agents with a per-turn contract (Claude
  Code and Hermes today) also get a small prompt-scoped block: hybrid-
  search hits for the prompt text, minus everything the session was
  already shown, budgeted by `memory.turn_context_tokens` (default 600;
  0 disables)
- consolidation folds sessions into observations over time (with
  `ai.enabled` and `memory.consolidate_days` > 0); raw capture stops
  being retrievable after 30 days, and rows are physically purged only
  once `memory.retention_days` enables the hourly sweep. Passes are
  dream-scheduled per namespace - enough new writes (20), a 30-minute
  idle debounce so a mid-burst region isn't consolidated between two
  writes, at least 6h between passes, at most 24h behind any activity -
  and a quiet namespace is skipped entirely. `punk consolidate [--ns]`
  runs a pass immediately, and `diagnose` reports
  `last_consolidated_at` + `writes_since_consolidation`

A cross-project **profile card** rides every
session-start block: stable facts and standing instructions about you,
kept under `/profile/` in the global `user-profile` namespace and capped
at 40 entries. Manage it with:

```sh
punk card add "Prefers table-driven tests"
punk card add --key timezone "Works from IST"
punk card list
punk card remove --key timezone
```

Environment for the hook or plugin process: `PUNK_URL` (default
`http://localhost:9090`), `PUNK_API_KEY` (required once API keys exist).
Capture is fail-open: a dead server never breaks the coding session.

If the `punk` binary is moved or removed after connecting, rerun `punk
connect <target>` to repoint the hook entries at the new path: Cursor's
`beforeSubmitPrompt` entry invokes `punk` directly and fails per Cursor's
own hook-error semantics if that path is gone, unlike capture elsewhere,
which stays fail-open.

Captured tool output re-enters the next session's context, so untrusted
content an agent reads can come back as injected context; set
`memory.defense` (`redact` or `block`) to scrub secrets at capture, and
review what lands under `/agent-sessions/` when working in untrusted
repos.

Mine recurring investigations into skill drafts (needs `ai.enabled`; reads
`config.yaml` by default, `--config` to override):

```sh
punk skills propose --min-count 3      # writes proposed-skills/<slug>/SKILL.md
punk skills insights --ns agent-myproj # distills a project's memory into
                                       # proposed-insights/<ns>/CLAUDE-additions.md -
                                       # style/workflow/architecture/gotcha rules, each
                                       # citing the fact IDs behind it; merge by hand,
                                       # punk never edits CLAUDE.md itself
```

## Declaring an agent

```markdown
---
name: database
version: 0.2.0
description: Database domain specialist
autonomy: propose            # observe | advise | propose | auto
triggers:
  - source: incidents
    labels: { domain: database }
tools: [incidents__*]
skills: [db-connection-triage, db-lock-contention]
budgets: { tokens: 120000, tool_calls: 30 }
---
You are the database domain agent. Work the evidence...
```

Drop it in `specs/agents/`, and the running coordinator validates and
hot-swaps it (bad edits keep the previous snapshot; in-flight tasks finish
on the version they started with). Skills are
[agentskills.io](https://agentskills.io)-conformant `SKILL.md` folders.

## Interfaces

| Surface | What |
|---|---|
| REST `/v1` | tasks, proposals, memory, webhook intake, agent hooks - bearer-keyed (`punk apikey create`) |
| MCP | `punk mcp` (stdio) or `/mcp` (HTTP): `submit_task`, `get_task`, `list_agents`, `remember`, `remember_document`, `remember_model`, `recall`, `recall_as_of`, `forget`, `search` (hybrid / scored / interleave / temporal / reranked / `max_tokens` / `expand`), `unified_search`, `triplet_search`, `reflect` (when a model is configured), `list_keys`, `list_models`, `list_entities`, `feedback`, `profile`, `diagnose`, `link` / `unlink` / `neighbors` |
| A2A (in) | `POST /v1/a2a` (Agent2Agent v0.3 JSON-RPC + SSE): `message/send`, `message/stream`, `tasks/{get,cancel,resubscribe}`, push configs; card at `/.well-known/agent-card.json` |
| A2A (out) | delegate to foreign agents: `punk a2a card\|send` (CLI) or the `delegate` MCP tool over `a2a.remotes` |
| CLI | `serve` / `migrate` / `validate` / `apikey` / `export` + `import` (memory JSONL) / `a2a` / `itbench` / `membench` / `hook` / `connect` / `skills` |
| Evals | `punk replay` (golden-ledger trajectory), `punk itbench run` (ITBench SRE faulty-entity scoring), `punk membench` (recall@k + MRR) |
| OTel | `invoke_agent`, `chat`, `execute_tool`, `task.route` spans, OTLP export |

Agents should ask for `format: compact` (MCP) or `?format=compact` (HTTP): each hit becomes key, clipped body, score and flags, which is what an agent needs to judge relevance and roughly a third of the tokens of a full fact. Put exact identifiers, error strings or file names in `anchors`; each becomes an extra phrase-match route in the fusion rather than a filter. Pass `repo_revision` to have code-map facts seeded from another revision flagged `stale`.

With `ai.embeddings.provider: local`, punk embeds in-process with a pinned static model (about 31 MB, downloaded once from huggingface.co) and needs no Ollama. Run `punk embed-backfill --ns <ns> --force` after switching providers or upgrading past a release that changed the embedding input.

## Brain view

Open the server in a browser and you get the brain: a live visualization of every memory namespace, served from `/` (alias `/brain`). Each region is one namespace; a region glows with write, claim and task activity in it, a spark marks each event, and a side log narrates who is doing what where. Every asset ships inside the binary (embedded Three.js), so the page needs no network; on an authenticated server it reads the bearer token from the `amk` localStorage key, same as the operator console at `/ui`.

```sh
punk brain            # print the brain URL of a running server
punk brain --open     # ...and open it in the default browser
punk brain serve      # start the server and print the brain URL first
```

The page loads one snapshot and then follows the event stream.

`GET /v1/brain/snapshot`: per-namespace counts, members, claims, task tallies and the 5-minute write rate, sorted by most recent write:

```json
{
  "version": "v1.6.0",
  "now": "2026-09-04T03:20:11Z",
  "namespaces": [
    {
      "name": "agent-codehamsa",
      "facts": 412, "observations": 3, "models": 0,
      "members": [{"namespace":"agent-codehamsa","agent":"worker-2","role":"worker","joined_at":"..."}],
      "claims":  [{"namespace":"agent-codehamsa","key":"/tasks/S1A-4","holder":"worker-2","claimed_at":"...","expires_at":"..."}],
      "tasks": {"total": 261, "done": 12, "blocked": 1, "pending": 248},
      "writes_5m": 17,
      "last_write_at": "2026-09-04T03:19:58Z"
    }
  ]
}
```

`GET /v1/brain/events`: server-wide SSE of bus events, one JSON envelope per event; a `hello` frame comes first and `: ping` keepalives run every 15 seconds:

```
event: memory
data: {"ts":"2026-09-04T03:20:12Z","kind":"memory","namespace":"agent-codehamsa","key":"/tasks/S1A-4/status","data":{"action":"add","writer":"worker-2","namespace":"agent-codehamsa","key":"/tasks/S1A-4/status"}}
```

## Repo map

```
cmd/punk/          one binary: serve, migrate, validate, apikey, mcp, export, import
internal/
  spec, registry    agent/skill/policy parsing + hot-reload snapshots
  task              event-sourced ledger, budgets, leases
  route             deterministic router, recorded decisions
  agent             investigation runtime, subagents, evidence contract
  policy            autonomy ladder, action classes, proposals
  memory            fact store: scored recall, bridges, observations,
                    mental models, reconciliation, salience, temporal
                    parsing, per-ns secret scrub, token budgets, doc ingest,
                    entity graph (opt-in), cross-encoder rerank (opt-in)
  reflect           agentic hierarchical retrieval, validated citations
  membench          deterministic recall benchmark (recall@k, MRR)
  llm, mcpclient    OpenAI-compatible profiles, MCP tool pool
  mcpserver, api    the two front doors (same service layer)
  region            brain regions, agent registration, work-claim leases
specs/              the shipped database pack (agent + 4 skills + policy)
docs/               operator documentation (configuration reference)
site/               GitHub Pages landing page
```

## Documentation

- [docs/CONFIG.md](docs/CONFIG.md) - every configuration key, with defaults

## License

Punk Records is free software, dual-licensed under **LGPL-2.1 or LGPL-3.0**,
at your option - the same arrangement [Ceph](https://github.com/ceph/ceph/blob/main/COPYING)
uses, and for the same reason: the version 3 option keeps the code combinable
with Apache-2.0 software, which punk already depends on.

```
SPDX-License-Identifier: LGPL-2.1-only OR LGPL-3.0-only
```

Full texts: [COPYING-LGPL2.1](COPYING-LGPL2.1), [COPYING-LGPL3](COPYING-LGPL3),
and [COPYING-GPL3](COPYING-GPL3) (LGPL-3.0 is written as additional permissions
on top of GPL-3.0, so its text is needed to read LGPL-3.0). Summary in
[LICENSE](LICENSE).

Copyright (c) 2026 ServerGurus and contributors. Trademarks are not licensed:
the software is free, the names are not. Licensing inquiries:
legal@servergurus.com.
