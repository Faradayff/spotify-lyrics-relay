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
	// errorCacheTTL: how long a failed provider fetch (429/5xx) is replayed
	// from the cache instead of being re-requested. Long enough to ride out a
	// rate-limit window, short enough to retry soon.
	errorCacheTTL = 60 * time.Second
	// maxFetchAttempts: how many requests a single lookup spends on the
	// provider before giving up and handing the failure to the
	// error-window cache. lrcLib is a free service that throttles bursts,
	// so a first request hitting its limiter does not mean the lyrics are
	// absent — we retry it inline before surfacing an error.
	maxFetchAttempts = 3
	// maxInlineWait: how long a /status call may block honoring a
	// Retry-After inline. Longer windows are honored through the
	// error-window cache instead of sleeping in the request path.
	maxInlineWait = 10 * time.Second
	// maxErrWindow: cap for the exponential error-window backoff when a
	// track keeps failing.
	maxErrWindow = 15 * time.Minute
	// userAgent: lrcLib asks every client to identify itself with name and a
	// link to its project (https://lrclib.net/docs, "we require you to
	// identify your client"). The Go default UA marks us as a generic
	// bot — a likely reason for being throttled with a 503.
	userAgent = "spotify-lyrics-relay (https://github.com/Faradayff/spotify-lyrics-relay)"
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

// providerError is a non-2xx answer from the lyrics provider. 429/5xx
// (lrcLib's "ServerOverloaded") are transient: worth retrying inline and,
// if that still fails, riding out through the error window.
type providerError struct {
	status     int
	detail     string        // truncated upstream answer body
	retryAfter time.Duration // from the Retry-After header, 0 when absent
}

func (e *providerError) Error() string {
	return fmt.Sprintf("lrclib %d: %s", e.status, e.detail)
}

// retryable reports whether the provider asked to be tried again later.
func (e *providerError) retryable() bool {
	return e.status == http.StatusTooManyRequests || e.status >= 500
}

// cacheEntry holds either a successful lookup (data) or a provider failure
// (err) so the wait-mode refresh loop can replay it for a short window
// instead of hammering the upstream again on every 300 ms tick.
type cacheEntry struct {
	at   time.Time
	data lyricsData
	err  error
	// errTTL is the effective replay window for err: the provider's
	// Retry-After when it sent one, otherwise the base error window backed
	// off exponentially while the failures pile up (0 = client default).
	errTTL time.Duration
	// fails counts consecutive provider failures for this key (a success
	// resets it) and feeds the exponential error-window backoff.
	fails int
}

type lyricsClient struct {
	mu    sync.Mutex
	base  string
	cache map[string]cacheEntry
	// inflight marks keys a fetch is already running for, so a concurrent
	// tick (or app + curl at once) skips its own request instead of
	// duplicating it.
	inflight  map[string]struct{}
	errTTL    time.Duration
	retryBase []time.Duration // inline waits between attempts (tests shorten)
	sleep     func(time.Duration)
	http      *http.Client
}

