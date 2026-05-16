package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadExpandsEnv(t *testing.T) {
	t.Setenv("OPDS_TOKEN_TEST", "secret-token")
	path := writeFile(t, `
instances:
  - name: "jerinn"
    mcp_url: "https://books.example.com/mcp"
    mcp_token: "${OPDS_TOKEN_TEST}"
    chat_secret: "abc"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Instances[0].MCPToken; got != "secret-token" {
		t.Errorf("MCPToken = %q, want secret-token", got)
	}
}

func TestLoadRejectsDuplicateSlug(t *testing.T) {
	path := writeFile(t, `
instances:
  - {name: "dup", mcp_url: "x", mcp_token: "y"}
  - {name: "dup", mcp_url: "x", mcp_token: "y"}
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected duplicate slug error")
	}
}

func TestLoadRejectsBadSlug(t *testing.T) {
	path := writeFile(t, `
instances:
  - {name: "Has Space", mcp_url: "x", mcp_token: "y"}
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected slug validation error")
	}
}

func TestLoadRejectsEmpty(t *testing.T) {
	path := writeFile(t, ``)
	if _, err := Load(path); err == nil {
		t.Fatal("expected empty-instances error")
	}
}

func TestUpsertAndSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.yaml")
	cfg := Default()
	cfg.Path = path
	cfg.Upsert(Instance{
		Name: "demo", MCPURL: "http://x/mcp", MCPToken: "tok",
		ChatSecret: "cs", WebhookSecret: "ws", Label: "Demo",
	})
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Instances) != 1 || loaded.Instances[0].Name != "demo" {
		t.Fatalf("unexpected loaded: %#v", loaded.Instances)
	}
	if loaded.Instances[0].ChatSecret != "cs" {
		t.Errorf("ChatSecret lost in roundtrip: %q", loaded.Instances[0].ChatSecret)
	}
}

func TestUpsertRotatesSecrets(t *testing.T) {
	cfg := Default()
	cfg.Instances = []Instance{{
		Name: "demo", MCPURL: "http://x/mcp", MCPToken: "old-tok",
		ChatSecret: "old-cs", Label: "Old Label",
	}}
	cfg.Upsert(Instance{
		Name: "demo", ChatSecret: "new-cs", WebhookSecret: "new-ws",
	})
	got := cfg.Instances[0]
	if got.ChatSecret != "new-cs" {
		t.Errorf("ChatSecret = %q, want new-cs", got.ChatSecret)
	}
	if got.WebhookSecret != "new-ws" {
		t.Errorf("WebhookSecret = %q, want new-ws", got.WebhookSecret)
	}
	if got.MCPToken != "old-tok" {
		t.Errorf("MCPToken lost: %q", got.MCPToken)
	}
	if got.Label != "Old Label" {
		t.Errorf("Label lost: %q", got.Label)
	}
}

func TestNxtOPDSBaseURL(t *testing.T) {
	cases := map[string]string{
		"https://books.jerinn.com/mcp":     "https://books.jerinn.com",
		"https://books.jerinn.com/mcp/":    "https://books.jerinn.com",
		"http://localhost:8080/mcp":        "http://localhost:8080",
		"http://localhost:8080/mcp?token=x": "http://localhost:8080",
		"http://localhost:8080/":           "http://localhost:8080",
		"":                                 "",
	}
	for in, want := range cases {
		if got := NxtOPDSBaseURL(in); got != want {
			t.Errorf("NxtOPDSBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRemove(t *testing.T) {
	cfg := Config{
		Instances: []Instance{
			{Name: "a"}, {Name: "b"},
		},
		DefaultInstance: "a",
	}
	if !cfg.Remove("a") {
		t.Fatal("Remove returned false")
	}
	if len(cfg.Instances) != 1 || cfg.Instances[0].Name != "b" {
		t.Fatalf("unexpected after remove: %#v", cfg.Instances)
	}
	if cfg.DefaultInstance != "" {
		t.Errorf("DefaultInstance not cleared: %q", cfg.DefaultInstance)
	}
}
