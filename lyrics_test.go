package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseLRC_Basic(t *testing.T) {
	lrc := "[00:12.00]Line one\n[00:15.500]Line two\n[00:20]Line three"
	lines := parseLRC(lrc)
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	if lines[0].T != 12000 || lines[0].Text != "Line one" {
		t.Fatalf("line 0 wrong: %+v", lines[0])
	}
	if lines[1].T != 15500 || lines[1].Text != "Line two" {
		t.Fatalf("line 1 wrong: %+v", lines[1])
	}
	if lines[2].T != 20000 || lines[2].Text != "Line three" {
		t.Fatalf("line 2 wrong: %+v", lines[2])
	}
}

func TestParseLRC_MultiStamp(t *testing.T) {
	// A line with two timestamps expands into two synced lines.
	lrc := "[00:01.00][45:01.00]Repeat"
	lines := parseLRC(lrc)
	if len(lines) != 2 || lines[0].T != 1000 || lines[1].T != 45*60000+1000 {
		t.Fatalf("multi-stamp wrong: %+v", lines)
	}
}

func TestParseLRC_Sorting(t *testing.T) {
	lrc := "[00:30.00]later\n[00:10.00]earlier"
	lines := parseLRC(lrc)
	if lines[0].T >= lines[1].T {
		t.Fatalf("not sorted: %+v", lines)
	}
}

func TestFindLine(t *testing.T) {
	lines := parseLRC("[00:00.00]a\n[00:10.00]b\n[00:20.00]c")
	// at 5s -> a is still active (b only starts at 10s)
	if i := findLine(lines, 5000); i != 0 {
		t.Fatalf("5s expected 0 got %d", i)
	}
	// at 12s -> b started at 10s and is active (c starts at 20s)
	if i := findLine(lines, 12000); i != 1 {
		t.Fatalf("12s expected 1 got %d", i)
	}
	// at 25s -> clamps to last (c)
	if i := findLine(lines, 25000); i != 2 {
		t.Fatalf("25s expected 2 got %d", i)
	}
}

// TestFindLine_SparseGaps is the Bohemian Rhapsody regression: long phrases
// with big gaps between them. The active line must stay put until the NEXT
// line actually starts (its timestamp), not at the midpoint between lines —
// with a 14.8s gap the old midpoint logic jumped 7.3s before the audio.
func TestFindLine_SparseGaps(t *testing.T) {
	lines := parseLRC("[00:00.15]Is this the real life?\n[00:14.80]Caught in a landslide")
	// 7s = old (buggy) switch point, well before the 14.8s audio start
	if i := findLine(lines, 7000); i != 0 {
		t.Fatalf("7s expected 0 (first line still active) got %d", i)
	}
	// 14s < 14.8s: still the first line
	if i := findLine(lines, 14000); i != 0 {
		t.Fatalf("14s expected 0 got %d", i)
	}
	// exactly at the start of line 1 -> it becomes active
	if i := findLine(lines, 14800); i != 1 {
		t.Fatalf("14.8s expected 1 got %d", i)
	}
	// after the last line -> stays on the last
	if i := findLine(lines, 60000); i != 1 {
		t.Fatalf("60s expected 1 got %d", i)
	}
}

// TestFindLine_BeforeFirstLine: position before the first timestamp must
// show the first line, never -1.
func TestFindLine_BeforeFirstLine(t *testing.T) {
	lines := parseLRC("[00:05.00]first\n[00:30.00]second")
	if i := findLine(lines, 1000); i != 0 {
		t.Fatalf("1s (before first line) expected 0 got %d", i)
	}
}

