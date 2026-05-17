package llm

import "testing"

func TestNormalizeOllamaEndpoint(t *testing.T) {
	cases := map[string]string{
		"http://localhost:11434":               "http://localhost:11434",
		"http://localhost:11434/":              "http://localhost:11434",
		"http://localhost:11434///":            "http://localhost:11434",
		"http://localhost:11434/api/chat":      "http://localhost:11434",
		"http://localhost:11434/api/chat/":     "http://localhost:11434",
		"http://192.168.3.25:11434/api":        "http://192.168.3.25:11434",
		"http://192.168.3.25:11434/api/generate": "http://192.168.3.25:11434",
		"https://ollama.lan/":                  "https://ollama.lan",
		"  http://x:11434  ":                   "http://x:11434",
		// path-less host stays untouched
		"http://host:11434":                    "http://host:11434",
	}
	for in, want := range cases {
		if got := normalizeOllamaEndpoint(in); got != want {
			t.Errorf("normalizeOllamaEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}
