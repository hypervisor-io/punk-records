package hookcli

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
)

// claudeEnvelope is the client-side encode of the same Claude Code hook
// shape the server decodes as agentHookIn (internal/api/agent_handlers.go).
// Field names and JSON tags here are load-bearing: they must match the
// server struct exactly, not just "look similar" - a typo'd tag silently
// drops the field server-side instead of failing to compile.
type claudeEnvelope struct {
	HookEventName        string          `json:"hook_event_name"`
	SessionID            string          `json:"session_id"`
	CWD                  string          `json:"cwd"`
	Source               string          `json:"source"`
	Prompt               string          `json:"prompt,omitempty"`
	PromptID             string          `json:"prompt_id,omitempty"`
	ToolName             string          `json:"tool_name,omitempty"`
	ToolInput            json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse         json.RawMessage `json:"tool_response,omitempty"`
	ToolUseID            string          `json:"tool_use_id,omitempty"`
	LastAssistantMessage string          `json:"last_assistant_message,omitempty"`
}

// cursorPayload is the superset of Cursor native hook stdin fields this
// translator reads, sourced from cursor.com/docs/hooks (fetched directly;
// field names are not guessed). Base fields (conversation_id,
// workspace_roots, ...) are documented as present on every agent hook
// event; the remaining fields are each event-specific and only populated
// for the events that carry them.
type cursorPayload struct {
	HookEventName  string `json:"hook_event_name"`
	ConversationID string `json:"conversation_id"`
	// GenerationID is documented (cursor.com/docs/agent/hooks) as a base
	// field present on every agent hook event, alongside conversation_id -
	// it identifies one specific generation (turn), which is exactly what
	// Claude Code's own per-prompt prompt_id keys. Unlike conversation_id
	// (stable for a whole conversation), a fresh generation_id is minted
	// per beforeSubmitPrompt call, matching prompt_id's per-prompt grain.
	GenerationID   string          `json:"generation_id"`
	WorkspaceRoots []string        `json:"workspace_roots"`
	CWD            string          `json:"cwd"`
	Prompt         string          `json:"prompt"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolOutput     json.RawMessage `json:"tool_output"`
	ToolUseID      string          `json:"tool_use_id"`
	FilePath       string          `json:"file_path"`
	// Edits is afterFileEdit's own field (cursor.com/docs/agent/hooks:
	// "edits": [{"old_string":..., "new_string":...}]) - the actual change
	// content. Kept as json.RawMessage and carried straight into the
	// synthesized tool_input rather than decoded/re-encoded, so the
	// translator never has to track Cursor's edit-entry shape.
	Edits json.RawMessage `json:"edits"`
	// Status is stop's own field ("completed"|"aborted"|"error" per
	// cursor.com/docs/agent/hooks). loop_count is documented alongside it
	// but is not carried through: it's a bare counter, not message content.
	Status string `json:"status"`
	// Reason and FinalStatus are sessionEnd's own fields (cursor.com/docs/
	// agent/hooks: reason is "completed"|"aborted"|"error"|"window_close"|
	// "user_close"; final_status is a free-form status string). duration_ms
	// and error_message are documented too but are not carried through here
	// - error_message only exists when reason=="error" and would need its
	// own conditional handling; reason+final_status alone already gives the
	// terminal Stop fact non-empty content, per this task's scope.
	Reason      string `json:"reason"`
	FinalStatus string `json:"final_status"`
}

// translator maps one agent's native hook payload to the Claude-shaped
// envelope. ok=false means the event has no mapping and must be skipped
// silently (never an error) - the caller forwards nothing in that case.
type translator func(raw []byte) (out []byte, ok bool, err error)

// translators is the from-agent registry. Adding a new agent means adding
// one entry here plus its own translateX function; Normalize itself never
// changes.
//
// Antigravity is deliberately NOT registered here. Every translator in
// this registry is looked up by RunFrom purely from the native payload's
// own bytes (Cursor's hook_event_name field self-identifies the event);
// Antigravity's documented hook payloads carry no such field at all (see
// translateAntigravity's own doc comment below for the exhaustive check),
// so its translation function needs an externally-supplied event name that
// the translator func(raw []byte) signature above has no parameter for.
// Antigravity is wired through its own entry point instead -
// RunFromAntigravity (hookcli.go), called directly by cmdHook - which
// calls translateAntigravity(event, raw) with the event name cmdHook's
// own --event flag supplies. This keeps the translator type, Normalize,
// and RunFrom (and every existing --from cursor/claude call site and
// test) completely untouched.
var translators = map[string]translator{
	"cursor":  translateCursor,
	"copilot": translateCopilot,
	"hermes":  translateHermes,
}

// Normalize translates a native hook payload from agent "from" into the
// Claude-shaped envelope JSON the server's /v1/agent/hooks already
// understands. from is matched case-insensitively against the registry (a
// --from Cursor or --from CURSOR typo-of-case must resolve the same as
// --from cursor, not silently fail to match a lowercase-only map key). An
// unrecognized "from" is an error (there is no silent fallback - a caller
// asking to normalize for an agent with no registered translator has a
// configuration bug, not a skippable event). A recognized agent whose
// specific event has no mapping returns ok=false with a nil error: that is
// normal, expected input (most native events have no Claude Code
// equivalent), not a failure.
func Normalize(from string, raw []byte) ([]byte, bool, error) {
	t, known := translators[strings.ToLower(from)]
	if !known {
		return nil, false, fmt.Errorf("hookcli: normalize: unknown agent %q", from)
	}
	return t(raw)
}

// translateCursor documents the sessionStart/beforeSubmitPrompt/
// postToolUse/afterFileEdit/stop/sessionEnd -> Claude event mapping this
// translator implements; see the switch below for the actual logic and
// per-case rationale comments.
//
// cursor native event   -> claude-shaped hook_event_name
// sessionStart           -> SessionStart
// beforeSubmitPrompt     -> UserPromptSubmit
// postToolUse            -> PostToolUse
// afterFileEdit          -> PostToolUse (tool_name synthesized "Edit")
// stop                   -> Stop
// sessionEnd             -> Stop
// anything else          -> skipped (ok=false), never an error
func translateCursor(raw []byte) ([]byte, bool, error) {
	var p cursorPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, false, fmt.Errorf("hookcli: cursor payload: %w", err)
	}

	env := claudeEnvelope{
		Source: "cursor",
		// session_id: conversation_id, not Cursor's own "session_id"
		// field. cursor.com/docs/hooks documents conversation_id as a
		// base field present on every agent hook event, while
		// "session_id" is only guaranteed on sessionStart/sessionEnd.
		// Using session_id would leave every other translated event
		// (postToolUse, afterFileEdit, beforeSubmitPrompt) with no
		// session key at all, breaking the server's per-session
		// grouping under /agent-sessions/<sid>/... . conversation_id is
		// the one field that can consistently key a whole conversation.
		SessionID: p.ConversationID,
		CWD:       cursorCWD(p),
	}

	switch p.HookEventName {
	case "sessionStart":
		env.HookEventName = "SessionStart"

	case "beforeSubmitPrompt":
		env.HookEventName = "UserPromptSubmit"
		env.Prompt = p.Prompt
		// prompt_id is required server-side (agent_handlers.go's
		// UserPromptSubmit case ignores any request whose sanitized
		// prompt_id is empty) - with no prompt_id at all, every translated
		// Cursor prompt was silently dropped as "ignored". generation_id is
		// cursor.com/docs/agent/hooks' documented per-generation base
		// field, present on every hook event including beforeSubmitPrompt,
		// and (unlike conversation_id) is fresh per turn - the same grain
		// Claude Code's own prompt_id has. Only fall back to a synthesized
		// id when generation_id is genuinely absent from the payload, so
		// capture still lands instead of silently vanishing.
		env.PromptID = p.GenerationID
		if env.PromptID == "" {
			env.PromptID = promptIDFallback(p.ConversationID, p.Prompt)
		}

	case "postToolUse":
		env.HookEventName = "PostToolUse"
		env.ToolName = p.ToolName
		env.ToolInput = p.ToolInput
		// Cursor's field is "tool_output"; the server's is "tool_response"
		// (internal/api/agent_handlers.go:21-33 has no tool_output tag at
		// all) - this rename is the whole point of translation here.
		env.ToolResponse = p.ToolOutput
		env.ToolUseID = p.ToolUseID

	case "afterFileEdit":
		env.HookEventName = "PostToolUse"
		env.ToolName = "Edit"
		// Carry Cursor's edits array (the actual change content - old/new
		// string pairs) into the synthesized tool_input alongside
		// file_path, not just file_path alone: without it, the fact
		// handleAgentHook stores for a file edit has no change content at
		// all, only the path. p.Edits is left as raw json.RawMessage (a nil
		// RawMessage marshals as JSON null when Cursor omits the field, so
		// this never fails to encode) rather than decoded into Cursor's
		// edit-entry shape, since this translator only needs to relay it,
		// not interpret it.
		input, err := json.Marshal(struct {
			FilePath string          `json:"file_path"`
			Edits    json.RawMessage `json:"edits,omitempty"`
		}{FilePath: p.FilePath, Edits: p.Edits})
		if err != nil {
			return nil, false, fmt.Errorf("hookcli: encode afterFileEdit tool_input: %w", err)
		}
		env.ToolInput = input
		// afterFileEdit carries no tool_use_id of its own (cursor.com/
		// docs/hooks: file_path, edits only). The server drops any
		// PostToolUse whose tool_use_id sanitizes to empty
		// (agent_handlers.go:163-166), so this must be non-empty and,
		// for the capture key to be stable rather than a fresh row per
		// hook invocation, deterministic in the file path alone.
		env.ToolUseID = fileEditToolUseID(p.FilePath)

	case "stop":
		env.HookEventName = "Stop"
		// Cursor's stop event carries no assistant-message text (cursor.com/
		// docs/agent/hooks: status, loop_count only) - there is genuinely
		// nothing to put in last_assistant_message verbatim. But leaving it
		// empty means the server's Stop case (body=clipStr(in.
		// LastAssistantMessage,...)) stores a fact with an empty body,
		// which carries no information at all. status ("completed"|
		// "aborted"|"error") is at least the terminal outcome of the loop,
		// so populate last_assistant_message with it (loop_count is a bare
		// counter, not message content, so it is left out) rather than
		// invent assistant text that was never provided.
		env.LastAssistantMessage = "status=" + p.Status

	case "sessionEnd":
		// cursor.com/docs/agent/hooks documents sessionEnd as a distinct
		// event from stop (its own fields: session_id, reason, duration_ms,
		// is_background_agent, final_status, error_message, vs stop's
		// status/loop_count). Both events are terminal-of-conversation with
		// no other Claude Code equivalent in this task's scope, and a
		// conversation can end via sessionEnd without an intervening stop
		// (e.g. the IDE window closes mid-turn), so mapping it to Stop too
		// ensures the server-side /agent-sessions/<sid>/stop key still gets
		// written. reason and final_status are carried into
		// last_assistant_message for the same reason stop's status is: an
		// empty body otherwise. duration_ms and error_message are not
		// carried through (error_message only exists when reason=="error");
		// that is out of this task's scope, not an oversight.
		env.HookEventName = "Stop"
		env.LastAssistantMessage = "reason=" + p.Reason + " final_status=" + p.FinalStatus

	default:
		return nil, false, nil
	}

	out, err := json.Marshal(env)
	if err != nil {
		return nil, false, fmt.Errorf("hookcli: encode envelope: %w", err)
	}
	return out, true, nil
}

// cursorCWD resolves a single cwd string for the envelope: an event's own
// "cwd" field (postToolUse, preToolUse, beforeShellExecution per
// cursor.com/docs/hooks) wins when present, else the first entry of the
// base "workspace_roots" array that every hook event carries. Cursor
// supports multiple workspace roots (a multi-root window); the first root
// is the same "pick one, coarse grouping is fine" tradeoff
// AgentNamespace's cwd already makes server-side.
func cursorCWD(p cursorPayload) string {
	if p.CWD != "" {
		return p.CWD
	}
	if len(p.WorkspaceRoots) > 0 {
		return p.WorkspaceRoots[0]
	}
	return ""
}

// fnv32aHex is the shared deterministic-hash primitive both fallback-id
// derivations below use, mirroring the fnv32a-hash-of-string technique
// internal/api/agent_handlers.go's cwdHash already uses for its own
// deterministic-fallback-slug case.
func fnv32aHex(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}

// fileEditToolUseID deterministically derives a tool_use_id from a file
// path for the afterFileEdit -> PostToolUse translation. Same file_path
// always yields the same id (repeated edits to one file collapse onto one
// capture key - acceptable, this is ephemeral hook capture, not a durable
// edit history); different file paths practically never collide.
func fileEditToolUseID(filePath string) string {
	return "edit-" + fnv32aHex(filePath)
}

// promptIDFallback deterministically synthesizes a prompt_id for a
// beforeSubmitPrompt event whose payload has no generation_id (Cursor
// documents generation_id as a base field present on every hook event, but
// this translator must not assume every real-world payload actually
// populates it - a missing prompt_id would otherwise make the server
// silently drop the whole translated prompt as "ignored", see
// agent_handlers.go's UserPromptSubmit case). conversation_id and prompt
// are joined with a NUL separator (neither field can itself contain a NUL
// byte in valid JSON text) so "ab"+"c" and "a"+"bc" never hash identically.
// This is a same-conversation-and-text collapse, not a durable per-turn
// identity: two identical prompts submitted back to back in one
// conversation collapse onto the same synthesized id and the same capture
// key - acceptable for ephemeral hook capture, same tradeoff
// fileEditToolUseID already makes for repeated edits to one file.
func promptIDFallback(conversationID, prompt string) string {
	return "gen-" + fnv32aHex(conversationID+"\x00"+prompt)
}

// antigravityPayload is the subset of Antigravity CLI hook stdin fields
// this translator reads, sourced directly from antigravity.google/docs/
// hooks (fetched directly, section by section - "Common Input Fields",
// "PostToolUse", "PreInvocation", "Stop" - field names are not guessed).
//
// Antigravity's complete "Supported Events" list is exactly five events:
// PreToolUse, PostToolUse, PreInvocation, PostInvocation, Stop - verified
// exhaustively (every section heading on the hooks doc page was fetched
// and enumerated, not sampled). NONE of their documented stdin payloads
// carry a field identifying which event fired - no hook_event_name
// equivalent anywhere in the schema, unlike Claude Code and Cursor. See
// this package's translators map (above) and RunFromAntigravity
// (hookcli.go) for how callers supply the event name externally instead.
type antigravityPayload struct {
	ConversationID string   `json:"conversationId"`
	WorkspacePaths []string `json:"workspacePaths"`

	// StepIdx is PostToolUse's own field: "The 0-based index of the
	// completed step." Its documented stdin schema is exactly stepIdx +
	// error + the common fields above - no tool name, arguments, or
	// output of any kind.
	StepIdx int `json:"stepIdx"`
	// Error is documented on BOTH PostToolUse ("Optional. The detailed
	// runtime error message if the tool call failed. Empty if
	// successful.") and Stop ("Optional. The error message if
	// termination was caused by a system error.") - the same JSON field
	// name, decoded from either event's payload into this one struct.
	Error string `json:"error"`

	// PreInvocation's own field (PostInvocation shares this identical
	// schema per the docs - "Input Fields (stdin): Same as PreInvocation
	// input fields" - but PostInvocation is never wired, see
	// connect_antigravity.go's antigravityFlatEvents doc comment).
	InvocationNum int `json:"invocationNum"`

	// Stop's own fields (Error above is also Stop's, see its own comment).
	TerminationReason string `json:"terminationReason"`
	FullyIdle         bool   `json:"fullyIdle"`
}

// translateAntigravity documents the wired-event mapping this translator
// implements. event comes from cmdHook's own --event flag (see
// punkAntigravityHookCommand, connect_antigravity.go), not from raw -
// Antigravity's payloads never self-identify their event (see
// antigravityPayload's doc comment). PreToolUse and PostInvocation are
// deliberately never wired into punk's hooks.json (see
// connect_antigravity.go's antigravityGroupEvents/antigravityFlatEvents
// doc comments for why) and so never reach here in production; event
// arriving as anything other than the three cases below - including
// "PreToolUse"/"PostInvocation" themselves, an empty string, or a future
// event name this translator doesn't yet know about - is skipped
// (ok=false), never an error, matching Normalize's documented contract
// for "recognized agent, unmapped event".
//
// antigravity event (via --event) -> claude-shaped hook_event_name
// PreInvocation (invocationNum==0 only) -> SessionStart
// PreInvocation (invocationNum!=0)      -> skipped (ok=false)
// PostToolUse                            -> PostToolUse
// Stop                                   -> Stop
//
// Antigravity has no native "session start" event at all - invocationNum
// ==0 (the conversation's first model call) is the closest available
// proxy, and doubles as RunFromAntigravity's stateless gate for
// once-per-conversation context injection (see hookcli.go): a punk hook
// invocation is a fresh subprocess every time, with nothing to remember
// between calls the way pi/OpenCode's long-lived plugin hosts use an
// in-process Set, so a field Antigravity itself hands back fresh on every
// call is what makes a "first time only" gate possible here at all.
//
// None of Antigravity's documented hook payloads carry prompt text or
// assistant response text anywhere (re-confirmed field by field against
// antigravityPayload above): PreInvocation/PostInvocation carry only
// invocationNum/initialNumSteps, PostToolUse carries only stepIdx/error,
// Stop carries only executionNum/terminationReason/error/fullyIdle. So
// unlike Cursor/pi/OpenCode, there is no Claude Code UserPromptSubmit
// mapping at all: no native Antigravity event exposes what the user
// actually typed. This is a genuine capability gap in Antigravity's hook
// system today (as documented at the time this was written), not an
// oversight here.
func translateAntigravity(event string, raw []byte) ([]byte, bool, error) {
	var p antigravityPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, false, fmt.Errorf("hookcli: antigravity payload: %w", err)
	}

	env := claudeEnvelope{
		Source:    "antigravity",
		SessionID: p.ConversationID,
		CWD:       antigravityCWD(p),
	}

	switch event {
	case "PreInvocation":
		if p.InvocationNum != 0 {
			return nil, false, nil
		}
		env.HookEventName = "SessionStart"

	case "PostToolUse":
		env.HookEventName = "PostToolUse"
		// No tool name is documented anywhere in PostToolUse's stdin
		// fields (see antigravityPayload's own doc comment: just stepIdx
		// + error + the common fields), so this is a synthesized,
		// self-describing placeholder rather than a real tool name -
		// there is no way to tell from the payload which actual tool
		// (bash, edit, ...) completed.
		env.ToolName = "AntigravityStep"
		// stepIdx is documented only as "the 0-based index of the
		// completed step" - not explicitly documented as unique or
		// collision-free across a conversation, just assumed so here
		// (a "step index" strongly implies a monotonically increasing
		// per-conversation counter, but that is an inference, not a
		// stated guarantee). It becomes the synthesized tool_use_id,
		// scoped within this conversation's own /agent-sessions/<sid>/
		// subtree (already keyed by conversationId - no extra prefix
		// needed here) and is also carried into tool_input so the stored
		// fact has some content beyond a bare error string. If stepIdx is
		// ever reused within one conversation (e.g. a retried step
		// reusing the same index), two distinct tool calls collapse onto
		// the same /agent-sessions/<sid>/tool-step<N> key and one capture
		// silently overwrites the other rather than erroring - the same
		// accepted tradeoff fileEditToolUseID and promptIDFallback
		// already make elsewhere in this file for their own synthesized
		// ids.
		env.ToolUseID = fmt.Sprintf("step%d", p.StepIdx)
		input, err := json.Marshal(struct {
			StepIdx int `json:"stepIdx"`
		}{StepIdx: p.StepIdx})
		if err != nil {
			return nil, false, fmt.Errorf("hookcli: encode antigravity PostToolUse tool_input: %w", err)
		}
		env.ToolInput = input
		if p.Error != "" {
			resp, err := json.Marshal(p.Error)
			if err != nil {
				return nil, false, fmt.Errorf("hookcli: encode antigravity PostToolUse tool_response: %w", err)
			}
			env.ToolResponse = resp
		}

	case "Stop":
		env.HookEventName = "Stop"
		// terminationReason ("model_stop"|"max_steps_exceeded"|"error"|...),
		// fullyIdle, and error (documented on Stop as "Optional. The error
		// message if termination was caused by a system error.", the same
		// stdin field PostToolUse also carries - see antigravityPayload's
		// Error field comment) are the only content Stop's payload
		// carries - no assistant message text exists here either (see
		// this function's own doc comment) - so, mirroring Cursor's
		// stop/sessionEnd and pi's agent_settled fallbacks, all of it is
		// packed into last_assistant_message so the terminal fact is
		// never empty.
		env.LastAssistantMessage = "terminationReason=" + p.TerminationReason + " fullyIdle=" + strconv.FormatBool(p.FullyIdle)
		if p.Error != "" {
			env.LastAssistantMessage += " error=" + p.Error
		}

	default:
		return nil, false, nil
	}

	out, err := json.Marshal(env)
	if err != nil {
		return nil, false, fmt.Errorf("hookcli: encode envelope: %w", err)
	}
	return out, true, nil
}

// antigravityCWD resolves a single cwd string for the envelope: the first
// entry of workspacePaths, the one cwd-shaped field every Antigravity hook
// payload documents ("Absolute directory paths representing the user's
// mounted workspaces" - Common Input Fields). Unlike Cursor's
// cursorCWD, no event-specific single-cwd field exists anywhere in
// Antigravity's schema to prefer over it. Picking the first of possibly
// several mounted workspace roots is the same "pick one, coarse grouping
// is fine" tradeoff cursorCWD and AgentNamespace's cwd already make
// server-side.
func antigravityCWD(p antigravityPayload) string {
	if len(p.WorkspacePaths) > 0 {
		return p.WorkspacePaths[0]
	}
	return ""
}

// copilotPayload is the subset of GitHub Copilot CLI hook stdin fields
// this translator reads, sourced from docs.github.com/en/copilot/
// reference/hooks-reference (fetched directly, section by section: Hook
// configuration format, Hook events, and each event's own "VS Code
// compatible input" schema block). Copilot delivers one of TWO payload
// shapes per event, selected purely by the casing of the event name used
// in hooks.json: registering under the native camelCase name (e.g.
// "sessionStart") yields camelCase fields (sessionId, toolName, ...);
// registering under the PascalCase name (e.g. "SessionStart") yields this
// snake_case shape instead - the docs' own words: "Fields use snake_case
// to match the VS Code Copilot extension format." ConnectCopilot
// (connect_copilot.go) always registers the PascalCase event names, so
// this translator only ever needs to decode the snake_case shape.
//
// Every event's snake_case schema block includes hook_event_name as a
// literal-typed field (e.g. hook_event_name: "SessionStart") - unlike
// Antigravity, this translator can self-identify the event straight from
// raw and needs no externally-supplied --event flag, the same as Cursor's
// own hook_event_name field.
type copilotPayload struct {
	HookEventName string `json:"hook_event_name"`
	SessionID     string `json:"session_id"`
	Timestamp     string `json:"timestamp"`
	CWD           string `json:"cwd"`

	// Source is sessionStart's own field ("startup"|"resume"|"new" per the
	// docs' sessionStart schema block) - a REASON the session started, not
	// an agent identity, the same shape as Claude Code's own SessionStart
	// "source" enum (see claudeSessionStartSourceReasons' doc comment
	// above). It is read here only so the struct decodes cleanly; it is
	// deliberately never copied into env.Source below (which is hardcoded
	// "copilot") - doing so would repeat that exact documented collision.
	Source string `json:"source"`

	// Prompt is userPromptSubmitted's own field.
	Prompt string `json:"prompt"`

	// ToolName/ToolInput/ToolResult are postToolUse's own fields.
	// postToolUse's own schema block documents tool_input only as an
	// unknown-typed field, with no fixed shape - the "parsed from JSON
	// string when possible" phrasing lives in the docs' PreToolUse block,
	// not postToolUse's, so it does not apply here and is not quoted as
	// postToolUse's own words. ToolInput is left as json.RawMessage and
	// carried straight into the envelope's ToolInput unchanged.
	ToolName   string             `json:"tool_name"`
	ToolInput  json.RawMessage    `json:"tool_input"`
	ToolResult *copilotToolResult `json:"tool_result"`

	// StopReason/StopHookActive are agentStop's (Stop's) own fields; the
	// docs document stop_reason as always "end_turn" today, but this is
	// decoded as a plain string rather than a matched enum so a future
	// added value still passes through unchanged.
	StopReason     string `json:"stop_reason"`
	StopHookActive bool   `json:"stop_hook_active"`

	// SessionEndReason is sessionEnd's own field:
	// "complete"|"error"|"abort"|"timeout"|"user_exit" per the docs'
	// sessionEnd schema block.
	SessionEndReason string `json:"reason"`
}

// copilotToolResult is postToolUse's nested tool_result object in the
// snake_case shape: {"result_type":"success","text_result_for_llm":"..."}
// per the docs' own postToolUse schema block. Copilot's postToolUse event
// only fires "After each tool completes successfully" (the docs' Hook
// events table) - a separate postToolUseFailure event (never wired here,
// see ConnectCopilot's own doc comment) covers failures - so result_type
// is documented as always "success" for the one event this translator
// maps.
type copilotToolResult struct {
	ResultType       string `json:"result_type"`
	TextResultForLlm string `json:"text_result_for_llm"`
}

// translateCopilot documents the wired-event mapping this translator
// implements; see the switch below for the per-case rationale.
//
// copilot event (PascalCase) -> claude-shaped hook_event_name
// SessionStart                -> SessionStart
// UserPromptSubmit            -> UserPromptSubmit
// PostToolUse                 -> PostToolUse
// Stop                        -> Stop
// SessionEnd                  -> Stop
// anything else               -> skipped (ok=false), never an error
func translateCopilot(raw []byte) ([]byte, bool, error) {
	var p copilotPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, false, fmt.Errorf("hookcli: copilot payload: %w", err)
	}

	env := claudeEnvelope{
		Source:    "copilot",
		SessionID: p.SessionID,
		CWD:       p.CWD,
	}

	switch p.HookEventName {
	case "SessionStart":
		env.HookEventName = "SessionStart"

	case "UserPromptSubmit":
		env.HookEventName = "UserPromptSubmit"
		env.Prompt = p.Prompt
		// Copilot's userPromptSubmitted/UserPromptSubmit payload carries no
		// id of any kind (see copilotPayload's own doc comment - the docs'
		// schema block for this event is exactly
		// session_id/timestamp/cwd/prompt, nothing else) - prompt_id is
		// required server-side (agent_handlers.go's UserPromptSubmit case
		// ignores any request whose sanitized prompt_id is empty), so
		// without one every translated Copilot prompt would be silently
		// dropped as "ignored". promptIDFallback (Cursor's own
		// no-generation-id fallback, above) synthesizes a deterministic id
		// from session_id+prompt - same same-session-and-text collapse
		// tradeoff documented on promptIDFallback itself.
		env.PromptID = promptIDFallback(p.SessionID, p.Prompt)

	case "PostToolUse":
		env.HookEventName = "PostToolUse"
		env.ToolName = p.ToolName
		env.ToolInput = p.ToolInput
		// Copilot's tool_result (result_type/text_result_for_llm) is the
		// closest thing to Claude's tool_response, but is its own nested
		// object rather than a raw passthrough value - re-encode it into
		// ToolResponse rather than leaving ToolResponse empty (an empty
		// ToolResponse would mean the fact PostToolUse stores carries no
		// tool-output content at all).
		result := p.ToolResult
		if result == nil {
			result = &copilotToolResult{}
		}
		resp, err := json.Marshal(result)
		if err != nil {
			return nil, false, fmt.Errorf("hookcli: encode copilot tool_result: %w", err)
		}
		env.ToolResponse = resp
		// Copilot's postToolUse payload carries no call-identifying id of
		// any kind (see copilotPayload's own doc comment) - the server
		// drops any PostToolUse whose tool_use_id sanitizes to empty
		// (agent_handlers.go), so one must be synthesized. Unlike Cursor's
		// afterFileEdit (which deliberately collapses repeated edits to
		// the SAME file onto one id, since file_path is the only stable
		// key available), Copilot's postToolUse fires once per discrete
		// tool CALL, not once per target resource, so collapsing on
		// tool_name+tool_input alone would merge two genuinely separate
		// calls with identical arguments (e.g. running the same shell
		// command twice) onto one capture key. timestamp is folded into
		// the hash specifically to keep repeated identical calls distinct;
		// two calls landing at the exact same timestamp value still
		// collapse onto one id - the same accepted last-write-wins
		// tradeoff fileEditToolUseID/promptIDFallback already make
		// elsewhere in this file for their own synthesized ids.
		env.ToolUseID = copilotToolUseID(p.SessionID, p.ToolName, p.Timestamp, p.ToolInput)

	case "Stop":
		env.HookEventName = "Stop"
		// agentStop/Stop carries no assistant-message text anywhere in its
		// documented payload (session_id/timestamp/cwd/transcript_path/
		// stop_reason/stop_hook_active only - see copilotPayload's own doc
		// comment) - stop_reason (documented today as always "end_turn")
		// and stop_hook_active are packed into last_assistant_message so
		// the terminal fact is never empty, mirroring Cursor's stop/
		// sessionEnd and Antigravity's Stop fallbacks.
		env.LastAssistantMessage = "stop_reason=" + p.StopReason + " stop_hook_active=" + strconv.FormatBool(p.StopHookActive)

	case "SessionEnd":
		// docs.github.com/en/copilot/reference/hooks-reference documents
		// sessionEnd as firing once per session when it terminates,
		// distinct from agentStop (which fires per TURN, not per session) -
		// the same "terminal-of-conversation with no other Claude Code
		// equivalent" reasoning Cursor's own sessionEnd->Stop mapping
		// documents (translateCursor, above), and the reason enum itself
		// ("abort"/"timeout"/"user_exit" alongside "complete"/"error")
		// implies a session can end without an intervening agentStop
		// having fired at all.
		env.HookEventName = "Stop"
		env.LastAssistantMessage = "session_end_reason=" + p.SessionEndReason

	default:
		return nil, false, nil
	}

	out, err := json.Marshal(env)
	if err != nil {
		return nil, false, fmt.Errorf("hookcli: encode envelope: %w", err)
	}
	return out, true, nil
}

// copilotToolUseID deterministically derives a tool_use_id for the
// PostToolUse translation from fields Copilot's payload actually carries:
// session_id scopes it to one session (mirroring every other
// /agent-sessions/<sid>/... capture key), tool_name+timestamp+toolInput
// bytes are folded in so two different calls in the same session hash to
// different ids. See translateCopilot's PostToolUse case for the accepted
// same-timestamp collision tradeoff.
func copilotToolUseID(sessionID, toolName, timestamp string, toolInput json.RawMessage) string {
	return "step-" + fnv32aHex(sessionID+"\x00"+toolName+"\x00"+timestamp+"\x00"+string(toolInput))
}

// hermesPayload is the subset of Hermes Agent shell-hook stdin fields this
// translator reads, sourced from hermes-agent.nousresearch.com/docs/
// user-guide/features/hooks (fetched directly; field names are not
// guessed). Hermes has four separate hook systems - gateway event hooks,
// in-process plugin hooks, SHELL hooks, and outbound webhooks - and only
// the shell system spawns a subprocess per event with JSON on stdin, which
// is the only one a punk binary can be wired into (see ConnectHermes,
// connect_hermes.go).
//
// The shell payload is closer to Claude Code's own envelope than any other
// agent this package supports: hook_event_name, session_id, cwd, tool_name,
// and tool_input are all top-level fields with the same names and meanings
// the server's agentHookIn already uses. Everything else an event carries
// lands in a single "extra" object - the docs' words for it are that extra
// holds the remaining event-specific kwargs - so every field this
// translator needs beyond that shared core is read from there.
type hermesPayload struct {
	HookEventName string          `json:"hook_event_name"`
	SessionID     string          `json:"session_id"`
	CWD           string          `json:"cwd"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
	Extra         hermesExtra     `json:"extra"`
}