// TestDecodeLyrics_RealAPIShape reproduces the exact field names the
// LRCLIB /get endpoint returns (verified live: "syncedLyrics" /
// "plainLyrics"), so regressions to "synced" / "plain" fail here.
func TestDecodeLyrics_RealAPIShape(t *testing.T) {
	m := map[string]any{
		"id":           "3q2CVvw9bZu6aWn1PnS4jF",
		"trackName":    "Bohemian Rhapsody",
		"albumName":    "A Night at the Opera",
		"duration":     354,
		"plainLyrics":  "Is this the real life?",
		"syncedLyrics": "[00:00.15] Is this the real life?\n[00:07.13] Caught in a landslide",
		"lyricsfile":   "https://lrclib.net/api/lyrics/...",
	}
	out := decodeLyrics(m)
	if !out.Synced {
		t.Fatalf("expected synced lyrics, got %+v", out)
	}
	if out.LinesCount != 2 || len(out.Lines) != 2 {
		t.Fatalf("expected 2 lines, got %+v", out.Lines)
	}
	if out.Lines[0].T != 150 || out.Lines[0].Text != "Is this the real life?" {
		t.Fatalf("line 0 wrong: %+v", out.Lines[0])
	}
	if out.Plain != "Is this the real life?" {
		t.Fatalf("plain wrong: %q", out.Plain)
	}

	// Plain-only response.
	plain := decodeLyrics(map[string]any{"plainLyrics": "hello"})
	if plain.Synced || plain.Plain != "hello" {
		t.Fatalf("plain-only wrong: %+v", plain)
	}

	// Empty / missing fields must yield empty data (no crash).
	if d := decodeLyrics(map[string]any{}); d.Synced || d.Plain != "" {
		t.Fatalf("empty wrong: %+v", d)
	}
	if d := decodeLyrics(nil); d.Synced || d.Plain != "" {
		t.Fatalf("nil wrong: %+v", d)
	}
}

func TestTrackFromState_ItemAndTrack(t *testing.T) {
	want := map[string]any{"id": "4uLU64mcE15tl0lwSjmOqz", "name": "Bohemian Rhapsody"}

	// Modern responses carry the track under "item".
	latest := map[string]any{"item": want, "is_playing": true, "progress_ms": float64(42000)}
	if got := trackFromState(latest); got["id"] != "4uLU64mcE15tl0lwSjmOqz" {
		t.Fatalf("item not picked: %+v", got)
	}

	// Older snapshots used "track"; fallback must still work.
	legacy := map[string]any{"track": want, "is_playing": true}
	if got := trackFromState(legacy); got["id"] != "4uLU64mcE15tl0lwSjmOqz" {
		t.Fatalf("track fallback failed: %+v", got)
	}

	// Nothing playing, item explicitly null.
	if got := trackFromState(map[string]any{"item": nil, "progress_ms": float64(0)}); got != nil {
		t.Fatalf("expected nil track, got: %+v", got)
	}
	if got := trackFromState(nil); got != nil {
		t.Fatalf("nil state must yield nil track")
	}
}

func TestLyricPayload(t *testing.T) {
	lines := []lyricLine{
		{T: 40000, Text: "one"}, {T: 45000, Text: "two"}, {T: 50000, Text: "three"},
		{T: 55000, Text: "four"}, {T: 60000, Text: "five"}, {T: 65000, Text: "six"},
	}
	synced := &lyricsData{Synced: true, Lines: lines, LinesCount: 6}

	// Synced data: active line + the line above + next three; the full list
	// is intentionally not part of the served payload.
	p := lyricPayload(synced, 2)
	if p["line"] != 2 || p["lineText"] != "three" {
		t.Fatalf("active line wrong: %+v", p)
	}
	if p["prevLine"] != "two" {
		t.Fatalf("prevLine missing or wrong: %+v", p)
	}
	if _, ok := p["lines"]; ok {
		t.Fatalf("full lines must not be served: %+v", p)
	}
	next, ok := p["nextLines"].([]lyricLine)
	if !ok || len(next) != 3 || next[0].Text != "four" || next[2].Text != "six" {
		t.Fatalf("nextLines wrong (want four/five/six): %+v", p)
	}

	// At the last line: window must be empty, not padded or panicking.
	last := lyricPayload(synced, 5)
	next, _ = last["nextLines"].([]lyricLine)
	if len(next) != 0 {
		t.Fatalf("expected empty nextLines at end, got: %+v", next)
	}

	// Out-of-range index: nothing leaked, no crash.
	if p := lyricPayload(synced, 99); len(p) != 0 {
		t.Fatalf("out-of-range idx should yield empty payload: %+v", p)
	}

	// Unsynced: only the plain text is exposed.
	plain := &lyricsData{Synced: false, Plain: "full lyrics, no stamps"}
	p2 := lyricPayload(plain, -1)
	if p2["plain"] != "full lyrics, no stamps" {
		t.Fatalf("plain missing: %+v", p2)
	}
	if _, ok := p2["line"]; ok {
		t.Fatalf("no line fields expected for plain-only lyrics")
	}

	// No data at all: empty payload.
	if p := lyricPayload(nil, -1); len(p) != 0 {
		t.Fatalf("nil data should yield empty payload: %+v", p)
	}
	if p := lyricPayload(&lyricsData{}, -1); len(p) != 0 {
		t.Fatalf("empty data should yield empty payload: %+v", p)
	}
}

