package mcpserver

import "github.com/hypervisor-io/punk-records/internal/hookcli"

// Instructions is sent to every MCP client at initialize. It is the only
// guidance punk ships into an agent: connect adapters install hooks, never
// prose, so this text has to carry the routing rules on its own. The
// retrieval and writing rules are the punk-memory skill's routing section
// verbatim (hookcli.RoutingSection), so the two can never drift.
var Instructions = "Punk Records is the shared memory plane for this workspace: prior sessions, decisions, conventions, incidents, entities and their relations. It is evidence about the past, not a substitute for reading the current code. Call whoami once at session start; a full usage skill named punk-memory is installed by punk connect.\n\nRetrieval and writing:\n" + hookcli.RoutingSection() + "\n\nCoordination: register once, claim_work before taking a task or editing a shared file, release_work after; tasks are facts at /tasks/<id> with status at /tasks/<id>/status (done: ... or blocked: ...); questions at /questions/<id>, answers at /answers/<id>. Do not launch a sub-agent solely to query memory."
