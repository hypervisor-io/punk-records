# Punk Records — capabilities for users and teams

Punk Records is a self-hosted memory and coordination service for AI agents. It gives agents a shared place to retain project knowledge, retrieve supporting evidence, coordinate work, and carry useful context between sessions and tools. It also provides a runtime for policy-controlled specialist investigations.

Use it to help a coding assistant remember project decisions, let several agents work from shared knowledge, turn operational investigations into reusable procedures, or build an auditable agent workflow around your own tools.

**Availability — v1.10.0, 25 September 2026:** "Core" means functionality present before the improvement pipeline; "Integrated" means reviewed additions included in v1.9.0 or v1.10.0. v1.10.0 adds opt-in agent-to-agent messaging (section 5); its [implementation review](reports/2026-09-25-agent-messaging-review.md) records automated verification and the limits still open (no live-client transcripts, Postgres and Windows runtime unverified). The installed version and enabled configuration determine what a deployment provides. Bounded reflect expansion and hierarchical summaries are Go-library opt-ins; they do not yet have dedicated CLI, MCP, or configuration switches.

## 1. Persistent project and team memory — Core

- Store decisions, conventions, incidents, runbooks, preferences, findings, and other knowledge under human-readable keys.
- Organize knowledge into namespaces for projects, teams, or domains.
- Share a namespace across compatible agents and multiple machines using a central server.
- Resolve project namespaces from repository/workspace identity, with explicit namespaces available for shared coordination.
- Preserve revisions when facts change; retrieve current facts or historical knowledge while that history is retained.
- Attach authorship, timestamps, importance, provenance, and structured attributes to facts.
- Deduplicate repeated content, write facts in batches, and ingest documents as chunks.
- Expire or forget facts and configure retention sweeps.

**Example:** An assistant starting a new session can recover why a migration was chosen, which workaround failed, and which conventions the team follows.

## 2. Search and evidence retrieval — Core; optional model components

- Exact-key and prefix recall for known knowledge.
- Full-text search, semantic vector search, and hybrid retrieval.
- Unified retrieval that combines facts and relationships.
- Search anchors for exact identifiers, error messages, filenames, and other precise terms.
- Time filters, historical recall, and supported natural-language time windows.
- Token-budgeted results and compact output for agent context windows.
- Inspectable ranking signals, including relevance, recency, importance, feedback, reinforcement, and derived-knowledge signals.
- Optional cross-encoder reranking through a configured endpoint.
- Optional quantized vectors and an approximate vector index.

Semantic search needs embeddings. Keyword recall and the deterministic memory/coordination functions can operate without a generative model.

## 3. Relationships and knowledge graphs — Core; typed extraction Integrated

- Link facts using relationships such as reinforces, contradicts, derives from, forms part of, precedes, or leads to.
- Query relationships and inspect neighboring facts.
- Discover connecting facts through graph links, including facts that do not directly match the query text.
- Optionally extract named entities from stored facts and record mentions and co-occurrences.
- **Integrated:** structured entity types, an optional domain vocabulary, aliases, and attribution to exact source revisions.
- **Integrated:** evidence-based alias-merge proposals, protected-type checks, reversible application, and preservation of merge history during later enrichment.

**Example:** Connect a service, an incident, a configuration decision, and the evidence explaining their relationship.

## 4. Context across agent sessions — Core; Codex hardening Integrated

- Capture supported lifecycle events through client-specific hooks or plugins.
- Inject relevant project memory at the start of a later session.
- Maintain a cross-project profile card for preferences and standing instructions.
- Generate rolling session summaries when configured with a model.
- Deliver deduplicated, prompt-relevant context on clients that support the required per-turn hooks.
- Seed architecture knowledge from Rinnegan output and refresh changed domains incrementally.
- Install the `punk-memory` and `punk-plan` skills to teach agents the memory and coordination workflows.
- Verify a configured connection through a real MCP round trip.

Connection targets in the current source include Claude Code, Cursor, OpenCode, GitHub Copilot CLI, Codex CLI, pi, Antigravity, Hermes, and OpenClaw. Capture, injection, and tool availability vary by client and version.

**Integrated:** Codex hook normalization, managed-integration reconciliation, namespace diagnostics, bounded guidance, safer default task waits, deduplicated context delivery, and the complete Codex 0.153.4 acceptance gate (native run evidence and the user-confirmed `terminal_title` mitigation are recorded in `docs/investigations/codex-0.153.4-terminal-spam.md`; the exact original renderer was not identified).

## 5. Multi-agent work coordination — Core; agent messaging Integrated (opt-in)

- Register agents in a shared coordination namespace.
- Define tasks, dependencies, and explicit completion criteria.
- Track pending, in-progress, review, blocked, and done states.
- Identify work whose prerequisites are complete.
- Claim tasks or file paths using renewable leases.
- Share handoffs, questions, answers, implementation evidence, and review feedback.
- Wait for task, claim, and status changes through MCP or event streams.
- Build a worker/reviewer workflow that requires reviewed completion before dependent work begins.

