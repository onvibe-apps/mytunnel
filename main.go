// mytunnel — forwards public requests hitting your onvibe relay to a local
// server, with a local web inspector (ngrok-style) on 127.0.0.1:4040.
//
//	mytunnel setup                            # configure endpoint + secret (secret in the OS keychain)
//	mytunnel --local 3000                     # run using the saved config
//	go run . --url https://x --local 3000 --secret S   # or pass config explicitly
//
// There is no default endpoint: each user deploys their own relay (fork of app/)
// and configures its URL. Endpoint resolution: --url > TUNNEL_URL > saved config.
// Secret: --secret > TUNNEL_SECRET > OS keychain (per endpoint) > interactive prompt.
// Passing --secret leaks it into `ps`/shell history — prefer `mytunnel setup`.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// version is stamped at build time via -ldflags "-X main.version=vX.Y.Z".
var version = "dev"

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

var reScheme = regexp.MustCompile(`^https?://`)
var reDigits = regexp.MustCompile(`^\d+$`)
var reTrailSlash = regexp.MustCompile(`/+$`)

// normalizeLocal accepts "3000" | "localhost:3000" | "http://127.0.0.1:8080".
func normalizeLocal(v string) string {
	if v == "" {
		return ""
	}
	if reScheme.MatchString(v) {
		return reTrailSlash.ReplaceAllString(v, "")
	}
	if reDigits.MatchString(v) {
		return "http://localhost:" + v
	}
	return reTrailSlash.ReplaceAllString("http://"+v, "")
}

func main() {
	log.SetFlags(0)

	// Subcommands.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "setup":
			runSetup()
			return
		case "version", "--version", "-v":
			fmt.Println("mytunnel", version)
			return
		}
	}

	// Flags default to empty so we can layer flag > env > saved config > default.
	url := flag.String("url", "", "relay base URL")
	secret := flag.String("secret", "", "shared secret (prefer `mytunnel setup`; --secret leaks into ps/history)")
	local := flag.String("local", "", "local target (port or URL)")
	concurrency := flag.Int("concurrency", envInt("CONCURRENCY", 8), "parallel workers")
	inspectPort := flag.Int("inspect-port", envInt("INSPECT_PORT", 4040), "inspector port")
	noInspect := flag.Bool("no-inspect", false, "disable the inspector")
	maxLog := flag.Int("max-log", envInt("MAX_LOG", 500), "ring buffer size")
	maxBody := flag.Int("max-body", envInt("MAX_BODY", 2097152), "max stored body bytes")
	inspectToken := flag.String("inspect-token", "", "require ?token= on the inspector")
	noAllowSelf := flag.Bool("no-allow-self", false, "do not auto-register this machine's IP on the relay allowlist")
	allowTTL := flag.Int("allow-ttl", envInt("ALLOW_TTL", 300), "seconds a temporary allowed IP lives before refresh")
	flag.Parse()

	fileCfg := loadConfig()

	relayURL := reTrailSlash.ReplaceAllString(
		firstNonEmpty(*url, os.Getenv("TUNNEL_URL"), fileCfg.URL), "")
	localTarget := normalizeLocal(firstNonEmpty(*local, os.Getenv("LOCAL"), fileCfg.Local))
	inspectTok := firstNonEmpty(*inspectToken, os.Getenv("INSPECT_TOKEN"))

	if relayURL == "" {
		fmt.Fprintln(os.Stderr, "No relay endpoint configured.\n\nRun `mytunnel setup` to configure your endpoint, or pass:\n  --url <https://your-relay>   (or TUNNEL_URL)")
		os.Exit(1)
	}

	// Secret: flag > env > keychain (per endpoint) > interactive prompt.
	sec := firstNonEmpty(*secret, os.Getenv("TUNNEL_SECRET"), getSecret(relayURL))
	if sec == "" && isTTY() {
		sec = promptSecret(fmt.Sprintf("Secret for %s (hidden): ", relayURL))
		if sec != "" && strings.ToLower(promptLine("Save it to "+secretStoreLocation()+"? [y/N]: ", "")) == "y" {
			if err := setSecret(relayURL, sec); err != nil {
				log.Printf("warning: could not store secret: %v", err)
			}
		}
	}

	cfg := Config{
		RelayURL:     relayURL,
		Secret:       sec,
		Local:        localTarget,
		Concurrency:  *concurrency,
		InspectPort:  *inspectPort,
		Inspect:      !*noInspect,
		MaxLog:       *maxLog,
		MaxBody:      *maxBody,
		InspectToken: inspectTok,
		AllowSelf:    !*noAllowSelf,
		AllowTTL:     *allowTTL,
	}

	if cfg.Secret == "" || cfg.Local == "" {
		fmt.Fprintln(os.Stderr, "Missing config.\n\nRun `mytunnel setup` to configure the endpoint and secret, or pass:\n  --secret <SECRET>   (or TUNNEL_SECRET, or the keychain via `mytunnel setup`)\n  --local  <PORT|URL>  (or LOCAL)\n\nExample:\n  mytunnel setup\n  mytunnel --local 3000")
		os.Exit(1)
	}

	store := NewRequestStore(cfg.MaxLog, cfg.MaxBody)

	log.Printf("▶ mytunnel %s", version)
	log.Printf("  relay:   %s", cfg.RelayURL)
	log.Printf("  local:   %s", cfg.Local)
	log.Printf("  workers: %d", cfg.Concurrency)
	if cfg.Inspect {
		startInspector(cfg, store)
	}
	if cfg.AllowSelf {
		log.Printf("  allowlist: auto-registering this IP (ttl %ds)", cfg.AllowTTL)
		startAllowHeartbeat(cfg)
	}
	log.Printf("  (Ctrl+C to stop)\n")

	startTunnel(cfg, store) // blocks forever
}
