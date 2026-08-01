package auth

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreRoundTripAndMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "auth.json")
	t.Setenv("ASK_AUTH_FILE", p)
	exp := time.Now().UTC().Truncate(time.Second)
	if err := Put("openai-codex", Credential{Type: "oauth", AccessToken: "tok", Expiry: exp}); err != nil {
		t.Fatal(err)
	}
	c, ok, err := Get("openai-codex")
	if err != nil || !ok {
		t.Fatalf("Get = %#v, %v, %v", c, ok, err)
	}
	if c.AccessToken != "tok" || !c.Expiry.Equal(exp) {
		t.Fatalf("credential = %#v", c)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %v, want 0600", got)
	}
	if err := Delete("openai-codex"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := Get("openai-codex"); err != nil || ok {
		t.Fatalf("after delete ok=%v err=%v", ok, err)
	}
}