// newTestLyricsClient points the cached client at a fake provider so the
// tests can count exactly how many upstream requests a sequence of lookups
// produces. Waits are no-ops so retry behavior is exercised without delay.
func newTestLyricsClient(base string, errTTL time.Duration) *lyricsClient {
	return &lyricsClient{
		base:        base,
		cache:       map[string]cacheEntry{},
		inflight:    map[string]struct{}{},
		now:         time.Now,
		minGap:      time.Millisecond, // keep multi-lookup tests snappy; gap has its own test
		logf:        func(string, ...any) {},
		errTTL:      errTTL,
		retryBase:   []time.Duration{time.Millisecond, time.Millisecond},
		sleep:       func(time.Duration) {},
		http:        &http.Client{Timeout: 2 * time.Second},
		ttl:         time.Hour, // explicit cache policy: zero would mean "never expires"
		noLyricsTTL: time.Hour,
	}
}

func songTrack() map[string]any {
	return map[string]any{"id": "track-1", "name": "Song", "artist": "Band"}
}

// A provider failure (LRCLIB 429 "ServerOverloaded") must cost exactly ONE
// request and come back as the pending state — the retry is owned by the
// backoff windows (per track + global), not by inline re-tries, and a
// second lookup of the same track replays the pending state without any
// further upstream traffic.
func TestGetForTrack_CachesProviderFailures(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"Too many requests"}`))
	}))
	defer srv.Close()

	l := newTestLyricsClient(srv.URL, time.Minute)
	tr := songTrack()
	_, err := l.getForTrack(tr)
	if p, ok := asLyricsPending(err); !ok || p.cause == nil {
		t.Fatalf("expected a pending state with a provider cause, got %v", err)
	}
	_, err = l.getForTrack(tr)
	if _, ok := asLyricsPending(err); !ok {
		t.Fatalf("expected the cached pending state to be replayed, got %v", err)
	}
	// One request per lookup; the 429 is final for the lookup and the
	// replayed pending state must add nothing.
	if hits != 1 {
		t.Fatalf("provider hit %d times, want 1", hits)
	}
}

// A successful lookup stays cached (existing behaviour kept): the second
// lookup must not contact the provider at all.
func TestGetForTrack_CachesSuccess(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"syncedLyrics":"[00:01.00]one","plainLyrics":"one"}`))
	}))
	defer srv.Close()

	l := newTestLyricsClient(srv.URL, time.Minute)
	tr := songTrack()
	d1, err := l.getForTrack(tr)
	if err != nil || d1 == nil || !d1.Synced {
		t.Fatalf("first fetch failed: %v %+v", err, d1)
	}
	if d2, err := l.getForTrack(tr); err != nil || d2 == nil || !d2.Synced {
		t.Fatalf("second fetch failed: %v %+v", err, d2)
	}
	if hits != 1 {
		t.Fatalf("provider hit %d times, want 1", hits)
	}
}

// A concurrent lookup of the same key (two 300 ms refresh ticks, or app +
// curl at once) must not duplicate the upstream request: the second caller
// skips this tick and the provider sees exactly one hit.
func TestGetForTrack_SkipsInflightConcurrentFetch(t *testing.T) {
	release := make(chan struct{})
	var hits int64 // atomic: the handler and the test goroutine touch it concurrently
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		<-release // hold the request open until the test lets it through
	}))
	defer srv.Close()

	l := newTestLyricsClient(srv.URL, time.Minute)
	tr := songTrack()
	done := make(chan struct{})
	go func() {
		l.getForTrack(tr)
		close(done)
	}()

	// Wait until the first request is actually in flight.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&hits) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// While the first fetch is in flight, a second lookup of the same key
	// must report pending for this tick instead of duplicating the request.
	d, err := l.getForTrack(tr)
	if d != nil {
		t.Fatalf("in-flight duplicate should not resolve data, got %+v", d)
	}
	if p, ok := asLyricsPending(err); !ok || p.cause != nil {
		t.Fatalf("in-flight duplicate should be a deferral (no cause), got %v", err)
	}
	close(release)
	<-done
	if n := atomic.LoadInt64(&hits); n != 1 {
		t.Fatalf("provider hit %d times, want 1", n)
	}
}

