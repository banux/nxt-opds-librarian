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

	"github.com/banux/librarian-agent/internal/agent"
	"github.com/banux/librarian-agent/internal/daemon"
	"github.com/banux/librarian-agent/internal/llm"
	"github.com/banux/librarian-agent/internal/mcp"
)

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
	case "-h", "--help", "help":
		usage()
	default:
		// Pas de sous-commande explicite → mode "run" pour compat,
		// où tout est traité comme un prompt one-shot.
		runOnce(os.Args[1:])
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `librarian — agent autonome OPDS

Sous-commandes :
  run    [flags] [prompt...]   Lance l'agent une fois (mode CLI)
  serve  [flags]               Lance le daemon : ticker + webhook + /trigger

Variables d'env communes :
  OPDS_MCP_URL, OPDS_MCP_TOKEN
  LIBRARIAN_BACKEND, LIBRARIAN_MODEL
  OLLAMA_HOST, ANTHROPIC_API_KEY

Exemples :
  librarian run "Le Chevalier et la Phalène"
  librarian serve --listen :8080 --interval 6h --prompt "..."
  curl -X POST localhost:8080/trigger -d '{"prompt":"Traite La Boussole..."}'`)
}

type commonFlags struct {
	backend  *string
	model    *string
	ollamaEP *string
	mcpURL   *string
	mcpToken *string
	quiet    *bool
}

func registerCommon(fs *flag.FlagSet) *commonFlags {
	return &commonFlags{
		backend:  fs.String("backend", envOr("LIBRARIAN_BACKEND", "auto"), "Backend LLM : auto | ollama | anthropic"),
		model:    fs.String("model", envOr("LIBRARIAN_MODEL", ""), "Modèle (qwen2.5:7b, claude-sonnet-4-6, …)"),
		ollamaEP: fs.String("ollama-url", envOr("OLLAMA_HOST", "http://localhost:11434"), "Endpoint Ollama"),
		mcpURL:   fs.String("mcp-url", envOr("OPDS_MCP_URL", "https://books.jerinn.com/mcp"), "Endpoint MCP OPDS"),
		mcpToken: fs.String("mcp-token", os.Getenv("OPDS_MCP_TOKEN"), "Bearer token MCP OPDS"),
		quiet:    fs.Bool("quiet", false, "Cache les appels d'outils"),
	}
}

func runOnce(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	c := registerCommon(fs)
	maxSteps := fs.Int("max-steps", 60, "Nombre maximum d'étapes")
	prompt := fs.String("prompt", "", "Prompt complet (sinon construit depuis les arguments positionnels)")
	_ = fs.Parse(args)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	a := buildAgent(ctx, c, *maxSteps, *c.quiet)

	instruction := *prompt
	if instruction == "" {
		instruction = buildInstruction(fs.Args())
	}
	if err := a.Run(ctx, instruction); err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		os.Exit(1)
	}
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	c := registerCommon(fs)
	listen := fs.String("listen", ":8080", "Adresse d'écoute HTTP")
	webhookPath := fs.String("webhook-path", "/webhook/book-added", "Chemin du webhook")
	webhookSecret := fs.String("webhook-secret", os.Getenv("LIBRARIAN_WEBHOOK_SECRET"), "Secret HMAC pour valider X-Signature (optionnel)")
	interval := fs.Duration("interval", 6*time.Hour, "Période entre deux maintenances")
	batchLimit := fs.Int("batch-limit", 10, "Nombre de livres traités par tick")
	prompt := fs.String("prompt", "", "Prompt remplaçant la maintenance batch par défaut (utilisé par le ticker et /trigger sans body)")
	maxSteps := fs.Int("max-steps", 60, "Nombre maximum d'étapes par job")
	_ = fs.Parse(args)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	a := buildAgent(ctx, c, *maxSteps, *c.quiet)

	d := daemon.New(daemon.Config{
		Listen:        *listen,
		WebhookPath:   *webhookPath,
		WebhookSecret: *webhookSecret,
		Interval:      *interval,
		BatchLimit:    *batchLimit,
		BatchPrompt:   *prompt,
	}, a)

	if err := d.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}

func buildAgent(ctx context.Context, c *commonFlags, maxSteps int, quiet bool) *agent.Agent {
	if *c.mcpToken == "" {
		fmt.Fprintln(os.Stderr, "OPDS_MCP_TOKEN non défini (flag --mcp-token ou variable d'env)")
		os.Exit(2)
	}

	var provider llm.Provider
	switch resolveBackend(*c.backend) {
	case "anthropic":
		key := os.Getenv("ANTHROPIC_API_KEY")
		if key == "" {
			fmt.Fprintln(os.Stderr, "backend=anthropic mais ANTHROPIC_API_KEY non défini")
			os.Exit(2)
		}
		provider = llm.NewAnthropic(key, *c.model)
	case "ollama":
		provider = llm.NewOllama(*c.ollamaEP, defaultModel(*c.model, "qwen2.5:7b"))
	default:
		fmt.Fprintf(os.Stderr, "backend inconnu: %s\n", *c.backend)
		os.Exit(2)
	}

	mc := mcp.New(*c.mcpURL, *c.mcpToken)
	if err := mc.Initialize(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "init MCP:", err)
		os.Exit(1)
	}

	a := agent.New(provider, mc)
	a.MaxSteps = maxSteps
	a.Verbose = !quiet
	if err := a.Init(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "init agent:", err)
		os.Exit(1)
	}
	return a
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
