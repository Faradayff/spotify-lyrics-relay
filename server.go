package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type relayServer struct {
	spot   *spotifyClient
	lyrics *lyricsClient
	hub    *statusHub
	base   string

	// delayMs is the audio-delay compensation (ms) the client most recently
	// requested via the /status query. The wait-mode refresh loop re-reads it
	// so every published snapshot applies the same shift the app asked for.
	// Single-tenant: the last request wins. 0 = no delay.
	delayMu sync.Mutex
	delayMs int64
}

func newRelayServer(base string) *relayServer {
	return &relayServer{
		spot:   newSpotifyClient(),
		lyrics: newLyricsClient(),
		hub:    newStatusHub(),
		base:   base,
	}
}

// delayMsMax caps the client-requested compensation. 30 s is plenty for a car
// audio chain — the same ceiling the app's settings expose.
const delayMsMax = 30000

// parseDelayMs validates the client-supplied delayMs query parameter: absent
// or invalid means "no delay" (0), a negative value is rejected, and anything
// above the ceiling is clamped down.
func parseDelayMs(raw string) int64 {
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	if n > delayMsMax {
		n = delayMsMax
	}
	return n
}

func (s *relayServer) setDelay(ms int64) {
	s.delayMu.Lock()
	s.delayMs = ms
	s.delayMu.Unlock()
}

func (s *relayServer) delay() int64 {
	s.delayMu.Lock()
	defer s.delayMu.Unlock()
	return s.delayMs
}

// --- /status (the only route the car app consumes) ---

// handleStatus serves GET /status in two modes:
//   - plain (no wait param, or wait != "1"): compute fresh, publish, answer now
//     (identical behavior and body to pre-wait-mode relays, plus "version");
//   - wait mode (wait=1): hold the response until the visible fingerprint
//     changes past sinceVersion, timeoutMs elapses, or the client disconnects.
func (s *relayServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")

	q := r.URL.Query()
	// Apply the client's audio-delay compensation from this request onward;
	// the wait-mode refresh loop keeps using it for this client. Single-tenant:
	// the last request wins (0 = no delay).
	s.setDelay(parseDelayMs(q.Get("delayMs")))

	if q.Get("wait") != "1" {
		s.hub.publish(s.computeStatusPayload())
		s.writeStatus(w)
		return
	}

	since, err := strconv.ParseInt(q.Get("sinceVersion"), 10, 64)
	if err != nil || since <= 0 {
		// Client's state is unknown (first request): answer immediately with
		// a fresh computation, so the first answer costs zero wait time.
		s.hub.publish(s.computeStatusPayload())
		s.writeStatus(w)
		return
	}

	ch, v := s.hub.changedAndVersion()
	if v > since {
		s.writeStatus(w) // already changed: fast path
		return
	}

	s.hub.addWaiter()
	defer s.hub.dropWaiter()

	timer := time.NewTimer(waitTimeout(q.Get("timeoutMs")))
	defer timer.Stop()

	select {
	case <-ch: // fingerprint changed past everything the client already saw
	case <-timer.C: // backstop: answer with the current (unchanged) state
	case <-r.Context().Done(): // client went away: bail without answering
		return
	}
	s.writeStatus(w)
}

// writeStatus answers with the hub's current snapshot plus the version.
func (s *relayServer) writeStatus(w http.ResponseWriter) {
	p, v := s.hub.response()
	p["version"] = v
	writeJSON(w, http.StatusOK, p)
}

// computeStatusPayload builds the /status body (without "version") by
// querying the current playback state and resolving the lyrics, applying the
// client's audio-delay compensation to the active line.
func (s *relayServer) computeStatusPayload() map[string]any {
	delay := s.delay()
	if !s.spot.authorized() {
		return map[string]any{
			"ok": false, "auth": false, "delayMs": delay,
			"error": "spotify not authenticated; open the base path from a browser and click log in",
		}
	}
	st, err := s.spot.mePlayer()
	if err != nil {
		return map[string]any{
			"ok": false, "auth": true, "delayMs": delay, "error": err.Error(),
		}
	}

	now := time.Now()
	trackRaw := trackFromState(st)
	isPlaying := boolOr(st["is_playing"])
	position := intOr(st["progress_ms"])

	ti := buildTrackInfo(s.lyrics, trackRaw)
	// Resolve the active line at position - delay so the line the app renders
	// is the one the listener hears now (the head unit plays ~delay late).
	lyrData, lineIdx, lyrErr := s.lyricsFor(ti, position-int(delay))

	lyrSynced := lyrData != nil && lyrData.Synced
	lyrCount := 0
	if lyrData != nil {
		lyrCount = lyrData.LinesCount
	}

	payload := map[string]any{
		"ok":           true,
		"auth":         true,
		"playing":      isPlaying,
		"positionMs":   position,
		"updated":      now.UTC().Format(time.RFC3339),
		"track":        ti,
		"delayMs":      delay,
		"lyricsSynced": lyrSynced,
		"lyricsLines":  lyrCount,
		"lyricsStatus": lyricsStatus(lyrData, lineIdx),
	}
	for k, v := range lyricPayload(lyrData, lineIdx) {
		payload[k] = v
	}
	if lyrErr != nil {
		payload["error"] = lyrErr.Error()
	}
	return payload
}

