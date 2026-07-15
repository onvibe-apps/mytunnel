package main

import (
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"
)

//go:embed ui.html
var uiHTML []byte

// startInspector serves the local web inspector on 127.0.0.1:InspectPort.
// It binds to loopback only — never 0.0.0.0.
func startInspector(cfg Config, store *RequestStore) {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		w.Write(uiHTML)
	})

	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		state := map[string]any{
			"relayUrl":    cfg.RelayURL,
			"localTarget": cfg.Local,
			"publicUrl":   cfg.RelayURL,
			"inspectPort": cfg.InspectPort,
		}
		if online, pending, ok := relayStatus(cfg); ok {
			state["online"] = online
			state["pending"] = pending
		}
		writeJSON(w, http.StatusOK, state)
	})

	mux.HandleFunc("GET /api/requests", func(w http.ResponseWriter, r *http.Request) {
		limit := 200
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				limit = n
			}
		}
		writeJSON(w, http.StatusOK, store.List(limit))
	})

	mux.HandleFunc("GET /api/requests/{id}", func(w http.ResponseWriter, r *http.Request) {
		c, ok := store.Get(r.PathValue("id"))
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, c)
	})

	mux.HandleFunc("GET /api/stream", func(w http.ResponseWriter, r *http.Request) {
		streamSSE(w, r, store)
	})

	mux.HandleFunc("POST /api/requests/{id}/replay", func(w http.ResponseWriter, r *http.Request) {
		id, ok := replay(cfg, store, r.PathValue("id"))
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
	})

	mux.HandleFunc("POST /api/clear", func(w http.ResponseWriter, r *http.Request) {
		store.Clear()
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})

	// Allowlist management — thin authenticated proxies to the relay control plane.
	mux.HandleFunc("GET /api/whoami", func(w http.ResponseWriter, r *http.Request) {
		proxyRelay(w, cfg, "GET", "/__tunnel/whoami", nil)
	})
	mux.HandleFunc("GET /api/allowed", func(w http.ResponseWriter, r *http.Request) {
		proxyRelay(w, cfg, "GET", "/__tunnel/allowed", nil)
	})
	mux.HandleFunc("POST /api/allowed", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		proxyRelay(w, cfg, "POST", "/__tunnel/allow", body)
	})
	mux.HandleFunc("DELETE /api/allowed", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		proxyRelay(w, cfg, "DELETE", "/__tunnel/allow", body)
	})
	mux.HandleFunc("GET /api/denied", func(w http.ResponseWriter, r *http.Request) {
		proxyRelay(w, cfg, "GET", "/__tunnel/denied", nil)
	})

	var handler http.Handler = mux
	if cfg.InspectToken != "" {
		handler = tokenGuard(cfg.InspectToken, mux)
	}

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.InspectPort))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("inspector failed to bind %s: %v", addr, err)
		return
	}
	log.Printf("inspector on http://%s", addr)
	go func() {
		if err := http.Serve(ln, handler); err != nil {
			log.Printf("inspector server stopped: %v", err)
		}
	}()
}

// tokenGuard requires ?token=<InspectToken> on every request when enabled.
func tokenGuard(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != token {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// streamSSE pushes store events to the client, with a keep-alive comment every
// 15s so proxies/browsers keep the connection open.
func streamSSE(w http.ResponseWriter, r *http.Request, store *RequestStore) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, cancel := store.Subscribe()
	defer cancel()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			fmt.Fprint(w, ":\n\n")
			flusher.Flush()
		case ev := <-ch:
			if ev.Kind == "cleared" {
				fmt.Fprint(w, "event: cleared\ndata:\n\n")
			} else {
				data, _ := json.Marshal(ev.Data)
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Kind, data)
			}
			flusher.Flush()
		}
	}
}

// replay re-sends a captured request to the local target (outside the tunnel)
// and records the result as a new entry marked with replayOf. It runs the same
// forwarding logic as handle. Returns the new id.
func replay(cfg Config, store *RequestStore, srcID string) (string, bool) {
	src, ok := store.Get(srcID)
	if !ok {
		return "", false
	}
	var reqBytes []byte
	if src.ReqBody != nil {
		reqBytes, _ = base64.StdEncoding.DecodeString(*src.ReqBody)
	}

	newID := uuid()
	orig := srcID
	start := time.Now()
	cap := &CapturedRequest{
		ID:         newID,
		At:         time.Now().UnixMilli(),
		Method:     src.Method,
		Path:       src.Path,
		ReqHeaders: src.ReqHeaders,
		Phase:      PhasePending,
		ReplayOf:   &orig,
	}
	cap.ReqBody, cap.ReqBytes, cap.ReqTruncated = store.snapBody(reqBytes)
	store.Add(cap)

	status, outHeaders, resBody, err := forward(cfg, src.Method, src.Path, src.ReqHeaders, reqBytes)
	dur := time.Since(start).Milliseconds()
	if err != nil {
		msg := fmt.Appendf(nil, "Local server unreachable: %v\n", err)
		store.Update(newID, func(c *CapturedRequest) {
			c.Phase = PhaseError
			c.Error = err.Error()
			c.Status = 502
			c.ResHeaders = map[string]string{"content-type": "text/plain; charset=utf-8"}
			c.ResBody, c.ResBytes, c.Truncated = store.snapBody(msg)
			c.DurationMs = dur
		})
		return newID, true
	}
	store.Update(newID, func(c *CapturedRequest) {
		c.Phase = PhaseDone
		c.Status = status
		c.ResHeaders = outHeaders
		c.ResBody, c.ResBytes, c.Truncated = store.snapBody(resBody)
		c.DurationMs = dur
	})
	return newID, true
}

// relayStatus best-effort fetches the relay's /__tunnel/status.
func relayStatus(cfg Config) (online bool, pending int, ok bool) {
	req, err := http.NewRequest("GET", cfg.RelayURL+"/__tunnel/status", nil)
	if err != nil {
		return false, 0, false
	}
	req.Header.Set("x-tunnel-secret", cfg.Secret)
	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return false, 0, false
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return false, 0, false
	}
	var s struct {
		Online  bool `json:"online"`
		Pending int  `json:"pending"`
	}
	if json.NewDecoder(res.Body).Decode(&s) != nil {
		return false, 0, false
	}
	return s.Online, s.Pending, true
}

// proxyRelay forwards a control-plane call to the relay and streams the JSON
// response (status + body) back to the inspector client.
func proxyRelay(w http.ResponseWriter, cfg Config, method, path string, body []byte) {
	status, b := callRelay(cfg, method, path, body)
	w.Header().Set("content-type", "application/json; charset=utf-8")
	if status == 0 {
		status = http.StatusBadGateway
	}
	w.WriteHeader(status)
	w.Write(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// uuid returns a random v4-ish identifier for replay entries.
func uuid() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
