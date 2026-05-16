package main

import (
	"reflect"
	"testing"
)

func TestIDLineRegexExtractsIDs(t *testing.T) {
	// Sample matches the formatBook() output in nxt-opds/internal/mcp/server.go.
	sample := `Trouvé 689 livre(s) au total (page : 1/14)

1. **Le goût de la victoire**
   ID: 560c157476ab3411
   Auteur(s): Jaworski Jean-Philippe
   Série: Récits du Vieux Royaume #1
   Éditeur: Folio SF

2. **Même pas mort**
   ID: 175094c512279242
   Auteur(s): Jaworski Jean-Philippe
`
	got := idLineRe.FindAllStringSubmatch(sample, -1)
	want := [][]string{
		{"   ID: 560c157476ab3411", "560c157476ab3411"},
		{"   ID: 175094c512279242", "175094c512279242"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("regex matched %v, want %v", got, want)
	}
	if m := totalRe.FindStringSubmatch(sample); len(m) < 2 || m[1] != "689" {
		t.Errorf("total regex got %v, want 689", m)
	}
}

func TestParseFilterValue(t *testing.T) {
	cases := map[string]any{
		"true":            true,
		"false":           false,
		"16":              16,
		"0":               0,
		"-1":              -1,
		"Dark Fantasy":    "Dark Fantasy",
		"Robin Hobb":      "Robin Hobb",
		"":                "",
	}
	for in, want := range cases {
		if got := parseFilterValue(in); got != want {
			t.Errorf("parseFilterValue(%q) = %v (%T), want %v (%T)", in, got, got, want, want)
		}
	}
}

func TestFilterListSet(t *testing.T) {
	var fl filterList
	if err := fl.Set("age_rating_min=16"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := fl.Set("tag=Dark Romance"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := fl.Set("not_indexed=true"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if len(fl) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(fl))
	}
	if fl[0].key != "age_rating_min" || fl[0].value != 16 {
		t.Errorf("entry 0 unexpected: %+v", fl[0])
	}
	if fl[1].key != "tag" || fl[1].value != "Dark Romance" {
		t.Errorf("entry 1 unexpected: %+v", fl[1])
	}
	if fl[2].key != "not_indexed" || fl[2].value != true {
		t.Errorf("entry 2 unexpected: %+v", fl[2])
	}
}

func TestFilterListSetRejectsMalformed(t *testing.T) {
	var fl filterList
	if err := fl.Set("noequal"); err == nil {
		t.Error("expected error on missing =")
	}
	if err := fl.Set("=value"); err == nil {
		t.Error("expected error on empty key")
	}
}

func TestIsRateLimitError(t *testing.T) {
	cases := map[string]bool{
		// Real provider strings
		"anthropic 429: rate_limit_error":                         true,
		"anthropic 529: overloaded_error":                         true,
		"ollama 429: Too Many Requests":                           true,
		"ollama 503: model unavailable":                           true,
		"rate limit exceeded":                                     true,
		"You exceeded your current quota":                         true,
		// Transient network classes worth retrying
		"Post … i/o timeout":                                      true,
		"read tcp …: connection reset by peer":                    true,
		"Post … EOF":                                              true,
		"dial tcp 1.2.3.4: connect: connection refused":           true,
		// Non-quota errors that should NOT retry
		"book not found":                                          false,
		"invalid json":                                            false,
		"max steps (60) reached":                                  false,
		"context canceled":                                        false,
	}
	for msg, want := range cases {
		got := isRateLimitError(errString(msg))
		if got != want {
			t.Errorf("isRateLimitError(%q) = %v, want %v", msg, got, want)
		}
	}
	if isRateLimitError(nil) {
		t.Error("nil error should not be a rate-limit")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
