package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestApplyHeadersBearerFallback(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "instance-token")
	if _, err := c.call(context.Background(), "ping", nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	if gotAuth != "Bearer instance-token" {
		t.Errorf("Authorization = %q, want instance-token", gotAuth)
	}
}

func TestApplyHeadersBearerOverride(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "instance-token")
	ctx := WithBearer(context.Background(), "user-token")
	if _, err := c.call(ctx, "ping", nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	if gotAuth != "Bearer user-token" {
		t.Errorf("Authorization = %q, want user-token", gotAuth)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Errorf("missing Bearer prefix: %q", gotAuth)
	}
}

func TestWithBearerEmptyIsNoop(t *testing.T) {
	parent := context.Background()
	ctx := WithBearer(parent, "")
	if got := bearerFromCtx(ctx); got != "" {
		t.Errorf("empty token leaked through: %q", got)
	}
}
