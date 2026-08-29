package provider

import (
	"fmt"
	"net/http"
	"sync"
)

// authHeaderTransport applies a credential supplied by the invoking process.
// The first request fixes the only origin that may receive it. Redirects to a
// different origin therefore fail before the credential is attached, even
// though this transport runs below net/http's redirect handling.
type authHeaderTransport struct {
	authorization string
	accountID     string
	codex         bool
	next          http.RoundTripper

	mu     sync.Mutex
	origin string
}

func (t *authHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	origin := req.URL.Scheme + "://" + req.URL.Host
	t.mu.Lock()
	if t.origin == "" {
		t.origin = origin
	}
	allowed := t.origin
	t.mu.Unlock()
	if origin != allowed {
		return nil, fmt.Errorf("refusing to send authorization header to redirected origin %s", origin)
	}

	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", t.authorization)
	if t.codex {
		clone.Header.Set("OpenAI-Beta", "responses=2026-02-06")
		clone.Header.Set("x-codex-turn-state", "ask")
		if t.accountID != "" {
			clone.Header.Set("ChatGPT-Account-Id", t.accountID)
		}
	}
	return t.next.RoundTrip(clone)
}

func authHeaderClient(authorization, accountID string, codex bool) *http.Client {
	if authorization == "" {
		return nil
	}
	return &http.Client{Transport: &authHeaderTransport{
		authorization: authorization,
		accountID:     accountID,
		codex:         codex,
		next:          http.DefaultTransport,
	}}
}