// Once the error window expires the lookup retries the provider: one
// request per attempt window.
func TestGetForTrack_RetriesAfterErrorTTL(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	l := newTestLyricsClient(srv.URL, 30*time.Millisecond)
	tr := songTrack()
	if _, err := l.getForTrack(tr); err == nil {
		t.Fatal("expected a pending state")
	}
	time.Sleep(60 * time.Millisecond) // let the error window expire
	if _, err := l.getForTrack(tr); err == nil {
		t.Fatal("expected a pending state after the retry")
	}
	// One inline-final request per lookup, two lookups.
	if hits != 2 {
		t.Fatalf("provider hit %d times, want 2", hits)
	}
}

// A transient 503 ("ServerOverloaded") followed by a 200 must surface a
// success AFTER its window expires — lrcLib's intermittent overload must not
// become a user-visible failure for a track it does have. It shows as the
// pending state in between, never as an error.
func TestGetForTrack_RetriesAfterWindowThenSucceeds(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"message":"The server is busy, please retry in a moment","name":"ServerOverloaded","statusCode":503}`))
			return
		}
		_, _ = w.Write([]byte(`{"syncedLyrics":"[00:01.00]one","plainLyrics":"one"}`))
	}))
	defer srv.Close()

	l := newTestLyricsClient(srv.URL, 30*time.Millisecond)
	d, err := l.getForTrack(songTrack())
	if d != nil {
		t.Fatalf("first lookup must be pending, got %+v", d)
	}
	if _, ok := asLyricsPending(err); !ok {
		t.Fatalf("expected a pending state after the transient 503, got %v", err)
	}
	time.Sleep(60 * time.Millisecond) // let the backoff window expire
	d, err = l.getForTrack(songTrack())
	if err != nil || d == nil || !d.Synced {
		t.Fatalf("expected success after the window expired, got %v %+v", err, d)
	}
	if hits != 2 {
		t.Fatalf("provider hit %d times, want 2 (one per window)", hits)
	}
}

// Transport errors (socket closed before the request reached the provider)
// are retried inline: the provider never saw them, so re-attempting is free.
func TestFetch_TransportErrorsRetriedInline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no local networking available: %v", err)
	}
	defer ln.Close()
	var conns int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt64(&conns, 1)
			c.Close() // connection aborted: client sees a transport error
		}
	}()

	l := newTestLyricsClient("http://"+ln.Addr().String(), time.Minute)
	_, perr, ferr := l.fetch(songTrack())
	if perr != nil || ferr == nil {
		t.Fatalf("expected a transport error, got perr=%v err=%v", perr, ferr)
	}
	if n := atomic.LoadInt64(&conns); n != maxFetchAttempts {
		t.Fatalf("provider saw %d connections, want %d inline (re)attempts", n, maxFetchAttempts)
	}
}

// A 429 that names a Retry-After must never be re-tried inline: the lookup
// settles after exactly one request and the window is carried by the error
// cache (per track) AND the global backoff, replaying it without hitting
// the provider.
func TestGetForTrack_HonorsLongRetryAfterViaCache(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"Rate limit exceeded"}`))
	}))
	defer srv.Close()

	l := newTestLyricsClient(srv.URL, time.Minute)
	_, err := l.getForTrack(songTrack())
	if p, ok := asLyricsPending(err); !ok || p.cause == nil {
		t.Fatalf("expected a pending state for a 429 provider, got %v", err)
	}
	if hits != 1 {
		t.Fatalf("provider hit %d times, want 1 (long Retry-After must not be re-tried inline)", hits)
	}
	l.mu.Lock()
	e, ok := l.cache["track-1|Band"]
	global := l.globalWindowUntil
	l.mu.Unlock()
	if !ok || e.err == nil {
		t.Fatalf("expected a cached failure entry, got %+v (%v)", e, ok)
	}
	if e.errTTL != 120*time.Second {
		t.Fatalf("cached per-track window = %v, want the provider's 120s", e.errTTL)
	}
	// The refusal must also extend the CLIENT-WIDE backoff window.
	if rem := global.Sub(l.now()); rem < 110*time.Second || rem > 125*time.Second {
		t.Fatalf("global backoff = %v, want ~the provider's 120s", rem)
	}
	// Within the window: replay, no new provider hit.
	if _, err := l.getForTrack(songTrack()); err == nil {
		t.Fatal("expected the pending state to be replayed")
	}
	if hits != 1 {
		t.Fatalf("provider hit %d times during the window, want 1", hits)
	}
}

