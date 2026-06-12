package agent

import (
	"encoding/json"
	"testing"
)

func TestParseFirecrawlSearchData(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantLen int
		wantTop string // URL of first result, "" when none
	}{
		{
			name:    "v2 grouped under web",
			raw:     `{"web":[{"url":"https://a","title":"A","description":"da"},{"url":"https://b","title":"B"}]}`,
			wantLen: 2,
			wantTop: "https://a",
		},
		{
			name:    "v1 flat array",
			raw:     `[{"url":"https://x","title":"X","description":"dx"}]`,
			wantLen: 1,
			wantTop: "https://x",
		},
		{
			name:    "empty grouped → none",
			raw:     `{"web":[]}`,
			wantLen: 0,
		},
		{
			name:    "empty flat → none",
			raw:     `[]`,
			wantLen: 0,
		},
		{
			name:    "unexpected shape → none",
			raw:     `{"images":[{"url":"https://i"}]}`,
			wantLen: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseFirecrawlSearchData(json.RawMessage(tc.raw))
			if len(got) != tc.wantLen {
				t.Fatalf("len = %d, want %d", len(got), tc.wantLen)
			}
			if tc.wantLen > 0 && got[0].URL != tc.wantTop {
				t.Errorf("top URL = %q, want %q", got[0].URL, tc.wantTop)
			}
		})
	}
}

func TestFilterCamofoxSearchLinks(t *testing.T) {
	links := []camofoxLink{
		{Text: "Google logo", Href: "https://www.google.com/imghp"},    // engine chrome → dropped
		{Text: "À propos", Href: "https://accounts.google.com/signin"}, // internal → dropped
		{Text: "anchor", Href: "#main"},                                // fragment → dropped
		{Text: "js", Href: "javascript:void(0)"},                       // js → dropped
		{Text: "Le Chevalier et la Phalène — Bragelonne", Href: "https://www.google.com/url?q=https://bragelonne.fr/livre/123&sa=U"}, // redirect → unwrapped
		{Text: "Decitre", Href: "https://www.decitre.fr/livres/le-chevalier.html"},                                                   // organic
		{Text: "Decitre dup", Href: "https://www.decitre.fr/livres/le-chevalier.html"},                                               // dup → dropped
		{Text: "Babelio", Href: "https://www.babelio.com/livres/x/456"},                                                              // organic
		{Text: "relative", Href: "/local/path"},                                                                                      // no host → dropped
	}

	got := filterCamofoxSearchLinks(links, 5)
	want := []string{
		"https://bragelonne.fr/livre/123",
		"https://www.decitre.fr/livres/le-chevalier.html",
		"https://www.babelio.com/livres/x/456",
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].URL != w {
			t.Errorf("result %d URL = %q, want %q", i, got[i].URL, w)
		}
	}
	if got[0].Title != "Le Chevalier et la Phalène — Bragelonne" {
		t.Errorf("result 0 title = %q", got[0].Title)
	}

	// limit is honoured.
	if l := filterCamofoxSearchLinks(links, 1); len(l) != 1 || l[0].URL != want[0] {
		t.Errorf("limit=1 → %+v", l)
	}
}

func TestParseCamofoxSnapshotLinks(t *testing.T) {
	// Mirrors a real @google_search accessibility snapshot: a searchbox and nav
	// tabs (no /url:), then organic results as link nodes with a nested /url:.
	snapshot := `- heading "Primal Hunter Zogarth - Recherche Google"
- searchbox "Search" [e1]: Primal Hunter Zogarth
- navigation:
  - link "Tous" [e2]
  - link "Livres" [e8]
- link "Primal Hunter, les 12 livres de la série" [e9]:
  - /url: https://booknode.com/serie/primal-hunter
  - cite: https://booknode.com › serie › primal-hunter
  - text: Primal Hunter - La série. Auteur : Zogarth.
- link "Tome 1 : "Primal" Hunter" [e10]:
  - /url: https://www.fnac.com/a20674148/Primal-Hunter-Tome-1
  - text: De gentil loser à dangereux chasseur !
- link "Aide sur l'accessibilité" [e40]:
  - /url: https://support.google.com/websearch/answer/181196?hl=fr`

	links := parseCamofoxSnapshotLinks(snapshot)
	// Three /url: lines → three links; nav tabs (no /url:) ignored.
	if len(links) != 3 {
		t.Fatalf("parsed %d links, want 3: %+v", len(links), links)
	}
	if links[0].Text != "Primal Hunter, les 12 livres de la série" || links[0].Href != "https://booknode.com/serie/primal-hunter" {
		t.Errorf("link 0 = %+v", links[0])
	}
	// Greedy title capture keeps the inner quotes intact.
	if links[1].Text != `Tome 1 : "Primal" Hunter` {
		t.Errorf("link 1 title = %q", links[1].Text)
	}

	// End to end: the support.google.com result is dropped as noise, the two
	// organic ones survive in document order.
	got := filterCamofoxSearchLinks(links, 5)
	want := []string{
		"https://booknode.com/serie/primal-hunter",
		"https://www.fnac.com/a20674148/Primal-Hunter-Tome-1",
	}
	if len(got) != len(want) {
		t.Fatalf("filtered %d, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].URL != w {
			t.Errorf("result %d URL = %q, want %q", i, got[i].URL, w)
		}
	}
}
