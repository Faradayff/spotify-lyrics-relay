package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	lrclibBase = "https://lrclib.net/api"
	// Default lyrics-cache policy (all overridable via env in
	// newLyricsClient; 0 always means "never expires" / "unlimited"). A
	// song's lyrics are effectively immutable, so a long TTL plus the
	// on-disk cache keep repeated plays of a known track off the wire.
	defaultLyricsTTLDays = 365 // resolved lyrics (LYRICS_TTL_DAYS)
	defaultNoneTTLDays   = 1   // "no lyrics" (404), memory only (LYRICS_TTL_NONE_DAYS)
	defaultMaxCacheMB    = 64  // on-disk budget (LYRICS_CACHE_MAX_MB)
	// memCapDefault bounds the in-memory layer: entries beyond it (oldest
	// first) leave the map but stay on disk, so the cost is one small file
	// re-read the next time the track plays.
	memCapDefault = 20000
	// janitorInterval: cadence of the periodic cache sweep (expired entries
	// + disk budget). Weekly: the budgets are generous and lyrics rarely
	// churn; the hot paths (startup, each new entry) already enforce.
	janitorInterval = 7 * 24 * time.Hour
	// errorCacheTTL: how long a failed provider fetch (429/5xx/transport)
	// is replayed as "pending" instead of being re-requested. Doubles with
	// each consecutive failure per track (see errWindowFor), capped by
	// maxErrWindow.
	errorCacheTTL = 60 * time.Second
	// maxFetchAttempts: inline re-attempts, reserved for TRANSPORT errors
	// only (socket errors, the request never reached the provider: safe to
	// retry). Any HTTP answer from the provider is treated as final for the
	// lookup — lrcLib is a free service that throttles hard, so re-spamming
	// it through an overload window only deepens the storm. The lookup
	// hands off to the backoff windows (per track + global) instead.
	maxFetchAttempts = 3
	// maxErrWindow: cap for the per-track AND the global backoff windows.
	maxErrWindow = 15 * time.Minute
	// minUpstreamGap: minimum spacing between real provider requests to
	// DIFFERENT tracks (shuffle burst, app + curl at once). A request that
	// would land earlier is deferred to the next refresh tick, so no burst
	// of new songs can stack requests second after second.
	minUpstreamGap = 2 * time.Second
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

// providerError is a non-2xx answer from the lyrics provider. 429 (lrcLib's
// "ServerOverloaded") and 5xx are transient: they are ridden out through
// the backoff windows instead of surfacing as an error.
type providerError struct {
	status     int
	detail     string        // truncated upstream answer body
	retryAfter time.Duration // from the Retry-After header, 0 when absent
}

func (e *providerError) Error() string {
	return fmt.Sprintf("lrclib %d: %s", e.status, e.detail)
}

// lyricsPending is the "not yet" state of a track's lyrics: the provider
// refused (429/5xx), is still answering, or the relay is deliberately inside
// a backoff window. It is NOT an error to surface: the payload reports the
// "loading" state and the relay retries on its own schedule (per-track
// window, global window), so the client only ever sees a state transition
// into synced/plain, never a lyrics API failure.
type lyricsPending struct {
	cause   error         // the underlying provider/transport failure, nil for a plain deferral
	retryIn time.Duration // ≈ time until the next attempt (0 = already in flight)
}

func (p *lyricsPending) Error() string {
	if p.cause != nil {
		return p.cause.Error()
	}
	return "lyrics pending"
}

func lyricsPendingError(cause error, retryIn time.Duration) error {
	if retryIn < 0 {
		retryIn = 0
	}
	return &lyricsPending{cause: cause, retryIn: retryIn}
}

// asLyricsPending reports whether err (possibly wrapped) is a pending state.
func asLyricsPending(err error) (*lyricsPending, bool) {
	var p *lyricsPending
	if errors.As(err, &p) {
		return p, true
	}
	return nil, false
}

