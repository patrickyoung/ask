package provider

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAuthHeaderClientAppliesCodexHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client := authHeaderClient("Bearer descriptor-secret", "acct-1", true)
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for name, want := range map[string]string{
		"Authorization":      "Bearer descriptor-secret",
		"ChatGPT-Account-Id": "acct-1",
		"OpenAI-Beta":        "responses=2026-02-06",
		"x-codex-turn-state": "ask",
	} {
		if value := got.Get(name); value != want {
			t.Errorf("%s = %q, want %q", name, value, want)
		}
	}
}

func TestAuthHeaderClientRefusesCrossOriginRedirect(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client := authHeaderClient("Bearer descriptor-secret", "", false)
	resp, err := client.Get(origin.URL)
	if resp != nil && resp.Body != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "redirected origin") {
		t.Fatalf("cross-origin redirect error = %v", err)
	}
	if leaked.Load() {
		t.Fatal("authorization header reached the redirect target")
	}
}

func TestNewOpenAICodexRequiresDescriptorAuthorization(t *testing.T) {
	if _, _, err := New("openai-codex/gpt-5.6-sol", Options{}); err == nil ||
		!strings.Contains(err.Error(), "-header-fd") {
		t.Fatalf("New openai-codex without descriptor auth err = %v", err)
	}
}
