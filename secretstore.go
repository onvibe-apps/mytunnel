package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// The secret is stored per relay URL (each endpoint has its own), so the same
// machine can hold secrets for several tunnels. On macOS it goes to the login
// keychain via the `security` CLI; elsewhere it falls back to a 0600 JSON file.
const keychainService = "mytunnel"

// secretStoreLocation describes where getSecret/setSecret persist, for messaging.
func secretStoreLocation() string {
	if runtime.GOOS == "darwin" {
		return "macOS Keychain"
	}
	return secretsFilePath() + " (0600)"
}

func secretsFilePath() string {
	dir, err := configDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "secrets.json")
}

// getSecret returns the stored secret for a relay URL, or "" if none/unavailable.
func getSecret(url string) string {
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("security", "find-generic-password", "-s", keychainService, "-a", url, "-w").Output()
		if err != nil {
			return ""
		}
		return strings.TrimRight(string(out), "\r\n")
	}
	return fileSecrets()[url]
}

// setSecret stores the secret for a relay URL.
func setSecret(url, secret string) error {
	if runtime.GOOS == "darwin" {
		// -U updates the item if it already exists. (The secret is briefly visible
		// in this child process's argv during setup only — not in the long-lived
		// tunnel process nor in shell history.)
		return exec.Command("security", "add-generic-password",
			"-U", "-s", keychainService, "-a", url, "-w", secret).Run()
	}
	m := fileSecrets()
	m[url] = secret
	return writeFileSecrets(m)
}

func fileSecrets() map[string]string {
	b, err := os.ReadFile(secretsFilePath())
	if err != nil {
		return map[string]string{}
	}
	m := map[string]string{}
	json.Unmarshal(b, &m)
	return m
}

func writeFileSecrets(m map[string]string) error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(secretsFilePath(), b, 0o600)
}
