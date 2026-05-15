// Package llm defines a small provider-agnostic interface for chat models
// that support tool calling. Anthropic and Ollama backends implement it.
package llm

import (
	"context"
	"encoding/json"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall is one tool invocation the model wants to make.
type ToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
}

// ToolResult is the response we feed back to the model after running a tool.
type ToolResult struct {
	CallID  string
	Name    string
	Content string
	IsError bool
}

// Message is a single conversation turn. Exactly one of Text / ToolCalls /
// ToolResults is meaningful, depending on Role.
type Message struct {
	Role        Role
	Text        string
	ToolCalls   []ToolCall
	ToolResults []ToolResult
}

// ToolSpec describes a tool that the model can call. The schema is a raw
// JSON Schema object (whatever the MCP server exposes).
type ToolSpec struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Response is one assistant turn returned by the model.
type Response struct {
	Text       string
	ToolCalls  []ToolCall
	StopReason string
}

type Provider interface {
	Name() string
	Chat(ctx context.Context, system string, msgs []Message, tools []ToolSpec) (*Response, error)
}
