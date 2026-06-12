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
	"net/url"
	"os"
	"os/exec"
	"regexp"
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

	// GoogleBooksAPIKey enables the google_books_search tool when non-empty.
	// When set, the system prompt is rendered with Google Books as the
	// PRIMARY metadata source (priority 1) — tried before any web_fetch.
	GoogleBooksAPIKey string

	// CamofoxURL is the base URL of a camofox-browser server (local stealth
	// Firefox). When non-empty, web_fetch tries camofox after Firecrawl and
	// before obscura, and web_search uses camofox's @google_search macro when
	// no Firecrawl key is configured. CamofoxAccessKey is sent as a bearer
	// token when the server is started with CAMOFOX_ACCESS_KEY.
	CamofoxURL       string
	CamofoxAccessKey string

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
	localTools := 1
	if a.GoogleBooksAPIKey != "" {
		a.tools = append(a.tools, llm.ToolSpec{
			Name:        "google_books_search",
			Description: "Source PRIORITAIRE pour rechercher des métadonnées de livre (titre, auteur, éditeur, date de parution, résumé, ISBN, catégories, langue) via l'API Google Books. À essayer EN PREMIER avant tout web_fetch quand tu cherches le résumé ou les métadonnées d'un livre — réponse structurée, rapide, fiable, couvre la quasi-totalité des livres édités. La requête `query` accepte du texte libre ou les opérateurs Google Books : `intitle:`, `inauthor:`, `isbn:`, `subject:`. Renvoie jusqu'à 5 volumes.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Requête Google Books. Texte libre ou opérateurs intitle:/inauthor:/isbn:/subject:. Ex: 'intitle:\"Le Chevalier et la Phalène\" inauthor:Chevreuse' ou 'isbn:9782811235567'."},"lang":{"type":"string","description":"Code langue ISO 639-1 pour filtrer (ex: 'fr', 'en'). Optionnel."}},"required":["query"]}`),
		})
		localTools++
	}
	if a.FirecrawlAPIKey != "" || a.CamofoxURL != "" {
		a.tools = append(a.tools, llm.ToolSpec{
			Name:        "web_search",
			Description: "Recherche sur le web : renvoie une liste de résultats (titre, URL, court extrait) pour une requête en langage naturel. Utilise CET outil pour DÉCOUVRIR la bonne page (fiche éditeur, libraire en ligne, Wikipedia, BnF…) d'un livre, puis appelle web_fetch sur l'URL la plus pertinente. N'appelle JAMAIS web_fetch directement sur une URL de moteur de recherche (google.com/search, bing.com, duckduckgo.com…) : passe par web_search. Paramètre optionnel `limit` (1-5, défaut 5).",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Requête de recherche en langage naturel, ex: 'Le Chevalier et la Phalène Chevreuse résumé éditeur'."},"limit":{"type":"integer","description":"Nombre de résultats (1-5, défaut 5). Optionnel."}},"required":["query"]}`),
		})
		localTools++
	}
	if a.Verbose {
		fmt.Printf("[agent] %d outils MCP (%d serveur(s)) + %d local(aux) (%s)\n", total, len(a.MCPs), localTools, a.LLM.Name())
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
	hasWebSearch := a.FirecrawlAPIKey != "" || a.CamofoxURL != ""
	switch mode {
	case ModeChat:
		system = renderChatPrompt(a.InstanceName, a.InstanceLabel, a.InstanceLocale, a.GoogleBooksAPIKey != "", hasWebSearch)
	default:
		system = renderSystemPrompt(a.InstanceName, a.InstanceLabel, a.InstanceLocale, a.GoogleBooksAPIKey != "", hasWebSearch)
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
	if tc.Name == "web_search" {
		if a.FirecrawlAPIKey == "" && a.CamofoxURL == "" {
			return "web_search appelé sans backend (ni clé Firecrawl ni camofox_url configurés)", true
		}
		query, _ := tc.Arguments["query"].(string)
		limit := 0
		if v, ok := tc.Arguments["limit"].(float64); ok {
			limit = int(v)
		}
		start := time.Now()
		var (
			text    string
			err     error
			backend string
		)
		if a.FirecrawlAPIKey != "" {
			backend = "firecrawl"
			text, err = webSearchFirecrawl(ctx, query, limit, a.FirecrawlAPIKey)
		} else {
			backend = "camofox"
			text, err = webSearchCamofox(ctx, a.CamofoxURL, a.CamofoxAccessKey, a.camofoxUserID(), query, limit)
		}
		elapsed := time.Since(start).Round(time.Millisecond)
		if err != nil {
			log.Printf("[web_search] %s échec en %s (%v) — query=%q", backend, elapsed, err, query)
			return fmt.Sprintf("erreur web_search: %v", err), true
		}
		log.Printf("[web_search] %s ok en %s (%d caractères) — query=%q", backend, elapsed, len(text), query)
		return text, false
	}
	if tc.Name == "google_books_search" {
		if a.GoogleBooksAPIKey == "" {
			return "google_books_search appelé sans clé API configurée", true
		}
		query, _ := tc.Arguments["query"].(string)
		lang, _ := tc.Arguments["lang"].(string)
		start := time.Now()
		text, err := googleBooksSearch(ctx, query, lang, a.GoogleBooksAPIKey)
		elapsed := time.Since(start).Round(time.Millisecond)
		if err != nil {
			log.Printf("[google_books] échec en %s (%v) — query=%q", elapsed, err, query)
			return fmt.Sprintf("erreur google_books_search: %v", err), true
		}
		log.Printf("[google_books] ok en %s (%d caractères) — query=%q", elapsed, len(text), query)
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
//  2. camofox-browser (when a.CamofoxURL is set) — local stealth Firefox,
//     anti-Cloudflare; returns a token-efficient accessibility snapshot.
//  3. obscura (when the binary is on $PATH) — local headless browser.
//  4. plain HTTP GET + crude HTML strip — last-resort fallback.
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
		log.Printf("[web_fetch] firecrawl échec en %s (%v) — fallback camofox/obscura/HTTP — url=%s",
			elapsed, err, url)
	}

	if a.CamofoxURL != "" {
		start := time.Now()
		text, err := webFetchCamofox(ctx, a.CamofoxURL, a.CamofoxAccessKey, a.camofoxUserID(), url)
		elapsed := time.Since(start).Round(time.Millisecond)
		if err == nil {
			log.Printf("[web_fetch] camofox ok en %s (%d caractères) — url=%s",
				elapsed, len(text), url)
			return truncate(text, webFetchLimit), nil
		}
		log.Printf("[web_fetch] camofox échec en %s (%v) — fallback obscura/HTTP — url=%s",
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
		// "stealth" routes the request through residential proxies. ~5x more
		// expensive in credits per page but the only mode that gets through
		// Cloudflare / DataDome / Babelio reliably.
		"proxy": "stealth",
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

const webSearchMaxResults = 5

type firecrawlSearchResult struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

// webSearchFirecrawl calls Firecrawl's v2 /search endpoint and returns the
// results formatted as a numbered list (title, URL, snippet). This replaces
// scraping a public search engine: with a Firecrawl key the agent gets clean,
// anti-bot SERP data it can then web_fetch. The agent must NOT web_fetch a
// search-engine URL — it calls web_search instead.
func webSearchFirecrawl(ctx context.Context, query string, limit int, apiKey string) (string, error) {
	if query == "" {
		return "", fmt.Errorf("query vide")
	}
	if limit <= 0 || limit > webSearchMaxResults {
		limit = webSearchMaxResults
	}
	payload, _ := json.Marshal(map[string]any{
		"query": query,
		"limit": limit,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.firecrawl.dev/v2/search", bytes.NewReader(payload))
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
		Success bool            `json:"success"`
		Error   string          `json:"error"`
		Data    json.RawMessage `json:"data"`
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

	results := parseFirecrawlSearchData(parsed.Data)
	if len(results) == 0 {
		return "", fmt.Errorf("firecrawl: aucun résultat pour %q", query)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d résultat(s) pour %q :\n\n", len(results), query)
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, strings.TrimSpace(r.Title), strings.TrimSpace(r.URL))
		if d := strings.TrimSpace(r.Description); d != "" {
			fmt.Fprintf(&b, "   %s\n", d)
		}
	}
	return truncate(b.String(), webFetchLimit), nil
}

// parseFirecrawlSearchData handles both the v2 grouped shape ({"web":[…]}) and
// the legacy v1 flat array ([…]) so a Firecrawl API bump doesn't break search.
func parseFirecrawlSearchData(raw json.RawMessage) []firecrawlSearchResult {
	var grouped struct {
		Web []firecrawlSearchResult `json:"web"`
	}
	if err := json.Unmarshal(raw, &grouped); err == nil && len(grouped.Web) > 0 {
		return grouped.Web
	}
	var flat []firecrawlSearchResult
	if err := json.Unmarshal(raw, &flat); err == nil && len(flat) > 0 {
		return flat
	}
	return nil
}

// --- camofox-browser backend -------------------------------------------------
//
// camofox-browser (https://github.com/jo-inc/camofox-browser) is a local
// stealth Firefox exposed over a small HTTP API. Pages are driven through a tab
// lifecycle: POST /tabs opens (and navigates) a tab, GET /tabs/{id}/snapshot
// returns a token-efficient accessibility snapshot, POST /tabs/{id}/navigate
// runs a search macro, GET /tabs/{id}/links lists anchors, DELETE /tabs/{id}
// closes the tab. Every tab we open is closed again, best-effort.

const (
	// camofoxSessionKey groups all librarian tabs under one camofox session per
	// user. We open and immediately close one tab per fetch/search, so the tab
	// limit per session is never an issue.
	camofoxSessionKey = "librarian"
	// camofoxSnapshotPages bounds snapshot pagination per fetch so a huge page
	// cannot loop forever; we stop earlier once webFetchLimit is reached.
	camofoxSnapshotPages = 8
)

// camofoxUserID is the per-instance camofox session owner. A stable id (one per
// catalog) keeps camofox's per-user concurrency accounting predictable while
// isolating instances from each other. Falls back to "librarian" when unnamed.
func (a *Agent) camofoxUserID() string {
	if a.InstanceName != "" {
		return a.InstanceName
	}
	return "librarian"
}

// camofoxRequest issues one JSON request to a camofox-browser server. body is
// marshalled as JSON when non-nil; accessKey (only needed when camofox runs
// with CAMOFOX_ACCESS_KEY) is sent as a bearer token. Returns the raw body and
// status code, erroring only on transport/marshal failure — HTTP status is left
// to the caller.
func camofoxRequest(ctx context.Context, method, endpoint, accessKey string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if accessKey != "" {
		req.Header.Set("Authorization", "Bearer "+accessKey)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, webFetchLimit*4))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return data, resp.StatusCode, nil
}

// camofoxStatusErr formats a non-2xx camofox response with a short body excerpt
// (404 "tab not found", 429 "tab limit", auth failures, …).
func camofoxStatusErr(action string, status int, body []byte) error {
	excerpt := strings.TrimSpace(string(body))
	if len(excerpt) > 300 {
		excerpt = excerpt[:300] + "…"
	}
	if excerpt == "" {
		return fmt.Errorf("camofox %s: http %d", action, status)
	}
	return fmt.Errorf("camofox %s: http %d: %s", action, status, excerpt)
}

// camofoxOpenTab opens a tab (optionally navigating to target) and returns its
// id. The caller is responsible for closing it via camofoxCloseTab.
func camofoxOpenTab(ctx context.Context, base, accessKey, userID, target string) (string, error) {
	payload := map[string]any{"userId": userID, "sessionKey": camofoxSessionKey}
	if target != "" {
		payload["url"] = target
	}
	body, status, err := camofoxRequest(ctx, http.MethodPost, base+"/tabs", accessKey, payload)
	if err != nil {
		return "", err
	}
	if status >= 400 {
		return "", camofoxStatusErr("create tab", status, body)
	}
	var tab struct {
		TabID string `json:"tabId"`
	}
	if err := json.Unmarshal(body, &tab); err != nil || tab.TabID == "" {
		return "", fmt.Errorf("camofox: réponse create tab inattendue: %s", truncate(strings.TrimSpace(string(body)), 200))
	}
	return tab.TabID, nil
}

// camofoxCloseTab closes a tab, best-effort. Uses a fresh background context so
// cleanup still runs when the parent context was cancelled mid-fetch.
func camofoxCloseTab(base, accessKey, userID, tabID string) {
	endpoint := base + "/tabs/" + url.PathEscape(tabID) + "?userId=" + url.QueryEscape(userID)
	_, _, _ = camofoxRequest(context.Background(), http.MethodDelete, endpoint, accessKey, nil)
}

// webFetchCamofox opens target in a camofox tab and returns the accessibility
// snapshot (text), paginating large pages up to webFetchLimit.
func webFetchCamofox(ctx context.Context, baseURL, accessKey, userID, target string) (string, error) {
	if target == "" {
		return "", fmt.Errorf("url vide")
	}
	base := strings.TrimRight(baseURL, "/")

	tabID, err := camofoxOpenTab(ctx, base, accessKey, userID, target)
	if err != nil {
		return "", err
	}
	defer camofoxCloseTab(base, accessKey, userID, tabID)

	snap, err := camofoxAccessibilitySnapshot(ctx, base, accessKey, userID, tabID)
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(snap)
	if out == "" {
		return "", fmt.Errorf("camofox: snapshot vide pour %s", target)
	}
	return out, nil
}

// camofoxAccessibilitySnapshot returns the accessibility-tree snapshot of a tab,
// concatenated across up to camofoxSnapshotPages pages and capped near
// webFetchLimit. It is the shared primitive behind both web_fetch (page text)
// and web_search (the SERP, whose organic results carry their destination on
// /url: lines — see parseCamofoxSnapshotLinks).
func camofoxAccessibilitySnapshot(ctx context.Context, base, accessKey, userID, tabID string) (string, error) {
	var b strings.Builder
	offset := 0
	for page := 0; page < camofoxSnapshotPages; page++ {
		snapURL := fmt.Sprintf("%s/tabs/%s/snapshot?userId=%s&offset=%d",
			base, url.PathEscape(tabID), url.QueryEscape(userID), offset)
		body, status, err := camofoxRequest(ctx, http.MethodGet, snapURL, accessKey, nil)
		if err != nil {
			return "", err
		}
		if status >= 400 {
			return "", camofoxStatusErr("snapshot", status, body)
		}
		var snap struct {
			Snapshot   string `json:"snapshot"`
			HasMore    bool   `json:"hasMore"`
			NextOffset int    `json:"nextOffset"`
		}
		if err := json.Unmarshal(body, &snap); err != nil {
			return "", fmt.Errorf("camofox: parse snapshot: %w", err)
		}
		b.WriteString(snap.Snapshot)
		if !snap.HasMore || snap.Snapshot == "" || b.Len() >= webFetchLimit || snap.NextOffset <= offset {
			break
		}
		offset = snap.NextOffset
	}
	return b.String(), nil
}

type camofoxLink struct {
	Text string `json:"text"`
	Href string `json:"href"`
}

// webSearchCamofox runs the @google_search macro in a camofox tab and returns
// the organic result links formatted as the same numbered list web_search
// produces with Firecrawl. Used as the web_search backend when no Firecrawl key
// is configured.
func webSearchCamofox(ctx context.Context, baseURL, accessKey, userID, query string, limit int) (string, error) {
	if query == "" {
		return "", fmt.Errorf("query vide")
	}
	if limit <= 0 || limit > webSearchMaxResults {
		limit = webSearchMaxResults
	}
	base := strings.TrimRight(baseURL, "/")

	tabID, err := camofoxOpenTab(ctx, base, accessKey, userID, "")
	if err != nil {
		return "", err
	}
	defer camofoxCloseTab(base, accessKey, userID, tabID)

	body, status, err := camofoxRequest(ctx, http.MethodPost,
		base+"/tabs/"+url.PathEscape(tabID)+"/navigate", accessKey, map[string]any{
			"userId": userID,
			"macro":  "@google_search",
			"query":  query,
		})
	if err != nil {
		return "", err
	}
	if status >= 400 {
		return "", camofoxStatusErr("navigate macro", status, body)
	}

	// The /links endpoint only surfaces the SERP chrome (Google's own footer:
	// policies./support. links) which the noise filter strips, leaving nothing.
	// The organic results live in the accessibility snapshot, each as a
	// `- link "Title" [eN]:` node with its destination on a `- /url: ...` line.
	snap, err := camofoxAccessibilitySnapshot(ctx, base, accessKey, userID, tabID)
	if err != nil {
		return "", err
	}

	results := filterCamofoxSearchLinks(parseCamofoxSnapshotLinks(snap), limit)
	if len(results) == 0 {
		return "", fmt.Errorf("camofox: aucun résultat exploitable pour %q", query)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d résultat(s) pour %q :\n\n", len(results), query)
	for i, r := range results {
		fmt.Fprintf(&sb, "%d. %s\n   %s\n", i+1, strings.TrimSpace(r.Title), strings.TrimSpace(r.URL))
	}
	return truncate(sb.String(), webFetchLimit), nil
}

// camofoxSERPNoise lists host/URL substrings that are search-engine chrome or
// Google's own internal links rather than organic result destinations.
var camofoxSERPNoise = []string{
	"google.", "gstatic.", "googleusercontent.", "schema.org",
	"bing.com", "duckduckgo.com", "accounts.", "policies.", "support.",
}

// normalizeCamofoxResultURL unwraps Google's /url?q= and /imgres redirect links
// to the real destination so the noise filter sees the actual host.
func normalizeCamofoxResultURL(href string) string {
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if strings.Contains(strings.ToLower(u.Host), "google") && (u.Path == "/url" || u.Path == "/imgres") {
		if q := u.Query().Get("q"); q != "" {
			return q
		}
		if q := u.Query().Get("url"); q != "" {
			return q
		}
	}
	return href
}

var (
	// camofoxSnapshotLinkRe captures the visible title of an accessibility-tree
	// link node, e.g. `- link "Primal Hunter, la série" [e9]:`. The capture is
	// greedy so titles containing quotes survive (the final quote is the
	// closing one, before the ` [eN]` ref).
	camofoxSnapshotLinkRe = regexp.MustCompile(`^\s*-\s+link\s+"(.*)"`)
	// camofoxSnapshotURLRe captures the destination on a node's /url: line,
	// e.g. `  - /url: https://booknode.com/serie/primal-hunter`.
	camofoxSnapshotURLRe = regexp.MustCompile(`^\s*-\s+/url:\s*(\S+)`)
)

// parseCamofoxSnapshotLinks extracts (title, href) pairs from a Playwright-style
// accessibility snapshot of a Google SERP. Each organic result is a
// `- link "Title" [eN]:` node whose real destination sits on a nested
// `- /url: ...` line; the title is associated with the nearest preceding link
// node and consumed once emitted so it never bleeds onto an unrelated /url:.
// Page chrome (nav tabs, footer) either carries no /url: or a google.* one that
// filterCamofoxSearchLinks drops as noise.
func parseCamofoxSnapshotLinks(snapshot string) []camofoxLink {
	var out []camofoxLink
	title := ""
	for _, line := range strings.Split(snapshot, "\n") {
		if m := camofoxSnapshotLinkRe.FindStringSubmatch(line); m != nil {
			title = strings.TrimSpace(m[1])
			continue
		}
		if m := camofoxSnapshotURLRe.FindStringSubmatch(line); m != nil {
			out = append(out, camofoxLink{Text: title, Href: m[1]})
			title = ""
		}
	}
	return out
}

// filterCamofoxSearchLinks turns the raw SERP anchors into up to limit organic
// results: it unwraps Google redirects, drops search-engine/internal noise and
// non-http links, and dedupes by URL while preserving order.
func filterCamofoxSearchLinks(links []camofoxLink, limit int) []firecrawlSearchResult {
	if limit <= 0 || limit > webSearchMaxResults {
		limit = webSearchMaxResults
	}
	var out []firecrawlSearchResult
	seen := map[string]bool{}
	for _, l := range links {
		href := normalizeCamofoxResultURL(strings.TrimSpace(l.Href))
		if !isCamofoxResultURL(href) || seen[href] {
			continue
		}
		seen[href] = true
		out = append(out, firecrawlSearchResult{URL: href, Title: strings.TrimSpace(l.Text)})
		if len(out) >= limit {
			break
		}
	}
	return out
}

func isCamofoxResultURL(href string) bool {
	if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "javascript:") {
		return false
	}
	u, err := url.Parse(href)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Host)
	for _, n := range camofoxSERPNoise {
		if strings.Contains(host, n) {
			return false
		}
	}
	return true
}

// webFetchObscura shells out to `obscura fetch --dump markdown` and returns
// stdout. Quiet mode suppresses obscura's own log lines.
func webFetchObscura(ctx context.Context, url string) (string, error) {
	cmd := exec.CommandContext(ctx, "obscura", "fetch",
		"--dump", "markdown",
		"--quiet",
		"--timeout", "25",
		"--stealth",
		// networkidle waits for anti-bot challenges (Cloudflare interstitials,
		// JS-injected content) to settle before extraction. Slower than the
		// default "load" but the only mode that gets stable output on Babelio
		// and similar guarded sites.
		"--wait-until", "networkidle",
		// Realistic recent Chrome UA — obscura's default is fingerprinted by
		// several WAFs. macOS+Chrome is the most common combo on the web, so
		// it blends into background traffic.
		"--user-agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
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
