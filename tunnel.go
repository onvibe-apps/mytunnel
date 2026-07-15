package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Config holds the client configuration resolved from flags/env.
type Config struct {
	RelayURL     string
	Secret       string
	Local        string
	Concurrency  int
	InspectPort  int
	Inspect      bool
	MaxLog       int
	MaxBody      int
	InspectToken string
	AllowSelf    bool // auto-register this machine's egress IP on the relay allowlist
	AllowTTL     int  // seconds a temporary allowed IP lives before it must be refreshed
}

// Headers that must not be relayed verbatim (hop-by-hop or length/encoding
// managed by the HTTP client). accept-encoding is dropped on the forwarded
// request so Go's transport controls compression and transparently decompresses
// gzip — matching the fetch-based original, where dropping content-encoding on
// the response is only correct because the body arrives already decoded.
var dropReq = map[string]bool{
	"host": true, "connection": true, "content-length": true,
	"transfer-encoding": true, "keep-alive": true, "proxy-connection": true,
	"upgrade": true, "accept-encoding": true,
}

var dropRes = map[string]bool{
	"content-length": true, "transfer-encoding": true, "content-encoding": true,
	"connection": true, "keep-alive": true,
}

// tunnelRequest is the shape delivered by the relay's /__tunnel/poll.
type tunnelRequest struct {
	ID      string            `json:"id"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    *string           `json:"body"` // base64 or null
}

// pollClient is used for the long-poll; its timeout must exceed the relay hold
// (≤15s). localClient forwards to the user's local server, with redirects left
// un-followed to mirror the original redirect:"manual".
var (
	pollClient  = &http.Client{Timeout: 30 * time.Second}
	localClient = &http.Client{
		Timeout: 60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	respondClient = &http.Client{Timeout: 30 * time.Second}
)

// forward sends a request to the local target and returns its response. It is
// shared by the tunnel handler and the inspector's replay. It never follows
// redirects and drops hop-by-hop headers on the way in and out.
func forward(cfg Config, method, path string, headers map[string]string, body []byte) (status int, outHeaders map[string]string, resBody []byte, err error) {
	var r io.Reader
	if len(body) > 0 && method != "GET" && method != "HEAD" {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, cfg.Local+path, r)
	if err != nil {
		return 0, nil, nil, err
	}
	for k, v := range headers {
		if !dropReq[strings.ToLower(k)] {
			req.Header.Set(k, v)
		}
	}
	res, err := localClient.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer res.Body.Close()
	buf, err := io.ReadAll(res.Body)
	if err != nil {
		return 0, nil, nil, err
	}
	out := make(map[string]string, len(res.Header))
	for k, vv := range res.Header {
		if dropRes[strings.ToLower(k)] {
			continue
		}
		out[k] = strings.Join(vv, ", ")
	}
	return res.StatusCode, out, buf, nil
}

// respond posts the full (never truncated) response back to the relay.
func respond(cfg Config, id string, status int, headers map[string]string, body []byte) {
	var b64 *string
	if len(body) > 0 {
		s := base64.StdEncoding.EncodeToString(body)
		b64 = &s
	}
	payload, _ := json.Marshal(map[string]any{
		"id": id, "status": status, "headers": headers, "body": b64,
	})
	req, _ := http.NewRequest("POST", cfg.RelayURL+"/__tunnel/respond", bytes.NewReader(payload))
	req.Header.Set("x-tunnel-secret", cfg.Secret)
	req.Header.Set("content-type", "application/json")
	res, err := respondClient.Do(req)
	if err != nil {
		log.Printf("  respond failed: %v", err)
		return
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
}

// callRelay performs an authenticated control-plane call to the relay and
// returns its status code and raw body. Used by the inspector's allowlist proxy
// and the heartbeat. Never follows the tunnel; talks only to /__tunnel/*.
func callRelay(cfg Config, method, path string, body []byte) (int, []byte) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, cfg.RelayURL+path, r)
	if err != nil {
		return 0, []byte(`{"error":"bad request"}`)
	}
	req.Header.Set("x-tunnel-secret", cfg.Secret)
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	res, err := respondClient.Do(req)
	if err != nil {
		return 502, []byte(`{"error":"relay unreachable"}`)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

// startAllowHeartbeat registers this machine's egress IP on the relay allowlist
// and refreshes it periodically so it survives while the client runs and expires
// on inactivity once the client stops. No-op when AllowSelf is false.
func startAllowHeartbeat(cfg Config) {
	if !cfg.AllowSelf {
		return
	}
	interval := max(cfg.AllowTTL/3, 15)
	body, _ := json.Marshal(map[string]any{"ttl_seconds": cfg.AllowTTL, "label": "system"})
	register := func() {
		if st, _ := callRelay(cfg, "POST", "/__tunnel/allow", body); st == 200 {
			return
		}
	}
	go func() {
		register()
		t := time.NewTicker(time.Duration(interval) * time.Second)
		defer t.Stop()
		for range t.C {
			register()
		}
	}()
}

// handle forwards one tunnel request to the local server and returns the
// response to the relay, capturing both phases for the inspector.
func handle(cfg Config, store *RequestStore, tr tunnelRequest) {
	start := time.Now()
	var reqBytes []byte
	if tr.Body != nil {
		reqBytes, _ = base64.StdEncoding.DecodeString(*tr.Body)
	}

	// Phase 1: capture the in-flight request.
	cap := &CapturedRequest{
		ID:         tr.ID,
		At:         time.Now().UnixMilli(),
		Method:     tr.Method,
		Path:       tr.Path,
		ReqHeaders: tr.Headers,
		Phase:      PhasePending,
	}
	cap.ReqBody, cap.ReqBytes, cap.ReqTruncated = store.snapBody(reqBytes)
	store.Add(cap)

	status, outHeaders, resBody, err := forward(cfg, tr.Method, tr.Path, tr.Headers, reqBytes)
	dur := time.Since(start).Milliseconds()

	if err != nil {
		// Local server unreachable: relay still gets a 502 with the message
		// (full body), and the entry is captured as an error.
		msg := fmt.Appendf(nil, "Local server unreachable: %v\n", err)
		errHeaders := map[string]string{"content-type": "text/plain; charset=utf-8"}
		respond(cfg, tr.ID, 502, errHeaders, msg)
		store.Update(tr.ID, func(c *CapturedRequest) {
			c.Phase = PhaseError
			c.Error = err.Error()
			c.Status = 502
			c.ResHeaders = errHeaders
			c.ResBody, c.ResBytes, c.Truncated = store.snapBody(msg)
			c.DurationMs = dur
		})
		log.Printf("  %s %s → local unreachable", tr.Method, tr.Path)
		return
	}

	// Phase 2: full body to the relay; truncated copy for the inspector.
	respond(cfg, tr.ID, status, outHeaders, resBody)
	store.Update(tr.ID, func(c *CapturedRequest) {
		c.Phase = PhaseDone
		c.Status = status
		c.ResHeaders = outHeaders
		c.ResBody, c.ResBytes, c.Truncated = store.snapBody(resBody)
		c.DurationMs = dur
	})
	log.Printf("  %s %s → %d", tr.Method, tr.Path, status)
}

// worker long-polls the relay and dispatches each claimed request to handle.
func worker(cfg Config, store *RequestStore) {
	for {
		req, _ := http.NewRequest("POST", cfg.RelayURL+"/__tunnel/poll", nil)
		req.Header.Set("x-tunnel-secret", cfg.Secret)
		res, err := pollClient.Do(req)
		if err != nil {
			time.Sleep(1500 * time.Millisecond) // relay unreachable — back off
			continue
		}
		switch {
		case res.StatusCode == 204:
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			continue // no work; poll again immediately
		case res.StatusCode == 401:
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			log.Printf("✗ unauthorized — TUNNEL_SECRET does not match the app. Fix it and restart.")
			time.Sleep(5 * time.Second)
			continue
		case res.StatusCode != 200:
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			time.Sleep(1 * time.Second)
			continue
		}
		var data struct {
			Request *tunnelRequest `json:"request"`
		}
		err = json.NewDecoder(res.Body).Decode(&data)
		res.Body.Close()
		if err == nil && data.Request != nil {
			handle(cfg, store, *data.Request)
		}
	}
}

// startTunnel launches Concurrency workers and blocks forever.
func startTunnel(cfg Config, store *RequestStore) {
	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Go(func() {
			worker(cfg, store)
		})
	}
	wg.Wait()
}