// cacheEntry holds either a successful lookup (data) or a provider failure
// (err). err entries are replayed as the pending state for the entry's
// window, so the wait-mode refresh loop never re-hits the upstream on
// every 300 ms tick while a backoff window is open.
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
	// tick (or app + curl at once) reports "pending" instead of duplicating
	// the request on the provider.
	inflight map[string]struct{}
	// globalWindowUntil: no real request for ANY track before this instant.
	// A provider refusal is client-wide (lrcLib limits us as a whole), so a
	// 429/5xx on one track also defers every other pending lookup until the
	// window opens — during an overload storm the relay makes roughly one
	// real request per window instead of one per new song.
	globalWindowUntil time.Time
	// lastUpstreamReq: when the last real provider request went out; with
	// minGap it spaces out request bursts (see getForTrack).
	lastUpstreamReq time.Time
	// now: injectable clock (tests) for the window arithmetic.
	now func() time.Time
	// minGap: minimum spacing between real provider requests (tests shorten).
	minGap time.Duration
	// logf: provider-activity logger (tests mute it).
	logf      func(format string, args ...any)
	errTTL    time.Duration
	retryBase []time.Duration // inline waits between transport-error retries (tests shorten)
	sleep     func(time.Duration)
	http      *http.Client

	// stateDir: where persisted state lives ($STATE_DIR in the container,
	// "data" otherwise); resolved lyrics are stored in stateDir/lyrics/.
	stateDir string
	// ttl: how long a RESOLVED lyric stays valid — in memory and on disk
	// (0 = never expires). LYRICS_TTL_DAYS.
	ttl time.Duration
	// noLyricsTTL: how long a 404/"no lyrics" result is remembered (memory
	// only, never persisted): a song can gain lyrics later, so it must be
	// re-checked within this window (0 = never re-check).
	// LYRICS_TTL_NONE_DAYS.
	noLyricsTTL time.Duration
	// maxSize: the on-disk cache budget in bytes (0 = unlimited).
	// LYRICS_CACHE_MAX_MB.
	maxSize int64
	// memCap: in-memory entry cap (0 = unlimited).
	memCap int
}

func newLyricsClient() *lyricsClient {
	sd := os.Getenv("STATE_DIR")
	if sd == "" {
		sd = "data"
	}
	l := &lyricsClient{
		base:        lrclibBase,
		cache:       map[string]cacheEntry{},
		inflight:    map[string]struct{}{},
		now:         time.Now,
		minGap:      minUpstreamGap,
		logf:        log.Printf,
		errTTL:      errorCacheTTL,
		retryBase:   []time.Duration{750 * time.Millisecond, 2 * time.Second},
		sleep:       time.Sleep,
		http:        &http.Client{Timeout: 20 * time.Second},
		stateDir:    sd,
		ttl:         time.Duration(envNonNeg("LYRICS_TTL_DAYS", defaultLyricsTTLDays)) * 24 * time.Hour,
		noLyricsTTL: time.Duration(envNonNeg("LYRICS_TTL_NONE_DAYS", defaultNoneTTLDays)) * 24 * time.Hour,
		maxSize:     int64(envNonNeg("LYRICS_CACHE_MAX_MB", defaultMaxCacheMB)) << 20,
		memCap:      memCapDefault,
	}
	// Startup: apply the disk budget (drop the oldest files past it). TTL
	// expiry is per-lookup (each entry is age-checked on read) and the
	// weekly janitor sweeps the rest.
	l.enforceDiskLimit()
	return l
}

// envNonNeg reads a non-negative integer env var, falling back to def when
// it is absent, non-numeric or negative (a negative encodes no valid policy).
func envNonNeg(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
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

// fetch resolves a track at the provider. Any HTTP answer the provider
// gives settles the lookup on the spot — a 429 "ServerOverloaded" / 5xx is
// backoff-window business (per track + global) and re-spamming an already
// overloaded free service only deepens the storm. Inline re-attempts are
// kept for transport errors alone: the request never reached the provider,
// so retrying it is free. It blocks at most the inline waits between
// attempts per lookup.
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
			i := attempt - 2 // the wait before attempt 2, 3, ...
			if len(l.retryBase) > 0 {
				if i >= len(l.retryBase) {
					i = len(l.retryBase) - 1
				}
				if wait := l.retryBase[i]; wait > 0 {
					l.sleep(wait)
				}
			}
		}
		m, p, err := l.fetchOnce(u)
		if p == nil && err == nil {
			return m, nil, nil // 2xx, or a 404 ("no lyrics for this track")
		}
		perr, lastErr = p, err
		if p != nil {
			break // an HTTP answer from the provider is final for this lookup
		}
		// transport error: retry inline (the provider never saw it)
	}
	return nil, perr, lastErr
}

