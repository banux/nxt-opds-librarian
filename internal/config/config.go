// Package config loads the librarian YAML configuration that lists the
// nxt-opds instances this binary manages. A single librarian process can
// drive several catalogs; each entry under `instances:` carries the MCP
// endpoint and the per-instance secrets used by the chat box and webhooks.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the on-disk shape of ~/.config/librarian/config.yaml.
type Config struct {
	Listen          string        `yaml:"listen"`
	IntervalRaw     string        `yaml:"interval"`
	Interval        time.Duration `yaml:"-"`
	BatchLimit      int           `yaml:"batch_limit"`
	MaxSteps        int           `yaml:"max_steps"`
	Backend         string        `yaml:"backend"`
	Model           string        `yaml:"model"`
	OllamaURL       string        `yaml:"ollama_url"`
	DefaultInstance string        `yaml:"default_instance"`
	Instances       []Instance    `yaml:"instances"`

	// Path is the absolute path the config was loaded from. Used by `librarian
	// pair` to write the file back after upsert. Not serialised.
	Path string `yaml:"-"`
}

// Instance describes one nxt-opds catalog the librarian can talk to.
type Instance struct {
	Name          string `yaml:"name"`
	MCPURL        string `yaml:"mcp_url"`
	MCPToken      string `yaml:"mcp_token"`
	ChatSecret    string `yaml:"chat_secret"`
	WebhookSecret string `yaml:"webhook_secret"`
	Label         string `yaml:"label"`
	Locale        string `yaml:"locale"`
}

// Default returns a Config populated with sensible defaults.
func Default() Config {
	return Config{
		Listen:      ":8080",
		IntervalRaw: "6h",
		Interval:    6 * time.Hour,
		BatchLimit:  10,
		MaxSteps:    60,
		Backend:     "auto",
		OllamaURL:   "http://localhost:11434",
	}
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Load reads and validates the config at path. It expands ${VAR} references
// against the process environment, parses the interval string, and ensures
// every instance has a unique slug plus the required MCP fields.
func Load(path string) (Config, error) {
	cfg := Default()
	cfg.Path = path

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config %q: %w", path, err)
	}
	expanded := os.Expand(string(data), func(k string) string {
		return os.Getenv(k)
	})
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %q: %w", path, err)
	}

	if cfg.IntervalRaw != "" {
		d, err := time.ParseDuration(cfg.IntervalRaw)
		if err != nil {
			return cfg, fmt.Errorf("interval %q: %w", cfg.IntervalRaw, err)
		}
		cfg.Interval = d
	}

	if len(cfg.Instances) == 0 {
		return cfg, fmt.Errorf("config %q: aucune instance configurée. Utilise `librarian pair` pour en ajouter une", path)
	}

	seen := map[string]bool{}
	for i, inst := range cfg.Instances {
		if !slugRe.MatchString(inst.Name) {
			return cfg, fmt.Errorf("instance %d: name %q invalide (slug attendu : [a-z0-9-]+)", i, inst.Name)
		}
		if seen[inst.Name] {
			return cfg, fmt.Errorf("instance %q: nom en double", inst.Name)
		}
		seen[inst.Name] = true
		if inst.MCPURL == "" {
			return cfg, fmt.Errorf("instance %q: mcp_url manquant", inst.Name)
		}
		if inst.MCPToken == "" {
			return cfg, fmt.Errorf("instance %q: mcp_token manquant", inst.Name)
		}
	}
	if cfg.DefaultInstance != "" && !seen[cfg.DefaultInstance] {
		return cfg, fmt.Errorf("default_instance %q ne correspond à aucune instance", cfg.DefaultInstance)
	}

	return cfg, nil
}

