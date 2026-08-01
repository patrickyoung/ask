package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// tokenServer answers RFC 6749 token requests, capturing the last form and
// counting grants. Each grant issues tok-<n>.
func tokenServer(t *testing.T, expiresIn int) (*httptest.Server, *atomic.Int64, *map[string]string) {
	t.Helper()
	var grants atomic.Int64
	last := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		clear(last)
		for k := range r.PostForm {
			last[k] = r.PostForm.Get(k)
		}
		n := grants.Add(1)
		resp := map[string]any{"access_token": "tok-" + itoa(n), "token_type": "Bearer"}
		if expiresIn > 0 {
			resp["expires_in"] = expiresIn
		}
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &grants, &last
}

func itoa(n int64) string { return string(rune('0' + n)) }

// TestTokenGrants pins the wire form of both machine grants and refresh
// token rotation.
func TestTokenGrants(t *testing.T) {
	srv, _, last := tokenServer(t, 3600)

	s := &tokenSource{url: srv.URL, id: "cid", secret: "shh", scope: "llm"}
	tok, err := s.get()
	if err != nil || tok != "tok-1" {
		t.Fatalf("get() = %q, %v", tok, err)
	}
	want := map[string]string{"grant_type": "client_credentials", "client_id": "cid", "client_secret": "shh", "scope": "llm"}
	for k, v := range want {
		if (*last)[k] != v {
			t.Errorf("form[%s] = %q, want %q", k, (*last)[k], v)
		}
	}

	s = &tokenSource{url: srv.URL, id: "cid", refresh: "r-1"}
	if _, err := s.get(); err != nil {
		t.Fatal(err)
	}
	if (*last)["grant_type"] != "refresh_token" || (*last)["refresh_token"] != "r-1" {
		t.Errorf("refresh grant form = %v", *last)
	}
}

func TestTokenRotation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "tok-after-" + r.PostForm.Get("refresh_token"),
			"refresh_token": "r-next",
			"expires_in":    1, // immediately stale after the margin math
		})
	}))
	defer srv.Close()

	s := &tokenSource{url: srv.URL, refresh: "r-1"}
	if _, err := s.get(); err != nil {
		t.Fatal(err)
	}
	defer func(f func() time.Time) { now = f }(now)
	now = func() time.Time { return time.Now().Add(time.Minute) } // past expiry
	tok, err := s.get()                                           // must re-grant with the rotated token
	if err != nil || tok != "tok-after-r-next" {
		t.Fatalf("after rotation get() = %q, %v; the new refresh token was not adopted", tok, err)
	}
}

// TestTokenCaching pins the cache: one grant serves many requests until
// expiry passes, then exactly one more.
func TestTokenCaching(t *testing.T) {
	srv, grants, _ := tokenServer(t, 3600)
	s := &tokenSource{url: srv.URL, id: "cid"}
	for range 5 {
		if _, err := s.get(); err != nil {
			t.Fatal(err)
		}
	}
	if n := grants.Load(); n != 1 {
		t.Fatalf("5 gets = %d grants, want 1", n)
	}
	defer func(f func() time.Time) { now = f }(now)
	now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, err := s.get(); err != nil {
		t.Fatal(err)
	}
	if n := grants.Load(); n != 2 {
		t.Fatalf("after expiry: %d grants, want 2", n)
	}
}

// TestTransport401 pins the no-hidden-retries rule: a 401 is returned to
// the caller, and only the *next* request re-authenticates.
func TestTransport401(t *testing.T) {
	tok, grants, _ := tokenServer(t, 3600)
	var apiCalls atomic.Int64
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apiCalls.Add(1) == 1 {
			w.WriteHeader(401)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer api.Close()

	c := &http.Client{Transport: &authTransport{
		src:  &tokenSource{url: tok.URL},
		next: http.DefaultTransport,
	}}
	resp, err := c.Get(api.URL)
	if err != nil || resp.StatusCode != 401 {
		t.Fatalf("first call = %v, %v; the 401 must surface, not be retried", resp.Status, err)
	}
	resp.Body.Close()
	resp, err = c.Get(api.URL)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("second call = %v, %v", resp.Status, err)
	}
	resp.Body.Close()
	if n := grants.Load(); n != 2 {
		t.Fatalf("%d grants, want 2 (one per side of the 401)", n)
	}
}

// TestGatewayEndToEnd drives the whole path through the environment:
// provider.New with no vendor key, a bearer-checking gateway in front of a
// real wire fixture, one token grant across turns — and the stream still
// honors the provider contract.
func TestGatewayEndToEnd(t *testing.T) {
	tok, grants, _ := tokenServer(t, 3600)
	var sawBearer atomic.Int64
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			w.WriteHeader(401)
			return
		}
		sawBearer.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, anthropicWire)
	}))
	defer gw.Close()

	t.Setenv("ASK_AUTH_URL", tok.URL)
	t.Setenv("ASK_AUTH_CLIENT_ID", "cid")
	t.Setenv("ASK_AUTH_CLIENT_SECRET", "shh")
	t.Setenv("ASK_AUTH_REFRESH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", gw.URL)
	t.Setenv("ANTHROPIC_API_KEY", "")           // the gateway owns the vendor key
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "") // plain gateway, not Vertex

	p, model, err := New("anthropic/test-model")
	if err != nil {
		t.Fatalf("New behind a gateway without a vendor key: %v", err)
	}
	req := Request{Model: model, Messages: []Message{{Role: User, Blocks: []Block{{Type: Text, Text: "hi"}}}}}
	for range 2 { // two turns: the token must be fetched once and cached
		d := checkContract(t, p.Stream(context.Background(), req))
		if d.stop != "end" {
			t.Fatalf("stop = %q, want end", d.stop)
		}
	}
	if n := sawBearer.Load(); n != 2 {
		t.Errorf("gateway accepted %d bearer requests, want 2", n)
	}
	if n := grants.Load(); n != 1 {
		t.Errorf("%d token grants for 2 turns, want 1 (cached)", n)
	}
}

// TestRequestSchemaCarriesNoAuth pins the property that makes the log safe
// to share: the normalized request — the thing ask records verbatim — has
// no field that could carry a credential.
func TestRequestSchemaCarriesNoAuth(t *testing.T) {
	full := Request{
		Model: "m", System: "s", MaxTokens: 1, Effort: "low", Digest: "d",
		Messages: []Message{{Role: User, Blocks: []Block{{
			Type: Text, Text: "x", Signature: "sig",
			Provider: "p", Raw: json.RawMessage(`{}`),
		}}}},
	}
	b, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	var keys []string
	collectKeys(m, &keys)
	for _, k := range keys {
		lk := strings.ToLower(k)
		// "token" alone or as a *_token credential; max_tokens counts, not carries
		if lk == "token" || strings.HasSuffix(lk, "_token") {
			t.Errorf("request schema has credential-shaped key %q", k)
		}
		for _, bad := range []string{"auth", "secret", "bearer", "api_key", "apikey", "password"} {
			if strings.Contains(lk, bad) {
				t.Errorf("request schema has credential-shaped key %q", k)
			}
		}
	}
}

func collectKeys(v any, keys *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, vv := range x {
			*keys = append(*keys, k)
			collectKeys(vv, keys)
		}
	case []any:
		for _, vv := range x {
			collectKeys(vv, keys)
		}
	}
}
