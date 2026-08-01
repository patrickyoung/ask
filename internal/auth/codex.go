package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const CodexRefreshURL = "https://auth.openai.com/oauth/token"

// ImportCodex reads the official Codex CLI auth file and converts its ChatGPT
// login token material to ask's narrow credential shape. It does not know how to
// mint credentials itself; users still log in with `codex login`.
func ImportCodex() (Credential, string, error) {
	path, err := codexAuthPath()
	if err != nil {
		return Credential{}, "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Credential{}, path, err
	}
	var f struct {
		AuthMode string `json:"auth_mode"`
		Tokens   struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return Credential{}, path, err
	}
	if f.AuthMode != "chatgpt" {
		return Credential{}, path, fmt.Errorf("Codex auth mode is %q, want chatgpt; run codex login", f.AuthMode)
	}
	if f.Tokens.AccessToken == "" && f.Tokens.RefreshToken == "" {
		return Credential{}, path, fmt.Errorf("Codex auth file has no ChatGPT tokens; run codex login")
	}
	cred := Credential{
		Type:         "oauth",
		AccessToken:  f.Tokens.AccessToken,
		RefreshToken: f.Tokens.RefreshToken,
		TokenURL:     CodexRefreshURL,
		AccountID:    f.Tokens.AccountID,
	}
	if exp := jwtExpiry(f.Tokens.AccessToken); !exp.IsZero() {
		cred.Expiry = exp
	}
	return cred, path, nil
}

func codexAuthPath() (string, error) {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return filepath.Join(home, "auth.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "auth.json"), nil
}

func jwtExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0).UTC()
}
