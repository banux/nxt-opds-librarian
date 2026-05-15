// Package updater downloads the latest GitHub release matching the running
// platform and atomically replaces the running binary.
package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const defaultRepo = "banux/nxt-opds-librarian"

type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

type Asset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"browser_download_url"`
	Size        int64  `json:"size"`
}

type Options struct {
	Repo       string // owner/repo — défaut banux/nxt-opds-librarian
	CurrentTag string // tag actuellement installé, "dev" si inconnu
	Force      bool   // forcer même si déjà à jour
	DryRun     bool   // n'écrit rien sur disque
}

type Result struct {
	Updated     bool
	FromVersion string
	ToVersion   string
	BinaryPath  string
}

func Update(ctx context.Context, opts Options) (*Result, error) {
	repo := opts.Repo
	if repo == "" {
		repo = defaultRepo
	}

	rel, err := fetchLatest(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("fetch latest release: %w", err)
	}

	res := &Result{FromVersion: opts.CurrentTag, ToVersion: rel.TagName}
	if !opts.Force && opts.CurrentTag != "" && opts.CurrentTag != "dev" && opts.CurrentTag == rel.TagName {
		return res, nil
	}

	wantName := fmt.Sprintf("librarian-%s-%s", runtime.GOOS, runtime.GOARCH)
	var asset *Asset
	for i, a := range rel.Assets {
		if a.Name == wantName {
			asset = &rel.Assets[i]
			break
		}
	}
	if asset == nil {
		return nil, fmt.Errorf("aucune release ne contient l'asset %q (release %s)", wantName, rel.TagName)
	}

	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locating self: %w", err)
	}
	exe, _ = filepath.EvalSymlinks(exe)
	res.BinaryPath = exe

	if opts.DryRun {
		return res, nil
	}

	if err := downloadAndReplace(ctx, asset, exe); err != nil {
		return nil, err
	}
	res.Updated = true
	return res, nil
}

func fetchLatest(ctx context.Context, repo string) (*Release, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("github %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	if rel.TagName == "" {
		return nil, errors.New("réponse GitHub vide (pas de tag_name)")
	}
	return &rel, nil
}

func downloadAndReplace(ctx context.Context, a *Asset, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.DownloadURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: http %d", a.Name, resp.StatusCode)
	}

	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, ".librarian-update-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		tmp.Close()
		if cleanup {
			os.Remove(tmpPath)
		}
	}()

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		return fmt.Errorf("écriture binaire: %w", err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	// On a few platforms (Linux included) you can rename over a running
	// executable — the kernel keeps the old inode alive for current
	// processes. Try that first; fall back to backup-and-replace otherwise.
	if err := os.Rename(tmpPath, dest); err == nil {
		cleanup = false
		return nil
	}
	backup := dest + ".old"
	_ = os.Remove(backup)
	if err := os.Rename(dest, backup); err != nil {
		return fmt.Errorf("backup ancien binaire: %w", err)
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		_ = os.Rename(backup, dest)
		return fmt.Errorf("rename: %w", err)
	}
	cleanup = false
	return nil
}
