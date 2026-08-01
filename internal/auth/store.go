package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Store is ask's durable credential file. It is deliberately small and explicit:
// provider name to OAuth-ish token material. Provider adapters decide how to use
// an entry; the store only owns persistence and file permissions.
type Store struct {
	Providers map[string]Credential `json:"providers"`
}

type Credential struct {
	Type         string    `json:"type"`
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
	TokenURL     string    `json:"token_url,omitempty"`
	ClientID     string    `json:"client_id,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	AccountID    string    `json:"account_id,omitempty"`
}

func Path() (string, error) {
	if p := os.Getenv("ASK_AUTH_FILE"); p != "" {
		return p, nil
	}
	dir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ".ask", "auth.json"), nil
}

func Load() (*Store, string, error) {
	p, err := Path()
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &Store{Providers: map[string]Credential{}}, p, nil
	}
	if err != nil {
		return nil, p, err
	}
	var s Store
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, p, err
	}
	if s.Providers == nil {
		s.Providers = map[string]Credential{}
	}
	return &s, p, nil
}

func Save(path string, s *Store) error {
	if s.Providers == nil {
		s.Providers = map[string]Credential{}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func Get(provider string) (Credential, bool, error) {
	s, _, err := Load()
	if err != nil {
		return Credential{}, false, err
	}
	c, ok := s.Providers[provider]
	return c, ok, nil
}

func Put(provider string, c Credential) error {
	if provider == "" {
		return fmt.Errorf("provider is required")
	}
	s, p, err := Load()
	if err != nil {
		return err
	}
	s.Providers[provider] = c
	return Save(p, s)
}

func Delete(provider string) error {
	s, p, err := Load()
	if err != nil {
		return err
	}
	delete(s.Providers, provider)
	return Save(p, s)
}
