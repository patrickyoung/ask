package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"ask/internal/auth"
)

// storedAuthClient returns an authenticating client backed by ~/.ask/auth.json.
// It is for human login/subscription providers, where a durable refresh token
// or access token belongs to the local operator rather than the shell env.
func storedAuthClient(provider string) (*http.Client, bool, error) {
	cred, ok, err := auth.Get(provider)
	if err != nil || !ok {
		return nil, ok, err
	}
	if cred.AccessToken == "" && cred.RefreshToken == "" {
		return nil, true, fmt.Errorf("%s auth has no access_token or refresh_token", provider)
	}
	return &http.Client{Transport: &storedAuthTransport{provider: provider, next: http.DefaultTransport}}, true, nil
}

type storedAuthTransport struct {
	provider string
	next     http.RoundTripper
}

func (t *storedAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := t.token()
	if err != nil {
		return nil, err
	}
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+tok)
	if t.provider == "openai-codex" {
		req.Header.Set("OpenAI-Beta", "responses=2026-02-06")
		req.Header.Set("x-codex-turn-state", "ask")
	}
	if cred, ok, err := auth.Get(t.provider); err == nil && ok && cred.AccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", cred.AccountID)
	}
	return t.next.RoundTrip(req)
}

func (t *storedAuthTransport) token() (string, error) {
	cred, ok, err := auth.Get(t.provider)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("not logged in to %s", t.provider)
	}
	if cred.AccessToken != "" && (cred.Expiry.IsZero() || time.Now().Before(cred.Expiry.Add(-30*time.Second))) {
		return cred.AccessToken, nil
	}
	if cred.RefreshToken == "" || cred.TokenURL == "" {
		return "", fmt.Errorf("%s token expired and no refresh token endpoint is configured; run ask login %s again", t.provider, t.provider)
	}
	if err := refreshStored(t.provider, &cred); err != nil {
		return "", err
	}
	return cred.AccessToken, nil
}

func refreshStored(provider string, cred *auth.Credential) error {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", cred.RefreshToken)
	if cred.ClientID != "" {
		form.Set("client_id", cred.ClientID)
	}
	if cred.Scope != "" {
		form.Set("scope", cred.Scope)
	}
	resp, err := http.PostForm(cred.TokenURL, form)
	if err != nil {
		return fmt.Errorf("%s token refresh: %w", provider, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return &Error{Status: resp.StatusCode, Msg: provider + " token refresh: " + firstNonEmptyLine(body)}
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("%s token refresh returned no access_token", provider)
	}
	cred.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		cred.RefreshToken = tok.RefreshToken
	}
	if tok.ExpiresIn > 0 {
		cred.Expiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	} else {
		cred.Expiry = time.Time{}
	}
	return auth.Put(provider, *cred)
}
