package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/banux/librarian-agent/internal/instances"
)

// runBatch is a deterministic alternative to `run --prompt "traite tous les
// 16+"`. Pagination is driven by Go, not the LLM, so small / verbose models
// cannot terminate the job early by writing "FIN" or by emitting fake
// "(L'exécution continue…)" stage directions. The agent is invoked ONCE
// per book with a tight per-book instruction; the surrounding loop decides
// when to stop based on what search_books returned.
func runBatch(args []string) {
	fs := flag.NewFlagSet("batch", flag.ExitOnError)
	c := registerCommon(fs)
	limit := fs.Int("limit", 50, "Taille de page pour search_books (max 100)")
	maxBooks := fs.Int("max-books", 0, "Plafond global de livres traités (0 = illimité)")
	maxSteps := fs.Int("max-steps", 60, "Étapes max par livre (5-10 suffisent en général)")
	startOffset := fs.Int("offset", 0, "Offset de départ (utile pour reprendre un batch interrompu)")
	tmpl := fs.String("prompt", "", "Prompt par livre. {{ID}} remplacé. Défaut : enrichissement complet.")
	dryRun := fs.Bool("dry-run", false, "Liste les IDs sans appeler l'agent")
	filters := newFilterList(fs, "filter", "Filtre passé à search_books, ex: age_rating_min=16 (répétable)")
	_ = fs.Parse(args)

	if *limit <= 0 || *limit > 100 {
		fmt.Fprintln(os.Stderr, "batch: --limit doit être entre 1 et 100")
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg, registry := loadConfigAndRegistry(c, *maxSteps, !*c.quiet)
	name := resolveInstance(cfg, *c.instance)

	entry, err := registry.Get(ctx, name)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	perBookTemplate := *tmpl
	if perBookTemplate == "" {
		perBookTemplate = defaultPerBookTemplate
	}

	processed := 0
	failed := 0
	start := time.Now()
	offset := *startOffset

	for {
		ids, total, err := searchBookIDs(ctx, entry, *filters, *limit, offset)
		if err != nil {
			fmt.Fprintf(os.Stderr, "batch: search_books offset=%d: %v\n", offset, err)
			os.Exit(1)
		}
		log.Printf("[batch %s] page offset=%d limit=%d → %d IDs (total estimé %d)",
			name, offset, *limit, len(ids), total)
		if len(ids) == 0 {
			break
		}

		stop := false
		for _, id := range ids {
			if *maxBooks > 0 && processed >= *maxBooks {
				log.Printf("[batch %s] plafond --max-books=%d atteint, arrêt", name, *maxBooks)
				stop = true
				break
			}
			if *dryRun {
				fmt.Println(id)
				processed++
				continue
			}
			instr := strings.ReplaceAll(perBookTemplate, "{{ID}}", id)
			log.Printf("[batch %s] ▶ %d/%d id=%s", name, processed+1, total, id)
			if err := runOneBook(ctx, entry, instr); err != nil {
				failed++
				log.Printf("[batch %s] id=%s ERROR: %v", name, id, err)
			} else {
				processed++
			}
		}
		if stop {
			break
		}
		// Short page → end reached.
		if len(ids) < *limit {
			break
		}
		offset += *limit
	}

	dur := time.Since(start).Round(time.Second)
	if *dryRun {
		fmt.Fprintf(os.Stderr, "batch: %d livre(s) listé(s) (dry-run), %s\n", processed, dur)
	} else {
		fmt.Fprintf(os.Stderr, "batch: %d livre(s) traité(s) / %d échec(s) en %s\n",
			processed, failed, dur)
	}
}

const defaultPerBookTemplate = `Enrichis UNIQUEMENT le livre dont l'id est "{{ID}}".
Étapes :
1. get_book(id:"{{ID}}") pour récupérer les métadonnées actuelles.
2. Si nécessaire, web_fetch sur Babelio / éditeur pour compléter résumé, age_rating, spice_rating, series_total.
3. update_book(id:"{{ID}}", …) avec les améliorations selon le workflow du système (tags normalisés en Title Case, summary nettoyé, age_rating, spice_rating si age_rating ≥ 16, series/series_index/series_total, titre nettoyé, last_maintenance_at:-1).
4. Si applicable, supprime la wishlist correspondante via list_wishlist + delete_wishlist_item.

Travaille uniquement sur ce livre. Termine par "FIN".`

// runOneBook drives one agent invocation against a single book id. Errors
// are returned so the loop can count and continue.
func runOneBook(ctx context.Context, entry *instances.Entry, instr string) error {
	entry.Lock.Lock()
	defer entry.Lock.Unlock()
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	return entry.Agent.Run(runCtx, instr)
}

// searchBookIDs invokes MCP search_books on the librarian's MCP client and
// extracts the book IDs from the formatted text response. The MCP server
// formats each book with a "   ID: <hex>\n" line — this regex is the
// contract enforced by formatBook in nxt-opds/internal/mcp/server.go.
func searchBookIDs(ctx context.Context, entry *instances.Entry, filters filterList, limit, offset int) ([]string, int, error) {
	args := map[string]any{
		"limit":  limit,
		"offset": offset,
	}
	for _, f := range filters {
		args[f.key] = f.value
	}
	res, err := entry.Client.CallTool(ctx, "search_books", args)
	if err != nil {
		return nil, 0, err
	}
	if res.IsError {
		return nil, 0, fmt.Errorf("search_books returned error: %s", res.Text)
	}
	matches := idLineRe.FindAllStringSubmatch(res.Text, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	total := 0
	if m := totalRe.FindStringSubmatch(res.Text); len(m) > 1 {
		total, _ = strconv.Atoi(m[1])
	}
	return out, total, nil
}

var (
	idLineRe = regexp.MustCompile(`(?m)^\s*ID:\s*([0-9a-fA-F]{8,64})\s*$`)
	totalRe  = regexp.MustCompile(`Trouvé\s+(\d+)\s+livre`)
)

// ---------- repeatable --filter flag ----------

type filterEntry struct {
	key   string
	value any
}
type filterList []filterEntry

func (f *filterList) String() string { return fmt.Sprintf("%v", []filterEntry(*f)) }
func (f *filterList) Set(raw string) error {
	parts := strings.SplitN(raw, "=", 2)
	if len(parts) != 2 || parts[0] == "" {
		return fmt.Errorf("filter doit être au format key=value (reçu: %q)", raw)
	}
	*f = append(*f, filterEntry{key: parts[0], value: parseFilterValue(parts[1])})
	return nil
}

// parseFilterValue coerces the raw flag value to the type search_books
// expects: integers stay integers, booleans stay booleans, the rest is a
// string. Most filters (age_rating, age_rating_min, spice_rating, limit,
// offset) are integers; unread_only / not_indexed are booleans; tag /
// author / series / publisher / collection are strings.
func parseFilterValue(s string) any {
	if s == "true" || s == "false" {
		return s == "true"
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return s
}

func newFilterList(fs *flag.FlagSet, name, usage string) *filterList {
	v := &filterList{}
	fs.Var(v, name, usage)
	return v
}