// fetchOnce performs a single GET against the provider. Every outcome is
// logged (stderr / container logs) so the actual provider answer behind any
// "loading" state is always diagnosable.
func (l *lyricsClient) fetchOnce(u string) (map[string]any, *providerError, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := l.http.Do(req)
	if err != nil {
		l.logf("lyrics provider %s: transport error: %v", u, err)
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		l.logf("lyrics provider: no lyrics (HTTP 404) for %s", u)
		return nil, nil, nil // the track simply has no lyrics there
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		detail := string(b)
		if len(detail) > 160 {
			detail = detail[:160] + "…"
		}
		l.logf("lyrics provider: HTTP %d %q for %s", resp.StatusCode, detail, u)
		return nil, &providerError{
			status:     resp.StatusCode,
			detail:     string(b),
			retryAfter: retryAfterFrom(resp),
		}, nil
	}
	l.logf("lyrics provider: ok (HTTP 200) for %s", u)
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		l.logf("lyrics provider %s: read error after HTTP 200: %v", u, err)
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

// --- persistent cache: one JSON file per resolved track, under STATE_DIR/lyrics ---

// persistentEntry is the on-disk form of a resolved lyrics entry. The key
// round-trips so the files are self-describing (diagnostics / cache report);
// the memory map stays keyed by the human-readable "id|artist" form.
type persistentEntry struct {
	Key  string     `json:"key"` // "spotifyTrackId|artistName"
	At   int64      `json:"at"`  // fetch time, unix milliseconds
	Data lyricsData `json:"data"`
}

func (l *lyricsClient) cacheDir() string {
	return filepath.Join(l.stateDir, "lyrics")
}

// diskFileName hashes the cache key: track ids and artist names can carry
// characters a filesystem should not have to deal with on file names.
func diskFileName(key string) string {
	sum := sha1.Sum([]byte(key))
	return hex.EncodeToString(sum[:]) + ".json"
}

func readPersistentEntry(path string) (persistentEntry, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return persistentEntry{}, false
	}
	var pe persistentEntry
	if err := json.Unmarshal(b, &pe); err != nil || pe.Key == "" || pe.At <= 0 {
		return persistentEntry{}, false
	}
	return pe, true
}

// storeToDisk persists a resolved lyric. Best-effort: a disk write failure
// must never fail the lookup (the entry still lives in memory).
func (l *lyricsClient) storeToDisk(key string, data lyricsData) {
	dir := l.cacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		l.logf("lyrics cache: cannot create %s: %v", dir, err)
		return
	}
	b, err := json.Marshal(persistentEntry{Key: key, At: l.now().UnixMilli(), Data: data})
	if err != nil {
		return
	}
	path := filepath.Join(dir, diskFileName(key))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		l.logf("lyrics cache: write %s: %v", path, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil { // atomic rename inside the same volume
		l.logf("lyrics cache: rename %s: %v", path, err)
		_ = os.Remove(tmp)
	}
}

// readFromDisk is the disk read-through: it returns a FRESH entry for the
// key, or a miss (deleting the file first) when it is corrupt, belongs to a
// different key, or is past its TTL. A missing file is a plain miss.
func (l *lyricsClient) readFromDisk(key string) (cacheEntry, bool) {
	path := filepath.Join(l.cacheDir(), diskFileName(key))
	pe, ok := readPersistentEntry(path)
	if !ok || pe.Key != key {
		_ = os.Remove(path) // corrupt or mismatched: drop it (no-op if absent)
		return cacheEntry{}, false
	}
	at := time.UnixMilli(pe.At)
	ttl := l.ttlFor(pe.Data)
	if ttl > 0 && l.now().Sub(at) >= ttl {
		_ = os.Remove(path) // expired: re-resolve against the provider
		return cacheEntry{}, false
	}
	return cacheEntry{at: at, data: pe.Data}, true
}

// ttlFor picks the TTL of a cached result: resolved content uses the long
// TTL, a 404/"no lyrics" one the short one (the song may gain lyrics later
// and must be re-checked).
func (l *lyricsClient) ttlFor(data lyricsData) time.Duration {
	if hasLyricsContent(data) {
		return l.ttl
	}
	return l.noLyricsTTL
}

// hasLyricsContent reports whether a lookup resolved to actual lyrics (as
// opposed to a 404 "no lyrics" / empty response).
func hasLyricsContent(d lyricsData) bool {
	return d.Synced || d.Plain != "" || len(d.Lines) > 0
}