// FindConfigFile returns the path to the first config file found.
// Search order:
//  1. LIBRARIAN_CONFIG environment variable
//  2. ./librarian.yaml (current working directory)
//  3. ~/.config/librarian/config.yaml
func FindConfigFile() string {
	if p := os.Getenv("LIBRARIAN_CONFIG"); p != "" {
		return p
	}
	if _, err := os.Stat("librarian.yaml"); err == nil {
		return "librarian.yaml"
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".config", "librarian", "config.yaml")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// DefaultPath returns the canonical write location used by `librarian pair`
// when no --config flag was provided.
func DefaultPath() string {
	if p := os.Getenv("LIBRARIAN_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "librarian.yaml"
	}
	return filepath.Join(home, ".config", "librarian", "config.yaml")
}

// Save writes cfg back to its Path (or DefaultPath() if empty) with 0600
// permissions and an automatic .bak alongside any existing file. Path is
// MkdirAll'd if needed.
func Save(cfg Config) error {
	path := cfg.Path
	if path == "" {
		path = DefaultPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %q: %w", filepath.Dir(path), err)
	}

	out, err := yaml.Marshal(&cfg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	if _, err := os.Stat(path); err == nil {
		if err := os.WriteFile(path+".bak", mustRead(path), 0o600); err != nil {
			return fmt.Errorf("backup %q: %w", path+".bak", err)
		}
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("write %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %q: %w", tmp, err)
	}
	return nil
}

func mustRead(path string) []byte {
	b, _ := os.ReadFile(path)
	return b
}

// Upsert inserts or replaces an entry by Name. The pair command uses this to
// merge what the nxt-opds /api/librarian/pair endpoint returned.
func (c *Config) Upsert(inst Instance) {
	for i, existing := range c.Instances {
		if existing.Name == inst.Name {
			merged := merge(existing, inst)
			c.Instances[i] = merged
			return
		}
	}
	c.Instances = append(c.Instances, inst)
}

// Remove deletes the instance with the given name. Returns true if removed.
func (c *Config) Remove(name string) bool {
	for i, existing := range c.Instances {
		if existing.Name == name {
			c.Instances = append(c.Instances[:i], c.Instances[i+1:]...)
			if c.DefaultInstance == name {
				c.DefaultInstance = ""
			}
			return true
		}
	}
	return false
}

// merge keeps existing values when the new entry leaves a field empty. Lets
// `pair --rotate` swap the secrets in place without erasing the label/locale.
func merge(old, new Instance) Instance {
	if new.Name == "" {
		new.Name = old.Name
	}
	if new.MCPURL == "" {
		new.MCPURL = old.MCPURL
	}
	if new.MCPToken == "" {
		new.MCPToken = old.MCPToken
	}
	if new.ChatSecret == "" {
		new.ChatSecret = old.ChatSecret
	}
	if new.WebhookSecret == "" {
		new.WebhookSecret = old.WebhookSecret
	}
	if new.Label == "" {
		new.Label = old.Label
	}
	if new.Locale == "" {
		new.Locale = old.Locale
	}
	return new
}

// Find returns the instance with the given name, or false.
func (c *Config) Find(name string) (Instance, bool) {
	for _, inst := range c.Instances {
		if inst.Name == name {
			return inst, true
		}
	}
	return Instance{}, false
}

// Strings used by callers to format helpful errors with the expected layout.
const ExampleYAML = `# librarian config
listen: ":8080"
interval: "6h"

instances:
  - name: "default"
    mcp_url: "https://books.example.com/mcp"
    mcp_token: "<opds_token>"
    chat_secret: "<64-hex>"
    webhook_secret: "<64-hex>"
    label: "Ma bibliothèque"
`

// FormatMissingHelp builds the human-readable hint shown when no config file
// is found.
func FormatMissingHelp() string {
	var b strings.Builder
	b.WriteString("librarian: aucune configuration trouvée.\n")
	b.WriteString("Crée une association via :\n")
	b.WriteString("  librarian pair --nxt-opds <URL> --code <CODE> --name <slug> --label \"...\"\n")
	b.WriteString("Le code se génère depuis l'UI admin de nxt-opds (Associer un librarian).\n")
	b.WriteString("La config sera écrite dans ")
	b.WriteString(DefaultPath())
	b.WriteString(".\n")
	return b.String()
}
