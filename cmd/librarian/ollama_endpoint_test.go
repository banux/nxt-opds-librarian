package main

import "testing"

func TestResolveOllamaEndpoint(t *testing.T) {
	cases := []struct {
		name   string
		flagEP string
		cfgURL string
		want   string
	}{
		{"flag/env wins over yaml", "http://ollama:11434", "http://yaml:11434", "http://ollama:11434"},
		{"yaml used when flag empty", "", "http://yaml:11434", "http://yaml:11434"},
		{"both empty → empty (NewOllama defaults)", "", "", ""},
		{"flag set, no yaml", "http://ollama:11434", "", "http://ollama:11434"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveOllamaEndpoint(tc.flagEP, tc.cfgURL); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
