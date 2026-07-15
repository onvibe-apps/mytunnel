package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// FileConfig holds the non-secret defaults persisted to disk. The secret never
// lives here — it goes to the OS keychain (see secretstore.go).
type FileConfig struct {
	URL   string `json:"url,omitempty"`
	Local string `json:"local,omitempty"`
}

// configDir is <user-config>/mytunnel (e.g. ~/Library/Application Support/
// mytunnel on macOS, ~/.config/mytunnel on Linux).
func configDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "mytunnel"), nil
}

func configFilePath() string {
	dir, err := configDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "config.json")
}

func loadConfig() FileConfig {
	b, err := os.ReadFile(configFilePath())
	if err != nil {
		return FileConfig{}
	}
	var c FileConfig
	json.Unmarshal(b, &c)
	return c
}

func saveConfig(c FileConfig) error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600)
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
