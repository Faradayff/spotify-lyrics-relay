# spotify-lyrics-relay: implement the long-poll ("wait") mode

> **Audience**: an agent (or developer) working in the `spotify-lyrics-relay`
> repository. This document describes **exactly what to change and why**.
> The Android client side is already implemented and tested — this document is
> written so that both sides can be developed independently and checked
> against each other with the acceptance checklist at the end.

## Context — why this exists

The Android app (Parrot Asteroid head unit, **Android 2.3.7, single slow ARM
core**) renders the karaoke lyrics from the relay's `GET /status`. When the
user wants tight sync they can set the polling interval to 300 ms. At that
cadence the head unit pays, every 300 ms:

- a full TCP/HTTP round trip through the reverse proxy (Android 2.3.7's
  `HttpURLConnection` does not reuse connections),
- a full JSON parse of a multi-KB payload (the app only uses a few fields),
- allocation churn → Dalvik GC stutter.

That is what shows up as "performance problems at 300 ms". The fix is to
**flip the direction of the traffic**: instead of the car asking "did anything
change?" 3.3 times per second, the relay **holds the request open and answers
the moment something changes** (with a timeout as the backstop). Result:

| | classic polling (300 ms) | wait mode |
|---|---|---|
| requests while a song plays | ~3.3/s | ~1 per lyric line (every few seconds) |
| head-unit CPU while waiting | parse + render each cycle | ~0 (a socket read) |
| perceived latency of a line change | ≤ 300 ms | ≤ (relay's own Spotify refresh cadence) |

## The contract

Everything extends the **existing** `GET /status` endpoint. No new endpoint,
no new headers, no new auth. Old clients are untouched.

### 1. New field in every `/status` response: `version`

```json
{
  "ok": true, "auth": true, "playing": true, "positionMs": 42000,
  "track": { "id": "…", "name": "…", "artist": "…", "album": "…", "durMs": 354000, "cover": ["…"] },
  "lyricsSynced": true, "lyricsLines": 54,
  "line": 16,
  "lineText": "…",
  "lines": [ { "t": 6000, "text": "…" }, … ],
  "nextLines": [ … ],
  "version": 42
}
```

Rules:

- `version` is a **JSON integer**, **present in every `/status` response**
  (with or without `wait=1`) once this feature is implemented.
- It starts at `0` (or `1`) on process start and **increments by 1 every time
  the visible fingerprint (below) changes**. It must be monotonic within the
  process lifetime. A reset to a small value after a relay restart is fine.
- **The Android client uses the mere presence of `version` (any integer ≥ 0)
  as feature detection.** Do not omit it "when it is 0" — `0` is a valid
  first version and must be sent.

### 2. The visible fingerprint (what increments `version`)

The relay increments `version` exactly when — and only when — one of these
changes between two state computations:

| Field | Why |
|---|---|
| `ok` | relay error vs ok flips the UI state |
| `auth` | shows the "Spotify not authenticated" screen |
| `playing` | green (active) vs dimmed (paused) rendering |
| `track.id` | new song: title, artist, album, cover |
| `line` | the active lyric line (the whole point of the app) |
| `lineText` | guards against a same-index different text |
| `lyricsSynced` | band mode vs plain-lyrics mode |
| `plain` (presence/length) | same as above, no-synced case |
| `error` (empty vs non-empty) | error detail line in the UI |

These must **NOT** increment it (they change on every Spotify refresh and
would defeat the purpose):

- `positionMs`, `updated` (timestamps),
- `lyricsLines` by itself (only meaningful together with `lyricsSynced`),
- `nextLines` (derived from `line`),
- `track.durMs`, `track.cover` (practically immutable per track; `track.id`
  already covers a real change).

Practical implementation: compute a canonical string (or comparable struct)
from the allowed fields, compare with the previous one; if different →
`version++` and wake the waiters.

### 3. The wait parameters

| Param | Meaning |
|---|---|
| `wait` | `1` = hold the response until the state changes or the timeout elapses. Absent/other = respond immediately (current behavior). |
| `timeoutMs` | Max hold time, in milliseconds. The client sends **8000**. Clamp: minimum `1000`, maximum `15000` (reject or ignore anything above the max). |
| `sinceVersion` | Last version the client already saw. |

Behavior with `wait=1`:

1. If `sinceVersion` is **absent or `-1`** → the client's state is unknown:
   **respond immediately** with the current state + `version`. (This is how
   the first request of a new client is answered, with zero extra latency.)
2. Otherwise **block** until the first of:
   - the state version becomes **greater than** `sinceVersion` → answer now
     with the current state (+ its new `version`);
   - `timeoutMs` elapses → answer with the current (unchanged) state;
   - the **client disconnects** (request context cancelled) → cancel the
     wait and release any per-waiter resources. **Never leak a goroutine
     per disconnected client.**
3. The timeout answer is a **normal `200`** with the standard JSON body —
   the client cannot (and must not have to) distinguish "changed" from
   "timed out"; it simply re-issues the wait request.

Auth: `wait=1` requests go through the **same** Basic-Auth requirements as a
plain `/status`. A held request is a request.

Concurrency: many clients may wait simultaneously (the app keeps at most one
open, but do not assume one). Waiting clients must be serviced by the state
refresh loop (broadcast/fan-out), never by the refresh loop being blocked.

### 4. Reverse-proxy notes (the deployed setup)

The endpoint is typically fronted by a Synology reverse proxy (HTTP port 80,
Basic Auth at the proxy). Consequences for the implementation:

- keep the max hold time short (≤ 15 s), well under any sane proxy read
  timeout — a held request that outlives the proxy's patience produces a
  504 and the client's read timeout;
- send the response only when waking (headers + body at the end is fine);
  no streaming/chunked tricks, no SSE;
- the client sets a read timeout of `timeoutMs + 8 s`, so a proxy that cuts
  the held connection early degrades to a client-side read timeout, which
  the client treats as a **normal** event (quiet retry), not a network
  failure.

### 5. Compatibility matrix

| client \ relay | without `version` (old) | with `version` (new) |
|---|---|---|
| **old client** (no wait params) | immediate response, as today | immediate response; `version` ignored by the client |
| **new client** (always sends wait params) | params ignored, answers immediately without `version` → the client **detects the absence of `version` and automatically falls back to classic adaptive polling** | wait mode: one held request per state change |

So the new client is safe to deploy before the relay, and the new relay is
safe to deploy before the client. No coordinated release needed.

## What the client does (for joint debugging)

- Every request carries `?wait=1&timeoutMs=8000&sinceVersion=N` (N starts at
  `-1`), appended with `?` or `&` depending on the stored URL.
- Socket timeouts: connect 3 s, read **16 s** in wait mode / 6 s in polling
  fallback.
- A response with `version ≥ 0` → wait mode: if the version differs from the
  last one seen, the next wait request is issued **immediately**; if it is
  unchanged and the request came back in < 3 s (i.e. the relay is not
  actually holding), the client paces itself (1 s) to avoid hot-looping a
  buggy relay.
- Response without `version` → classic adaptive polling (1 s / 3 s / 5 s).
- Transport failures are retried **quietly 4 times at 500 ms** before the
  first on-screen diagnosis (a 1 s blip must not flip the UI to the error
  screen); HTTP status errors (401/403/404/5xx) always diagnose at once,
  then back off 5 s / 10 s / 15 s.

## Suggested implementation shape (Go)

Keep the existing state-refresh loop that polls Spotify and recomputes the
lyrics line. Add, around the shared state:

```go
type waitState struct {
    mu       sync.Mutex
    snapshot *relayState   // the existing serializable state
    version  uint64        // increments on fingerprint change
    changed  chan struct{} // closed + replaced on each version bump
}

func (w *waitState) publish(s *relayState) {
    if w.fingerprintOf(s) == w.lastFingerprint {
        return // position/updated moved, visible state identical: no bump
    }
    w.mu.Lock()
    w.snapshot = s
    w.version++
    w.lastFingerprint = w.fingerprintOf(s)
    ch := w.changed
    w.changed = make(chan struct{})
    w.mu.Unlock()
    close(ch) // wakes every waiter that registered this channel
}
```

Handler sketch:

```go
func handleStatus(w http.ResponseWriter, r *http.Request) {
    if r.URL.Query().Get("wait") != "1" {
        writeStatus(w, r, currentSnapshot())          // old behavior
        return
    }
    st := st.waitState()                               // consistent read
    since, _ := strconv.ParseInt(r.URL.Query().Get("sinceVersion"), 10, 64)
    if since <= 0 {
        writeStatusWithVersion(w, r, st)               // fast path
        return
    }
    timeout := clamp(r.URL.Query().Get("timeoutMs"), 1000, 15000)

    st.mu.Lock()
    if st.version > uint64(since) {
        st.mu.Unlock()
        writeStatusWithVersion(w, r, st)               // already changed
        return
    }
    ch := st.changed
    st.mu.Unlock()

    done := make(chan struct{})
    go func() {                                        // release on disconnect
        <-r.Context().Done()
        close(done)
    }()
    timer := time.NewTimer(time.Duration(timeout) * time.Millisecond)
    defer timer.Stop()

    select {
    case <-ch:
    case <-timer.C:
    case <-done:
        return                                          // client went away
    }
    writeStatusWithVersion(w, r, st.waitState())
}
```

(Adapt to the existing repo's structure and state handling — the point is
the contract above, not this exact code. Add unit tests for the fingerprint
logic: `positionMs` changes → no bump; `line`/`playing`/`track.id` changes →
bump; wait timeout → snapshot returned with unchanged version.)

## Acceptance checklist

Run against a locally running relay (replace `HOST` / credentials):

1. **Plain request unchanged**
   `curl -u USER:PASS -s http://HOST/status` → 200, body as before, plus
   a JSON integer `version`.
2. **First wait request answers immediately** (sinceVersion unknown)
   `curl -u USER:PASS -s "http://HOST/status?wait=1&timeoutMs=3000&sinceVersion=-1"`
   → returns in < 1 s, includes `version`.
3. **Hold until timeout, no change**
   `time curl -u USER:PASS -s "http://HOST/status?wait=1&timeoutMs=3000&sinceVersion=<V>"`
   (with `<V>` = current version, nothing changing) → takes ≈ 3000 ms and
   returns the current state with the same `version`.
4. **Wake on change**: start the same request as (3) in the background, then
   play/pause a track (or let the current line advance) → the request
   returns **immediately**, with an updated `version` (or at least an updated
   visible state, e.g. `playing`).
5. **Version is selective**: advance the playback position without crossing a
   lyric line boundary → `version` must NOT change (position is not part of
   the fingerprint). Cross a line boundary → it must change.
6. **`sinceVersion` in the past** (lower than current) → immediate response.
7. **Auth**: `wait=1` without/with wrong credentials → same 401 as a plain
   request; the proxy-level Basic Auth still applies.
8. **No leak**: leave 5 held requests open (3 s timeout each), kill their
   clients while held, then check goroutine count returns to baseline
   (`/debug/pprof/goroutine` or a `len` of a waiter registry).
9. **Concurrency**: two clients waiting, one state change → both return.
10. **Timeout clamp**: `timeoutMs=100000` → held at most 15 s.
11. **Through the real reverse proxy**: the setup the app actually uses
    (port 80 vhost + Basic Auth) must deliver the held response without a
    504 within the 15 s clamp.
12. `go build ./...` and `go test ./...` green, including the new fingerprint
    tests.
