// Package agent runs the tool-calling loop: ask the LLM, execute requested
// tools, feed results back, repeat until the model stops calling tools.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/banux/librarian-agent/internal/llm"
	"github.com/banux/librarian-agent/internal/mcp"
)

// Event is one observable step in the agent loop. Used by the daemon's /chat
// handler to translate the run into a Server-Sent Events stream.
type Event struct {
	Kind       string         // "tool_call" | "tool_result" | "text" | "done" | "error"
	Name       string         // tool name (tool_call / tool_result)
	Arguments  map[string]any // tool_call payload
	Result     string         // tool_result content
	IsError    bool           // tool_result error flag
	Delta      string         // text frame
	StopReason string         // done frame
	Steps      int            // done frame
	Message    string         // error frame
}

// EmitFunc receives Events as the loop progresses. Default emitter writes to
// stdout/stderr for CLI parity.
type EmitFunc func(Event)

type Agent struct {
	LLM      llm.Provider
	MCP      *mcp.Client
	MaxSteps int
	Verbose  bool

	// Instance identification — surfaced in the system prompt so the LLM cannot
	// confuse different catalogs when one librarian process drives several.
	InstanceName  string
	InstanceLabel string
	InstanceLocale string

	// Emit receives every step of the run. Defaults to a stdout/stderr printer
	// when nil — preserving the existing CLI behaviour.
	Emit EmitFunc

	tools      []llm.ToolSpec
	mcpTools   map[string]bool
	transcript []llm.Message
}

func New(p llm.Provider, m *mcp.Client) *Agent {
	return &Agent{LLM: p, MCP: m, MaxSteps: 40, Verbose: true}
}

