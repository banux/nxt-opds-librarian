// Package daemon runs the librarian agent as a long-lived service: a
// periodic batch ticker plus an HTTP receiver. One worker goroutine per
// configured instance keeps jobs serialised within an instance while
// allowing different instances to make progress in parallel.
package daemon

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/banux/librarian-agent/internal/agent"
	"github.com/banux/librarian-agent/internal/instances"
	"github.com/banux/librarian-agent/internal/llm"
	"github.com/banux/librarian-agent/internal/mcp"
)

type Config struct {
	Listen     string
	Interval   time.Duration
	BatchLimit int
	// BatchPrompt overrides the default maintenance instruction sent both on
	// tick and on POST /trigger with an empty body. Empty = use the default.
	BatchPrompt string
	// PublicURL is the librarian's base URL announced to every paired
	// nxt-opds at startup so the catalog can rewrite its stored
	// librarian_url after a host/port change without re-pairing.
	PublicURL string
}

type Daemon struct {
	cfg      Config
	registry *instances.Registry

	wg sync.WaitGroup
}

func New(cfg Config, r *instances.Registry) *Daemon {
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 6 * time.Hour
	}
	if cfg.BatchLimit <= 0 {
		cfg.BatchLimit = 10
	}
	return &Daemon{cfg: cfg, registry: r}
}

func (d *Daemon) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /instances", d.handleListInstances)
	mux.HandleFunc("POST /chat", d.handleChat)
	mux.HandleFunc("POST /webhooks/{instance}/book-event", d.handleWebhook)
	mux.HandleFunc("POST /trigger/{instance}", d.handleTrigger)
	mux.HandleFunc("POST /instances/{instance}/forget", d.handleForget)

	srv := &http.Server{Addr: d.cfg.Listen, Handler: mux}

	for _, name := range d.registry.Names() {
		name := name
		d.wg.Add(1)
		go d.worker(ctx, name)
	}

	d.wg.Add(1)
	go d.scheduler(ctx)

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		// Close every instance's Jobs channel so its worker exits.
		for _, name := range d.registry.Names() {
			if e, err := d.registry.Get(context.Background(), name); err == nil {
				close(e.Jobs)
			}
		}
	}()

	log.Printf("daemon listening on %s (interval %s, %d instances)", d.cfg.Listen, d.cfg.Interval, len(d.registry.Names()))
	d.announceAll(ctx)
	d.startHeartbeats(ctx)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	d.wg.Wait()
	return nil
}

func (d *Daemon) scheduler(ctx context.Context) {
	defer d.wg.Done()
	t := time.NewTicker(d.cfg.Interval)
	defer t.Stop()

	d.enqueueAll(ctx, "tick", d.batchInstruction())

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.enqueueAll(ctx, "tick", d.batchInstruction())
		}
	}
}

func (d *Daemon) enqueueAll(ctx context.Context, source, instr string) {
	for _, name := range d.registry.Names() {
		entry, err := d.registry.Get(ctx, name)
		if err != nil {
			log.Printf("[%s] init failed: %v", name, err)
			continue
		}
		d.enqueue(entry, instances.Job{Source: source, Instr: instr})
	}
}

func (d *Daemon) worker(ctx context.Context, name string) {
	defer d.wg.Done()

	entry, err := d.registry.Get(ctx, name)
	if err != nil {
		log.Printf("[%s] worker abandonné: %v", name, err)
		return
	}

	for j := range entry.Jobs {
		log.Printf("[%s job %s] start: %s", name, j.Source, truncate(j.Instr, 120))
		start := time.Now()
		runCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		entry.Lock.Lock()
		err := entry.Agent.Run(runCtx, j.Instr)
		entry.Lock.Unlock()
		cancel()
		if err != nil {
			log.Printf("[%s job %s] error after %s: %v", name, j.Source, time.Since(start), err)
		} else {
			log.Printf("[%s job %s] done in %s", name, j.Source, time.Since(start))
		}
	}
}

func (d *Daemon) enqueue(entry *instances.Entry, j instances.Job) {
	select {
	case entry.Jobs <- j:
	default:
		log.Printf("[%s queue] full, dropping %s job", entry.Cfg.Name, j.Source)
	}
}

func (d *Daemon) batchInstruction() string {
	if d.cfg.BatchPrompt != "" {
		return d.cfg.BatchPrompt
	}
	return fmt.Sprintf(
		"Lance la maintenance batch : search_books(not_indexed: true, limit: %d) puis enrichis chaque livre selon les règles du système. Termine par 'FIN'.",
		d.cfg.BatchLimit,
	)
}

// --- HTTP handlers ----------------------------------------------------------