// Repeated failures of the same track back the error window off
// exponentially (base, base*2, ...): the second failure's window must be
// double the first's for the same base TTL.
func TestGetForTrack_BacksoffErrorWindowOnRepeatedFailures(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	base := 30 * time.Millisecond
	l := newTestLyricsClient(srv.URL, base)
	tr := songTrack()

	l.getForTrack(tr)
	l.mu.Lock()
	first := l.cache["track-1|Band"].errTTL
	l.mu.Unlock()
	if first != base {
		t.Fatalf("first failure window = %v, want the base %v", first, base)
	}

	time.Sleep(60 * time.Millisecond) // expire the first window
	l.getForTrack(tr)
	l.mu.Lock()
	second := l.cache["track-1|Band"].errTTL
	l.mu.Unlock()
	if second != base*2 {
		t.Fatalf("second failure window = %v, want double the base %v", second, base*2)
	}
}

// An overload refusal on ONE track is client-wide: while the global
// backoff window is open, a DIFFERENT track must not fire its own request —
// it comes back pending and the refresh loop retries it when the window
// opens.
func TestGetForTrack_GlobalWindowDefersOtherSongs(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	l := newTestLyricsClient(srv.URL, time.Minute)
	// Song A consumes its request and gets 429'd.
	if _, err := l.getForTrack(songTrack()); err == nil {
		t.Fatal("expected a pending state for the 429 track")
	}
	// Song B arrives while the global window is open: it must be pending,
	// NOT re-requested.
	other := map[string]any{"id": "track-2", "name": "Other", "artist": "Other Band"}
	d, err := l.getForTrack(other)
	if d != nil {
		t.Fatalf("other song must not resolve during the global backoff: %+v", d)
	}
	if p, ok := asLyricsPending(err); !ok || p.retryIn <= 0 {
		t.Fatalf("other song must be pending with a retry window, got %v", err)
	}
	if hits != 1 {
		t.Fatalf("provider hit %d times, want 1 (global backoff blocks track B)", hits)
	}
}

// minUpstreamGap spaces real requests even for fresh tracks: a burst of new
// songs (shuffle) may not stack requests back to back — the second one is
// deferred to a later tick.
func TestGetForTrack_MinGapDefersBursts(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusServiceUnavailable) // any refusal; per-track window is tiny
	}))
	defer srv.Close()

	gap := 150 * time.Millisecond
	l := newTestLyricsClient(srv.URL, time.Millisecond)
	l.minGap = gap
	// Song A consumes the request slot.
	l.getForTrack(songTrack())
	// Immediately a different song: deferred by the gap (its per-track
	// window does not even exist yet — never fetched).
	other := map[string]any{"id": "track-2", "name": "Other", "artist": "Other Band"}
	d, err := l.getForTrack(other)
	if d != nil {
		t.Fatalf("burst song must not resolve during the gap: %+v", d)
	}
	if p, ok := asLyricsPending(err); !ok || p.retryIn <= 0 {
		t.Fatalf("expected pending with a retry window during the gap, got %v", err)
	}
	if hits != 1 {
		t.Fatalf("provider hit %d times, want 1", hits)
	}
	// After the gap opens, the deferred song finally consumes a slot.
	time.Sleep(200 * time.Millisecond)
	if _, err := l.getForTrack(other); err == nil {
		t.Fatal("expected a pending state (503) once the gap allowed a real request")
	}
	if hits != 2 {
		t.Fatalf("provider hit %d times, want 2 (one per gap)", hits)
	}
}

