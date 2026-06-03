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