func (d *Daemon) handleListInstances(w http.ResponseWriter, r *http.Request) {
	type publicInst struct {
		Name   string `json:"name"`
		Label  string `json:"label"`
		Locale string `json:"locale"`
	}
	insts := d.registry.List()
	out := make([]publicInst, 0, len(insts))
	for _, i := range insts {
		out = append(out, publicInst{Name: i.Name, Label: i.Label, Locale: i.Locale})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// WebhookPayload covers both the flat shape used by nxt-opds' admin webhooks
// and the older flat-with-id / OPDS-envelope forms still emitted by some
// tooling. The id is preferred over the title.
type WebhookPayload struct {
	Event   string      `json:"event"`
	BookID  string      `json:"book_id"`
	ID      string      `json:"id"`
	Title   string      `json:"title"`
	Author  string      `json:"author"`
	Authors []string    `json:"authors"`
	Data    *bookFields `json:"data"`
	Book    *bookFields `json:"book"`
}

type bookFields struct {
	BookID  string   `json:"book_id"`
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Author  string   `json:"author"`
	Authors []string `json:"authors"`
}

func (p *WebhookPayload) resolve() (id, title, author string) {
	id = firstNonEmpty(p.BookID, p.ID)
	title = p.Title
	author = p.Author
	if author == "" && len(p.Authors) > 0 {
		author = p.Authors[0]
	}
	for _, nested := range []*bookFields{p.Data, p.Book} {
		if nested == nil {
			continue
		}
		if v := firstNonEmpty(nested.BookID, nested.ID); v != "" {
			id = v
		}
		if nested.Title != "" {
			title = nested.Title
		}
		if nested.Author != "" {
			author = nested.Author
		} else if len(nested.Authors) > 0 {
			author = nested.Authors[0]
		}
	}
	return id, title, author
}

func (d *Daemon) handleWebhook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("instance")
	log.Printf("[webhook %s] %s from %s sig=%t", name, r.Method, clientIP(r), r.Header.Get("X-Signature") != "")

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if secret := d.registry.WebhookSecret(name); secret != "" {
		if !verifySignature(secret, body, r.Header.Get("X-Signature")) {
			log.Printf("[webhook %s] reject 401: bad signature", name)
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
	}

	entry, err := d.registry.Get(r.Context(), name)
	if err != nil {
		http.Error(w, "unknown instance", http.StatusNotFound)
		return
	}

	var p WebhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	id, title, author := p.resolve()
	if id == "" && title == "" {
		http.Error(w, "need id or title", http.StatusBadRequest)
		return
	}
	log.Printf("[webhook %s] accept event=%q id=%q title=%q", name, p.Event, id, title)
	d.enqueue(entry, instances.Job{Source: "webhook", Instr: webhookInstruction(id, title, author)})
	w.WriteHeader(http.StatusAccepted)
	_, _ = fmt.Fprintln(w, "queued")
}

func (d *Daemon) handleTrigger(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("instance")
	entry, err := d.registry.Get(r.Context(), name)
	if err != nil {
		http.Error(w, "unknown instance", http.StatusNotFound)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	instr := d.batchInstruction()
	if len(body) > 0 {
		ct := r.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "application/json") {
			var p struct {
				Prompt string `json:"prompt"`
			}
			if err := json.Unmarshal(body, &p); err != nil {
				http.Error(w, "invalid json", http.StatusBadRequest)
				return
			}
			if p.Prompt != "" {
				instr = p.Prompt
			}
		} else {
			instr = strings.TrimSpace(string(body))
		}
	}
	d.enqueue(entry, instances.Job{Source: "manual", Instr: instr})
	w.WriteHeader(http.StatusAccepted)
	_, _ = fmt.Fprintln(w, "queued")
}

// handleForget removes the instance from the on-disk config when nxt-opds
// initiates a dissociation. Auth: Authorization: Bearer <chat_secret>.
//
// Note: this drops the entry from the *running* config but does not yet
// rewrite the YAML — that requires `SIGHUP` reload, intentionally out of
// scope for v1. The daemon will keep serving the existing entry until
// restarted, which matches the nxt-opds best-effort dissociation flow.
func (d *Daemon) handleForget(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("instance")
	secret := bearerToken(r.Header.Get("Authorization"))
	entry, ok := d.registry.GetByChatSecret(r.Context(), secret)
	if !ok || entry.Cfg.Name != name {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	log.Printf("[forget %s] requested via chat_secret", name)
	w.WriteHeader(http.StatusAccepted)
	_, _ = fmt.Fprintln(w, "acknowledged")
}

// handleChat serves POST /chat as a single JSON response. The agent runs the
// tool-calling loop to completion and the handler returns the concatenated
// assistant text. Tool details and intermediate reasoning are not part of
// the response — only the final answer the model produced for the user.
func (d *Daemon) handleChat(w http.ResponseWriter, r *http.Request) {
	secret := bearerToken(r.Header.Get("Authorization"))
	entry, ok := d.registry.GetByChatSecret(r.Context(), secret)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		Message string `json:"message"`
		// UserToken (optional) is the connected user's per-account OPDS/MCP
		// token. When set, MCP tool calls in this chat scope to that user
		// (their to_read list, wishlist, recommendations, unread, …) instead
		// of falling back to the librarian's admin credential.
		UserToken string `json:"user_token"`
		History   []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"history"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		http.Error(w, "empty message", http.StatusBadRequest)
		return
	}

	history := make([]llm.Message, 0, len(req.History))
	for _, m := range req.History {
		role := llm.RoleUser
		if m.Role == "assistant" {
			role = llm.RoleAssistant
		}
		history = append(history, llm.Message{Role: role, Text: m.Content})
	}

	// Scope MCP tool calls to the connected user when the relay forwarded
	// their token. The bearer override flows through r.Context() into every
	// MCP request issued by the agent loop.
	ctx := mcp.WithBearer(r.Context(), req.UserToken)

	name := entry.Cfg.Name
	scope := "instance"
	if req.UserToken != "" {
		scope = "user"
	}
	log.Printf("[chat %s] ◀ %s (scope=%s, history=%d): %s",
		name, clientIP(r), scope, len(history), truncate(req.Message, 200))

	entry.Lock.Lock()
	defer entry.Lock.Unlock()

	// Collect every text fragment the model produces. Tool calls are still
	// executed by the loop but discarded for the response — the user only
	// wants the final answer. The Emit hook also tees structured events to
	// the daemon log so operators can follow what the agent is doing.
	var (
		texts     []string
		runError  string
		toolCount int
	)
	start := time.Now()
	prevEmit := entry.Agent.Emit
	entry.Agent.Emit = func(e agent.Event) {
		switch e.Kind {
		case "text":
			if e.Delta != "" {
				texts = append(texts, e.Delta)
			}
		case "tool_call":
			toolCount++
			log.Printf("[chat %s] tool_call %s %s", name, e.Name, summarizeArgs(e.Arguments))
		case "tool_result":
			status := "ok"
			if e.IsError {
				status = "err"
			}
			log.Printf("[chat %s] tool_result %s [%s] %s",
				name, e.Name, status, truncate(strings.TrimSpace(e.Result), 200))
		case "done":
			log.Printf("[chat %s] done in %s (steps=%d, tools=%d, stop=%s)",
				name, time.Since(start).Round(time.Millisecond), e.Steps, toolCount, e.StopReason)
		case "error":
			runError = e.Message
			log.Printf("[chat %s] event error: %s", name, e.Message)
		}
	}
	defer func() { entry.Agent.Emit = prevEmit }()

	if err := entry.Agent.RunWithHistory(ctx, req.Message, history); err != nil {
		log.Printf("[chat %s] agent error after %s: %v",
			name, time.Since(start).Round(time.Millisecond), err)
		if runError == "" {
			runError = err.Error()
		}
	}

	reply := strings.TrimSpace(strings.Join(texts, "\n"))
	log.Printf("[chat %s] ▶ reply (%d chars, tools=%d): %s",
		name, len(reply), toolCount, truncate(reply, 200))

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	resp := map[string]any{"reply": strings.TrimSpace(strings.Join(texts, "\n"))}
	if runError != "" {
		resp["error"] = runError
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// --- helpers ----------------------------------------------------------------

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.Index(v, ","); i >= 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	return r.RemoteAddr
}

func bearerToken(h string) string {
	const prefix = "Bearer "
	if strings.HasPrefix(h, prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

func webhookInstruction(id, title, author string) string {
	if id != "" {
		return fmt.Sprintf("Un nouveau livre vient d'être ajouté à la bibliothèque (id=%q). Appelle get_book avec cet id puis applique le workflow d'enrichissement complet. Termine par 'FIN'.", id)
	}
	if author != "" {
		return fmt.Sprintf("Un nouveau livre vient d'être ajouté : titre %q, auteur %q. Cherche-le avec search_books puis applique le workflow d'enrichissement complet. Termine par 'FIN'.", title, author)
	}
	return fmt.Sprintf("Un nouveau livre vient d'être ajouté : titre %q. Cherche-le avec search_books puis applique le workflow d'enrichissement complet. Termine par 'FIN'.", title)
}

func verifySignature(secret string, body []byte, signature string) bool {
	signature = strings.TrimPrefix(signature, "sha256=")
	if signature == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func summarizeArgs(args map[string]any) string {
	if len(args) == 0 {
		return "()"
	}
	b, _ := json.Marshal(args)
	return truncate(string(b), 200)
}