// lrcLib requires an identifying User-Agent on every request; without one
// we are indistinguishable from a generic crawler and get throttled.
func TestFetchOnce_SendsIdentifyingUserAgent(t *testing.T) {
	var ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{"syncedLyrics":"[00:01.00]one"}`))
	}))
	defer srv.Close()

	l := newTestLyricsClient(srv.URL, time.Minute)
	if _, _, err := l.fetchOnce(srv.URL + "/get?track_name=x"); err != nil {
		t.Fatalf("fetchOnce failed: %v", err)
	}
	if !strings.HasPrefix(ua, "spotify-lyrics-relay") {
		t.Fatalf("User-Agent %q does not identify the client (lrcLib requires it)", ua)
	}
}

func TestLyricsStatus(t *testing.T) {
	synced := &lyricsData{Synced: true, Lines: []lyricLine{{T: 0, Text: "a"}}, LinesCount: 1}
	plain := &lyricsData{Synced: false, Plain: "hello"}

	if got := lyricsStatus(synced, 0); got != "synced" {
		t.Fatalf("want synced, got %q", got)
	}
	// Synced data wins even when plain text also exists for the same track.
	if got := lyricsStatus(&lyricsData{Synced: true, Lines: []lyricLine{{T: 0, Text: "a"}}, Plain: "hello"}, 0); got != "synced" {
		t.Fatalf("want synced (plain present), got %q", got)
	}
	if got := lyricsStatus(plain, -1); got != "plain" {
		t.Fatalf("want plain, got %q", got)
	}
	if got := lyricsStatus(nil, -1); got != "none" {
		t.Fatalf("want none, got %q", got)
	}
	if got := lyricsStatus(&lyricsData{}, -1); got != "none" {
		t.Fatalf("want none for empty data, got %q", got)
	}
}

// --- Persistent cache: on-disk layer, TTL, limits, wipe ---

// TestDiskCache_RoundTripSurvivesRestart: a resolved lyric is persisted and
// a brand-new client (a "container restart") serves the same track from
// disk with zero provider traffic.
func TestDiskCache_RoundTripSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"syncedLyrics":"[00:01.00]one","plainLyrics":"one"}`))
	}))
	l1 := newTestLyricsClient(srv.URL, time.Minute)
	l1.stateDir = dir
	if d, err := l1.getForTrack(songTrack()); err != nil || d == nil || !d.Synced {
		t.Fatalf("first fetch failed: %v %+v", err, d)
	}
	srv.Close()

	diskDir := filepath.Join(dir, "lyrics")
	ents, err := os.ReadDir(diskDir)
	if err != nil || len(ents) != 1 {
		t.Fatalf("expected exactly one cached file, got %v (%v)", ents, err)
	}

	// Restart: a fresh client whose provider now refuses.
	denied := 0
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		denied++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv2.Close()
	l2 := newTestLyricsClient(srv2.URL, time.Minute)
	l2.stateDir = dir
	d, err := l2.getForTrack(songTrack())
	if err != nil || d == nil || !d.Synced || len(d.Lines) != 1 {
		t.Fatalf("fresh client must serve from disk: %v %+v", err, d)
	}
	if denied != 0 {
		t.Fatalf("provider hit %d times, want 0 (restart must be disk-served)", denied)
	}
}

