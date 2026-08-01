package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/patrickyoung/ask/internal/auth"
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
	cred, err := t.credential()
	if err != nil {
		return nil, err
	}
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	if t.provider == "openai-codex" {
		req.Header.Set("OpenAI-Beta", "responses=2026-02-06")
		req.Header.Set("x-codex-turn-state", "ask")
	}
	if cred.AccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", cred.AccountID)
	}
	return t.next.RoundTrip(req)
}

// credential returns a usable credential, refreshing it if the access token
// has expired.
//
// The refresh happens under the credential file's lock, and the credential
// is read again once it is held. Refresh tokens rotate: two ask processes
// refreshing at once would each spend the same token, and the slower one's
// write would land on top of the faster one's — leaving a stored refresh
// token the issuer has already retired. An unattended fan-out, which is
// exactly what this tool invites, would log the user out of their own
// account and give no clue why. The second process waits a moment and finds
// the first one's new token already there.
func (t *storedAuthTransport) credential() (auth.Credential, error) {
	cred, ok, err := auth.Get(t.provider)
	if err != nil {
		return cred, err
	}
	if !ok {
		return cred, fmt.Errorf("not logged in to %s", t.provider)
	}
	if usable(cred) {
		return cred, nil
	}
	release, err := auth.Lock()
	if err != nil {
		return cred, err
	}
	defer release()
	if cred, ok, err = auth.Get(t.provider); err != nil {
		return cred, err
	} else if !ok {
		return cred, fmt.Errorf("not logged in to %s", t.provider)
	}
	if usable(cred) {
		return cred, nil // another process refreshed while this one waited
	}
	if cred.RefreshToken == "" || cred.TokenURL == "" {
		return cred, fmt.Errorf("%s token expired and no refresh token endpoint is configured; run ask login %s again", t.provider, t.provider)
	}
	if err := refreshStored(t.provider, &cred); err != nil {
		return cred, err
	}
	return cred, nil
}

// usable reports whether an access token is present and will still be valid
// when the request it is about to authenticate arrives.
func usable(c auth.Credential) bool {
	return c.AccessToken != "" &&
		(c.Expiry.IsZero() || time.Now().Before(c.Expiry.Add(-30*time.Second)))
}

func refreshStored(provider string, cred *auth.Credential) error {
	// Checked again here, not only at login: this URL came off disk, where
	// it may have been written by an older ask, edited by hand, or dropped
	// in by something else entirely. A credential file is not a promise.
	if err := auth.CheckTokenURL(cred.TokenURL); err != nil {
		return fmt.Errorf("%s: %w", provider, err)
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", cred.RefreshToken)
	if cred.ClientID != "" {
		form.Set("client_id", cred.ClientID)
	}
	if cred.Scope != "" {
		form.Set("scope", cred.Scope)
	}
	// tokenClient, not the default: the body carries the refresh token, so
	// this must not follow a redirect to another host. See oauth.go.
	resp, err := tokenClient.PostForm(cred.TokenURL, form)
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
