package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/banux/librarian-agent/internal/config"
)

// pairResponse is the JSON nxt-opds returns from POST /api/librarian/pair
// (and POST /api/librarian/rotate). chat_secret / webhook_secret are stored
// in the librarian YAML; the librarian never displays them on screen.
type pairResponse struct {
	MCPURL        string `json:"mcp_url"`
	MCPToken      string `json:"mcp_token"`
	ChatSecret    string `json:"chat_secret"`
	WebhookSecret string `json:"webhook_secret"`
	Instance      string `json:"instance"`
	Label         string `json:"label"`
}

func runPair(args []string) {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	configPath := fs.String("config", envOr("LIBRARIAN_CONFIG", ""), "Chemin du YAML (défaut: découverte automatique, sinon ~/.config/librarian/config.yaml)")
	nxtOPDS := fs.String("nxt-opds", "", "URL de base du nxt-opds (ex: https://books.jerinn.com)")
	code := fs.String("code", "", "Code d'appairage one-time généré dans l'UI admin nxt-opds")
	name := fs.String("name", "", "Slug local de l'instance (ex: jerinn)")
	label := fs.String("label", "", "Étiquette humaine (ex: \"Bibliothèque Jerinn\")")
	librarianURL := fs.String("librarian-url", "", "URL publique du librarian que nxt-opds doit appeler (chat + webhooks). Défaut : public_url du YAML, sinon dérivé du champ listen.")
	rotate := fs.Bool("rotate", false, "Renouvelle les secrets pour une instance déjà associée (n'exige pas de code)")
	force := fs.Bool("force", false, "Force le pairing même si une association existe déjà côté nxt-opds")
	_ = fs.Parse(args)

	if *nxtOPDS == "" {
		fmt.Fprintln(os.Stderr, "pair: --nxt-opds est requis")
		os.Exit(2)
	}
	if !*rotate && *code == "" {
		fmt.Fprintln(os.Stderr, "pair: --code est requis (générer dans l'UI admin nxt-opds → Associer un librarian)")
		os.Exit(2)
	}
	if *name == "" {
		fmt.Fprintln(os.Stderr, "pair: --name est requis")
		os.Exit(2)
	}
	warnInsecure(*nxtOPDS)

	path := *configPath
	if path == "" {
		path = config.FindConfigFile()
	}
	if path == "" {
		path = config.DefaultPath()
	}

	cfg := loadOrInit(path)
	resolvedURL := config.ResolveLibrarianURL(*librarianURL, cfg)
	if resolvedURL == "" {
		fmt.Fprintln(os.Stderr, "pair: impossible de déterminer --librarian-url. Préciser le flag ou ajouter `public_url:` dans le YAML.")
		os.Exit(2)
	}
	warnLocalhost(resolvedURL)

	// Rotation flow: reuse the current chat_secret as authentication.
	if *rotate {
		inst, ok := cfg.Find(*name)
		if !ok {
			fmt.Fprintf(os.Stderr, "pair --rotate: instance %q introuvable dans %s\n", *name, path)
			os.Exit(2)
		}
		resp, err := postRotate(context.Background(), *nxtOPDS, inst.ChatSecret)
		if err != nil {
			fmt.Fprintln(os.Stderr, "pair --rotate:", err)
			os.Exit(1)
		}
		applyAndSave(&cfg, *name, *label, resp)
		fmt.Printf("✓ Rotation réussie pour l'instance « %s »\n", *name)
		return
	}

	// Initial pairing flow: send the one-time code, receive the secrets.
	resp, err := postPair(context.Background(), *nxtOPDS, pairRequest{
		Code:         *code,
		LibrarianURL: resolvedURL,
		Instance:     *name,
		Label:        *label,
		Force:        *force,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "pair:", err)
		os.Exit(1)
	}
	applyAndSave(&cfg, *name, *label, resp)
	fmt.Printf("✓ Pairing réussi avec %s\n", *nxtOPDS)
	fmt.Printf("✓ nxt-opds appellera ce librarian sur : %s\n", resolvedURL)
	fmt.Printf("✓ Instance « %s » écrite dans %s\n", *name, path)
	fmt.Println("→ Redémarrer le librarian (serve) pour activer l'instance. La chat box nxt-opds est active immédiatement.")
}

