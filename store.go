package main

import (
	"encoding/base64"
	"sync"
)

// Phase describes where a captured request is in its lifecycle.
type Phase string

const (
	PhasePending Phase = "pending"
	PhaseDone    Phase = "done"
	PhaseError   Phase = "error"
)

// CapturedRequest is the full record kept for the inspector. Bodies are stored
// base64-encoded (possibly truncated to MaxBody — see snapBody). The summary
// form (list/SSE) is the same object with headers and bodies stripped.
type CapturedRequest struct {
	ID           string            `json:"id"`
	At           int64             `json:"at"` // epoch ms of reception
	Method       string            `json:"method"`
	Path         string            `json:"path"` // includes query string
	ReqHeaders   map[string]string `json:"reqHeaders,omitempty"`
	ReqBody      *string           `json:"reqBody,omitempty"` // base64 or nil
	ReqBytes     int               `json:"reqBytes"`
	ReqTruncated bool              `json:"reqTruncated,omitempty"`
	Phase        Phase             `json:"phase"`
	Status       int               `json:"status,omitempty"`
	ResHeaders   map[string]string `json:"resHeaders,omitempty"`
	ResBody      *string           `json:"resBody,omitempty"` // base64 or nil, possibly truncated
	ResBytes     int               `json:"resBytes,omitempty"`
	Truncated    bool              `json:"truncated,omitempty"` // response body was truncated
	DurationMs   int64             `json:"durationMs,omitempty"`
	Error        string            `json:"error,omitempty"`
	ReplayOf     *string           `json:"replayOf,omitempty"`
}

// summary returns a copy with headers and bodies stripped, for list and SSE.
func (c *CapturedRequest) summary() CapturedRequest {
	s := *c
	s.ReqHeaders = nil
	s.ReqBody = nil
	s.ResHeaders = nil
	s.ResBody = nil
	return s
}

// Event is what the pub/sub delivers to SSE subscribers.
type Event struct {
	Kind string           // "new" | "update" | "cleared"
	Data *CapturedRequest // summary; nil for "cleared"
}

// RequestStore is an in-memory ring buffer of the last MaxLog entries plus a
// map for detail/update lookups, with a simple pub/sub for the SSE endpoint.
// Go is multi-goroutine, so every access is guarded by a mutex.
type RequestStore struct {
	mu      sync.Mutex
	maxLog  int
	maxBody int
	order   []string // ids, oldest first
	byID    map[string]*CapturedRequest

	subMu   sync.Mutex
	subs    map[int]chan Event
	nextSub int
}

func NewRequestStore(maxLog, maxBody int) *RequestStore {
	return &RequestStore{
		maxLog:  maxLog,
		maxBody: maxBody,
		byID:    make(map[string]*CapturedRequest),
		subs:    make(map[int]chan Event),
	}
}

// snapBody base64-encodes a body for storage, truncating to maxBody. It returns
// the encoded string (nil if empty), the real byte length before truncation,
// and whether it was truncated.
func (s *RequestStore) snapBody(raw []byte) (*string, int, bool) {
	if len(raw) == 0 {
		return nil, 0, false
	}
	real := len(raw)
	b := raw
	truncated := false
	if real > s.maxBody {
		b = raw[:s.maxBody]
		truncated = true
	}
	enc := base64.StdEncoding.EncodeToString(b)
	return &enc, real, truncated
}

// Add inserts a new captured request and emits a "new" event (summary).
// It evicts the oldest entry when the ring buffer is full.
func (s *RequestStore) Add(c *CapturedRequest) {
	s.mu.Lock()
	s.byID[c.ID] = c
	s.order = append(s.order, c.ID)
	for len(s.order) > s.maxLog {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.byID, oldest)
	}
	sum := c.summary()
	s.mu.Unlock()
	s.emit(Event{Kind: "new", Data: &sum})
}

// Update mutates an existing entry in place under lock and emits "update".
// If the id is unknown (already evicted) it is a no-op.
func (s *RequestStore) Update(id string, fn func(*CapturedRequest)) {
	s.mu.Lock()
	c, ok := s.byID[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	fn(c)
	sum := c.summary()
	s.mu.Unlock()
	s.emit(Event{Kind: "update", Data: &sum})
}

// Get returns a copy of the full record, or false if not present.
func (s *RequestStore) Get(id string) (CapturedRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.byID[id]
	if !ok {
		return CapturedRequest{}, false
	}
	return *c, true
}

// List returns up to limit summaries, newest first.
func (s *RequestStore) List(limit int) []CapturedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.order)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]CapturedRequest, 0, limit)
	for i := n - 1; i >= 0 && len(out) < limit; i-- {
		if c, ok := s.byID[s.order[i]]; ok {
			out = append(out, c.summary())
		}
	}
	return out
}

// Clear empties the store and emits "cleared".
func (s *RequestStore) Clear() {
	s.mu.Lock()
	s.order = nil
	s.byID = make(map[string]*CapturedRequest)
	s.mu.Unlock()
	s.emit(Event{Kind: "cleared"})
}

// Subscribe registers a listener and returns its channel plus an unsubscribe
// func. Events are delivered non-blocking; a slow subscriber drops events and
// is expected to resync via GET /api/requests on reconnect.
func (s *RequestStore) Subscribe() (<-chan Event, func()) {
	s.subMu.Lock()
	id := s.nextSub
	s.nextSub++
	ch := make(chan Event, 256)
	s.subs[id] = ch
	s.subMu.Unlock()
	return ch, func() {
		s.subMu.Lock()
		if c, ok := s.subs[id]; ok {
			delete(s.subs, id)
			close(c)
		}
		s.subMu.Unlock()
	}
}

func (s *RequestStore) emit(ev Event) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for _, ch := range s.subs {
		select {
		case ch <- ev:
		default: // subscriber too slow; drop (UI resyncs on reconnect)
		}
	}
}
