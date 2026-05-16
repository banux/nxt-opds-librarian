package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/banux/librarian-agent/internal/config"
	"github.com/banux/librarian-agent/internal/daemon"
	"github.com/banux/librarian-agent/internal/instances"
	"github.com/banux/librarian-agent/internal/llm"
	"github.com/banux/librarian-agent/internal/updater"
)

// version is injected at build time via -ldflags "-X main.version=v0.2".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "run":
		runOnce(args)
	case "serve":
		serve(args)
	case "pair":
		runPair(args)
	case "unpair":
		runUnpair(args)
	case "update":
		update(args)
	case "version", "--version", "-v":
		fmt.Println(version)
	case "-h", "--help", "help":
		usage()
	default:
		// Pas de sous-commande explicite → mode "run" pour compat,
		// où tout est traité comme un prompt one-shot.
		runOnce(os.Args[1:])
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `librarian %s — agent autonome OPDS multi-instances

Sous-commandes :
  pair    [flags]               Associe ce librarian à un nxt-opds via un code
                                d'appairage one-time généré dans l'UI admin
  unpair  [flags]               Dissocie une instance des deux côtés
  run     [flags] [prompt...]   Lance l'agent une fois (mode CLI)
  serve   [flags]               Lance le daemon : ticker + webhook + /chat
  update  [flags]               Télécharge et remplace le binaire
  version                       Affiche la version installée

Variables d'env :
  LIBRARIAN_CONFIG              Chemin du YAML (défaut: ~/.config/librarian/config.yaml)
  LIBRARIAN_BACKEND             auto | ollama | anthropic
  LIBRARIAN_MODEL               Nom de modèle
  OLLAMA_HOST, ANTHROPIC_API_KEY

Exemples :
  librarian pair --nxt-opds https://books.jerinn.com --code K4Q9-PN2X \
                 --name jerinn --label "Bibliothèque Jerinn"
  librarian run --instance jerinn "Le Chevalier et la Phalène"     # un livre par titre
  librarian run --instance jerinn --prompt "Traite TOUS les livres non indexés un par un, sans limite. Termine par FIN." \
                --max-steps 1000                                    # maintenance totale
  librarian serve --listen :8080 --interval 6h \
                  --max-steps 500 --job-timeout 2h                 # daemon longue durée
  librarian update
`, version)
}

type commonFlags struct {
	configPath *string
	instance   *string
	backend    *string
	model      *string
	ollamaEP   *string
	quiet      *bool
}

func registerCommon(fs *flag.FlagSet) *commonFlags {
	return &commonFlags{
		configPath: fs.String("config", envOr("LIBRARIAN_CONFIG", ""), "Chemin du YAML de configuration (défaut: découverte automatique)"),
		instance:   fs.String("instance", "", "Slug de l'instance à utiliser (obligatoire si plusieurs instances configurées)"),
		backend:    fs.String("backend", envOr("LIBRARIAN_BACKEND", "auto"), "Backend LLM : auto | ollama | anthropic"),
		model:      fs.String("model", envOr("LIBRARIAN_MODEL", ""), "Modèle (qwen2.5:7b, claude-sonnet-4-6, …)"),
		ollamaEP:   fs.String("ollama-url", envOr("OLLAMA_HOST", "http://localhost:11434"), "Endpoint Ollama"),
		quiet:      fs.Bool("quiet", false, "Cache les appels d'outils"),
	}
}

func runOnce(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	c := registerCommon(fs)
	maxSteps := fs.Int("max-steps", 200, "Nombre maximum d'étapes (chaque livre = 5-10 étapes)")
	prompt := fs.String("prompt", "", "Prompt complet à exécuter verbatim (sans wrap titre). Préférer cette forme pour les maintenances longues, ex: --prompt \"search_books(not_indexed:true, limit:50) puis traite chaque livre selon le workflow complet. Termine par FIN.\"")
	_ = fs.Parse(args)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg, registry := loadConfigAndRegistry(c, *maxSteps, !*c.quiet)
	name := resolveInstance(cfg, *c.instance)

	entry, err := registry.Get(ctx, name)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	instruction := *prompt
	if instruction == "" {
		instruction = buildInstruction(fs.Args())
	}
	if err := entry.Agent.Run(ctx, instruction); err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		os.Exit(1)
	}
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	c := registerCommon(fs)
	listen := fs.String("listen", "", "Adresse d'écoute HTTP (override le YAML)")
	interval := fs.Duration("interval", 0, "Période entre deux maintenances (override le YAML)")
	batchLimit := fs.Int("batch-limit", 0, "Nombre de livres traités par tick (override le YAML)")
	prompt := fs.String("prompt", "", "Prompt verbatim remplaçant la maintenance batch par défaut. Utilisable pour une maintenance totale longue, ex: --prompt \"Traite TOUS les livres non indexés un par un, sans limite. Termine par FIN.\"")
	maxSteps := fs.Int("max-steps", 0, "Nombre maximum d'étapes par job (override le YAML, défaut 200 — chaque livre coûte 5-10 étapes)")
	jobTimeout := fs.Duration("job-timeout", 0, "Timeout par job (override le YAML, défaut 1h). Augmenter pour de la maintenance totale sur grosse bibliothèque.")
	_ = fs.Parse(args)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	maxS := *maxSteps
	if maxS == 0 {
		maxS = 200
	}
	cfg, registry := loadConfigAndRegistry(c, maxS, !*c.quiet)
	if *maxSteps == 0 && cfg.MaxSteps > 0 {
		maxS = cfg.MaxSteps
	}

	dcfg := daemon.Config{
		Listen:      pickStr(*listen, cfg.Listen, ":8080"),
		Interval:    pickDur(*interval, cfg.Interval, 6*time.Hour),
		BatchLimit:  pickInt(*batchLimit, cfg.BatchLimit, 10),
		BatchPrompt: *prompt,
		JobTimeout:  pickDur(*jobTimeout, 0, time.Hour),
	}
	// Compute the public URL once, using the actual listen we ended up with
	// so the derivation reflects any --listen override on the CLI.
	dcfg.PublicURL = config.ResolveLibrarianURL("", config.Config{
		PublicURL: cfg.PublicURL,
		Listen:    dcfg.Listen,
	})

	d := daemon.New(dcfg, registry)
	if err := d.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}

