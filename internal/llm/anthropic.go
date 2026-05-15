package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type Anthropic struct {
	apiKey string
	model  string
	http   *http.Client
}

func NewAnthropic(apiKey, model string) *Anthropic {
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	return &Anthropic{apiKey: apiKey, model: model, http: &http.Client{}}
}

func (a *Anthropic) Name() string { return "anthropic:" + a.model }

type anthropicReq struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
}

type anthropicMessage struct {
	Role    string             `json:"role"`
	Content []anthropicContent `json:"content"`
}

type anthropicContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     map[string]any  `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicResp struct {
	StopReason string             `json:"stop_reason"`
	Content    []anthropicContent `json:"content"`
}

func (a *Anthropic) Chat(ctx context.Context, system string, msgs []Message, tools []ToolSpec) (*Response, error) {
	req := anthropicReq{
		Model:     a.model,
		MaxTokens: 4096,
		System:    system,
	}
	for _, m := range msgs {
		am := anthropicFromMessage(m)
		if am != nil {
			req.Messages = append(req.Messages, *am)
		}
	}
	for _, t := range tools {
		req.Tools = append(req.Tools, anthropicTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := a.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("anthropic %d: %s", resp.StatusCode, string(b))
	}
	var out anthropicResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}

	r := &Response{StopReason: out.StopReason}
	for _, c := range out.Content {
		switch c.Type {
		case "text":
			r.Text += c.Text
		case "tool_use":
			r.ToolCalls = append(r.ToolCalls, ToolCall{
				ID:        c.ID,
				Name:      c.Name,
				Arguments: c.Input,
			})
		}
	}
	return r, nil
}

func anthropicFromMessage(m Message) *anthropicMessage {
	switch m.Role {
	case RoleUser:
		return &anthropicMessage{
			Role:    "user",
			Content: []anthropicContent{{Type: "text", Text: m.Text}},
		}
	case RoleAssistant:
		am := &anthropicMessage{Role: "assistant"}
		if m.Text != "" {
			am.Content = append(am.Content, anthropicContent{Type: "text", Text: m.Text})
		}
		for _, tc := range m.ToolCalls {
			am.Content = append(am.Content, anthropicContent{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Name,
				Input: tc.Arguments,
			})
		}
		return am
	case RoleTool:
		am := &anthropicMessage{Role: "user"}
		for _, tr := range m.ToolResults {
			body, _ := json.Marshal(tr.Content)
			am.Content = append(am.Content, anthropicContent{
				Type:      "tool_result",
				ToolUseID: tr.CallID,
				Content:   body,
				IsError:   tr.IsError,
			})
		}
		return am
	}
	return nil
}
