package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// --- helpers ---

// newUnauthServer builds a relay with EMPTY credentials: /status goes through
// the "not authenticated" path, so the tests exercise the whole hub/version
// machinery without any network access.
func newUnauthServer(t *testing.T) *relayServer {
	t.Helper()
	t.Setenv("STATE_DIR", t.TempDir())
	t.Setenv("SPOTIFY_CLIENT_ID", "")
	t.Setenv("SPOTIFY_CLIENT_SECRET", "")
	return newRelayServer("")
}

func statusRequest(srv *relayServer, query string, ctx context.Context) (int, map[string]any) {
	u := "/status"
	if query != "" {
		u += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, u, nil)
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	rr := httptest.NewRecorder()
	srv.handleStatus(rr, req)
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	return rr.Code, body
}

func basePayload(over map[string]any) map[string]any {
	p := map[string]any{
		"ok":           true,
		"auth":         true,
		"playing":      true,
		"positionMs":   1000,
		"updated":      "2026-01-01T00:00:00Z",
		"track":        &trackInfo{ID: "track-1", Name: "N", Artist: "A"},
		"lyricsSynced": true,
		"lyricsLines":  54,
		"line":         16,
		"lineText":     "hello world",
	}
	for k, v := range over {
		p[k] = v
	}
	return p
}

// --- fingerprint ---

func TestFingerprintIgnoresPositionAndTimestamps(t *testing.T) {
	a := fingerprint(basePayload(nil))
	b := fingerprint(basePayload(map[string]any{
		"positionMs":  999999,
		"updated":     "2026-01-02T00:00:00Z",
		"lyricsLines": 55,
	}))
	if a != b {
		t.Fatalf("positionMs/updated/lyricsLines must not affect the fingerprint:\n%q\n%q", a, b)
	}
}

func TestFingerprintDetectsEveryVisibleChange(t *testing.T) {
	cases := map[string]map[string]any{
		"line":          {"line": 17},
		"lineText":      {"lineText": "different text"},
		"playing":       {"playing": false},
		"trackID":       {"track": &trackInfo{ID: "track-2"}},
		"lyricsSynced":  {"lyricsSynced": false},
		"plainPresence": {"plain": "abc"},
		"errorPresence": {"error": "boom"},
		"notAuthed":     {"ok": false, "auth": false},
	}
	base := fingerprint(basePayload(nil))
	for name, over := range cases {
		if fingerprint(basePayload(over)) == base {
			t.Errorf("%s: expected a fingerprint change, got none", name)
		}
	}
	// plain length change (same presence) must also bump.
	a := fingerprint(basePayload(map[string]any{"plain": "abc"}))
	b := fingerprint(basePayload(map[string]any{"plain": "abcdef"}))
	if a == b {
		t.Error("plain text length change must change the fingerprint")
	}
}

// A payload whose "track" field carries a typed-nil *trackInfo (what
// computeStatusPayload stores when Spotify has no active track) must NOT
// panic the fingerprint — and must fingerprint identically to a payload
// where the track is absent. Regression: this used to dereference the nil
// pointer out of the /status handler and crash the whole relay (502 storm).
func TestFingerprintTypedNilTrackDoesNotPanic(t *testing.T) {
	var nilTrack *trackInfo
	var withNil, without string
	didPanic := false
	func() {
		defer func() {
			if recover() != nil {
				didPanic = true
			}
		}()
		withNil = fingerprint(basePayload(map[string]any{"track": nilTrack}))
		without = fingerprint(basePayload(map[string]any{"track": nil}))
		// publish() is the code path reached from the request handler.
		newStatusHub().publish(basePayload(map[string]any{"track": nilTrack}))
	}()
	if didPanic {
		t.Fatal("fingerprint must not panic on a typed-nil track")
	}
	if withNil != without {
		t.Fatalf("typed-nil track and absent track must fingerprint identically:\n%q\n%q", withNil, without)
	}
}

// --- hub: version bump + wakeup ---

func TestHubNoBumpWhenFingerprintUnchanged(t *testing.T) {
	h := newStatusHub()
	h.publish(basePayload(nil))
	_, v1 := h.response()
	h.publish(basePayload(map[string]any{"positionMs": 5000}))
	_, v2 := h.response()
	if v1 != v2 {
		t.Fatalf("position-only change must not bump the version: %d -> %d", v1, v2)
	}
}

func TestHubBumpAndWake(t *testing.T) {
	h := newStatusHub()
	h.publish(basePayload(nil))
	_, vBefore := h.response()
	ch, cv := h.changedAndVersion()
	if cv != vBefore {
		t.Fatalf("changedAndVersion must be atomic: %d != %d", cv, vBefore)
	}

	go h.publish(basePayload(map[string]any{"line": 99, "lineText": "changed"}))

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("waiter was not woken by a fingerprint change")
	}
	_, vAfter := h.response()
	if vAfter <= vBefore {
		t.Fatalf("version must bump on a fingerprint change: %d -> %d", vBefore, vAfter)
	}
}

func TestHubAllWaitersWake(t *testing.T) {
	h := newStatusHub()
	h.publish(basePayload(nil))
	chs := make([]<-chan struct{}, 3)
	for i := range chs {
		chs[i], _ = h.changedAndVersion()
	}
	h.publish(basePayload(map[string]any{"playing": false}))
	for i, ch := range chs {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("waiter %d was not woken (broadcast fan-out broken)", i)
		}
	}
}

