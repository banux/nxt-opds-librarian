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

	mu       sync.RWMutex
	byName   map[string]*Entry
	bySecret map[string]string // chat_secret -> name
}

// New builds a Registry from cfg. Per-instance MCP clients are NOT created
// here; they are initialised lazily by Get / GetByChatSecret.
func New(cfg config.Config, provider llm.Provider, maxSteps int, verbose bool) *Registry {
	r := &Registry{
		provider: provider,
		maxSteps: maxSteps,
		verbose:  verbose,
		byName:   map[string]*Entry{},
		bySecret: map[string]string{},
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
