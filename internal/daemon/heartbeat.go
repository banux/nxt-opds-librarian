package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/banux/librarian-agent/internal/config"
)

// HeartbeatInterval is how often each instance is pinged. Picked so the
// admin UI's "connecté il y a Ns" readout stays under one minute stale
// while keeping the wire chatter trivial.
const HeartbeatInterval = 60 * time.Second

// startHeartbeats fires one goroutine per configured instance. Each loop
// pings the matching nxt-opds at HeartbeatInterval and stops cleanly on
// ctx cancellation. Failures (transport, 4xx, 5xx) are logged but never
// terminate the loop — a flapping nxt-opds must not silence the heartbeat
// for the recovery interval. 404 (older nxt-opds without /heartbeat) is
// logged once and then suppressed for that instance to keep the journal
// clean during mixed-version operation.
func (d *Daemon) startHeartbeats(ctx context.Context) {
	for _, inst := range d.registry.List() {
		if inst.ChatSecret == "" {
			continue
		}
		base := config.NxtOPDSBaseURL(inst.MCPURL)
		if base == "" {
			continue
		}
		d.wg.Add(1)
		go d.heartbeatLoop(ctx, inst.Name, base, inst.ChatSecret)
	}
}

func (d *Daemon) heartbeatLoop(ctx context.Context, name, base, chatSecret string) {
	defer d.wg.Done()

	t := time.NewTicker(HeartbeatInterval)
	defer t.Stop()

	var notSupported atomic.Bool

	tick := func() {
		if notSupported.Load() {
			return
		}
		reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		status, err := postHeartbeat(reqCtx, base, chatSecret)
		switch {
		case err != nil:
			log.Printf("[heartbeat %s] %v", name, err)
		case status == http.StatusNotFound:
			log.Printf("[heartbeat %s] nxt-opds ne supporte pas /api/librarian/heartbeat — désactivation jusqu'au prochain redémarrage", name)
			notSupported.Store(true)
		case status >= 400:
			log.Printf("[heartbeat %s] http %d", name, status)
		}
	}

	// One immediate hit so the admin UI shows "connecté à l'instant" right
	// after `librarian serve` is up, then settle into the interval cadence.
	tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// postHeartbeat fires one request and returns the HTTP status code so the
// caller can branch on 404 vs generic 4xx/5xx. err is non-nil only on
// transport failures (DNS, TCP, timeout, malformed response).
func postHeartbeat(ctx context.Context, base, chatSecret string) (int, error) {
	payload, _ := json.Marshal(map[string]string{"agent": "librarian"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/librarian/heartbeat", bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Librarian-Chat-Secret", chatSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// Drain so the connection can be reused by keep-alive.
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound {
		return resp.StatusCode, fmt.Errorf("http %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}
