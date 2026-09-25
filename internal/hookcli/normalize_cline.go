package hookcli

import (
	"encoding/json"
	"fmt"
)

// Native extension file-hook schema, verified 2026-09-25:
// https://github.com/cline/cline/blob/main/apps/vscode/src/core/hooks/templates.ts
type clinePayload struct {
	TaskID         string         `json:"taskId"`
	HookName       string         `json:"hookName"`
	WorkspaceRoots []string       `json:"workspaceRoots"`
	CWD            string         `json:"cwd"`
	Timestamp      string         `json:"timestamp"`
	TaskStart      clineTaskEvent `json:"taskStart"`
	TaskComplete   clineTaskEvent `json:"taskComplete"`
	TaskCancel     clineTaskEvent `json:"taskCancel"`
	PostToolUse    struct {
		ToolName   string          `json:"toolName"`
		Tool       string          `json:"tool"` // legacy spelling
		Parameters json.RawMessage `json:"parameters"`
		Result     json.RawMessage `json:"result"`
	} `json:"postToolUse"`
	UserPromptSubmit struct {
		Prompt string `json:"prompt"`
	} `json:"userPromptSubmit"`
}

type clineTaskEvent struct {
	Task         string `json:"task"` // legacy TaskStart payload
	TaskMetadata struct {
		InitialTask      string `json:"initialTask"`
		Result           string `json:"result"`
		CompletionStatus string `json:"completionStatus"`
	} `json:"taskMetadata"`
}

func translateCline(raw []byte) ([]byte, bool, error) {
	var p clinePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, false, fmt.Errorf("hookcli: cline payload: %w", err)
	}
	if p.TaskID == "" || p.HookName == "" {
		return nil, false, fmt.Errorf("hookcli: cline payload: taskId and hookName required")
	}
	env := claudeEnvelope{Source: "cline", SessionID: p.TaskID, CWD: p.CWD}
	if len(p.WorkspaceRoots) > 0 {
		env.CWD = p.WorkspaceRoots[0]
	}
	switch p.HookName {
	case "TaskStart":
		env.HookEventName = "SessionStart"
		env.Prompt = p.TaskStart.TaskMetadata.InitialTask
		if env.Prompt == "" {
			env.Prompt = p.TaskStart.Task
		}
	case "TaskResume":
		env.HookEventName = "SessionStart"
	case "UserPromptSubmit":
		env.HookEventName = "UserPromptSubmit"
		env.Prompt = p.UserPromptSubmit.Prompt
		env.PromptID = promptIDFallback(p.TaskID+"\x00"+p.Timestamp, env.Prompt)
	case "PostToolUse":
		env.HookEventName = "PostToolUse"
		env.ToolName = p.PostToolUse.ToolName
		if env.ToolName == "" {
			env.ToolName = p.PostToolUse.Tool
		}
		env.ToolInput, env.ToolResponse = p.PostToolUse.Parameters, p.PostToolUse.Result
		// No native call ID. Timestamp distinguishes repeated identical calls;
		// identical calls in the same millisecond retain last-write-wins capture.
		env.ToolUseID = copilotToolUseID(p.TaskID, env.ToolName, p.Timestamp, env.ToolInput)
	case "TaskComplete":
		env.HookEventName = "Stop"
		env.LastAssistantMessage = p.TaskComplete.TaskMetadata.Result
	case "TaskCancel":
		env.HookEventName = "Stop"
		env.LastAssistantMessage = p.TaskCancel.TaskMetadata.CompletionStatus
		if env.LastAssistantMessage == "" {
			env.LastAssistantMessage = "cancelled"
		}
	default:
		// No pre-tool veto surface; PostToolUse already captures each call.
		return nil, false, nil
	}
	out, err := json.Marshal(env)
	return out, err == nil, err
}
