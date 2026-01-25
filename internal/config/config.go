// Package config handles configuration and session persistence
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const (
	configDir    = ".icloud-cli"
	sessionFile  = "session.json"
	cookiesFile  = "cookies.json"
)

// Session holds authentication session data
type Session struct {
	ClientID       string            `json:"client_id"`
	SessionID      string            `json:"session_id,omitempty"`
	SessionToken   string            `json:"session_token,omitempty"`
	TrustToken     string            `json:"trust_token,omitempty"`
	Scnt           string            `json:"scnt,omitempty"`
	AccountCountry string            `json:"account_country,omitempty"`
	AppleID        string            `json:"apple_id,omitempty"`
	Dsid           string            `json:"dsid,omitempty"`
	Webservices    map[string]string `json:"webservices,omitempty"`
	Cookies        map[string]string `json:"cookies,omitempty"`
}

// ConfigPath returns the path to the config directory
func ConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, configDir), nil
}

// EnsureConfigDir creates the config directory if it doesn't exist
func EnsureConfigDir() error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	return os.MkdirAll(path, 0700)
}

// SessionPath returns the path to the session file
func SessionPath() (string, error) {
	cfgPath, err := ConfigPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfgPath, sessionFile), nil
}

// LoadSession loads session from disk
func LoadSession() (*Session, error) {
	path, err := SessionPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, err
	}

	return &session, nil
}

// SaveSession saves session to disk
func (s *Session) Save() error {
	if err := EnsureConfigDir(); err != nil {
		return err
	}

	path, err := SessionPath()
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0600)
}

// Clear removes the session file
func Clear() error {
	path, err := SessionPath()
	if err != nil {
		return err
	}

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
