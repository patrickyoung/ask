package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Corporate gateways (Kong and kin) front provider APIs behind OAuth: a
// token endpoint issues short-lived bearer tokens, and the gateway holds
// the vendor keys itself. ask treats that as a transport property, not a
// provider feature — one authenticating http.Client, shared by every
// adapter, switched on by ASK_AUTH_URL:
//
//	ASK_AUTH_URL            token endpoint (presence enables OAuth)
//	ASK_AUTH_CLIENT_ID      \ client credentials; secret sent as
//	ASK_AUTH_CLIENT_SECRET  / client_secret_post form fields
//	ASK_AUTH_REFRESH_TOKEN  optional: use the refresh_token grant instead
//	ASK_AUTH_SCOPE          optional
//
// Point the providers at the gateway with ANTHROPIC_BASE_URL,
// OPENAI_BASE_URL, GEMINI_BASE_URL, OPENROUTER_BASE_URL. Gateways that
// front Google Vertex AI need the Vertex URL shape as well — see
// vertex.go.

// tokenSource speaks the two machine grants of RFC 6749 — refresh_token
// (§6) when a refresh token is configured, client_credentials (§4.4)
// otherwise. Tokens live in memory for the life of the process: nothing
// touches disk, and a fresh process re-authenticates.
type tokenSource struct {
	url, id, secret, refresh, scope string

	mtx    sync.Mutex
	token  string
	expiry time.Time
}

// now is a test seam.
var now = time.Now

func (s *tokenSource) get() (string, error) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if s.token != "" && now().Before(s.expiry) {
		return s.token, nil
	}
	form := url.Values{}
	if s.refresh != "" {
		form.Set("grant_type", "refresh_token")
		form.Set("refresh_token", s.refresh)
	} else {
		form.Set("grant_type", "client_credentials")
	}
	if s.id != "" {
		form.Set("client_id", s.id)
	}
	if s.secret != "" {
		form.Set("client_secret", s.secret)
	}
	if s.scope != "" {
		form.Set("scope", s.scope)
	}
	resp, err := http.PostForm(s.url, form)
	if err != nil {
		return "", fmt.Errorf("oauth token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", &Error{Status: resp.StatusCode, Msg: "oauth token: " + firstNonEmptyLine(body)}
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		ExpiresIn    int    `json:"expires_in"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("oauth token: %s returned no access_token", s.url)
	}
	ttl := time.Duration(tok.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute // endpoint named no expiry; refresh conservatively
	}
	margin := min(ttl/10, 30*time.Second)
	s.token = tok.AccessToken
	s.expiry = now().Add(ttl - margin)
	if s.refresh != "" && tok.RefreshToken != "" {
		s.refresh = tok.RefreshToken // some IdPs rotate the refresh token per grant
	}
	return s.token, nil
}

// stale forgets the current token, so the next request re-authenticates.
func (s *tokenSource) stale() {
	s.mtx.Lock()
	s.token = ""
	s.mtx.Unlock()
}

// authTransport bears the token on every request. It never retries — ask
// owns retries, and they must be visible in the log — so a 401 only marks
// the token stale for the attempt that follows.
type authTransport struct {
	src  *tokenSource
	next http.RoundTripper
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := t.src.get()
	if err != nil {
		return nil, err
	}
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := t.next.RoundTrip(req)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		t.src.stale()
	}
	return resp, err
}

// oauthClient returns the authenticating client shared by every adapter,
// or nil when ASK_AUTH_URL is not set.
func oauthClient() *http.Client {
	u := os.Getenv("ASK_AUTH_URL")
	if u == "" {
		return nil
	}
	return &http.Client{Transport: &authTransport{
		src: &tokenSource{
			url:     u,
			id:      os.Getenv("ASK_AUTH_CLIENT_ID"),
			secret:  os.Getenv("ASK_AUTH_CLIENT_SECRET"),
			refresh: os.Getenv("ASK_AUTH_REFRESH_TOKEN"),
			scope:   os.Getenv("ASK_AUTH_SCOPE"),
		},
		next: http.DefaultTransport,
	}}
}

func firstNonEmptyLine(b []byte) string {
	for line := range strings.Lines(string(b)) {
		if s := strings.TrimSpace(line); s != "" {
			return s
		}
	}
	return "empty response"
}