// enforceMemCap trims the in-memory cache to memCap entries, dropping the
// OLDEST resolved ones — their files stay on disk and are re-read on demand.
// Error entries are never evicted: they carry the backoff state that keeps
// the refresh loop from re-hitting the provider. Called with l.mu held.
func (l *lyricsClient) enforceMemCap() {
	if l.memCap <= 0 {
		return
	}
	type kv struct {
		k  string
		at time.Time
	}
	list := make([]kv, 0, len(l.cache))
	for k, e := range l.cache {
		if e.err == nil {
			list = append(list, kv{k, e.at})
		}
	}
	if over := len(list) - l.memCap; over > 0 {
		sort.Slice(list, func(i, j int) bool { return list[i].at.Before(list[j].at) })
		for _, it := range list[:over] {
			delete(l.cache, it.k)
		}
	}
}

// enforceDiskLimit trims the on-disk cache to maxSize bytes, dropping the
// OLDEST files first. It uses file mtimes (write time ≈ fetch time), so no
// file needs a full parse.
func (l *lyricsClient) enforceDiskLimit() {
	if l.stateDir == "" || l.maxSize <= 0 {
		return
	}
	dir := l.cacheDir()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type df struct {
		path  string
		mtime time.Time
		size  int64
	}
	files := make([]df, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, df{filepath.Join(dir, e.Name()), fi.ModTime(), fi.Size()})
	}
	// Newest first: keep each file while the budget allows it.
	sort.Slice(files, func(i, j int) bool { return files[i].mtime.After(files[j].mtime) })
	budget := l.maxSize
	for _, f := range files {
		if f.size > budget { // over budget: evict (this walks oldest last)
			_ = os.Remove(f.path)
			continue
		}
		budget -= f.size
	}
}

// sweepExpiredMemory drops in-memory resolved entries past their TTL (error
// entries keep their backoff semantics: their window is always short and
// self-clearing).
func (l *lyricsClient) sweepExpiredMemory() {
	l.mu.Lock()
	now := l.now()
	for k, e := range l.cache {
		if e.err != nil {
			continue
		}
		ttl := l.ttlFor(e.data)
		if ttl > 0 && now.Sub(e.at) >= ttl {
			delete(l.cache, k)
		}
	}
	l.mu.Unlock()
}

// sweepExpiredDisk drops on-disk files past their TTL (and corrupt files
// outright).
func (l *lyricsClient) sweepExpiredDisk() {
	if l.stateDir == "" {
		return
	}
	dir := l.cacheDir()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		pe, ok := readPersistentEntry(path)
		if !ok {
			_ = os.Remove(path)
			continue
		}
		ttl := l.ttlFor(pe.Data)
		if ttl > 0 && l.now().Sub(time.UnixMilli(pe.At)) >= ttl {
			_ = os.Remove(path)
		}
	}
}

// sweep is the full janitor pass: expire the memory and disk entries, then
// re-apply the disk budget.
func (l *lyricsClient) sweep() {
	l.sweepExpiredMemory()
	l.sweepExpiredDisk()
	l.enforceDiskLimit()
}

// runJanitor runs a full sweep once every interval. Only the long-lived
// process (main) starts it; tests drive the individual passes directly.
func (l *lyricsClient) runJanitor(interval time.Duration) {
	tick := time.NewTicker(interval)
	for range tick.C {
		l.sweep()
	}
}

