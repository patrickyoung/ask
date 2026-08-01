package auth

import (
	"strings"
	"testing"
)

// TestCheckTokenURL pins the rule exactly: https anywhere, http only where
// the bytes never reach a wire. The refusals matter more than the passes —
// each one is a client secret or a refresh token that would otherwise have
// been readable by everything between here and the endpoint.
func TestCheckTokenURL(t *testing.T) {
	for _, c := range []struct {
		url  string
		ok   bool
		note string
	}{
		{"https://auth.openai.com/oauth/token", true, "the ordinary case"},
		{"https://idp.corp/oauth/token", true, "a corporate gateway, properly"},
		{"http://127.0.0.1:8080/token", true, "loopback v4, what httptest gives"},
		{"http://localhost:9000/token", true, "loopback by name"},
		{"http://[::1]:9000/token", true, "loopback v6"},
		{"http://127.0.0.2/token", true, "all of 127/8 is this machine"},

		{"http://idp.corp/oauth/token", false, "cleartext to another host"},
		{"http://example.com/token", false, "cleartext to the internet"},
		{"http://169.254.169.254/token", false, "link-local is still a wire"},
		{"http://10.0.0.5/token", false, "a private network is still a network"},
		{"idp.corp/oauth/token", false, "no scheme at all"},
		{"ftp://idp.corp/token", false, "some other scheme"},
		{"", false, "empty"},
	} {
		err := CheckTokenURL(c.url)
		if c.ok && err != nil {
			t.Errorf("CheckTokenURL(%q) = %v, want allowed (%s)", c.url, err, c.note)
		}
		if !c.ok && err == nil {
			t.Errorf("CheckTokenURL(%q) = nil, want refused (%s)", c.url, c.note)
		}
	}
}

// TestCheckTokenURLSaysWhy: the error has to name the risk and the way out,
// or it reads as an arbitrary rule and gets worked around.
func TestCheckTokenURLSaysWhy(t *testing.T) {
	err := CheckTokenURL("http://idp.corp/oauth/token")
	if err == nil {
		t.Fatal("cleartext endpoint accepted")
	}
	for _, want := range []string{"in the clear", "https", "loopback"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestCodexRefreshURLIsHTTPS: the one endpoint ask ships a constant for has
// to satisfy its own rule.
func TestCodexRefreshURLIsHTTPS(t *testing.T) {
	if err := CheckTokenURL(CodexRefreshURL); err != nil {
		t.Fatalf("the built-in Codex refresh URL is refused: %v", err)
	}
}