- Send durable, namespace-scoped messages between registered agent sessions: idempotent sends, threaded replies, owner-scoped delivery leases, explicit ACK, unread counts and sender receipts.
- Deliver messages into a running coding agent through its own hook events (Claude Code, Codex, Cursor, Copilot CLI, Antigravity, Hermes, Cline), or wake an idle session where the client supports it (Pi, OpenCode); OpenClaw catches up on its next prompt.
- Enable per server (`messaging.enabled` or `PUNK_MESSAGING=1`) and per client (`punk connect <client> --messaging`); disabled deployments keep the unchanged lean tool set. Details and limits: [agent messaging](agent-messaging.md).

Claims coordinate cooperating agents; workers must report their status and respect ownership. External coding workers are launched by their host or coordinator. Punk supplies the shared state and coordination tools.

## 6. Specialist investigation runtime — Core; model/tool configuration required

- Declare specialist agents in files, with triggers, instructions, tool access, skills, and budgets.
- Route incoming work using declarative rules and record the routing decision.
- Accept tasks through REST, MCP, webhook intake, and supported agent-to-agent interfaces.
- Run investigations against configured MCP tool servers.
- Require findings to reference the tool-call evidence used to produce them.
- Track tasks and investigations in an append-only event ledger.
- Support bounded subagent work and delegation to configured remote agents.
- Hot-reload valid agent, skill, and policy specifications while existing tasks retain their starting specification version.

A database specialist and procedures are included as a starting pack; other specialists can be defined for the user's environment.

## 7. Policy, approval, access, and budgets — Core; namespace authorization Integrated

- Configure autonomy levels: observe, advise, propose, or auto.
- Classify tool actions and allow, deny, or require a proposal according to configured policy.
- Record proposals for external approval, including requester/approver separation and expiry.
- Apply per-task token, tool-call, wall-time, and subagent limits.
- Inspect cost records and configure daily spending-projection alerts.
- Park exhausted tasks with their recorded history rather than silently continuing beyond limits.
- Issue and revoke API keys.
- Configure memory-prefix write policies.
- Configure per-namespace secret detection with redaction or rejection modes.
- **Integrated:** explicit namespace read/write/admin grants tied to verified identities, with enforcement across credential-verified HTTP access paths, including HTTP MCP, when enabled.

These controls depend on configuration. Namespace authorization enforcement is opt-in; a namespace name alone is not an access-control boundary. Local CLI and stdio MCP access use the local operating-system authority. Data sent to a configured external model, embedding service, or tool follows that service's deployment and data-handling arrangement. Under `authz.enforcement: deny`, calling `search_skills` or `load_skill` with an omitted namespace requires a read grant on the skill index namespace (`SkillNamespace`, falling back to `DefaultNamespace`, default `agent-default`), independent of any grant on the caller's workspace-root namespace.

## 8. Consolidation and evidence-grounded answers — Core; model-dependent

- Compact retained memory and consolidate raw facts into source-linked observations.
- Track supporting source IDs and proof counts on derived observations.
- Reconcile similar observations and flag stale derived knowledge when newer evidence exists.
- Optionally identify contradictions and represent them as relationships.
- Maintain curated mental models for important, reusable syntheses.
- Use `reflect` to gather evidence across mental models, observations, and raw facts before answering.
- Validate answer citations against retrieved evidence and optionally produce a structured answer matching a supplied schema.
- Adjust reasoning effort and retrieval budgets.

These mechanisms make evidence inspectable; citation validation establishes that cited evidence was retrieved, not that every generated claim is correct.

## 9. Reusable procedures and learning from outcomes — Core / Integrated

- **Core:** author procedures as `SKILL.md` files and associate them with specialist agents.
- **Core:** mine recurring investigations into proposed skill drafts.
- **Core:** generate source-cited suggestions for project instructions and practices.
- **Integrated:** discover procedures through concise metadata and load the exact selected version on demand.
- **Integrated:** activate/deactivate versions, reject changes to an already published version's content, and retain its content identity after unpublishing and retention.
- **Integrated:** attach outcomes to exact skill versions and originating task evidence; propose improvements for approval; apply approved changes idempotently and support return to a prior version. The shipped acceptance fixtures are deterministic contains/absent assertions on procedure content; they do not execute a procedure or prove runtime task success.

“Learning” here means updating memory and proposing versioned procedures. Model-weight training is outside this pipeline.

## 10. Documents and ingestion — Core / Integrated

