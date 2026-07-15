package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// stdinReader is shared across all prompts: a fresh bufio.Reader per prompt
// would swallow buffered read-ahead and misalign subsequent reads.
var stdinReader = bufio.NewReader(os.Stdin)

// isTTY reports whether stdin is an interactive terminal (so we may prompt).
func isTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

// promptLine reads a line from stdin, returning def if the user just hits Enter.
func promptLine(prompt, def string) string {
	fmt.Fprint(os.Stderr, prompt)
	line, _ := stdinReader.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return def
	}
	return line
}

// promptSecret reads a line with terminal echo disabled (via stty on Unix), so
// the secret is not shown as it is typed.
func promptSecret(prompt string) string {
	fmt.Fprint(os.Stderr, prompt)
	if runtime.GOOS != "windows" {
		off := exec.Command("stty", "-echo")
		off.Stdin = os.Stdin
		off.Run()
		defer func() {
			on := exec.Command("stty", "echo")
			on.Stdin = os.Stdin
			on.Run()
			fmt.Fprintln(os.Stderr)
		}()
	}
	line, _ := stdinReader.ReadString('\n')
	return strings.TrimRight(line, "\r\n")
}

// runSetup is the `mytunnel setup` subcommand: it configures the endpoint URL,
// the secret (stored in the OS keychain), and an optional default local target.
func runSetup() {
	fmt.Fprintln(os.Stderr, "mytunnel setup — configure endpoint and secret")
	fmt.Fprintln(os.Stderr)
	cfg := loadConfig()

	// No default endpoint — each user deploys their own relay and enters its URL.
	prompt := "Relay URL (e.g. https://your-relay.onvibe.run): "
	if cfg.URL != "" {
		prompt = fmt.Sprintf("Relay URL [%s]: ", cfg.URL)
	}
	url := strings.TrimRight(promptLine(prompt, cfg.URL), "/")
	if url == "" {
		fmt.Fprintln(os.Stderr, "A relay URL is required. Aborting.")
		return
	}

	secret := promptSecret("Secret (hidden, leave blank to keep current): ")
	if secret != "" {
		if err := setSecret(url, secret); err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not store secret:", err)
		} else {
			fmt.Fprintf(os.Stderr, "✓ secret stored in %s for %s\n", secretStoreLocation(), url)
		}
	}

	local := promptLine(fmt.Sprintf("Default local target [%s]: ", cfg.Local), cfg.Local)

	if err := saveConfig(FileConfig{URL: url, Local: local}); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not save config:", err)
	} else {
		fmt.Fprintln(os.Stderr, "✓ config saved to", configFilePath())
	}
	fmt.Fprintln(os.Stderr, "\nDone. Run:  mytunnel --local", firstNonEmpty(local, "3000"))
}