func (s *relayServer) lyricsFor(ti *trackInfo, estMs int) (*lyricsData, int, error) {
	if ti == nil || ti.ID == "" {
		return nil, -1, nil
	}
	track := map[string]any{
		"id":          ti.ID,
		"name":        ti.Name,
		"duration_ms": ti.DurMs,
		"artists":     artistToAny(ti.Artist),
	}
	if ti.Album != "" {
		track["album"] = map[string]any{"name": ti.Album}
	}
	data, err := s.lyrics.getForTrack(track)
	if err != nil {
		return nil, -1, err
	}
	if data == nil || !data.Synced || len(data.Lines) == 0 {
		return data, -1, nil
	}
	return data, findLine(data.Lines, estMs), nil
}

// lyricPayload builds the lyric fields appended to /status: the active line
// (index + text), the line above it, and the upcoming karaoke window, or the
// plain (unsynced) text when that is the only version available. The full
// timestamped line list is never served: clients render the served line
// directly, so the response stays small for the weakest screens.
func lyricPayload(data *lyricsData, idx int) map[string]any {
	out := map[string]any{}
	if data == nil {
		return out
	}
	if data.Synced && idx >= 0 && idx < len(data.Lines) {
		out["line"] = idx
		out["lineText"] = data.Lines[idx].Text
		// The line above the active one, so the app can render a karaoke band
		// without carrying (and re-parsing) the full "lines" array.
		prev := ""
		if idx > 0 {
			prev = data.Lines[idx-1].Text
		}
		out["prevLine"] = prev
		end := idx + 1 + karaokeWindow
		if end > len(data.Lines) {
			end = len(data.Lines)
		}
		out["nextLines"] = data.Lines[idx+1 : end]
	} else if !data.Synced && data.Plain != "" {
		out["plain"] = data.Plain
	}
	return out
}

// lyricsStatus tells the client which lyric mode applies, so it can render
// one of three UIs unambiguously:
//   - "synced": timestamped lines are present (line/lineText/nextLines/lines)
//   - "plain":  only plain text is available (single "plain" field)
//   - "none":   no lyrics found for this track at all — show "no lyrics"
func lyricsStatus(data *lyricsData, idx int) string {
	if data != nil && data.Synced && idx >= 0 && len(data.Lines) > 0 {
		return "synced"
	}
	if data != nil && data.Plain != "" {
		return "plain"
	}
	return "none"
}

// karaokeWindow is how many upcoming lines are sent after the active one.
const karaokeWindow = 3

// findLine returns the active lyric line at time ms: the most recently
// STARTED line, i.e. the last one whose timestamp is <= ms. A line stays
// active until the next line actually begins, so sparse sections (long
// phrases with big gaps, e.g. Bohemian Rhapsody) keep the current line on
// screen for its full duration instead of jumping ahead at the midpoint.
// Positions before the first line show the first line; after the last one,
// the last one stays.
func findLine(lines []lyricLine, ms int) int {
	if len(lines) == 0 {
		return -1
	}
	idx := sort.Search(len(lines), func(i int) bool { return lines[i].T > int64(ms) })
	if idx == 0 {
		return 0 // before the first line starts: show the first
	}
	if idx == len(lines) {
		return len(lines) - 1 // after the last line: keep the last
	}
	return idx - 1
}

func buildTrackInfo(lc *lyricsClient, raw map[string]any) *trackInfo {
	if raw == nil {
		return nil
	}
	ti := &trackInfo{
		ID:     strOr(raw["id"]),
		Name:   strOr(raw["name"]),
		Artist: lc.artistName(raw),
		URI:    strOr(raw["uri"]),
	}
	if a, ok := raw["album"].(map[string]any); ok {
		ti.Album = strOr(a["name"])
		if imgs, ok2 := a["images"].([]any); ok2 {
			for i := len(imgs) - 1; i >= 0; i-- {
				if m, ok3 := imgs[i].(map[string]any); ok3 {
					if u, ok4 := m["url"].(string); ok4 && u != "" {
						ti.Cover = append(ti.Cover, u)
						break
					}
				}
			}
		}
	}
	ti.DurMs = intOr(raw["duration_ms"])
	return ti
}

