package main

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// Wait-mode parameters (see relay-wait-mode.md contract).
const (
	waitTimeoutMinMS = 1000  // shorter holds are clamped up
	waitTimeoutMaxMS = 15000 // longer holds are clamped down (proxy read-timeout safety)
	waitTimeoutDefMS = 8000  // used when timeoutMs is absent or unparsable
)

// waitTimeout clamps the client-supplied timeoutMs into the allowed range.
func waitTimeout(raw string) time.Duration {
	if raw == "" {
		return waitTimeoutDefMS * time.Millisecond
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return waitTimeoutDefMS * time.Millisecond
	}
	if n < waitTimeoutMinMS {
		n = waitTimeoutMinMS
	}
	if n > waitTimeoutMaxMS {
		n = waitTimeoutMaxMS
	}
	return time.Duration(n) * time.Millisecond
}

// runRefreshLoop recomputes the status state and publishes it, but ONLY while
// a client is holding a wait request. Without waiters it idles and performs
// no Spotify API calls, so an idle relay costs nothing extra.
func (s *relayServer) runRefreshLoop(interval time.Duration) {
	tick := time.NewTicker(interval)
	for range tick.C {
		if s.hub.waiters() == 0 || !s.spot.authorized() {
			continue
		}
		s.hub.publish(s.computeStatusPayload())
	}
}

// statusHub is the shared state that both the wait-mode handlers and the
// background refresh loop publish to. The version only bumps when the visible
// fingerprint changes, so positionMs/updated churn never disturbs waiters.
type statusHub struct {
	mu       sync.Mutex
	snapshot map[string]any // last computed /status payload (without "version")
	version  int64          // monotonic within the process lifetime
	lastFP   string
	changed  chan struct{} // closed + replaced on every version bump
	waiting  int           // clients currently holding a wait request
}

func newStatusHub() *statusHub {
	return &statusHub{changed: make(chan struct{})}
}

func (h *statusHub) addWaiter()  { h.mu.Lock(); h.waiting++; h.mu.Unlock() }
func (h *statusHub) dropWaiter() { h.mu.Lock(); h.waiting--; h.mu.Unlock() }

func (h *statusHub) waiters() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.waiting
}

// publish stores a computed payload. It always refreshes the snapshot, and
// bumps the version + wakes the waiters only when the visible fingerprint
// changed.
func (h *statusHub) publish(p map[string]any) {
	fp := fingerprint(p)
	h.mu.Lock()
	h.snapshot = p
	if fp != h.lastFP {
		h.lastFP = fp
		h.version++
		ch := h.changed
		h.changed = make(chan struct{})
		h.mu.Unlock()
		close(ch)
		return
	}
	h.mu.Unlock()
}

// changedAndVersion returns the current change channel and version under the
// same lock, so a handler either sees a version above "since" (answer now) or
// a channel that will be closed when the version bumps past it.
func (h *statusHub) changedAndVersion() (chan struct{}, int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.changed, h.version
}

// response returns a copy of the current snapshot plus the version, so
// callers can attach "version" without mutating the shared map.
func (h *statusHub) response() (map[string]any, int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]any, len(h.snapshot)+1)
	for k, v := range h.snapshot {
		out[k] = v
	}
	return out, h.version
}

// fingerprint is the "visible state" of a /status payload: exactly the fields
// that can change what the client renders. Deliberately excluded (they change
// on every Spotify refresh and would defeat wait mode): positionMs, updated,
// lyricsLines, nextLines, track.durMs, track.cover.
func fingerprint(p map[string]any) string {
	get := func(k string) any {
		v, ok := p[k]
		if !ok || v == nil {
			return ""
		}
		return v
	}
	var b strings.Builder
	b.WriteString(strconv.FormatBool(boolOr(get("ok"))))
	b.WriteString("|")
	b.WriteString(strconv.FormatBool(boolOr(get("auth"))))
	b.WriteString("|")
	b.WriteString(strconv.FormatBool(boolOr(get("playing"))))
	b.WriteString("|")
	// "track" may hold a typed-nil *trackInfo (payload["track"] = nil pointer)
	// when Spotify reports no active track; a bare "ok" assertion does not
	// catch that, and dereferencing it would panic out of the HTTP handler
	// and kill the whole relay (502 loop behind the proxy).
	if ti, ok := get("track").(*trackInfo); ok && ti != nil {
		b.WriteString(ti.ID)
	}
	b.WriteString("|")
	b.WriteString(strconv.Itoa(asInt(get("line"))))
	b.WriteString("|")
	b.WriteString(strOr(get("lineText")))
	b.WriteString("|")
	b.WriteString(strconv.FormatBool(boolOr(get("lyricsSynced"))))
	b.WriteString("|")
	b.WriteString(strconv.Itoa(len(strOr(get("plain")))))
	b.WriteString("|")
	b.WriteString(strconv.FormatBool(strOr(get("error")) != ""))
	return b.String()
}

// asInt tolerates both JSON-decoded numbers (float64) and native Go ints.
func asInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}