- **Core:** ingest text documents as chunks and update changed content on re-ingestion.
- **Core:** import/export memory in JSONL and seed code-map knowledge.
- **Integrated:** retain document identity, source URI, revision metadata, and chunk-level locations.
- **Integrated:** keep unaffected chunks stable when content is inserted elsewhere in a source document.
- **Integrated:** respect source ownership and preserve user-replaced chunks during reconciliation.
- **Integrated:** loaders for plain text, Markdown, HTML, and structured incident JSON.
- **Integrated:** optional PDF extraction through an external adapter, explicit URL fetching with destination checks, and bounded input/process handling.
- **Integrated:** preview ingestion/enrichment work before execution (`punk ingest --dry-run`), separating measured input bytes from approximate (`bytes/4`) token and cost estimates.

The PDF work provides an adapter contract; an extractor must be supplied separately.

## 11. Retrieval improvements — Integrated (opt-in)

- Explicit retrieval strategies with inspectable routing reasons and fallbacks (REST `?strategy=`, MCP `search`/`unified_search` `strategy`; omitting it keeps the legacy fused behavior).
- Optional deterministic selection of an appropriate retrieval strategy (`auto`); no classifier model call.
- Bounded expansion of related evidence during `reflect`, with stopping conditions and budget controls (Go-library opt-in `reflect.Opts.ExpandEvidence`; no CLI/MCP switch in this build).
- Optional hierarchical summaries linked to their sources (Go-library opt-in `reflect.Opts.Summaries` over `Store.BuildSummaryTree`; no CLI/MCP switch in this build).
- Incremental invalidation of affected summaries when their source material changes.
- Comparative evaluation of these additions before changing default behavior; the evaluation fixtures are synthetic and measured neutral, not a demonstrated quality gain.

## 12. Reliability and observability — Core; durable enrichment Integrated

- SQLite or PostgreSQL persistence.
- An outbox-backed event path for memory updates.
- A task ledger for auditing routes, tool calls, policy decisions, findings, and status changes.
- Live task boards, an operator console, and a browser-based brain view of namespaces and activity.
- Event streams, logs, memory diagnostics, and OpenTelemetry/OTLP tracing.
- Backup/export/import tools and schema migration commands.
- Region branch/merge workflows for experimenting with memory snapshots.
- Import service topology from Backstage catalogs.
- **Integrated:** durable enrichment run records, stage status, retry lineage, restart recovery, and transactional checks to prevent stale workers from committing outdated derived state.

## 13. Evaluation and regression measurement — Core; expanded evaluation Integrated

- Retrieval benchmarks with recall and ranking metrics.
- Dataset-based comparisons and retrieval ablations.
- Golden-ledger trajectory replay and an ITBench-oriented evaluation workflow.
- **Integrated:** reproducible retrieval baselines with build/configuration provenance and saved evidence.
- **Integrated:** separate evaluation of answer correctness, citation existence, claim support, and abstention.
- A way to assess whether a change improves the user's corpus and workflow under comparable conditions.

Published performance claims should come from the selected dataset, configuration, hardware, and model rather than a universal accuracy or speed promise.

## 14. Deployment and integration options — Core

- A Go binary with SQLite as the default local storage option.
- PostgreSQL for deployments that need an external database.
- Local or centrally hosted operation, plus container deployment.
- MCP over stdio or HTTP, a REST API, CLI commands, and supported A2A endpoints.
- Configurable OpenAI-compatible model endpoints, including locally hosted models.
- Local in-process embeddings or a configured embedding service.
- Linux and macOS builds for amd64/arm64, and a Windows amd64 release target.
- Free-software licensing under LGPL-2.1-only or LGPL-3.0-only.

## Who can use Punk?

- **Developers:** carry project decisions, preferences, and debugging history between assistant sessions.
- **Agent builders:** add persistent memory, retrieval, evidence, and coordination to existing agents.
- **Engineering teams:** give cooperating agents shared conventions, task ownership, and review handoffs.
- **Operations teams:** run auditable, budgeted investigations against their own tools and build a reusable procedural knowledge base.
- **Organizations hosting their own infrastructure:** choose where memory, databases, models, and tools run.

## Short description to share

> Punk Records gives AI agents persistent shared memory and a place to coordinate work. It combines searchable project knowledge, relationships between facts, session context, task ownership, evidence trails, and policy-controlled investigations in a self-hosted Go service. Its current improvement pipeline adds stronger source provenance, versioned skill discovery, more reliable enrichment, richer document ingestion, and inspectable retrieval strategies.

## Basis of this guide

Reviewed against local integration source `9159cb069ec014ddc9667f4877925200435d4d90` on 7 September 2026. Sources: [README](../README.md), [configuration](CONFIG.md), [MCP toolsets](../internal/mcpserver/toolset.go), [CLI integration targets](../cmd/punk/main.go), [policy engine](../internal/policy/policy.go), [namespace authorization](../internal/authz/authz.go), [release targets](../.github/workflows/release.yml), the [improvement plan](superpowers/plans/2026-09-06-cognee-borrowing.md), and the [pipeline acceptance report](reports/punk-improvement-pipeline.md). The README client tables' Codex row is now current (hooks plus MCP entry; `terminal_title` mitigation documented).