func update(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	repo := fs.String("repo", "banux/nxt-opds-librarian", "Dépôt GitHub owner/repo")
	force := fs.Bool("force", false, "Réinstaller même si déjà à jour")
	dryRun := fs.Bool("dry-run", false, "Ne télécharge rien, affiche juste la version cible")
	_ = fs.Parse(args)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	res, err := updater.Update(ctx, updater.Options{
		Repo:       *repo,
		CurrentTag: version,
		Force:      *force,
		DryRun:     *dryRun,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "update:", err)
		os.Exit(1)
	}
	switch {
	case *dryRun:
		fmt.Printf("dry-run: %s → %s (%s)\n", res.FromVersion, res.ToVersion, res.BinaryPath)
	case res.Updated:
		fmt.Printf("mis à jour : %s → %s (%s)\n", res.FromVersion, res.ToVersion, res.BinaryPath)
	default:
		fmt.Printf("déjà à jour (%s)\n", res.FromVersion)
	}
}

// loadConfigAndRegistry resolves the config path, parses the YAML, and builds
// a Registry around the chosen LLM provider. Exits the process on any error
// so callers can keep their happy path linear.
func loadConfigAndRegistry(c *commonFlags, maxSteps int, verbose bool) (config.Config, *instances.Registry) {
	path := *c.configPath
	if path == "" {
		path = config.FindConfigFile()
	}
	if path == "" {
		fmt.Fprint(os.Stderr, config.FormatMissingHelp())
		os.Exit(2)
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	provider := buildProvider(*c.backend, *c.model, *c.ollamaEP)
	return cfg, instances.New(cfg, provider, maxSteps, verbose)
}

func resolveInstance(cfg config.Config, flag string) string {
	if flag != "" {
		return flag
	}
	if cfg.DefaultInstance != "" {
		return cfg.DefaultInstance
	}
	if len(cfg.Instances) == 1 {
		return cfg.Instances[0].Name
	}
	fmt.Fprintln(os.Stderr, "plusieurs instances configurées — préciser --instance <slug>")
	for _, i := range cfg.Instances {
		fmt.Fprintf(os.Stderr, "  - %s (%s)\n", i.Name, i.Label)
	}
	os.Exit(2)
	return ""
}

func buildProvider(backend, model, ollamaEP string) llm.Provider {
	switch resolveBackend(backend) {
	case "anthropic":
		key := os.Getenv("ANTHROPIC_API_KEY")
		if key == "" {
			fmt.Fprintln(os.Stderr, "backend=anthropic mais ANTHROPIC_API_KEY non défini")
			os.Exit(2)
		}
		return llm.NewAnthropic(key, model)
	case "ollama":
		return llm.NewOllama(ollamaEP, defaultModel(model, "qwen2.5:7b"))
	default:
		fmt.Fprintf(os.Stderr, "backend inconnu: %s\n", backend)
		os.Exit(2)
		return nil
	}
}

func buildInstruction(args []string) string {
	if len(args) == 0 {
		return "Lance la maintenance batch : search_books(not_indexed: true, limit: 5) puis enrichis chaque livre selon les règles. Termine par 'FIN'."
	}
	title := strings.Join(args, " ")
	return fmt.Sprintf("Traite uniquement le livre dont le titre est ou contient : %q. search_books pour le trouver puis applique le workflow complet. Termine par 'FIN'.", title)
}

func resolveBackend(b string) string {
	if b != "auto" {
		return b
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return "anthropic"
	}
	return "ollama"
}

func defaultModel(provided, fallback string) string {
	if provided != "" {
		return provided
	}
	return fallback
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func pickStr(flag, yaml, def string) string {
	if flag != "" {
		return flag
	}
	if yaml != "" {
		return yaml
	}
	return def
}

func pickInt(flag, yaml, def int) int {
	if flag > 0 {
		return flag
	}
	if yaml > 0 {
		return yaml
	}
	return def
}

func pickDur(flag, yaml, def time.Duration) time.Duration {
	if flag > 0 {
		return flag
	}
	if yaml > 0 {
		return yaml
	}
	return def
}
