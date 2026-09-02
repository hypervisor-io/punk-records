package mcpserver

// Instructions is sent to every MCP client at initialize. It is the only
// guidance punk ships into an agent: connect adapters install hooks, never
// prose, so this text has to carry the routing rules on its own.
const Instructions = `Punk Records is the shared memory plane for this workspace: prior sessions, decisions, conventions, incidents, entities and their relations. It is evidence about the past, not a substitute for reading the current code.

Retrieval routing:
- recall: you know the key prefix (for example /decisions, /code-map, /entities). Deterministic, unranked.
- search: you know words or identifiers. Set hybrid and scored for ranked fusion. Put exact identifiers, error strings, flags or file names in anchors; they become extra retrieval routes, not hard filters. Pass format "compact" unless you need attributes or timestamps.
- unified_search: wording unknown, or the answer spans facts and relations (architecture, causality, history, "why" questions). Prefer it first; pass format "compact".
- triplet_search and neighbors: follow relations from a known key.
- recall_as_of: what was believed at a past instant.
- list_keys: discover keys. Never invent a key.

Reading results:
- A compact hit is already-read evidence. Call recall on its key only when the clipped body is insufficient.
- flags: stale means newer raw facts exist since this synthesis was made; invalidated means a later fact superseded it (demoted, not hidden); model means a curated mental model; relation means the hit is an edge, rendered as "from -> type -> to".
- Do not launch a sub-agent solely to query memory; one or two direct calls are cheaper.

Writing:
- remember a durable decision, fix, convention or gotcha under a hierarchical key. One fact per key; the latest revision wins and history is kept.
- remember_document for long text; only changed chunks are rewritten.
- feedback with the ids of hits that helped or misled, so ranking learns.
- Do not store secrets. Write-time scrubbing may redact or block them.`
