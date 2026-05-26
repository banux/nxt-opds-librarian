package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// googleBooksMaxResults caps the number of volumes returned by one search.
// Google Books accepts up to 40; 5 is plenty for the agent to pick the best
// match by title/author/ISBN without flooding the transcript.
const googleBooksMaxResults = 5

// googleBooksEndpoint is the API base. Overridden in tests to point at an
// httptest server.
var googleBooksEndpoint = "https://www.googleapis.com/books/v1/volumes"

type googleBooksResp struct {
	TotalItems int `json:"totalItems"`
	Items      []struct {
		ID         string `json:"id"`
		VolumeInfo struct {
			Title               string   `json:"title"`
			Subtitle            string   `json:"subtitle"`
			Authors             []string `json:"authors"`
			Publisher           string   `json:"publisher"`
			PublishedDate       string   `json:"publishedDate"`
			Description         string   `json:"description"`
			Language            string   `json:"language"`
			PageCount           int      `json:"pageCount"`
			Categories          []string `json:"categories"`
			AverageRating       float64  `json:"averageRating"`
			IndustryIdentifiers []struct {
				Type       string `json:"type"`
				Identifier string `json:"identifier"`
			} `json:"industryIdentifiers"`
			InfoLink string `json:"infoLink"`
		} `json:"volumeInfo"`
	} `json:"items"`
}

// googleBooksSearch hits Google Books v1 /volumes and formats the response as
// a human-readable Markdown digest the model can fold into its enrichment
// decisions. Returns the raw text payload (≤ ~10k chars) and an error.
func googleBooksSearch(ctx context.Context, query, langRestrict, apiKey string) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return "", fmt.Errorf("query vide")
	}
	q := url.Values{}
	q.Set("q", query)
	q.Set("maxResults", fmt.Sprintf("%d", googleBooksMaxResults))
	q.Set("printType", "books")
	if langRestrict != "" {
		q.Set("langRestrict", langRestrict)
	}
	if apiKey != "" {
		q.Set("key", apiKey)
	}
	endpoint := googleBooksEndpoint + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		excerpt := strings.TrimSpace(string(body))
		if len(excerpt) > 300 {
			excerpt = excerpt[:300] + "…"
		}
		return "", fmt.Errorf("google books http %d: %s", resp.StatusCode, excerpt)
	}
	var parsed googleBooksResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parse google books: %w", err)
	}
	if len(parsed.Items) == 0 {
		return fmt.Sprintf("Aucun résultat Google Books pour %q.", query), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Google Books — %d résultat(s) affiché(s) sur %d total\n\n", len(parsed.Items), parsed.TotalItems)
	for i, it := range parsed.Items {
		v := it.VolumeInfo
		title := v.Title
		if v.Subtitle != "" {
			title += " — " + v.Subtitle
		}
		fmt.Fprintf(&b, "## %d. %s\n", i+1, title)
		if len(v.Authors) > 0 {
			fmt.Fprintf(&b, "- Auteur(s): %s\n", strings.Join(v.Authors, ", "))
		}
		if v.Publisher != "" {
			fmt.Fprintf(&b, "- Éditeur: %s\n", v.Publisher)
		}
		if v.PublishedDate != "" {
			fmt.Fprintf(&b, "- Parution: %s\n", v.PublishedDate)
		}
		if v.Language != "" {
			fmt.Fprintf(&b, "- Langue: %s\n", v.Language)
		}
		if v.PageCount > 0 {
			fmt.Fprintf(&b, "- Pages: %d\n", v.PageCount)
		}
		if len(v.Categories) > 0 {
			fmt.Fprintf(&b, "- Catégories: %s\n", strings.Join(v.Categories, ", "))
		}
		if v.AverageRating > 0 {
			fmt.Fprintf(&b, "- Note moyenne: %.1f/5\n", v.AverageRating)
		}
		for _, id := range v.IndustryIdentifiers {
			fmt.Fprintf(&b, "- %s: %s\n", id.Type, id.Identifier)
		}
		if v.Description != "" {
			desc := stripHTML(strings.TrimSpace(v.Description))
			if len(desc) > 2000 {
				desc = desc[:2000] + "…"
			}
			fmt.Fprintf(&b, "- Résumé: %s\n", desc)
		}
		if v.InfoLink != "" {
			fmt.Fprintf(&b, "- Lien: %s\n", v.InfoLink)
		}
		fmt.Fprintln(&b)
	}
	return b.String(), nil
}
