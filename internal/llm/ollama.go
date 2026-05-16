package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type Ollama struct {
	endpoint string
	model    string
	http     *http.Client
}

func NewOllama(endpoint, model string) *Ollama {
	if endpoint == "" {
		endpoint = "http://localhost:11434"
	}
	if model == "" {
		model = "gemma4:31b-cloud"
	}
	return &Ollama{endpoint: endpoint, model: model, http: &http.Client{}}
}

func (o *Ollama) Name() string { return "ollama:" + o.model }

type ollamaChatReq struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Tools    []ollamaTool    `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
	Options  map[string]any  `json:"options,omitempty"`
}

type ollamaMessage struct {
	Role      string               `json:"role"`
	Content   string               `json:"content"`
	ToolCalls []ollamaToolCallResp `json:"tool_calls,omitempty"`
	ToolName  string               `json:"tool_name,omitempty"`
}

type ollamaTool struct {
	Type     string             `json:"type"`
	Function ollamaToolFunction `json:"function"`
}

type ollamaToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type ollamaToolCallResp struct {
	Function struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"function"`
}

type ollamaChatResp struct {
	Message ollamaMessage `json:"message"`
	Done    bool          `json:"done"`
}

func (o *Ollama) Chat(ctx context.Context, system string, msgs []Message, tools []ToolSpec) (*Response, error) {
	req := ollamaChatReq{
		Model:  o.model,
		Stream: false,
		Options: map[string]any{
			"num_ctx": 16384,
		},
	}
	if system != "" {
		req.Messages = append(req.Messages, ollamaMessage{Role: "system", Content: system})
	}
	for _, m := range msgs {
		req.Messages = append(req.Messages, ollamaFromMessage(m)...)
	}
	for _, t := range tools {
		req.Tools = append(req.Tools, ollamaTool{
			Type: "function",
			Function: ollamaToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := o.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama %d: %s", resp.StatusCode, string(b))
	}
	var out ollamaChatResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}

	r := &Response{Text: out.Message.Content}
	for i, tc := range out.Message.ToolCalls {
		r.ToolCalls = append(r.ToolCalls, ToolCall{
			ID:        fmt.Sprintf("call_%d", i),
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	if len(r.ToolCalls) > 0 {
		r.StopReason = "tool_use"
	} else {
		r.StopReason = "stop"
	}
	return r, nil
}

func ollamaFromMessage(m Message) []ollamaMessage {
	switch m.Role {
	case RoleUser:
		return []ollamaMessage{{Role: "user", Content: m.Text}}
	case RoleAssistant:
		om := ollamaMessage{Role: "assistant", Content: m.Text}
		for _, tc := range m.ToolCalls {
			var tcr ollamaToolCallResp
			tcr.Function.Name = tc.Name
			tcr.Function.Arguments = tc.Arguments
			om.ToolCalls = append(om.ToolCalls, tcr)
		}
		return []ollamaMessage{om}
	case RoleTool:
		var out []ollamaMessage
		for _, tr := range m.ToolResults {
			out = append(out, ollamaMessage{
				Role:     "tool",
				Content:  tr.Content,
				ToolName: tr.Name,
			})
		}
		return out
	}
	return nil
}
