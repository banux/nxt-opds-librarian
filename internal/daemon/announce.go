package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/banux/librarian-agent/internal/config"
)

// announceAll walks every configured instance and tells the matching nxt-opds
// what URL it should call back on. Best-effort: failures are logged but do
// not block daemon startup, so an unreachable nxt-opds cannot prevent the
// other instances from coming up.
//
// This makes a serve restart with a fresh public_url self-heal — without
// it, the operator would have to re-pair every time the librarian moved.
func (d *Daemon) announceAll(ctx context.Context) {
	if d.cfg.PublicURL == "" {
		log.Printf("[announce] aucun public_url calculé — la mise à jour automatique de l'URL côté nxt-opds est désactivée")
		return
	}
	for _, name := range d.registry.Names() {
		insts := d.registry.List()
		var inst *config.Instance
		for i := range insts {
			if insts[i].Name == name {
				inst = &insts[i]
				break
			}
		}
		if inst == nil || inst.ChatSecret == "" {
			continue
		}
		base := config.NxtOPDSBaseURL(inst.MCPURL)
		if base == "" {
			log.Printf("[announce %s] mcp_url invalide — saut", name)
			continue
		}
		go func(name, base, chat string) {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if err := postAnnounce(ctx, base, chat, d.cfg.PublicURL); err != nil {
				log.Printf("[announce %s] %v", name, err)
				return
			}
			log.Printf("[announce %s] librarian_url=%s OK", name, d.cfg.PublicURL)
		}(name, base, inst.ChatSecret)
	}
}

func postAnnounce(ctx context.Context, base, chatSecret, librarianURL string) error {
	body, _ := json.Marshal(map[string]string{"librarian_url": librarianURL})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/librarian/announce", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Librarian-Chat-Secret", chatSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// nxt-opds is not yet running the announce-aware build: silently treat
		// this as a no-op so a mixed-version cluster keeps working. The
		// operator will see one log line per restart pointing to the missing
		// endpoint.
		return fmt.Errorf("nxt-opds ne supporte pas /api/librarian/announce (404) — mettre à jour nxt-opds")
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
		return fmt.Errorf("http %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
