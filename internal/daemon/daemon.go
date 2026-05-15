// Package daemon runs the librarian agent as a long-lived service: a
// periodic batch ticker plus an HTTP webhook receiver. All work is funneled
// through a single channel so the agent only runs one job at a time.
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
)

type Config struct {
	Listen        string
	WebhookPath   string
	WebhookSecret string
	Interval      time.Duration
	BatchLimit    int
	// BatchPrompt overrides the default maintenance instruction sent both
	// on tick and on POST /trigger with an empty body. Empty = use default.
	BatchPrompt string
}

type Daemon struct {
	cfg   Config
	agent *agent.Agent

	jobs chan job
	wg   sync.WaitGroup
}

type job struct {
	source string // "tick" | "webhook"
	instr  string
}

func New(cfg Config, a *agent.Agent) *Daemon {
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if cfg.WebhookPath == "" {
		cfg.WebhookPath = "/webhook/book-added"
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 6 * time.Hour
	}
	if cfg.BatchLimit <= 0 {
		cfg.BatchLimit = 10
	}
	return &Daemon{cfg: cfg, agent: a, jobs: make(chan job, 16)}
}

func (d *Daemon) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc(d.cfg.WebhookPath, d.handleWebhook)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/trigger", d.handleTrigger)

	srv := &http.Server{Addr: d.cfg.Listen, Handler: mux}

	d.wg.Add(1)
	go d.worker(ctx)

	d.wg.Add(1)
	go d.scheduler(ctx)

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		close(d.jobs)
	}()

	log.Printf("daemon listening on %s (webhook %s, interval %s)", d.cfg.Listen, d.cfg.WebhookPath, d.cfg.Interval)
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

	// kick once at startup so the daemon doesn't sit idle for `interval` first.
	d.enqueue(job{source: "tick", instr: d.batchInstruction()})

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.enqueue(job{source: "tick", instr: d.batchInstruction()})
		}
	}
}

func (d *Daemon) worker(ctx context.Context) {
	defer d.wg.Done()
	for j := range d.jobs {
		log.Printf("[job %s] start: %s", j.source, truncate(j.instr, 120))
		start := time.Now()
		runCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		err := d.agent.Run(runCtx, j.instr)
		cancel()
		if err != nil {
			log.Printf("[job %s] error after %s: %v", j.source, time.Since(start), err)
		} else {
			log.Printf("[job %s] done in %s", j.source, time.Since(start))
		}
	}
}

func (d *Daemon) enqueue(j job) {
	select {
	case d.jobs <- j:
	default:
		log.Printf("[queue] full, dropping %s job", j.source)
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

// WebhookPayload accepts a flexible payload — book_id is preferred, title is
// the fallback. Anything beyond that is logged but ignored.
type WebhookPayload struct {
	BookID string `json:"book_id"`
	ID     string `json:"id"`
	Title  string `json:"title"`
	Author string `json:"author"`
}

func (d *Daemon) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if d.cfg.WebhookSecret != "" {
		if !verifySignature(d.cfg.WebhookSecret, body, r.Header.Get("X-Signature")) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
	}

	var p WebhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	id := firstNonEmpty(p.BookID, p.ID)
	if id == "" && p.Title == "" {
		http.Error(w, "need book_id or title", http.StatusBadRequest)
		return
	}

	instr := webhookInstruction(id, p.Title, p.Author)
	d.enqueue(job{source: "webhook", instr: instr})
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintln(w, "queued")
}

// handleTrigger lets an operator force a run. Without a body it triggers the
// default batch instruction; with a JSON body {"prompt":"..."} or a raw text
// body, it runs the agent against that custom prompt.
//
//	curl -X POST http://localhost:8080/trigger
//	curl -X POST http://localhost:8080/trigger -d '{"prompt":"Traite Le Chevalier et la Phalène"}'
//	curl -X POST http://localhost:8080/trigger -H 'Content-Type: text/plain' --data 'Traite Le Chevalier'
func (d *Daemon) handleTrigger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
	d.enqueue(job{source: "manual", instr: instr})
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintln(w, "queued")
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