// TestDiskCache_NoLyricsNotPersistedAndShortTTL: a 404 ("no lyrics") is
// never persisted (a song can gain lyrics later) and its memory shadow
// expires on the SHORT TTL, so the relay re-checks the provider and picks
// the lyric up once it appears.
func TestDiskCache_NoLyricsNotPersistedAndShortTTL(t *testing.T) {
	dir := t.TempDir()
	hits := 0
	hasLyrics := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if !hasLyrics {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"syncedLyrics":"[00:01.00]one"}`))
	}))
	defer srv.Close()

	l := newTestLyricsClient(srv.URL, time.Minute)
	l.stateDir = dir
	l.noLyricsTTL = 60 * time.Millisecond

	// No lyrics yet: a clean "none" result — an empty (non-nil) lyricsData,
	// no error, no pending.
	if d, err := l.getForTrack(songTrack()); err != nil || d == nil || hasLyricsContent(*d) {
		t.Fatalf("404 must be a clean no-lyrics result, got %v %+v", err, d)
	}
	diskDir := filepath.Join(dir, "lyrics")
	if ents, err := os.ReadDir(diskDir); !os.IsNotExist(err) && len(ents) > 0 {
		t.Fatalf("a 404 must not be persisted to disk: %v", ents)
	}

	// Lyrics appear on the provider; after the short window the relay must
	// re-check and pick them up.
	time.Sleep(120 * time.Millisecond)
	hasLyrics = true
	d, err := l.getForTrack(songTrack())
	if err != nil || d == nil || !d.Synced {
		t.Fatalf("the track must resolve once the provider has lyrics, got %v %+v", err, d)
	}
	if hits != 2 {
		t.Fatalf("provider hit %d times, want 2 (404, then re-check)", hits)
	}
	// This time it IS persisted (resolved lyrics do go to disk).
	if ents, err := os.ReadDir(diskDir); err != nil || len(ents) != 1 {
		t.Fatalf("resolved lyrics must be persisted, got %v (%v)", ents, err)
	}
	// Third lookup: pure cache, no new hit.
	if _, err := l.getForTrack(songTrack()); err != nil {
		t.Fatalf("third lookup failed: %v", err)
	}
	if hits != 2 {
		t.Fatalf("provider hit %d times, want 2 (third lookup must be cached)", hits)
	}
}

// TestDiskCache_TTLExpiryForcesRefetch: a resolved entry past its TTL is
// re-fetched from the provider (both memory and disk copies are stale).
func TestDiskCache_TTLExpiryForcesRefetch(t *testing.T) {
	dir := t.TempDir()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"syncedLyrics":"[00:01.00]one"}`))
	}))
	defer srv.Close()

	l := newTestLyricsClient(srv.URL, time.Minute)
	l.stateDir = dir
	l.ttl = 80 * time.Millisecond

	if _, err := l.getForTrack(songTrack()); err != nil {
		t.Fatalf("first fetch failed: %v", err)
	}
	time.Sleep(150 * time.Millisecond) // let the TTL expire
	if _, err := l.getForTrack(songTrack()); err != nil {
		t.Fatalf("lookup after TTL expiry failed: %v", err)
	}
	if hits != 2 {
		t.Fatalf("provider hit %d times, want 2 (TTL expiry must force a refetch)", hits)
	}
}

