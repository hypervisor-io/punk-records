// Package a2a implements the wire types and mapping for the Agent2Agent
// (A2A) protocol v0.3 / v1.0 JSON-RPC transport. Punk Records exposes its
// task ledger to foreign A2A clients through this adapter: every A2A Task
// is a ledger task, every A2A artifact is a finding, streaming rides the
// in-process bus. See https://a2a-protocol.org/ for the specification.
//
// This package is transport-agnostic: it defines the JSON shapes and the
// ledger<->A2A mapping. The HTTP/JSON-RPC handler lives in internal/api.
package a2a

import "encoding/json"

// ProtocolVersion is the A2A spec version this server implements.
const ProtocolVersion = "0.3.0"

// JSON-RPC method strings (the wire "method" field values).
const (
	MethodMessageSend      = "message/send"
	MethodMessageStream    = "message/stream"
	MethodTasksGet         = "tasks/get"
	MethodTasksCancel      = "tasks/cancel"
	MethodTasksResubscribe = "tasks/resubscribe"
	MethodPushConfigSet    = "tasks/pushNotificationConfig/set"
	MethodPushConfigGet    = "tasks/pushNotificationConfig/get"
	MethodPushConfigList   = "tasks/pushNotificationConfig/list"
	MethodPushConfigDelete = "tasks/pushNotificationConfig/delete"
	MethodExtendedCard     = "agent/getAuthenticatedExtendedCard"
)

// Object kind discriminators.
const (
	KindTask           = "task"
	KindMessage        = "message"
	KindStatusUpdate   = "status-update"
	KindArtifactUpdate = "artifact-update"
	KindTextPart       = "text"
	KindDataPart       = "data"
	KindFilePart       = "file"
)

// Roles.
const (
	RoleUser  = "user"
	RoleAgent = "agent"
)

// TaskState is the A2A lifecycle state (wire JSON values, kebab-case).
type TaskState string

const (
	StateSubmitted     TaskState = "submitted"
	StateWorking       TaskState = "working"
	StateInputRequired TaskState = "input-required"
	StateCompleted     TaskState = "completed"
	StateCanceled      TaskState = "canceled"
	StateFailed        TaskState = "failed"
	StateRejected      TaskState = "rejected"
	StateAuthRequired  TaskState = "auth-required"
	StateUnknown       TaskState = "unknown"
)

// Terminal reports whether a state ends the task lifecycle.
func (s TaskState) Terminal() bool {
	switch s {
	case StateCompleted, StateCanceled, StateFailed, StateRejected:
		return true
	}
	return false
}

// ---- JSON-RPC 2.0 envelope ----

// Request is a JSON-RPC 2.0 request object.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response object.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

// Error codes: standard JSON-RPC plus A2A-specific (spec section 8).
const (
	ErrParse          = -32700
	ErrInvalidRequest = -32600
	ErrMethodNotFound = -32601
	ErrInvalidParams  = -32602
	ErrInternal       = -32603

	ErrTaskNotFound          = -32001
	ErrTaskNotCancelable     = -32002
	ErrPushNotSupported      = -32003
	ErrUnsupportedOperation  = -32004
	ErrContentTypeNotSupport = -32005
)

// ---- message / part ----

// Part is a single content part (text | file | data), discriminated by Kind.
type Part struct {
	Kind     string          `json:"kind"`
	Text     string          `json:"text,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
	File     *FileContent    `json:"file,omitempty"`
	Metadata map[string]any  `json:"metadata,omitempty"`
}

// FileContent carries a file part by bytes (base64) or URI.
type FileContent struct {
	Name     string `json:"name,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Bytes    string `json:"bytes,omitempty"`
	URI      string `json:"uri,omitempty"`
}

// Message is one turn in a task conversation.
type Message struct {
	Kind             string         `json:"kind"` // "message"
	MessageID        string         `json:"messageId"`
	Role             string         `json:"role"`
	Parts            []Part         `json:"parts"`
	TaskID           string         `json:"taskId,omitempty"`
	ContextID        string         `json:"contextId,omitempty"`
	ReferenceTaskIDs []string       `json:"referenceTaskIds,omitempty"`
	Metadata         map[string]any `json:"metadata,omitempty"`
}

