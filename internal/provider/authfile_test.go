package provider

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"ask/internal/auth"
)

func TestStoredAuthClientAddsBearer(t *testing.T) {
	p := filepath.Join(t.TempDir(), "auth.json")
	t.Setenv("ASK_AUTH_FILE", p)
	if err := auth.Put("openai-codex", auth.Credential{Type: "oauth", AccessToken: "tok", AccountID: "acct"}); err != nil {
		t.Fatal(err)
	}
	hc, ok, err := storedAuthClient("openai-codex")
	if err != nil || !ok {
		t.Fatalf("storedAuthClient ok=%v err=%v", ok, err)
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-Id"); got != "acct" {
			t.Errorf("ChatGPT-Account-Id = %q", got)
		}
		if got := r.Header.Get("OpenAI-Beta"); got == "" {
			t.Error("OpenAI-Beta is empty")
		}
		if got := r.Header.Get("x-codex-turn-state"); got == "" {
			t.Error("x-codex-turn-state is empty")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer s.Close()
	resp, err := hc.Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestStoredAuthClientRefreshesExpiredToken(t *testing.T) {
	p := filepath.Join(t.TempDir(), "auth.json")
	t.Setenv("ASK_AUTH_FILE", p)
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.FormValue("grant_type"); got != "refresh_token" {
			t.Errorf("grant_type = %q", got)
		}
		if got := r.FormValue("refresh_token"); got != "old-refresh" {
			t.Errorf("refresh_token = %q", got)
		}
		w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`))
	}))
	defer tok.Close()
	if err := auth.Put("openai-codex", auth.Credential{Type: "oauth", AccessToken: "old", RefreshToken: "old-refresh", Expiry: time.Now().Add(-time.Hour), TokenURL: tok.URL}); err != nil {
		t.Fatal(err)
	}
	hc, ok, err := storedAuthClient("openai-codex")
	if err != nil || !ok {
		t.Fatalf("storedAuthClient ok=%v err=%v", ok, err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer new-access" {
			t.Errorf("Authorization = %q", got)
		}
	}))
	defer api.Close()
	resp, err := hc.Get(api.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	cred, _, err := auth.Get("openai-codex")
	if err != nil {
		t.Fatal(err)
	}
	if cred.RefreshToken != "new-refresh" || cred.AccessToken != "new-access" {
		t.Fatalf("stored credential = %#v", cred)
	}
}
