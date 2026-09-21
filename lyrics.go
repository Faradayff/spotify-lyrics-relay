package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	lrclibBase = "https://lrclib.net/api"
	cacheTTL   = 7 * 24 * time.Hour
)

type lyricLine struct {
	T    int64  `json:"t"`
	Text string `json:"text"`
}

type lyricsData struct {
	Synced     bool        `json:"synced"`
	Lines      []lyricLine `json:"lines,omitempty"`
	Plain      string      `json:"plain,omitempty"`
	LinesCount int         `json:"linesCount,omitempty"`
}

type cacheEntry struct {
	at   time.Time
	data lyricsData
}

type lyricsClient struct {
	mu    sync.Mutex
	cache map[string]cacheEntry
	http  *http.Client
}

func newLyricsClient() *lyricsClient {
	return &lyricsClient{
		cache: map[string]cacheEntry{},
		http:  &http.Client{Timeout: 20 * time.Second},
	}
}

var reStamp = regexp.MustCompile(`\[(\d{1,3}):(\d{1,2})(?:[.:](\d{1,3}))?\]`)

func parseLRC(text string) []lyricLine {
	out := make([]lyricLine, 0, 64)
	for _, raw := range strings.Split(text, "\n") {
		var stamps []int64
		for _, sub := range reStamp.FindAllStringSubmatch(raw, -1) {
			min, err1 := strconv.Atoi(sub[1])
			sec, err2 := strconv.Atoi(sub[2])
			if err1 != nil || err2 != nil {
				continue
			}
			ms := int64(min)*60000 + int64(sec)*1000
			if sub[3] != "" {
				frac := sub[3]
				if len(frac) > 3 {
					frac = frac[:3]
				}
				for len(frac) < 3 {
					frac += "0"
				}
				if f, err := strconv.Atoi(frac); err == nil {
					ms += int64(f)
				}
			}
			stamps = append(stamps, ms)
		}
		if len(stamps) == 0 {
			continue
		}
		body := strings.TrimSpace(reStamp.ReplaceAllString(raw, ""))
		if body == "" {
			continue
		}
		for _, t := range stamps {
			out = append(out, lyricLine{T: t, Text: body})
		}
	}
	sortLines(out)
	return out
}

func sortLines(lines []lyricLine) {
	for i := 1; i < len(lines); i++ {
		for j := i; j > 0 && lines[j].T < lines[j-1].T; j-- {
			lines[j], lines[j-1] = lines[j-1], lines[j]
		}
	}
}

func (l *lyricsClient) artistName(track map[string]any) string {
	if track == nil {
		return ""
	}
	if a, ok := track["artists"].([]any); ok {
		var parts []string
		for _, item := range a {
			if m, ok := item.(map[string]any); ok {
				if n, ok := m["name"].(string); ok && n != "" {
					parts = append(parts, n)
				}
			}
		}
		return strings.Join(parts, ", ")
	}
	if v, ok := track["artist"].(string); ok {
		return v
	}
	return ""
}

func (l *lyricsClient) fetch(track map[string]any) (map[string]any, error) {
	name, _ := track["name"].(string)
	albumName := ""
	if a, ok := track["album"].(map[string]any); ok {
		albumName, _ = a["name"].(string)
	}
	durMs := 0
	switch v := track["duration_ms"].(type) {
	case float64:
		durMs = int(v)
	case int:
		durMs = v
	}
	q := url.Values{}
	q.Set("track_name", name)
	q.Set("artist_name", l.artistName(track))
	q.Set("album_name", albumName)
	if durMs > 0 {
		q.Set("duration", strconv.Itoa(durMs/1000))
	}
	u := lrclibBase + "/get?" + q.Encode()
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := l.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("lrclib %d: %s", resp.StatusCode, string(b))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{}, nil
	}
	return m, nil
}

// decodeLyrics maps a LRCLIB /get response (fields "syncedLyrics" /
// "plainLyrics") into lyricsData.
func decodeLyrics(m map[string]any) lyricsData {
	out := lyricsData{}
	if m == nil {
		return out
	}
	var synced, plain string
	if s, ok := m["syncedLyrics"].(string); ok {
		synced = s
	}
	if p, ok := m["plainLyrics"].(string); ok {
		plain = p
	}
	if lines := parseLRC(synced); len(lines) > 0 {
		return lyricsData{Synced: true, Lines: lines, Plain: plain, LinesCount: len(lines)}
	}
	if plain != "" {
		return lyricsData{Synced: false, Plain: plain}
	}
	return out
}

func (l *lyricsClient) getForTrack(track map[string]any) (*lyricsData, error) {
	if track == nil {
		return nil, fmt.Errorf("nil track")
	}
	key := fmt.Sprintf("%v", track["id"]) + "|" + l.artistName(track)
	l.mu.Lock()
	if e, ok := l.cache[key]; ok && time.Since(e.at) < cacheTTL {
		l.mu.Unlock()
		return &e.data, nil
	}
	l.mu.Unlock()

	m, err := l.fetch(track)
	if err != nil {
		return nil, err
	}
	out := decodeLyrics(m)
	l.mu.Lock()
	l.cache[key] = cacheEntry{at: time.Now(), data: out}
	l.mu.Unlock()
	return &out, nil
}