func newLyricsClient() *lyricsClient {
	return &lyricsClient{
		base:      lrclibBase,
		cache:     map[string]cacheEntry{},
		inflight:  map[string]struct{}{},
		errTTL:    errorCacheTTL,
		retryBase: []time.Duration{750 * time.Millisecond, 2 * time.Second},
		sleep:     time.Sleep,
		http:      &http.Client{Timeout: 20 * time.Second},
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

// fetch resolves a track at the provider, retrying transient failures
// (429/5xx) inline before surfacing an error: lrcLib is a small free
// service that answers intermittently with "ServerOverloaded", and the
// error-window cache alone would take a whole minute to notice that the
// storm has passed. It never blocks longer than maxInlineWait per lookup.
func (l *lyricsClient) fetch(track map[string]any) (map[string]any, *providerError, error) {
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
	u := l.base + "/get?" + q.Encode()

	var perr *providerError
	var lastErr error
	for attempt := 1; attempt <= maxFetchAttempts; attempt++ {
		if attempt > 1 {
			// Wait out the provider's window before retrying. A longer
			// window (big Retry-After) is not slept inline: the error
			// window carries it and the next lookup retries for real.
			wait := l.retryWait(perr, attempt)
			if wait > maxInlineWait {
				break
			}
			l.sleep(wait)
		}
		m, p, err := l.fetchOnce(u)
		if p == nil && err == nil {
			return m, nil, nil // 2xx, or a 404 ("no lyrics for this track")
		}
		perr, lastErr = p, err
		if p != nil && !p.retryable() {
			break // 4xx (other than 429): retrying will not help
		}
	}
	return nil, perr, lastErr
}

// retryWait is the inline wait before the given attempt: the provider's
// Retry-After for a 429, otherwise the exponential base.
func (l *lyricsClient) retryWait(perr *providerError, attempt int) time.Duration {
	if perr != nil && perr.status == http.StatusTooManyRequests && perr.retryAfter > 0 {
		return perr.retryAfter
	}
	if len(l.retryBase) == 0 {
		return time.Second
	}
	i := attempt - 2 // the wait before attempt 2, 3, ...
	if i < 0 {
		i = 0
	}
	if i >= len(l.retryBase) {
		i = len(l.retryBase) - 1
	}
	return l.retryBase[i]
}

// fetchOnce performs a single GET against the provider.
func (l *lyricsClient) fetchOnce(u string) (map[string]any, *providerError, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := l.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil, nil // the track simply has no lyrics there
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, &providerError{
			status:     resp.StatusCode,
			detail:     string(b),
			retryAfter: retryAfterFrom(resp),
		}, nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{}, nil, nil
	}
	return m, nil, nil
}

// retryAfterFrom parses a plain-seconds Retry-After header (0 when absent).
func retryAfterFrom(resp *http.Response) time.Duration {
	n, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
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

// errWindow is the effective replay window for a cached provider failure.
func (l *lyricsClient) errWindow(e cacheEntry) time.Duration {
	if e.errTTL > 0 {
		return e.errTTL
	}
	return l.errTTL
}

// errWindowFor decides how long a fresh provider failure stays cached: the
// provider's Retry-After when it sent one (clamped), otherwise the base
// window doubled for each consecutive failure of the same track.
func (l *lyricsClient) errWindowFor(perr *providerError, prevFails int) time.Duration {
	if perr != nil && perr.status == http.StatusTooManyRequests && perr.retryAfter > 0 {
		w := perr.retryAfter
		if w < l.errTTL {
			w = l.errTTL
		}
		if w > maxErrWindow {
			w = maxErrWindow
		}
		return w
	}
	shift := prevFails
	if shift > 5 {
		shift = 5
	}
	w := l.errTTL << shift
	if w <= 0 {
		w = l.errTTL
	}
	if w > maxErrWindow {
		w = maxErrWindow
	}
	return w
}

func (l *lyricsClient) getForTrack(track map[string]any) (*lyricsData, error) {
	if track == nil {
		return nil, fmt.Errorf("nil track")
	}
	key := fmt.Sprintf("%v", track["id"]) + "|" + l.artistName(track)
	l.mu.Lock()
	if e, ok := l.cache[key]; ok {
		age := time.Since(e.at)
		if e.err != nil && age < l.errWindow(e) {
			// Replay the recent provider failure without re-requesting it:
			// the wait-mode loop recomputes every 300 ms, and without this
			// each tick re-rains LRCLIB during a 429 window.
			l.mu.Unlock()
			return nil, e.err
		}
		if e.err == nil && age < cacheTTL {
			l.mu.Unlock()
			return &e.data, nil
		}
	}
	if _, busy := l.inflight[key]; busy {
		// Another caller is already resolving this key: skip this tick
		// (the payload simply has no lyrics for those few hundred ms)
		// and let the next tick retry instead of stacking a duplicate
		// request on the provider.
		l.mu.Unlock()
		return nil, nil
	}
	l.inflight[key] = struct{}{}
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.inflight, key)
		l.mu.Unlock()
	}()

	m, perr, ferr := l.fetch(track)
	var toSave error
	if perr != nil {
		toSave = perr
	} else {
		toSave = ferr
	}
	out := decodeLyrics(m)
	l.mu.Lock()
	// While the failures pile up for this key, back the error window off
	// (a success resets the fail count).
	prevFails := 0
	if prev, ok := l.cache[key]; ok && prev.err != nil {
		prevFails = prev.fails
	}
	e := cacheEntry{at: time.Now(), data: out, err: toSave}
	if toSave != nil {
		e.errTTL = l.errWindowFor(perr, prevFails)
		e.fails = prevFails + 1
	}
	l.cache[key] = e
	l.mu.Unlock()
	return &out, toSave
}