// Text returns the concatenated text of all text parts.
func (m Message) Text() string {
	var s string
	for _, p := range m.Parts {
		if p.Kind == KindTextPart {
			if s != "" {
				s += "\n"
			}
			s += p.Text
		}
	}
	return s
}

// ---- task / status / artifact ----

// TaskStatus is the current state plus optional agent message and timestamp.
type TaskStatus struct {
	State     TaskState `json:"state"`
	Message   *Message  `json:"message,omitempty"`
	Timestamp string    `json:"timestamp,omitempty"` // RFC3339
}

// Artifact is a task output (Punk Records findings map to artifacts).
type Artifact struct {
	ArtifactID  string         `json:"artifactId"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Parts       []Part         `json:"parts"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// Task is the A2A view of a ledger task.
type Task struct {
	Kind      string         `json:"kind"` // "task"
	ID        string         `json:"id"`
	ContextID string         `json:"contextId,omitempty"`
	Status    TaskStatus     `json:"status"`
	Artifacts []Artifact     `json:"artifacts,omitempty"`
	History   []Message      `json:"history,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// ---- streaming events ----

// StatusUpdateEvent is streamed on every task state change.
type StatusUpdateEvent struct {
	Kind      string         `json:"kind"` // "status-update"
	TaskID    string         `json:"taskId"`
	ContextID string         `json:"contextId,omitempty"`
	Status    TaskStatus     `json:"status"`
	Final     bool           `json:"final"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// ArtifactUpdateEvent is streamed when a new artifact is produced.
type ArtifactUpdateEvent struct {
	Kind      string   `json:"kind"` // "artifact-update"
	TaskID    string   `json:"taskId"`
	ContextID string   `json:"contextId,omitempty"`
	Artifact  Artifact `json:"artifact"`
	Append    bool     `json:"append"`
	LastChunk bool     `json:"lastChunk"`
}

// ---- method params ----

// SendMessageParams is the params of message/send and message/stream.
type SendMessageParams struct {
	Message       Message            `json:"message"`
	Configuration *SendMessageConfig `json:"configuration,omitempty"`
	Metadata      map[string]any     `json:"metadata,omitempty"`
}

// SendMessageConfig tunes a send.
type SendMessageConfig struct {
	AcceptedOutputModes []string                `json:"acceptedOutputModes,omitempty"`
	HistoryLength       int                     `json:"historyLength,omitempty"`
	Blocking            bool                    `json:"blocking,omitempty"`
	PushNotification    *PushNotificationConfig `json:"pushNotificationConfig,omitempty"`
}

// TaskQueryParams is the params of tasks/get.
type TaskQueryParams struct {
	ID            string `json:"id"`
	HistoryLength int    `json:"historyLength,omitempty"`
}

// TaskIDParams is the params of tasks/cancel and tasks/resubscribe.
type TaskIDParams struct {
	ID       string         `json:"id"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// ---- push notification config ----

// PushNotificationConfig is a webhook the server calls on task updates.
type PushNotificationConfig struct {
	ID    string `json:"id,omitempty"`
	URL   string `json:"url"`
	Token string `json:"token,omitempty"`
}

// TaskPushNotificationConfig binds a push config to a task.
type TaskPushNotificationConfig struct {
	TaskID                 string                 `json:"taskId"`
	PushNotificationConfig PushNotificationConfig `json:"pushNotificationConfig"`
}

// ---- agent card (client-side parse) ----

// AgentCard is the discovery document a remote A2A agent publishes.
type AgentCard struct {
	ProtocolVersion    string          `json:"protocolVersion"`
	Name               string          `json:"name"`
	Description        string          `json:"description"`
	Version            string          `json:"version"`
	URL                string          `json:"url"`
	PreferredTransport string          `json:"preferredTransport"`
	Capabilities       map[string]bool `json:"capabilities"`
	Skills             []AgentSkill    `json:"skills"`
}

// AgentSkill is one capability advertised in an AgentCard.
type AgentSkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags,omitempty"`
}
