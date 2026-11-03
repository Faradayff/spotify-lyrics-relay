package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestParseDelayMs tables the validation rules for the client delayMs
// query parameter: absent/invalid -> 0, negative -> 0, over the ceiling -> clamp.
func TestParseDelayMs(t *testing.T) {
	cases := []struct {
		raw  string
		want int64
	}{
		{"", 0}, // absent -> no delay
		{"0", 0},
		{"100", 100},
		{"1200", 1200},
		{"29999", 29999},
		{"30000", 30000},  // exactly the ceiling
		{"30001", 30000},  // clamped down
		{"100000", 30000}, // clamped down
		{"-5", 0},         // negative rejected -> 0
		{"abc", 0},        // non-numeric -> 0
		{"1.5", 0},        // not an integer -> 0
	}
	for _, c := range cases {
		if got := parseDelayMs(c.raw); got != c.want {
			t.Errorf("parseDelayMs(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}

// TestPlainStatusEchoesDelayMs: a client that sends delayMs gets it echoed
// back in the plain /status answer, which is the app's feature detection for
// a relay that applies the delay server-side.
func TestPlainStatusEchoesDelayMs(t *testing.T) {
	srv := newUnauthServer(t)
	_, body := statusRequest(srv, "delayMs=1500", nil)
	v, ok := body["delayMs"]
	if !ok {
		t.Fatal("plain /status must echo the delayMs the client sent")
	}
	i, ok := v.(float64)
	if !ok || i != 1500 {
		t.Fatalf("delayMs = %v, want 1500", v)
	}
}

// TestPlainStatusEchoesZeroDelayMs: even with no delay the field must be
// present (its presence is the contract, the value is the amount).
func TestPlainStatusEchoesZeroDelayMs(t *testing.T) {
	srv := newUnauthServer(t)
	_, body := statusRequest(srv, "delayMs=0", nil)
	v, ok := body["delayMs"]
	if !ok {
		t.Fatal("delayMs=0 must still be echoed")
	}
	i, ok := v.(float64)
	if !ok || i != 0 {
		t.Fatalf("delayMs = %v, want 0", v)
	}
}

// TestFindLineDelayShiftPicksEarlierLine proves the delay mapping: resolving
// at position - delay selects the line the listener is hearing now (earlier
// in the list), matching the app's local LyricIndex re-derivation semantics.
func TestFindLineDelayShiftPicksEarlierLine(t *testing.T) {
	lines := parseLRC("[00:00.00]a\n[00:10.00]b\n[00:20.00]c")
	// No delay, at 12 s the active line is "b" (index 1).
	if i := findLine(lines, 12000); i != 1 {
		t.Fatalf("12 s no-delay: expected 1 got %d", i)
	}
	// A 5 s car-audio delay shifts the heard position to 7 s: line "a".
	if i := findLine(lines, 12000-5000); i != 0 {
		t.Fatalf("12 s - 5 s delay: expected 0 got %d", i)
	}
	// A large delay that lands before the first line stays on the first line.
	if i := findLine(lines, 5000-30000); i != 0 {
		t.Fatalf("delay before first line: expected 0 got %d", i)
	}
}

// TestLyricsStateFields_PendingIsLoadingNotError: while the lyrics are in
// flight or riding out a backoff window the payload must say "loading"
// (with a pendingMs hint) and must NOT carry an error field — the app
// renders a loading state, and the relay's own backoff owns the retry.
func TestLyricsStateFields_PendingIsLoadingNotError(t *testing.T) {
	cause := &providerError{status: http.StatusTooManyRequests, detail: "ServerOverloaded"}
	pend := lyricsPendingError(cause, 5*time.Second)
	out := lyricsStateFields(nil, -1, pend)
	if out["lyricsStatus"] != "loading" {
		t.Fatalf("pending must surface as lyricsStatus=loading, got %v", out["lyricsStatus"])
	}
	if _, ok := out["error"]; ok {
		t.Fatalf("pending must not surface an error field: %+v", out)
	}
	if v, ok := out["pendingMs"].(int64); !ok || v != 5000 {
		t.Fatalf("pendingMs = %v, want 5000", out["pendingMs"])
	}
	if _, ok := out["line"]; ok {
		t.Fatalf("no line fields while loading: %+v", out)
	}
	if out["lyricsSynced"] != false {
		t.Fatalf("lyricsSynced must be false while loading: %+v", out)
	}
	if v, ok := out["lyricsLines"].(int); !ok || v != 0 {
		t.Fatalf("lyricsLines must be 0 while loading: %+v", out)
	}
}

// TestLyricsStateFields_SyncedRegression: a successful resolution renders
// exactly the pre-change fields (line/lineText/prevLine/nextLines).
func TestLyricsStateFields_SyncedRegression(t *testing.T) {
	data := &lyricsData{Synced: true, Lines: []lyricLine{{T: 0, Text: "a"}, {T: 1000, Text: "b"}}, LinesCount: 2}
	out := lyricsStateFields(data, 0, nil)
	if out["lyricsStatus"] != "synced" {
		t.Fatalf("want synced, got %v", out["lyricsStatus"])
	}
	if out["line"] != 0 || out["lineText"] != "a" {
		t.Fatalf("active line wrong: %+v", out)
	}
	if _, ok := out["error"]; ok {
		t.Fatalf("no error field on success: %+v", out)
	}
	if _, ok := out["pendingMs"]; ok {
		t.Fatalf("no pendingMs on success: %+v", out)
	}
}

// TestLyricsStateFields_NoLyricsAtAll: a 404 ("no lyrics") stays a clean
// "none" — no error, no pending, no loading.
func TestLyricsStateFields_NoLyricsAtAll(t *testing.T) {
	out := lyricsStateFields(nil, -1, nil)
	if out["lyricsStatus"] != "none" {
		t.Fatalf("want none, got %v", out["lyricsStatus"])
	}
	if _, ok := out["error"]; ok {
		t.Fatalf("no-lyrics must not surface an error: %+v", out)
	}
	if _, ok := out["pendingMs"]; ok {
		t.Fatalf("no-lyrics must not carry pendingMs: %+v", out)
	}
}

// TestLyricsStateFields_NonPendingErrorStillSurfaces: errors that are NOT a
// provider pending state (Spotify-side or unexpected) keep the error field.
func TestLyricsStateFields_NonPendingErrorStillSurfaces(t *testing.T) {
	out := lyricsStateFields(nil, -1, errors.New("spotify 500: boom"))
	if out["error"] != "spotify 500: boom" {
		t.Fatalf("non-pending errors must keep the error field: %+v", out)
	}
	if out["lyricsStatus"] != "none" {
		t.Fatalf("want none, got %v", out["lyricsStatus"])
	}
}

// TestLyricPayloadPrevLine: prevLine is the text of the line above the active
// one and is the empty string (still present) at the first line.
func TestLyricPayloadPrevLine(t *testing.T) {
	lines := []lyricLine{{T: 0, Text: "a"}, {T: 1000, Text: "b"}, {T: 2000, Text: "c"}}
	data := &lyricsData{Synced: true, Lines: lines, LinesCount: 3}

	p := lyricPayload(data, 1)
	if v, _ := p["prevLine"].(string); v != "a" {
		t.Fatalf("prevLine = %v, want a", p["prevLine"])
	}

	p0 := lyricPayload(data, 0)
	v, ok := p0["prevLine"].(string)
	if !ok || v != "" {
		t.Fatalf("prevLine at first line should be empty string, got %v (present=%v)", v, ok)
	}
}

// TestHandleCacheClearWipesMemoryAndDisk: the operator endpoint empties
// both cache layers (in-memory entries + on-disk files) and reports the
// totals it freed.
func TestHandleCacheClearWipesMemoryAndDisk(t *testing.T) {
	srv := newUnauthServer(t) // STATE_DIR -> a fresh temp dir
	lc := srv.lyrics

	d := filepath.Join(lc.stateDir, "lyrics")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "x.json"),
		[]byte(`{"key":"k","at":1,"data":{"plain":"p"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// A couple of in-memory entries (one resolved, one backoff).
	lc.mu.Lock()
	lc.cache["a|A"] = cacheEntry{at: time.Now(), data: lyricsData{Plain: "x"}}
	lc.cache["b|B"] = cacheEntry{at: time.Now(), err: errors.New("boom")}
	lc.mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/cache/clear", nil)
	rr := httptest.NewRecorder()
	srv.handleCacheClear(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("cache/clear = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad JSON body: %v (%s)", err, rr.Body.String())
	}
	if ok, _ := body["ok"].(bool); !ok {
		t.Fatalf("ok must be true: %v", body)
	}
	if removed, _ := body["removed"].(float64); removed != 3 {
		t.Fatalf("removed = %v, want 3 (2 memory + 1 file)", body["removed"])
	}
	if freed, _ := body["freedBytes"].(float64); freed <= 0 {
		t.Fatalf("freedBytes = %v, want > 0", body["freedBytes"])
	}

	lc.mu.Lock()
	notEmpty := len(lc.cache) != 0
	lc.mu.Unlock()
	if notEmpty {
		t.Fatal("memory cache must be empty after /cache/clear")
	}
	if ents, err := os.ReadDir(d); err != nil || len(ents) != 0 {
		t.Fatalf("disk cache must be empty after /cache/clear, got %v (%v)", ents, err)
	}
}

// TestHandleCacheClearPostOnly: same contract as /control — operator
// actions are POST-only.
func TestHandleCacheClearPostOnly(t *testing.T) {
	srv := newUnauthServer(t)
	req := httptest.NewRequest(http.MethodGet, "/cache/clear", nil)
	rr := httptest.NewRecorder()
	srv.handleCacheClear(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET cache/clear = %d, want 405", rr.Code)
	}
}