// clear wipes the WHOLE lyrics cache: the in-memory map (resolved entries
// and backoff states) and the on-disk files. It reports how many entries
// and files were removed and how many bytes were freed.
func (l *lyricsClient) clear() (int, int64, error) {
	l.mu.Lock()
	removed := len(l.cache)
	for k := range l.cache {
		delete(l.cache, k)
	}
	l.mu.Unlock()

	if l.stateDir == "" {
		return removed, 0, nil
	}
	dir := l.cacheDir()
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return removed, 0, nil
		}
		return removed, 0, err
	}
	var freed int64
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if fi, err := e.Info(); err == nil {
			freed += fi.Size()
		}
		if err := os.Remove(path); err != nil {
			return removed, freed, err
		}
		removed++
	}
	return removed, freed, nil
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
	now := l.now()
	if e, ok := l.cache[key]; ok {
		age := now.Sub(e.at)
		if e.err != nil {
			if w := l.errWindow(e); age < w {
				// Replay the pending state without re-requesting the
				// provider: the wait-mode loop recomputes every 300 ms,
				// and without this a backoff window would turn into a
				// request per tick.
				l.mu.Unlock()
				return nil, lyricsPendingError(e.err, w-age)
			}
		} else if ttl := l.ttlFor(e.data); ttl == 0 || age < ttl {
			l.mu.Unlock()
			return &e.data, nil // cached success: zero provider traffic
		}
	}
	l.mu.Unlock()

	// Persistent cache (the on-disk file, survives restarts): a fresh entry
	// resolves with zero provider traffic, even while a backoff window is
	// open — a disk read is not a provider request. Expired or corrupt
	// files drop themselves (readFromDisk).
	if l.stateDir != "" {
		if e, ok := l.readFromDisk(key); ok {
			l.mu.Lock()
			l.cache[key] = e
			l.enforceMemCap()
			l.mu.Unlock()
			return &e.data, nil
		}
	}

	l.mu.Lock()
	now = l.now()
	// Provider overload is client-wide (lrcLib limits US, not the track):
	// while the global backoff window is open, no track at all may fire a
	// real request. The refresh loop retries this track when it opens.
	if rem := l.globalWindowUntil.Sub(now); rem > 0 {
		l.mu.Unlock()
		return nil, lyricsPendingError(nil, rem)
	}
	// Burst spacing: never stack more than one real request per minGap, so
	// a shuffle burst resolves at one request a few seconds apart instead
	// of one per new song back to back. The next refresh tick picks it up.
	if rem := l.lastUpstreamReq.Add(l.minGap).Sub(now); rem > 0 {
		l.mu.Unlock()
		return nil, lyricsPendingError(nil, rem)
	}
	if _, busy := l.inflight[key]; busy {
		// Another caller is already resolving this key: report "loading"
		// for this tick and let the next one retry instead of stacking a
		// duplicate request on the provider.
		l.mu.Unlock()
		return nil, lyricsPendingError(nil, 0)
	}
	l.inflight[key] = struct{}{}
	l.lastUpstreamReq = now
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.inflight, key)
		l.mu.Unlock()
	}()

	m, perr, ferr := l.fetch(track)
	var cause error
	if perr != nil {
		cause = perr
	} else {
		cause = ferr
	}
	out := decodeLyrics(m)

	// Persist the resolved lyrics (best-effort: a disk problem must never
	// fail the lookup). A 404 "no lyrics" result is deliberately NOT
	// written to disk — it keeps the short memory-only re-check TTL, so a
	// song that gains lyrics later is picked up again.
	if l.stateDir != "" && hasLyricsContent(out) {
		l.storeToDisk(key, out)
		l.enforceDiskLimit()
	}

	var retryIn time.Duration
	l.mu.Lock()
	now = l.now()
	// While the failures pile up for this key, back the error window off
	// (a success resets the fail count).
	prevFails := 0
	if prev, ok := l.cache[key]; ok && prev.err != nil {
		prevFails = prev.fails
	}
	e := cacheEntry{at: now, data: out}
	if cause != nil {
		retryIn = l.errWindowFor(perr, prevFails)
		e.err = cause
		e.errTTL = retryIn
		e.fails = prevFails + 1
		l.applyGlobalWindow(perr, now)
	}
	l.cache[key] = e
	l.enforceMemCap()
	l.mu.Unlock()
	if cause != nil {
		// Pending, not an error: the surface shows "loading" and the relay
		// itself re-tries on the window schedule.
		return nil, lyricsPendingError(cause, retryIn)
	}
	return &out, nil
}

// applyGlobalWindow extends the client-wide backoff window when the
// provider answered with a refusal (429/5xx) or the transport failed: the
// refusal is about us as a client, so no other track may fire its own
// request until the window opens either. Called with l.mu held.
func (l *lyricsClient) applyGlobalWindow(perr *providerError, now time.Time) {
	w := l.errTTL
	if perr != nil && perr.status == http.StatusTooManyRequests && perr.retryAfter > 0 {
		w = perr.retryAfter
	}
	if w < l.errTTL {
		w = l.errTTL
	}
	if w > maxErrWindow {
		w = maxErrWindow
	}
	until := now.Add(w)
	if until.After(l.globalWindowUntil) {
		l.globalWindowUntil = until
		l.logf("lyrics provider refused — global backoff until %s (%v)", until.UTC().Format(time.RFC1123), w)
	}
}