func runUnpair(args []string) {
	fs := flag.NewFlagSet("unpair", flag.ExitOnError)
	configPath := fs.String("config", envOr("LIBRARIAN_CONFIG", ""), "Chemin du YAML")
	nxtOPDS := fs.String("nxt-opds", "", "URL de base du nxt-opds (best-effort, ignore les échecs)")
	name := fs.String("name", "", "Slug de l'instance à dissocier")
	_ = fs.Parse(args)

	if *name == "" {
		fmt.Fprintln(os.Stderr, "unpair: --name est requis")
		os.Exit(2)
	}
	path := *configPath
	if path == "" {
		path = config.FindConfigFile()
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, "unpair: aucun YAML trouvé — rien à faire côté librarian.")
		return
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unpair:", err)
		os.Exit(1)
	}
	inst, ok := cfg.Find(*name)
	if !ok {
		fmt.Fprintf(os.Stderr, "unpair: instance %q introuvable\n", *name)
		os.Exit(2)
	}

	if *nxtOPDS != "" {
		if err := deleteAssociation(context.Background(), *nxtOPDS, inst.ChatSecret); err != nil {
			fmt.Fprintln(os.Stderr, "unpair: avertissement côté nxt-opds:", err)
		} else {
			fmt.Printf("✓ Association supprimée sur %s\n", *nxtOPDS)
		}
	}

	cfg.Remove(*name)
	if err := config.Save(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "unpair:", err)
		os.Exit(1)
	}
	fmt.Printf("✓ Instance « %s » retirée de %s\n", *name, path)
}

// --- HTTP plumbing ----------------------------------------------------------

type pairRequest struct {
	Code         string `json:"code"`
	LibrarianURL string `json:"librarian_url"`
	Instance     string `json:"instance"`
	Label        string `json:"label,omitempty"`
	Force        bool   `json:"force,omitempty"`
}

func postPair(ctx context.Context, base string, body pairRequest) (pairResponse, error) {
	u, err := joinURL(base, "/api/librarian/pair")
	if err != nil {
		return pairResponse{}, err
	}
	return doJSON(ctx, http.MethodPost, u, body, nil)
}

func postRotate(ctx context.Context, base, chatSecret string) (pairResponse, error) {
	u, err := joinURL(base, "/api/librarian/rotate")
	if err != nil {
		return pairResponse{}, err
	}
	return doJSON(ctx, http.MethodPost, u, struct{}{}, map[string]string{
		"X-Librarian-Chat-Secret": chatSecret,
	})
}

func deleteAssociation(ctx context.Context, base, chatSecret string) error {
	u, err := joinURL(base, "/api/librarian/forget")
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Librarian-Chat-Secret", chatSecret)
	resp, err := httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
		return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

func doJSON(ctx context.Context, method, u string, body any, extraHeaders map[string]string) (pairResponse, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return pairResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(buf))
	if err != nil {
		return pairResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return pairResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
		return pairResponse{}, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out pairResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return pairResponse{}, fmt.Errorf("invalid response: %w", err)
	}
	if out.MCPURL == "" || out.MCPToken == "" || out.ChatSecret == "" {
		return pairResponse{}, fmt.Errorf("invalid response: missing fields")
	}
	return out, nil
}

func httpClient() *http.Client {
	return &http.Client{Timeout: 20 * time.Second}
}

func joinURL(base, path string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("invalid --nxt-opds URL: %w", err)
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	return u.String(), nil
}

func warnInsecure(base string) {
	u, err := url.Parse(base)
	if err != nil {
		return
	}
	if u.Scheme == "https" {
		return
	}
	host := u.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return
	}
	fmt.Fprintf(os.Stderr, "⚠ %s utilise HTTP en clair — les secrets transitent en clair sur le réseau. Préférer HTTPS.\n", base)
}

func loadOrInit(path string) config.Config {
	if _, err := os.Stat(path); err != nil {
		cfg := config.Default()
		cfg.Path = path
		return cfg
	}
	cfg, err := config.Load(path)
	if err != nil {
		// Validation may fail because the file has no instances yet — in that
		// case we still want pair to succeed, so fall back to a fresh config.
		if strings.Contains(err.Error(), "aucune instance configurée") {
			cfg = config.Default()
			cfg.Path = path
			return cfg
		}
		fmt.Fprintln(os.Stderr, "pair:", err)
		os.Exit(1)
	}
	return cfg
}

func applyAndSave(cfg *config.Config, name, label string, resp pairResponse) {
	inst := config.Instance{
		Name:          name,
		MCPURL:        resp.MCPURL,
		MCPToken:      resp.MCPToken,
		ChatSecret:    resp.ChatSecret,
		WebhookSecret: resp.WebhookSecret,
		Label:         pickLabel(label, resp.Label),
	}
	cfg.Upsert(inst)
	if err := config.Save(*cfg); err != nil {
		fmt.Fprintln(os.Stderr, "pair: écriture du YAML:", err)
		os.Exit(1)
	}
}

func pickLabel(flagLabel, respLabel string) string {
	if flagLabel != "" {
		return flagLabel
	}
	return respLabel
}

func warnLocalhost(u string) {
	parsed, err := url.Parse(u)
	if err != nil {
		return
	}
	h := parsed.Hostname()
	if h == "localhost" || h == "127.0.0.1" || h == "::1" {
		fmt.Fprintf(os.Stderr, "⚠ --librarian-url = %s — nxt-opds doit pouvoir résoudre cette URL.\n", u)
		fmt.Fprintln(os.Stderr, "  Si nxt-opds tourne sur un autre hôte ou dans Docker, préciser --librarian-url ou public_url: dans le YAML.")
	}
}
