// Package agent runs the tool-calling loop: ask the LLM, execute requested
// tools, feed results back, repeat until the model stops calling tools.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

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
	MaxSteps int
	Verbose  bool

	// MCPs holds every MCP server the agent can call. The first one (added by
	// New) is always the per-instance OPDS server; additional clients (e.g.
	// obscura's browser MCP) can be attached via AddMCP before Init.
	MCPs []*mcp.Client

	// Instance identification — surfaced in the system prompt so the LLM cannot
	// confuse different catalogs when one librarian process drives several.
	InstanceName  string
	InstanceLabel string
	InstanceLocale string

	// FirecrawlAPIKey enables the Firecrawl /scrape backend in web_fetch when
	// non-empty. Tried first, before obscura and the raw HTTP fallback.
	FirecrawlAPIKey string

	// Emit receives every step of the run. Defaults to a stdout/stderr printer
	// when nil — preserving the existing CLI behaviour.
	Emit EmitFunc

	tools        []llm.ToolSpec
	toolOwner    map[string]*mcp.Client // MCP tool name → owning client
	transcript   []llm.Message
}

func New(p llm.Provider, m *mcp.Client) *Agent {
	return &Agent{LLM: p, MCPs: []*mcp.Client{m}, MaxSteps: 40, Verbose: true}
}

// AddMCP attaches an additional MCP client (e.g. obscura) whose tools will be
// merged into the agent's tool list at Init time. If two clients expose the
// same tool name, the FIRST one registered (i.e. OPDS) wins and the conflict
// is logged when Verbose.
func (a *Agent) AddMCP(m *mcp.Client) {
	if m == nil {
		return
	}
	a.MCPs = append(a.MCPs, m)
}

