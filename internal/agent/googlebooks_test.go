package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGoogleBooksSearchFormatsResults(t *testing.T) {
	body := `{
  "totalItems": 1,
  "items": [
    {
      "id": "abc",
      "volumeInfo": {
        "title": "Le Chevalier et la Phalène",
        "subtitle": "Tome 1",
        "authors": ["Camille Chevreuse"],
        "publisher": "Bragelonne",
        "publishedDate": "2024-03-15",
        "description": "<p>Un royaume au bord de la nuit, un chevalier rongé par le doute.</p>",
        "language": "fr",
        "pageCount": 432,
        "categories": ["Fiction / Fantasy"],
        "industryIdentifiers": [
          {"type": "ISBN_13", "identifier": "9782811235567"}
        ],
        "infoLink": "https://books.google.com/?id=abc"
      }
    }
  ]
}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("key"); got != "k" {
			t.Errorf("missing api key: q=%v", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	prev := googleBooksEndpoint
	googleBooksEndpoint = srv.URL
	defer func() { googleBooksEndpoint = prev }()

	text, err := googleBooksSearch(context.Background(), "intitle:Phalène", "fr", "k")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Le Chevalier et la Phalène",
		"Camille Chevreuse",
		"Bragelonne",
		"2024-03-15",
		"ISBN_13: 9782811235567",
		"Un royaume au bord de la nuit",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q\n---\n%s", want, text)
		}
	}
	if strings.Contains(text, "<p>") {
		t.Errorf("HTML not stripped:\n%s", text)
	}
}

func TestGoogleBooksSearchNoResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"totalItems":0}`))
	}))
	defer srv.Close()
	prev := googleBooksEndpoint
	googleBooksEndpoint = srv.URL
	defer func() { googleBooksEndpoint = prev }()

	text, err := googleBooksSearch(context.Background(), "zzz-no-match", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Aucun résultat") {
		t.Errorf("expected 'Aucun résultat' message, got: %s", text)
	}
}

func TestGoogleBooksSearchEmptyQuery(t *testing.T) {
	if _, err := googleBooksSearch(context.Background(), "  ", "", "k"); err == nil {
		t.Fatal("expected error on empty query")
	}
}