// Init pulls the tool list from the MCP server and adds the local web_fetch.
func (a *Agent) Init(ctx context.Context) error {
	mcpTools, err := a.MCP.ListTools(ctx)
	if err != nil {
		return fmt.Errorf("listing MCP tools: %w", err)
	}
	a.mcpTools = make(map[string]bool, len(mcpTools))
	for _, t := range mcpTools {
		a.tools = append(a.tools, llm.ToolSpec{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
		a.mcpTools[t.Name] = true
	}
	a.tools = append(a.tools, llm.ToolSpec{
		Name:        "web_fetch",
		Description: "Récupère le contenu textuel d'une page web (utile pour Babelio, Wikipedia, sites d'éditeurs). Retourne le HTML brut tronqué à ~30k caractères.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","description":"URL complète https://..."}},"required":["url"]}`),
	})
	if a.Verbose {
		fmt.Printf("[agent] %d outils MCP + 1 local (%s)\n", len(mcpTools), a.LLM.Name())
	}
	return nil
}

// Mode controls which system prompt the agent runs with. Autonomous batch
// jobs (ticker / webhook / `run` CLI) use ModeBatch — a rigid enrichment
// workflow that terminates on "FIN". Chat sessions over /chat use
// ModeChat — a conversational French-speaking librarian that answers
// open questions and asks confirmation before mutations.
type Mode int

const (
	ModeBatch Mode = iota
	ModeChat
)

// Run drives the loop with the user instruction until the model stops.
// Defaults to ModeBatch so existing callers (run, ticker, webhook) keep
// the autonomous-enrichment behaviour.
func (a *Agent) Run(ctx context.Context, userInstruction string) error {
	return a.run(ctx, userInstruction, nil, ModeBatch)
}

// RunWithHistory drives the loop seeded with prior conversation turns and
// the chat-mode system prompt. The /chat endpoint is the only caller.
func (a *Agent) RunWithHistory(ctx context.Context, userInstruction string, history []llm.Message) error {
	return a.run(ctx, userInstruction, history, ModeChat)
}

func (a *Agent) run(ctx context.Context, userInstruction string, history []llm.Message, mode Mode) error {
	a.transcript = append([]llm.Message{}, history...)
	a.transcript = append(a.transcript, llm.Message{Role: llm.RoleUser, Text: userInstruction})
	emit := a.emit
	var system string
	switch mode {
	case ModeChat:
		system = renderChatPrompt(a.InstanceName, a.InstanceLabel, a.InstanceLocale)
	default:
		system = renderSystemPrompt(a.InstanceName, a.InstanceLabel, a.InstanceLocale)
	}

	for step := 0; step < a.MaxSteps; step++ {
		resp, err := a.LLM.Chat(ctx, system, a.transcript, a.tools)
		if err != nil {
			emit(Event{Kind: "error", Message: fmt.Sprintf("llm chat: %v", err)})
			return fmt.Errorf("llm chat: %w", err)
		}

		assistant := llm.Message{
			Role:      llm.RoleAssistant,
			Text:      resp.Text,
			ToolCalls: resp.ToolCalls,
		}
		a.transcript = append(a.transcript, assistant)

		if resp.Text != "" {
			emit(Event{Kind: "text", Delta: resp.Text})
		}

		if len(resp.ToolCalls) == 0 {
			emit(Event{Kind: "done", StopReason: resp.StopReason, Steps: step + 1})
			return nil
		}

		toolMsg := llm.Message{Role: llm.RoleTool}
		for _, tc := range resp.ToolCalls {
			emit(Event{Kind: "tool_call", Name: tc.Name, Arguments: tc.Arguments})
			content, isErr := a.execTool(ctx, tc)
			emit(Event{Kind: "tool_result", Name: tc.Name, Result: content, IsError: isErr})
			toolMsg.ToolResults = append(toolMsg.ToolResults, llm.ToolResult{
				CallID:  tc.ID,
				Name:    tc.Name,
				Content: content,
				IsError: isErr,
			})
		}
		a.transcript = append(a.transcript, toolMsg)
	}
	emit(Event{Kind: "error", Message: fmt.Sprintf("max steps (%d) reached", a.MaxSteps)})
	return fmt.Errorf("max steps (%d) reached", a.MaxSteps)
}

// emit returns the configured emitter or the default CLI printer when nil.
func (a *Agent) emit(e Event) {
	if a.Emit != nil {
		a.Emit(e)
		return
	}
	switch e.Kind {
	case "text":
		if e.Delta != "" {
			fmt.Println(e.Delta)
		}
	case "tool_call":
		if a.Verbose {
			fmt.Printf("[tool] %s %s\n", e.Name, summarizeArgs(e.Arguments))
		}
	case "done":
		if a.Verbose {
			fmt.Printf("[agent] terminé en %d étapes (stop=%s)\n", e.Steps, e.StopReason)
		}
	case "error":
		fmt.Fprintln(osStderr, "[agent]", e.Message)
	}
}

var osStderr io.Writer = os.Stderr

func (a *Agent) execTool(ctx context.Context, tc llm.ToolCall) (string, bool) {
	if tc.Name == "web_fetch" {
		url, _ := tc.Arguments["url"].(string)
		text, err := webFetch(ctx, url)
		if err != nil {
			return fmt.Sprintf("erreur web_fetch: %v", err), true
		}
		return text, false
	}
	if !a.mcpTools[tc.Name] {
		return fmt.Sprintf("outil inconnu: %s", tc.Name), true
	}
	res, err := a.MCP.CallTool(ctx, tc.Name, tc.Arguments)
	if err != nil {
		return fmt.Sprintf("erreur MCP: %v", err), true
	}
	return res.Text, res.IsError
}

func summarizeArgs(args map[string]any) string {
	if len(args) == 0 {
		return "()"
	}
	b, _ := json.Marshal(args)
	s := string(b)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func webFetch(ctx context.Context, url string) (string, error) {
	if url == "" {
		return "", fmt.Errorf("url vide")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "librarian-agent/0.1 (+local)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}
	const limit = 30_000
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit*4))
	if err != nil {
		return "", err
	}
	text := stripHTML(string(body))
	if len(text) > limit {
		text = text[:limit] + "\n…[tronqué]"
	}
	return text, nil
}

// stripHTML is intentionally crude — we only want plain text the model can
// read, not perfectly rendered Markdown.
func stripHTML(s string) string {
	for _, tag := range []string{"<script", "<style"} {
		for {
			i := strings.Index(strings.ToLower(s), tag)
			if i < 0 {
				break
			}
			end := strings.Index(strings.ToLower(s[i:]), "</")
			if end < 0 {
				s = s[:i]
				break
			}
			close := strings.Index(s[i+end:], ">")
			if close < 0 {
				s = s[:i]
				break
			}
			s = s[:i] + s[i+end+close+1:]
		}
	}
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
			b.WriteByte(' ')
		case !inTag:
			b.WriteRune(r)
		}
	}
	out := b.String()
	out = strings.ReplaceAll(out, "&nbsp;", " ")
	out = strings.ReplaceAll(out, "&amp;", "&")
	out = strings.ReplaceAll(out, "&quot;", "\"")
	out = strings.ReplaceAll(out, "&#39;", "'")
	out = strings.ReplaceAll(out, "&lt;", "<")
	out = strings.ReplaceAll(out, "&gt;", ">")
	// collapse whitespace
	fields := strings.Fields(out)
	return strings.Join(fields, " ")
}
