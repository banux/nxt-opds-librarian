// Package instances holds the per-nxt-opds runtime state: one MCP client and
// one Agent per configured instance. Clients are built lazily on first
// access so the librarian can start even when one of its targets is offline.
package instances

import (
	"context"
	"crypto/subtle"
	"fmt"
	"sync"

	"github.com/banux/librarian-agent/internal/agent"
	"github.com/banux/librarian-agent/internal/config"
	"github.com/banux/librarian-agent/internal/llm"
	"github.com/banux/librarian-agent/internal/mcp"
)

// Entry is the runtime view of one configured instance. The Lock guards the
// Agent's transcript so two concurrent runs cannot race; Jobs is the per-
// instance queue fed by the scheduler / webhook handlers.
type Entry struct {
	Cfg    config.Instance
	Client *mcp.Client
	Agent  *agent.Agent
	Lock   sync.Mutex
	Jobs   chan Job

	once sync.Once
	err  error
}

// Job is the unit of work the daemon dispatches against one instance.
type Job struct {
	Source string // "tick" | "webhook" | "manual"
	Instr  string
}

// Registry owns one Entry per configured instance plus the secret indexes
// used by the chat / forget endpoints to resolve back to a name.
type Registry struct {
	provider llm.Provider
	maxSteps int
	verbose  bool

	// obscuraURL is the optional Streamable-HTTP MCP endpoint to obscura.
	// When non-empty, every lazily-initialised agent gets it attached as a
	// secondary MCP client. The underlying *mcp.Client is shared across
	// instances and built once on first use (obscuraOnce).
	obscuraURL    string
	obscuraOnce   sync.Once
	obscuraClient *mcp.Client
	obscuraErr    error

	// firecrawlAPIKey is the optional Firecrawl key forwarded to every agent
	// so its web_fetch tool can use Firecrawl's /scrape backend before
	// obscura / raw HTTP. Empty disables that backend.
	firecrawlAPIKey string

	// googleBooksAPIKey is the optional Google Books key forwarded to every
	// agent. When set, the agent exposes a google_books_search tool and the
	// batch system prompt elevates it to priority 1 for metadata lookups.
	googleBooksAPIKey string

	// camofoxURL / camofoxAccessKey are the optional camofox-browser endpoint
	// and bearer token forwarded to every agent: web_fetch tries camofox after
	// Firecrawl and before obscura, and web_search uses camofox's @google_search
	// macro when no Firecrawl key is configured.
	camofoxURL       string
	camofoxAccessKey string

	mu       sync.RWMutex
	byName   map[string]*Entry
	bySecret map[string]string // chat_secret -> name
}

// New builds a Registry from cfg. Per-instance MCP clients are NOT created
// here; they are initialised lazily by Get / GetByChatSecret.
func New(cfg config.Config, provider llm.Provider, maxSteps int, verbose bool) *Registry {
	r := &Registry{
		provider:        provider,
		maxSteps:        maxSteps,
		verbose:         verbose,
		obscuraURL:        cfg.ObscuraMCPURL,
		firecrawlAPIKey:   cfg.FirecrawlAPIKey,
		googleBooksAPIKey: cfg.GoogleBooksAPIKey,
		camofoxURL:        cfg.CamofoxURL,
		camofoxAccessKey:  cfg.CamofoxAccessKey,
		byName:            map[string]*Entry{},
		bySecret:          map[string]string{},
	}
	for _, inst := range cfg.Instances {
		entry := &Entry{Cfg: inst, Jobs: make(chan Job, 16)}
		r.byName[inst.Name] = entry
		if inst.ChatSecret != "" {
			r.bySecret[inst.ChatSecret] = inst.Name
		}
	}
	return r
}

// obscura returns the shared obscura MCP client, initialised once. Returns
// (nil, nil) when no obscura URL is configured.
func (r *Registry) obscura(ctx context.Context) (*mcp.Client, error) {
	if r.obscuraURL == "" {
		return nil, nil
	}
	r.obscuraOnce.Do(func() {
		c := mcp.New(r.obscuraURL, "")
		if err := c.Initialize(ctx); err != nil {
			r.obscuraErr = fmt.Errorf("init obscura MCP %q: %w", r.obscuraURL, err)
			return
		}
		r.obscuraClient = c
	})
	return r.obscuraClient, r.obscuraErr
}

// List returns the static config of every known instance (no secrets, no
// runtime state). Used by GET /instances and by the scheduler.
func (r *Registry) List() []config.Instance {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]config.Instance, 0, len(r.byName))
	for _, e := range r.byName {
		out = append(out, e.Cfg)
	}
	return out
}

// Names returns the unique set of instance slugs.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, n)
	}
	return out
}

// Get returns the entry for name, initialising the MCP client + Agent on
// first use. Returns an error if the instance does not exist or its first
// initialisation failed.
func (r *Registry) Get(ctx context.Context, name string) (*Entry, error) {
	r.mu.RLock()
	entry, ok := r.byName[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("instance %q inconnue", name)
	}
	entry.once.Do(func() {
		client := mcp.New(entry.Cfg.MCPURL, entry.Cfg.MCPToken)
		if err := client.Initialize(ctx); err != nil {
			entry.err = fmt.Errorf("init MCP %q: %w", name, err)
			return
		}
		a := agent.New(r.provider, client)
		a.MaxSteps = r.maxSteps
		a.Verbose = r.verbose
		a.InstanceName = entry.Cfg.Name
		a.InstanceLabel = entry.Cfg.Label
		a.InstanceLocale = entry.Cfg.Locale
		a.FirecrawlAPIKey = r.firecrawlAPIKey
		a.GoogleBooksAPIKey = r.googleBooksAPIKey
		a.CamofoxURL = r.camofoxURL
		a.CamofoxAccessKey = r.camofoxAccessKey
		// Attach the shared obscura MCP client (if configured) so the agent
		// exposes browser_* tools alongside the OPDS catalog tools. A failure
		// to reach obscura is logged but non-fatal — the agent falls back to
		// its built-in web_fetch.
		if obs, err := r.obscura(ctx); err != nil {
			if r.verbose {
				fmt.Printf("[registry] obscura indisponible: %v (agent %q sans browser_*)\n", err, name)
			}
		} else if obs != nil {
			a.AddMCP(obs)
		}
		if err := a.Init(ctx); err != nil {
			entry.err = fmt.Errorf("init agent %q: %w", name, err)
			return
		}
		entry.Client = client
		entry.Agent = a
	})
	if entry.err != nil {
		return nil, entry.err
	}
	return entry, nil
}

// GetByChatSecret resolves an entry by its bearer secret. The comparison is
// constant-time so an attacker cannot probe for valid secrets by timing.
func (r *Registry) GetByChatSecret(ctx context.Context, secret string) (*Entry, bool) {
	if secret == "" {
		return nil, false
	}
	r.mu.RLock()
	var name string
	for s, n := range r.bySecret {
		if subtle.ConstantTimeCompare([]byte(s), []byte(secret)) == 1 {
			name = n
		}
	}
	r.mu.RUnlock()
	if name == "" {
		return nil, false
	}
	entry, err := r.Get(ctx, name)
	if err != nil {
		return nil, false
	}
	return entry, true
}

// WebhookSecret returns the HMAC secret for instance name, or empty if the
// instance is unknown / has no secret configured.
func (r *Registry) WebhookSecret(name string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e, ok := r.byName[name]; ok {
		return e.Cfg.WebhookSecret
	}
	return ""
}