type trackInfo struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Artist string   `json:"artist"`
	Album  string   `json:"album"`
	URI    string   `json:"uri"`
	DurMs  int      `json:"durMs"`
	Cover  []string `json:"cover,omitempty"`
}

// --- authentication ---

func (s *relayServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	url, err := s.spot.loginURL()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, url, http.StatusFound)
}

func (s *relayServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	errStr := r.URL.Query().Get("error")
	if errStr != "" {
		http.Error(w, "spotify returned an error: "+errStr, http.StatusBadRequest)
		return
	}
	if code == "" {
		http.Error(w, "missing code parameter", http.StatusBadRequest)
	}
	if err := s.spot.exchangeCode(code); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><html><head><meta charset="utf-8"></head><body style="background:#0d0d0d;color:#e8e8e8;font-family:monospace;padding:24px;">
<h3>OK, session saved</h3>
<p>The car app can now consume <code>GET status</code> on the same base path. You can close this window.</p>
</body></html>`)
}

func (s *relayServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.spot.logout()
	http.Redirect(w, r, s.base+"/", http.StatusFound)
}

func (s *relayServer) handleControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	_ = r.ParseForm()
	action := r.Form.Get("action")
	if err := s.spot.control(action); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": action})
}

// handleHealthz is a dependency-free liveness probe: it reports that the
// process is serving and whether a Spotify session exists, without calling
// Spotify or the lyrics API. It must always answer fast, because it backs
// the container health-check that runs on a 5 s budget. Always 200 (liveness,
// not readiness): a relay with no session is still a healthy process — the
// operator just needs a browser to log in.
func (s *relayServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"auth": s.spot.authorized(),
	})
}

// indexStateTimeout caps how long the index page waits on the Spotify API
// for its "playing/paused/status" line. mePlayer carries a 20 s client
// timeout; holding a request that long (or whatever is polling this route)
// for a status page is worse than a neutral "still loading" line.
const indexStateTimeout = 3 * time.Second

// indexStatus builds the Spotify line without being able to block the
// response for the full Spotify timeout: the API call runs in the
// background, and after indexStateTimeout we answer with a neutral status
// anyway (the call keeps running and simply stops being waited on; the
// buffered channel guarantees it can always write its result and exit).
func (s *relayServer) indexStatus(r *http.Request) string {
	type state struct {
		st  map[string]any
		err error
	}
	ch := make(chan state, 1)
	go func() {
		st, err := s.spot.mePlayer()
		ch <- state{st: st, err: err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			return "authenticated (error reading state)"
		}
		trackName, _ := trackFromState(res.st)["name"].(string)
		if trackName == "" {
			return "authenticated, no track"
		}
		if playing, _ := res.st["is_playing"].(bool); playing {
			return "playing: " + trackName
		}
		return "paused: " + trackName
	case <-time.After(indexStateTimeout):
		return "authenticated (state loading…)"
	case <-r.Context().Done():
		return "authenticated (state cancelled)"
	}
}

func (s *relayServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	status := "no session"
	if s.spot.authorized() {
		status = s.indexStatus(r)
	}
	fmt.Fprintf(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>lyrics-relay</title>
<style>body{font-family:monospace;background:#0d0d0d;color:#e8e8e8;padding:24px}
.box{border:1px solid #2a2a2a;padding:16px;margin-bottom:12px}
a{color:#1db954} code{background:#1a1a1a;padding:1px 4px}</style>
</head><body>
<h3>lyrics-relay</h3>
<div class="box">
  <b>Spotify</b>: %s<br>
  <b>Actions</b>: <a href="login">log in</a> &middot; <a href="logout">log out</a><br>
  <b>Car app</b>: <code>GET status</code> (JSON, CORS open)<br>
  <b>Controls</b>: <code>POST control?action=next|prev|pause|resume</code>
</div>
</body></html>
`, status)
}

// --- utilities ---

// trackFromState extracts the playing track from a playback-state object.
// Modern Spotify responses use "item"; older snapshots used "track".
func trackFromState(st map[string]any) map[string]any {
	if m, ok := st["item"].(map[string]any); ok && m != nil {
		return m
	}
	if m, ok := st["track"].(map[string]any); ok && m != nil {
		return m
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func strOr(v any) string {
	s, _ := v.(string)
	return s
}

func intOr(v any) int {
	f, ok := v.(float64)
	if !ok {
		return 0
	}
	return int(f)
}

func boolOr(v any) bool {
	b, _ := v.(bool)
	return b
}

func artistToAny(name string) []any {
	if name == "" {
		return []any{}
	}
	parts := strings.Split(name, ", ")
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		out = append(out, map[string]any{"name": strings.TrimSpace(p)})
	}
	return out
}
