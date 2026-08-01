package auth

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestImportCodexChatGPTAuth(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	jwt := fakeJWT(exp)
	body := fmt.Sprintf(`{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"id_token":"id","access_token":%q,"refresh_token":"refresh","account_id":"acct"}}`, jwt)
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cred, path, err := ImportCodex()
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "auth.json") {
		t.Fatalf("path = %q", path)
	}
	if cred.AccessToken != jwt || cred.RefreshToken != "refresh" || cred.AccountID != "acct" || cred.TokenURL != CodexRefreshURL {
		t.Fatalf("cred = %#v", cred)
	}
	if !cred.Expiry.Equal(exp) {
		t.Fatalf("expiry = %v, want %v", cred.Expiry, exp)
	}
}

func TestImportCodexRejectsAPIKeyMode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"auth_mode":"api_key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ImportCodex(); err == nil {
		t.Fatal("ImportCodex succeeded for api_key mode")
	}
}

func fakeJWT(exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix()))) + ".sig"
}