// --- wait timeout clamp ---

func TestWaitTimeoutClamp(t *testing.T) {
	// raw -> expected clamped ms (identity: def 8000, min 1000, max 15000)
	cases := []string{"=8000", "abc=8000", "0=1000", "500=1000", "8000=8000", "100000=15000"}
	for _, c := range cases {
		parts := strings.SplitN(c, "=", 2)
		if len(parts) != 2 {
			t.Fatal("bad test case: " + c)
		}
		raw, wantStr := parts[0], parts[1]
		w, err := strconv.Atoi(wantStr)
		if err != nil {
			t.Fatal(err)
		}
		got := int64(waitTimeout(raw) / time.Millisecond)
		if got != int64(w) {
			t.Errorf("waitTimeout(%q) = %d ms, want %d ms", raw, got, w)
		}
	}
}

// --- handler: contract checks (offline, unauthenticated state) ---

func TestPlainStatusHasVersion(t *testing.T) {
	srv := newUnauthServer(t)
	code, body := statusRequest(srv, "", nil)
	if code != http.StatusOK {
		t.Fatalf("status code = %d", code)
	}
	if v, ok := body["version"]; !ok {
		t.Fatal("plain /status must include a JSON integer version")
	} else if _, ok := v.(float64); !ok {
		t.Fatalf("version must be a JSON integer, got %T", v)
	}
}

func TestWaitFirstRequestAnswersImmediately(t *testing.T) {
	srv := newUnauthServer(t)
	start := time.Now()
	code, body := statusRequest(srv, "wait=1&timeoutMs=10000&sinceVersion=-1", nil)
	if code != http.StatusOK {
		t.Fatalf("status code = %d", code)
	}
	if _, ok := body["version"]; !ok {
		t.Fatal("wait=1 response must include version")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("first wait request must answer immediately, took %v", elapsed)
	}
}

func TestWaitPastSinceVersionAnswersImmediately(t *testing.T) {
	srv := newUnauthServer(t)
	_, seed := statusRequest(srv, "", nil)
	v := int64(seed["version"].(float64))

	code, body := statusRequest(srv, "wait=1&timeoutMs=10000&sinceVersion="+itoa(v-1), nil)
	if code != http.StatusOK {
		t.Fatalf("status code = %d", code)
	}
	if got := int64(body["version"].(float64)); got < v {
		t.Fatalf("response version = %d, expected >= %d", got, v)
	}
}

func TestWaitHoldsUntilTimeout(t *testing.T) {
	srv := newUnauthServer(t)
	_, seed := statusRequest(srv, "", nil)
	v := int64(seed["version"].(float64))

	start := time.Now()
	code, body := statusRequest(srv, "wait=1&timeoutMs=1000&sinceVersion="+itoa(v), nil)
	elapsed := time.Since(start)

	if code != http.StatusOK {
		t.Fatalf("status code = %d", code)
	}
	if got := int64(body["version"].(float64)); got != v {
		t.Fatalf("timeout answer must carry the unchanged version: got %d, want %d", got, v)
	}
	if elapsed < 800*time.Millisecond {
		t.Fatalf("request should have been held ~1s, returned in %v", elapsed)
	}
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("timeout clamp broken: held for %v", elapsed)
	}
}

func TestWaitWakesOnChange(t *testing.T) {
	srv := newUnauthServer(t)
	_, seed := statusRequest(srv, "", nil)
	v := int64(seed["version"].(float64))

	result := make(chan map[string]any, 1)
	go func() {
		start := time.Now()
		code, body := statusRequest(srv, "wait=1&timeoutMs=5000&sinceVersion="+itoa(v), nil)
		if code != http.StatusOK {
			t.Errorf("status code = %d", code)
		}
		if waited := time.Since(start); waited > 2*time.Second {
			t.Errorf("woken wait should return quickly, took %v", waited)
		}
		result <- body
	}()

	// Wait a beat so the request is holding, then simulate a visible state
	// change (the refresh loop does this against live Spotify).
	time.Sleep(150 * time.Millisecond)
	srv.hub.publish(basePayload(map[string]any{"playing": false}))

	select {
	case body := <-result:
		if got := int64(body["version"].(float64)); got <= v {
			t.Fatalf("woken response must carry a newer version: got %d, want > %d", got, v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiter was not woken by a publish")
	}
}

func TestWaitDisconnectionLeaksNothing(t *testing.T) {
	srv := newUnauthServer(t)
	_, seed := statusRequest(srv, "", nil)
	v := int64(seed["version"].(float64))

	baseline := srv.hub.waiters()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		statusRequest(srv, "wait=1&timeoutMs=15000&sinceVersion="+itoa(v), ctx)
	}()

	time.Sleep(100 * time.Millisecond)
	if w := srv.hub.waiters(); w != baseline+1 {
		t.Fatalf("waiting client not registered: %d != %d+1", w, baseline)
	}
	cancel() // client goes away while holding

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("handler did not return after context cancel")
	}
	if w := srv.hub.waiters(); w != baseline {
		t.Fatalf("waiter counter leaked: %d != %d", w, baseline)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