// hermesExtra is the per-event kwargs bag described above. Each field below
// is named after the parameter of the matching plugin-hook callback
// signature the same docs page publishes for that event (the shell payload
// is the same kwargs, serialized), and is only populated for the events
// that carry it.
type hermesExtra struct {
	// UserMessage and IsFirstTurn are pre_llm_call's own kwargs
	// (pre_llm_call(session_id, user_message, conversation_history,
	// is_first_turn, model, platform)). conversation_history is
	// deliberately not decoded: it is the whole transcript so far, which
	// this translator has no use for and which would be re-encoded into
	// every forwarded envelope for nothing.
	UserMessage string `json:"user_message"`
	IsFirstTurn bool   `json:"is_first_turn"`

	// AssistantResponse is post_llm_call's own kwarg - the one field in
	// any Hermes hook payload that carries actual assistant text, which is
	// why post_llm_call (not on_session_end) is what maps to Stop below.
	AssistantResponse string `json:"assistant_response"`

	// TaskID, ToolCallID, Result and DurationMs are post_tool_call's own
	// kwargs (post_tool_call(tool_name, args, result, task_id,
	// duration_ms); task_id and tool_call_id both appear in the docs' own
	// example "extra" object). Result is kept as json.RawMessage rather
	// than a string: the callback signature types it as a string, but the
	// serializer is documented to stringify only values it cannot
	// serialize, so a structured result would arrive here as a JSON object
	// - carrying it raw relays either shape unchanged instead of failing
	// to decode one of them.
	TaskID     string          `json:"task_id"`
	ToolCallID string          `json:"tool_call_id"`
	Result     json.RawMessage `json:"result"`
	DurationMs json.Number     `json:"duration_ms"`
}

