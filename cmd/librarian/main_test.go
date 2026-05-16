package main

import (
	"reflect"
	"testing"
)

func TestStripBinaryNameRepeat(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty", []string{}, []string{}},
		{"plain run", []string{"run", "--instance", "x"}, []string{"run", "--instance", "x"}},
		{"plain prompt", []string{"Le Chevalier"}, []string{"Le Chevalier"}},
		{"librarian repeat", []string{"librarian", "run", "--instance", "x"}, []string{"run", "--instance", "x"}},
		{"librarian-linux-amd64 repeat", []string{"librarian-linux-amd64", "run"}, []string{"run"}},
		{"librarian-darwin-arm64 repeat", []string{"librarian-darwin-arm64", "run"}, []string{"run"}},
		{"middle librarian not stripped", []string{"run", "librarian"}, []string{"run", "librarian"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripBinaryNameRepeat(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLooksLikeSubcommandTypo(t *testing.T) {
	cases := map[string]bool{
		"":                false, // empty
		"-prompt":         false, // flag
		"--help":          false, // flag
		"run":             true,  // valid candidate
		"servve":          true,  // typo
		"Le Chevalier":    false, // multi-word prompt
		"hello world":     false,
		"foo123":          false, // digits → not a typo
		"foo-bar":         true,
		"livre-au-titre":  true, // ambiguous but rare; user gets the error and learns
	}
	for in, want := range cases {
		if got := looksLikeSubcommandTypo(in); got != want {
			t.Errorf("looksLikeSubcommandTypo(%q) = %v, want %v", in, got, want)
		}
	}
}