// Init pulls the tool list from every MCP server and adds the local web_fetch.
func (a *Agent) Init(ctx context.Context) error {
	a.toolOwner = map[string]*mcp.Client{}
	total := 0
	for i, c := range a.MCPs {
		if c == nil {
			continue
		}
		mcpTools, err := c.ListTools(ctx)
		if err != nil {
			return fmt.Errorf("listing MCP tools (client #%d): %w", i, err)
		}
		for _, t := range mcpTools {
			if _, dup := a.toolOwner[t.Name]; dup {
				if a.Verbose {
					fmt.Printf("[agent] tool %q exposed by multiple MCP servers — keeping first\n", t.Name)
				}
				continue
			}
			a.tools = append(a.tools, llm.ToolSpec{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: t.InputSchema,
			})
			a.toolOwner[t.Name] = c
			total++
		}
	}
	a.tools = append(a.tools, llm.ToolSpec{
		Name:        "web_fetch",
		Description: "Récupère le contenu d'une page web et renvoie le Markdown rendu (utile pour Babelio, Wikipedia, sites d'éditeurs). Essaie d'abord Firecrawl quand une clé est configurée (markdown propre, JS, anti-bot), puis obscura (navigateur headless local), puis un simple GET HTTP. Tronqué à ~30k caractères. Pour des interactions plus poussées (cliquer, remplir un formulaire, naviguer), préfère les outils browser_* exposés par obscura quand ils sont disponibles.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","description":"URL complète https://..."}},"required":["url"]}`),
	})
	if a.Verbose {
		fmt.Printf("[agent] %d outils MCP (%d serveur(s)) + 1 local (%s)\n", total, len(a.MCPs), a.LLM.Name())
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
		text, err := a.webFetch(ctx, url)
		if err != nil {
			return fmt.Sprintf("erreur web_fetch: %v", err), true
		}
		return text, false
	}
	owner, ok := a.toolOwner[tc.Name]
	if !ok {
		return fmt.Sprintf("outil inconnu: %s", tc.Name), true
	}
	res, err := owner.CallTool(ctx, tc.Name, tc.Arguments)
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

const webFetchLimit = 30_000

// webFetch returns the textual content of url, trying backends in order of
// quality:
//  1. Firecrawl /scrape (when a.FirecrawlAPIKey is set) — clean markdown, JS
//     rendering, anti-bot bypass via a paid hosted API.
//  2. obscura (when the binary is on $PATH) — local headless browser.
//  3. plain HTTP GET + crude HTML strip — last-resort fallback.
//
// Every call emits structured [web_fetch] log lines so the daemon trace shows
// which backend was tried, how long each step took, and — on 4xx errors — a
// short body excerpt (Cloudflare / captcha / "page introuvable" copy is
// usually visible there).
func (a *Agent) webFetch(ctx context.Context, url string) (string, error) {
	if url == "" {
		return "", fmt.Errorf("url vide")
	}

	if a.FirecrawlAPIKey != "" {
		start := time.Now()
		text, err := webFetchFirecrawl(ctx, url, a.FirecrawlAPIKey)
		elapsed := time.Since(start).Round(time.Millisecond)
		if err == nil {
			log.Printf("[web_fetch] firecrawl ok en %s (%d caractères) — url=%s",
				elapsed, len(text), url)
			return truncate(text, webFetchLimit), nil
		}
		log.Printf("[web_fetch] firecrawl échec en %s (%v) — fallback obscura/HTTP — url=%s",
			elapsed, err, url)
	}

	obscuraPath, obscuraLookErr := exec.LookPath("obscura")
	if obscuraLookErr != nil {
		log.Printf("[web_fetch] obscura indisponible (%v) — bascule directe sur HTTP — url=%s",
			obscuraLookErr, url)
	} else {
		start := time.Now()
		text, err := webFetchObscura(ctx, url)
		elapsed := time.Since(start).Round(time.Millisecond)
		if err == nil {
			log.Printf("[web_fetch] obscura ok en %s (%d caractères, bin=%s) — url=%s",
				elapsed, len(text), obscuraPath, url)
			return truncate(text, webFetchLimit), nil
		}
		log.Printf("[web_fetch] obscura échec en %s (%v) — fallback HTTP — url=%s",
			elapsed, err, url)
	}

	start := time.Now()
	text, err := webFetchHTTP(ctx, url)
	elapsed := time.Since(start).Round(time.Millisecond)
	if err != nil {
		log.Printf("[web_fetch] http échec en %s: %v — url=%s", elapsed, err, url)
		return "", err
	}
	log.Printf("[web_fetch] http ok en %s (%d caractères) — url=%s",
		elapsed, len(text), url)
	return truncate(text, webFetchLimit), nil
}

// webFetchFirecrawl calls Firecrawl's v2 /scrape endpoint and returns the
// markdown payload. The API is described at https://docs.firecrawl.dev — the
// response wraps the markdown under data.markdown when success is true.
func webFetchFirecrawl(ctx context.Context, url, apiKey string) (string, error) {
	payload, _ := json.Marshal(map[string]any{
		"url":     url,
		"formats": []string{"markdown"},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.firecrawl.dev/v2/scrape", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, webFetchLimit*4))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		excerpt := strings.TrimSpace(string(body))
		if len(excerpt) > 300 {
			excerpt = excerpt[:300] + "…"
		}
		return "", fmt.Errorf("firecrawl http %d: %s", resp.StatusCode, excerpt)
	}

	var parsed struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
		Data    struct {
			Markdown string `json:"markdown"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("firecrawl: parse response: %w", err)
	}
	if !parsed.Success {
		msg := parsed.Error
		if msg == "" {
			msg = "succès=false sans message"
		}
		return "", fmt.Errorf("firecrawl: %s", msg)
	}
	if parsed.Data.Markdown == "" {
		return "", fmt.Errorf("firecrawl: markdown vide")
	}
	return parsed.Data.Markdown, nil
}

// webFetchObscura shells out to `obscura fetch --dump markdown` and returns
// stdout. Quiet mode suppresses obscura's own log lines.
func webFetchObscura(ctx context.Context, url string) (string, error) {
	cmd := exec.CommandContext(ctx, "obscura", "fetch",
		"--dump", "markdown",
		"--quiet",
		"--timeout", "25",
		"--stealth",
		url,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return "", fmt.Errorf("obscura: %s", msg)
	}
	return stdout.String(), nil
}

func webFetchHTTP(ctx context.Context, url string) (string, error) {
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
		// Read a short snippet of the body so the daemon log surfaces useful
		// hints (Cloudflare challenge, captcha, redirect copy, etc.).
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		excerpt := strings.TrimSpace(stripHTML(string(snippet)))
		if len(excerpt) > 240 {
			excerpt = excerpt[:240] + "…"
		}
		finalURL := url
		if resp.Request != nil && resp.Request.URL != nil {
			finalURL = resp.Request.URL.String()
		}
		if excerpt == "" {
			return "", fmt.Errorf("http %d %s (final=%s)", resp.StatusCode, http.StatusText(resp.StatusCode), finalURL)
		}
		return "", fmt.Errorf("http %d %s (final=%s) — body: %s",
			resp.StatusCode, http.StatusText(resp.StatusCode), finalURL, excerpt)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, webFetchLimit*4))
	if err != nil {
		return "", err
	}
	return stripHTML(string(body)), nil
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "\n…[tronqué]"
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