// translateHermes documents the wired-event mapping this translator
// implements; see the switch below for the per-case rationale.
//
// hermes shell event -> claude-shaped hook_event_name
// on_session_start    -> SessionStart
// pre_llm_call        -> UserPromptSubmit
// post_tool_call      -> PostToolUse
// post_llm_call       -> Stop
// anything else       -> skipped (ok=false), never an error
//
// Two Hermes events with a plausible-looking mapping are deliberately NOT
// wired, here or in ConnectHermes' config.yaml entries:
//
//   - on_session_end. It would also map to Stop, and Stop's capture key
//     (/agent-sessions/<sid>/stop) is a single key per session, so whichever
//     of the two fires last wins. on_session_end's payload carries only
//     completed/interrupted booleans, so letting it fire would routinely
//     overwrite post_llm_call's real assistant text with a bare
//     "completed=true interrupted=false" - strictly less information in the
//     same slot. Cursor's own stop+sessionEnd pair (translateCursor, above)
//     maps BOTH because neither Cursor event carries assistant text at all,
//     so there is nothing better to lose there; here there is.
//   - pre_tool_call. It is one of Hermes' two blocking hook events (a
//     stdout {"decision":"block"} vetoes the tool call). punk only observes
//     hook traffic, and post_tool_call already captures the same call with
//     its result attached, so wiring the blocking twin would add a veto
//     surface punk must never use in exchange for no extra capture.
func translateHermes(raw []byte) ([]byte, bool, error) {
	var p hermesPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, false, fmt.Errorf("hookcli: hermes payload: %w", err)
	}

	env := claudeEnvelope{
		Source:    "hermes",
		SessionID: p.SessionID,
		CWD:       p.CWD,
	}

	switch p.HookEventName {
	case "on_session_start":
		env.HookEventName = "SessionStart"

	case "pre_llm_call":
		env.HookEventName = "UserPromptSubmit"
		env.Prompt = p.Extra.UserMessage
		// Hermes' pre_llm_call kwargs carry no per-turn id of any kind
		// (session_id, user_message, conversation_history, is_first_turn,
		// model, platform - nothing turn-identifying), and prompt_id is
		// required server-side: agent_handlers.go's UserPromptSubmit case
		// ignores any request whose sanitized prompt_id is empty, so
		// without one every translated Hermes prompt would be silently
		// dropped. promptIDFallback synthesizes a deterministic id from
		// session_id+prompt, with the same same-session-and-text collapse
		// tradeoff documented on promptIDFallback itself.
		env.PromptID = promptIDFallback(p.SessionID, p.Extra.UserMessage)

	case "post_tool_call":
		env.HookEventName = "PostToolUse"
		env.ToolName = p.ToolName
		env.ToolInput = p.ToolInput
		env.ToolResponse = p.Extra.Result
		// tool_call_id is listed in the docs' own example extra object, so
		// it is preferred when present - a real per-call id beats any
		// synthesized one. The server drops any PostToolUse whose
		// tool_use_id sanitizes to empty (agent_handlers.go), so a payload
		// without it still needs a synthesized fallback rather than an
		// empty field.
		env.ToolUseID = p.Extra.ToolCallID
		if env.ToolUseID == "" {
			env.ToolUseID = hermesToolUseID(p)
		}

	case "post_llm_call":
		env.HookEventName = "Stop"
		// Unlike every other agent in this package (Cursor, Antigravity and
		// Copilot all have to pack status/reason strings into
		// last_assistant_message because none of their terminal events
		// carry assistant text), Hermes' post_llm_call hands over the
		// actual assistant response, so this is the verbatim message rather
		// than a synthesized summary. An empty assistant_response stays
		// empty here instead of being padded with a status string: it means
		// the model genuinely produced no text, which is itself the fact.
		env.LastAssistantMessage = p.Extra.AssistantResponse

	default:
		return nil, false, nil
	}

	out, err := json.Marshal(env)
	if err != nil {
		return nil, false, fmt.Errorf("hookcli: encode envelope: %w", err)
	}
	return out, true, nil
}

// hermesToolUseID deterministically derives a tool_use_id for the
// post_tool_call translation when the payload carries no tool_call_id of
// its own. post_tool_call fires once per discrete tool CALL, not once per
// target resource, so - per this package's established rule for per-call
// events - a per-call discriminator is folded in rather than hashing
// tool_name+tool_input alone (which would merge two genuinely separate
// calls with identical arguments, e.g. running the same shell command
// twice, onto one capture key). Hermes exposes no timestamp on this event,
// so task_id (per-task) and duration_ms (the call's own measured runtime)
// stand in as the discriminators. Two identical calls in one task that also
// happen to report the exact same duration still collapse onto one id - the
// same accepted last-write-wins tradeoff copilotToolUseID and
// fileEditToolUseID already make for their own synthesized ids.
func hermesToolUseID(p hermesPayload) string {
	return "call-" + fnv32aHex(p.SessionID+"\x00"+p.Extra.TaskID+"\x00"+p.ToolName+
		"\x00"+p.Extra.DurationMs.String()+"\x00"+string(p.ToolInput))
}
