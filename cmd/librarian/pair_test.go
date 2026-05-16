package main

import (
	"testing"

	"github.com/banux/librarian-agent/internal/config"
)

func TestResolveLibrarianURL(t *testing.T) {
	cases := []struct {
		name     string
		flag     string
		listen   string
		public   string
		want     string
	}{
		{"flag wins", "https://lib.example/", ":8080", "http://x:1", "https://lib.example"},
		{"public_url fallback", "", ":8080", "http://librarian:9090", "http://librarian:9090"},
		{"derive from listen :8080", "", ":8080", "", "http://localhost:8080"},
		{"derive from listen :9090", "", ":9090", "", "http://localhost:9090"},
		{"derive replaces 0.0.0.0", "", "0.0.0.0:8080", "", "http://localhost:8080"},
		{"derive keeps explicit host", "", "internal.lan:7000", "", "http://internal.lan:7000"},
		{"nothing → empty", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{Listen: tc.listen, PublicURL: tc.public}
			if got := config.ResolveLibrarianURL(tc.flag, cfg); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
