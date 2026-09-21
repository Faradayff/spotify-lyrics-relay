package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

type relayServer struct {
	spot   *spotifyClient
	lyrics *lyricsClient
	base   string
}

func newRelayServer(base string) *relayServer {
	return &relayServer{
		spot:   newSpotifyClient(),
		lyrics: newLyricsClient(),
		base:   base,
	}
}

// --- /status (the only route the car app consumes) ---

func (s *relayServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")

	if !s.spot.authorized() {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "auth": false, "error": "spotify not authenticated; open the base path from a browser and click log in",
		})
		return
	}
	st, err := s.spot.mePlayer()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "auth": true, "error": err.Error(),
		})
		return
	}

	now := time.Now()
	trackRaw := trackFromState(st)
	isPlaying := boolOr(st["is_playing"])
	position := intOr(st["progress_ms"])

	ti := buildTrackInfo(s.lyrics, trackRaw)
	lineIdx, lyrSynced, lyrCount, lyrErr := s.lyricsFor(ti, position)

	resp := map[string]any{
		"ok":           true,
		"auth":         true,
		"playing":      isPlaying,
		"positionMs":   position,
		"updated":      now.UTC().Format(time.RFC3339),
		"track":        ti,
		"lyricsSynced": lyrSynced,
		"lyricsLines":  lyrCount,
	}
	if lyrErr != nil {
		resp["error"] = lyrErr.Error()
	}
	if lineIdx >= 0 {
		resp["line"] = lineIdx
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *relayServer) lyricsFor(ti *trackInfo, estMs int) (int, bool, int, error) {
	if ti == nil || ti.ID == "" {
		return -1, false, 0, nil
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
		return -1, false, 0, err
	}
	if data == nil || (!data.Synced && data.Plain == "") {
		return -1, false, 0, nil
	}
	if !data.Synced {
		return -1, false, 0, nil
	}
	idx := findLine(data.Lines, estMs)
	return idx, true, len(data.Lines), nil
}

func findLine(lines []lyricLine, ms int) int {
	if len(lines) == 0 {
		return -1
	}
	idx := sort.Search(len(lines), func(i int) bool { return lines[i].T > int64(ms) })
	if idx == len(lines) {
		idx = len(lines) - 1
	}
	if idx > 0 {
		if int64(ms)-lines[idx-1].T <= lines[idx].T-int64(ms) {
			idx--
		}
	}
	return idx
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

func (s *relayServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	authorized := s.spot.authorized()
	status, trackName := "no session", ""
	if authorized {
		if st, err := s.spot.mePlayer(); err == nil {
			trackName, _ = trackFromState(st)["name"].(string)
			playing, _ := st["is_playing"].(bool)
			if trackName == "" {
				status = "authenticated, no track"
			} else if playing {
				status = "playing: " + trackName
			} else {
				status = "paused: " + trackName
			}
		} else {
			status = "authenticated (error reading state)"
		}
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
