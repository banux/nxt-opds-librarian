package daemon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPostHeartbeatSendsHeader(t *testing.T) {
	var gotSecret, gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotSecret = r.Header.Get("X-Librarian-Chat-Secret")
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	status, err := postHeartbeat(context.Background(), srv.URL, "topsecret")
	if err != nil {
		t.Fatalf("postHeartbeat: %v", err)
	}
	if status != http.StatusNoContent {
		t.Errorf("status = %d, want 204", status)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/api/librarian/heartbeat" {
		t.Errorf("path = %s", gotPath)
	}
	if gotSecret != "topsecret" {
		t.Errorf("secret header = %q", gotSecret)
	}
}

func TestPostHeartbeatPropagatesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	status, err := postHeartbeat(context.Background(), srv.URL, "x")
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", status)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v", err)
	}
}

func TestPostHeartbeatTreats404AsSoftFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	status, err := postHeartbeat(context.Background(), srv.URL, "x")
	if err != nil {
		t.Fatalf("404 should NOT raise an error (so the caller can branch): %v", err)
	}
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}
