package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthURLUsesLoopbackForWildcardAddresses(t *testing.T) {
	cases := map[string]string{
		":8080":         "http://127.0.0.1:8080/health",
		"0.0.0.0:9000":  "http://127.0.0.1:9000/health",
		"[::]:8080":     "http://127.0.0.1:8080/health",
		"10.0.0.5:8080": "http://10.0.0.5:8080/health",
		"[::1]:7000":    "http://[::1]:7000/health",
	}
	for addr, want := range cases {
		got, err := healthURL(addr)
		if err != nil {
			t.Fatalf("healthURL(%q): %v", addr, err)
		}
		if got != want {
			t.Fatalf("healthURL(%q) = %q, want %q", addr, got, want)
		}
	}
	if _, err := healthURL("8080"); err == nil {
		t.Fatal("healthURL accepted an address without a port")
	}
}

func TestHealthcheckReportsServerState(t *testing.T) {
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	}))
	defer server.Close()
	addr := strings.TrimPrefix(server.URL, "http://")

	if err := healthcheck(context.Background(), addr); err != nil {
		t.Fatalf("healthy server: %v", err)
	}
	status = http.StatusServiceUnavailable
	if err := healthcheck(context.Background(), addr); err == nil {
		t.Fatal("healthcheck accepted HTTP 503")
	}
}