// TestEnforceDiskLimit_EvictsOldest: once the cache exceeds the size budget
// the oldest files go first; the newest survive.
func TestEnforceDiskLimit_EvictsOldest(t *testing.T) {
	dir := t.TempDir()
	l := newTestLyricsClient("unused", time.Minute)
	l.stateDir = dir
	d := filepath.Join(dir, "lyrics")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	content := make([]byte, 1024)
	mk := func(name string, age time.Duration) {
		p := filepath.Join(d, name)
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	mk("old.json", 3*time.Hour)
	mk("mid.json", 2*time.Hour)
	mk("new.json", time.Hour)

	l.maxSize = 2 * 1024 // room for exactly two 1KiB files
	l.enforceDiskLimit()

	have := map[string]bool{}
	ents, err := os.ReadDir(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		have[e.Name()] = true
	}
	if have["old.json"] {
		t.Fatalf("oldest file must be evicted: %v", ents)
	}
	if !have["mid.json"] || !have["new.json"] {
		t.Fatalf("the two newest files must survive: %v", ents)
	}
}

// TestEnforceMemCap_Bounded: the memory map is bounded — the oldest
// RESOLVED entries leave the map (their files stay on disk) while error
// entries are never evicted (they carry the backoff state).
func TestEnforceMemCap_Bounded(t *testing.T) {
	l := newTestLyricsClient("unused", time.Minute)
	l.memCap = 3
	base := time.Now().Add(-time.Hour)
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := 0; i < 5; i++ {
		l.cache[fmt.Sprintf("k%d|A", i)] = cacheEntry{
			at:   base.Add(time.Duration(i) * time.Minute),
			data: lyricsData{Plain: "x"},
		}
	}
	l.cache["err|A"] = cacheEntry{at: base, err: errors.New("boom")}

	l.enforceMemCap()

	if _, ok := l.cache["k0|A"]; ok {
		t.Fatal("oldest entry must be evicted")
	}
	if _, ok := l.cache["k1|A"]; ok {
		t.Fatal("second-oldest entry must be evicted")
	}
	if _, ok := l.cache["k2|A"]; !ok {
		t.Fatal("newer entries must survive")
	}
	if _, ok := l.cache["k4|A"]; !ok {
		t.Fatal("newest entry must survive")
	}
	if _, ok := l.cache["err|A"]; !ok {
		t.Fatal("error entries carry backoff state and must not be evicted")
	}
}

// TestClearCache_WipesBothLayers: the operator wipe empties the memory map
// and the on-disk files, reporting what was removed/freed.
func TestClearCache_WipesBothLayers(t *testing.T) {
	dir := t.TempDir()
	l := newTestLyricsClient("unused", time.Minute)
	l.stateDir = dir
	d := filepath.Join(dir, "lyrics")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.json", "b.json"} {
		b := []byte(`{"key":"x","at":1,"data":{"plain":"a"}}`)
		if err := os.WriteFile(filepath.Join(d, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l.mu.Lock()
	l.cache["m1|A"] = cacheEntry{at: time.Now(), data: lyricsData{Plain: "mem"}}
	l.cache["m2|A"] = cacheEntry{at: time.Now(), err: errors.New("boom")}
	l.mu.Unlock()

	removed, freed, err := l.clear()
	if err != nil {
		t.Fatalf("clear failed: %v", err)
	}
	if removed != 4 {
		t.Fatalf("removed = %d, want 4 (2 memory + 2 files)", removed)
	}
	if freed <= 0 {
		t.Fatalf("freedBytes = %d, want > 0", freed)
	}
	l.mu.Lock()
	notEmpty := len(l.cache) != 0
	l.mu.Unlock()
	if notEmpty {
		t.Fatal("memory cache must be empty after clear")
	}
	if ents, err := os.ReadDir(d); err != nil || len(ents) != 0 {
		t.Fatalf("disk cache must be empty after clear, got %v (%v)", ents, err)
	}
}

// TestJanitorSweep_ExpiresMemoryAndDisk: a sweep pass drops entries past
// their TTL in BOTH layers — resolved lyrics (long TTL) and "no lyrics"
// shadows (short TTL) — and keeps fresh ones.
func TestJanitorSweep_ExpiresMemoryAndDisk(t *testing.T) {
	dir := t.TempDir()
	l := newTestLyricsClient("unused", time.Minute)
	l.stateDir = dir
	l.ttl = 80 * time.Millisecond
	l.noLyricsTTL = 80 * time.Millisecond

	stale := time.Now().Add(-time.Hour)
	fresh := time.Now()
	l.mu.Lock()
	l.cache["gone|A"] = cacheEntry{at: stale, data: lyricsData{Plain: "old"}}
	l.cache["none|A"] = cacheEntry{at: stale, data: lyricsData{}} // 404 shadow
	l.cache["fresh|A"] = cacheEntry{at: fresh, data: lyricsData{Plain: "kept"}}
	l.mu.Unlock()

	d := filepath.Join(dir, "lyrics")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	w := func(name string, at time.Time, plain string) {
		if err := os.WriteFile(filepath.Join(d, name),
			[]byte(fmt.Sprintf(`{"key":"%s","at":%d,"data":{"plain":"%s"}}`, name, at.UnixMilli(), plain)),
			0o644); err != nil {
			t.Fatal(err)
		}
	}
	w("stale.json", stale, "old")
	w("fresh.json", fresh, "kept")

	l.sweep()

	l.mu.Lock()
	_, hasGone := l.cache["gone|A"]
	_, hasNone := l.cache["none|A"]
	onlyFresh := len(l.cache) == 1
	l.mu.Unlock()
	if hasGone || hasNone {
		t.Fatal("expired entries must leave the memory cache")
	}
	if !onlyFresh {
		t.Fatal("fresh entries must survive the sweep")
	}
	ents, err := os.ReadDir(d)
	if err != nil || len(ents) != 1 || ents[0].Name() != "fresh.json" {
		t.Fatalf("stale files must be dropped, the fresh one kept, got %v (%v)", ents, err)
	}
}
