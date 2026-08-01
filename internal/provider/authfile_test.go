package provider

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrickyoung/ask/internal/auth"
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

// TestStoredCleartextTokenURLIsRefused. This URL came off disk, so it is
// checked where it is used and not only where it was written: an auth.json
// from an older ask, edited by hand, or dropped in by something else must
// not be able to send a refresh token over http. The refusal happens before
// the request, so nothing is spent finding out.
func TestStoredCleartextTokenURLIsRefused(t *testing.T) {
	p := filepath.Join(t.TempDir(), "auth.json")
	t.Setenv("ASK_AUTH_FILE", p)

	var reached atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		fmt.Fprint(w, `{"access_token":"a","expires_in":3600}`)
	}))
	defer evil.Close()

	// httptest listens on loopback, which is legitimately allowed — so name
	// a host that is not, pointing nowhere the test needs to reach.
	if err := auth.Put("openai-codex", auth.Credential{
		Type: "oauth", AccessToken: "stale", RefreshToken: "r-0",
		Expiry: time.Now().Add(-time.Hour), TokenURL: "http://idp.corp/oauth/token",
	}); err != nil {
		t.Fatal(err)
	}
	tr := &storedAuthTransport{provider: "openai-codex", next: http.DefaultTransport}
	if _, err := tr.credential(); err == nil {
		t.Fatal("refreshed a credential over a cleartext endpoint")
	} else if !strings.Contains(err.Error(), "in the clear") {
		t.Errorf("error %q does not say why", err)
	}
	if reached.Load() != 0 {
		t.Error("a request was made before the endpoint was checked")
	}
}

// TestConcurrentRefreshSpendsTheTokenOnce. Refresh tokens rotate, so the
// second process to present one gets nothing back — and its write lands on
// top of the first's, storing a refresh token the issuer has already
// retired. That is a fan-out logging the user out of their own account, and
// this tool invites fan-out. Whoever loses the race must wait and then find
// the winner's token already stored, not spend the credential again.
func TestConcurrentRefreshSpendsTheTokenOnce(t *testing.T) {
	p := filepath.Join(t.TempDir(), "auth.json")
	t.Setenv("ASK_AUTH_FILE", p)

	var grants atomic.Int32
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := grants.Add(1)
		if got := r.FormValue("refresh_token"); got != "r-0" {
			// A rotating issuer refuses a token it has already retired.
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		time.Sleep(20 * time.Millisecond) // widen the window a real network gives for free
		fmt.Fprintf(w, `{"access_token":"a-%d","refresh_token":"r-%d","expires_in":3600}`, n, n)
	}))
	defer tok.Close()

	if err := auth.Put("openai-codex", auth.Credential{
		Type: "oauth", AccessToken: "stale", RefreshToken: "r-0",
		Expiry: time.Now().Add(-time.Hour), TokenURL: tok.URL,
	}); err != nil {
		t.Fatal(err)
	}

	const racers = 8
	var wg sync.WaitGroup
	errs := make(chan error, racers)
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr := &storedAuthTransport{provider: "openai-codex", next: http.DefaultTransport}
			if _, err := tr.credential(); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a racing refresh failed: %v", err)
	}
	if n := grants.Load(); n != 1 {
		t.Errorf("presented the refresh token %d times, want 1", n)
	}
	cred, _, err := auth.Get("openai-codex")
	if err != nil {
		t.Fatal(err)
	}
	if cred.RefreshToken != "r-1" || cred.AccessToken != "a-1" {
		t.Errorf("stored credential = %#v, want the one the single grant issued", cred)
	}
}
