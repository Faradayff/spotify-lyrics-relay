# Spotify Lyrics Relay

A small, self-hosted service that turns your **Spotify** account into a plain
JSON API. It fetches your current playback state and the **synced lyrics** of
the track that's playing (via [LRClIB](https://lrclib.net)) and serves it
over a single, dependency-free HTTP endpoint.

No native SDK, no browser extensions, no mobile SDK — just an HTTP GET that
any device or language can call.

```
[Spotify on any device (your Premium)]  ─plays─▶  [Spotify Web API]
                                                       ▲
                                                       │ OAuth 2.0 (the relay holds the tokens)
[Your app: dashboard · Home Assistant · script · car display · kiosk …]
                                                       │
                                        GET /status ───┘
```

## Why a relay in the first place?

Talking to Spotify directly requires OAuth 2.0, token refresh, HTTPS, and a
lyrics source. `spotify-lyrics-relay` bundles all of that once on a server so
the client side is trivial:

- one `GET` returns the track, play position, and the exact lyric line that
  should be on screen;
- works from anything that speaks HTTP: a smart display, a head unit, a
  Raspberry Pi, a Home Assistant template, a shell script, a web page, a
  native app on any OS…
- ideal where a full Spotify client is overkill or unsupported (old/embedded
  Android, thin clients, custom UIs, automation).

## What it exposes

```
GET /status   →   200 JSON
```

```json
{
  "ok": true,
  "auth": true,
  "playing": true,
  "positionMs": 42000,
  "track": { "id":"...", "name":"...", "artist":"...", "album":"...", "uri":"..." },
  "lyricsSynced": true,
  "lyricsLines": 42,
  "line": 7
}
```

- `line` is the zero-based index of the line that should currently be shown,
  resolved by binary search over the timestamped lyric lines.
- `lyricsSynced` is `false` when only plain (unsynced) lyrics are available.
- When not authenticated: `{ "ok": false, "auth": false, "error": "..." }`.

### Other endpoints

| Route | Purpose |
|---|---|
| `GET /` | HTML status page (handy for a quick check) |
| `GET /login` | Starts the Spotify OAuth flow (open in a browser) |
| `GET /callback?code=...` | Exchanges the auth code for tokens |
| `GET /status` | The JSON API your apps consume |
| `POST /control?action=next\|prev\|pause\|resume` | Control playback remotely |
| `GET /logout` | Clears the stored tokens |

### Example use in an automation

```bash
# Which line should the display show right now?
curl -s http://localhost:8899/status | jq -r .line
```

```js
// In any JS/TS frontend
const s = await fetch("http://relay.local:8899/status").then(r => r.json());
if (s.ok && s.lyricsSynced) renderLine(s.line);
```

## Features

- **Go**, standard library only — no external Go modules, one static binary.
- Multi-stage **Dockerfile**, non-root user, small Alpine runtime image.
- Secrets (Spotify credentials) live only in environment variables / a
  volume, never baked into the image.
- Automatic token refresh; tokens persisted to a `data/` volume.
- CORS enabled on `/status` so browser-based frontends can call it.

## Spotify Developer Dashboard setup

Before the first `/login` works you need an app in <https://developer.spotify.com/dashboard>.
Fill the form like this:

1. Click **Create app**.
2. **Type**: **Server-side** (not "Native / Browser-based").
3. **App name**: anything, e.g. `spotify-lyrics-relay`.
4. **Description**: free text.

**Redirect URIs** (this is the field you asked about — enter exactly one, and
it must be *identical* to the one you put in `SPOTIFY_REDIRECT_URI`):

```
https://<your-public-hostname>/lyrics/callback
```

For example, with a reverse proxy mounted under a base path `/lyrics`:

```
https://your-public.hostname/lyrics/callback
```

> **Why HTTPS and a base path?** Spotify rejects insecure `http://` redirect
> URIs for self-hosted apps (the dashboard shows *"This redirect URI is not
> secure"*). The only `http://` exception is the **loopback address** —
> `http://127.0.0.1:PORT/...` (or `http://[::1]:PORT/...`) for local
> development. The literal name `localhost` is rejected, and a loopback URI
> only works from the machine that runs the relay (browser on the same host),
> so it does not replace a real HTTPS URL behind a reverse proxy.
>
> If a reverse proxy or web server already terminates TLS on ports **80** /
> **443** for your host, let it forward a base path — `/lyrics` here — to the
> relay instead of running a second TLS stack. The relay supports any base
> path via `RELAY_BASE_PATH`. See *"Running behind a reverse proxy"*.
>
> Keep these in sync or Spotify will reject the callback with
> `redirect_uri_mismatch`:
>
> 1. The URI you paste into the Spotify dashboard
> 2. `SPOTIFY_REDIRECT_URI` in `.env`
> 3. `RELAY_BASE_PATH` in `.env` (must match the proxy rule's *URI*)

**Which API/SDKs are you planning to use?** — tick exactly **one**:

- [x] **Web API**
- [ ] Ads API
- [ ] Web Playback SDK
- [ ] iOS
- [ ] Android

Why **Web API** and only that: this relay calls the Web API
(`api.spotify.com/v1/me/player*`) using OAuth 2.0, and it never renders audio
itself. The **Web Playback SDK** is the one that actually plays music in a
browser — you don't need it here, because the song is already playing on
whatever device is connected to **your** Premium account. The mobile SDKs
(iOS/Android/Ads) are also irrelevant to a self-hosted HTTP relay.

**Scopes** (auto-generated on the "Settings" tab of the app; the code requests
these — no UI tick needed, they are sent on the `/login` redirect):

```
user-read-playback-state user-read-currently-playing streaming
```

- `user-read-playback-state` → lets the relay read `/me/player`
  (current track, position, playing state).
- `user-read-currently-playing` → `/v1/me/player/currently-playing`.
- `streaming` → keeps the Premium license valid when the relay issues the
  controls (`/control?action=…`) that would otherwise be rejected with
  `403`.

**Required** (the two secrets you copy into your `.env`):

| Field | Goes into |
|---|---|
| **Client ID** | `SPOTIFY_CLIENT_ID` |
| **Client Secret** | `SPOTIFY_CLIENT_SECRET` |
| **Callback URI** | `SPOTIFY_REDIRECT_URI` |

> Keep the **type = Server-side** selection — it unlocks `client_id` +
> `client_secret` (the "Authorization Code with Client Secret" flow) that this
> relay uses. A "Native / Browser-based" app would only get an implicit-flow
> flow and would **not** work with the current code.

## Configuration

| Variable | Required | Description |
|---|---|---|
| `SPOTIFY_CLIENT_ID` | yes | App ID from developer.spotify.com (type: **Server-side**) |
| `SPOTIFY_CLIENT_SECRET` | yes | App secret |
| `SPOTIFY_REDIRECT_URI` | yes | Must match the registered one exactly (Spotify requires `https://...`, except loopback `http://127.0.0.1:PORT/...`). |
| `RELAY_BASE_PATH` | no | Base path all routes are served under (e.g. `/lyrics`); empty for `http://127.0.0.1:8899/...`. Must be a prefix of `SPOTIFY_REDIRECT_URI`. |
| `RELAY_ADDR` | no | Listen address, defaults to `:8899` |
| `STATE_DIR` | no | Token storage dir (defaults to `/data` inside the container) |

## Running with Docker

### Option A — local development on any machine

No public hostname needed. Spotify accepts the **loopback** redirect
`http://127.0.0.1:8899/callback` (the literal name `localhost` is rejected),
so this is the fastest way to develop against a live session:

```bash
cp .env.example .env
# in .env set:
#   SPOTIFY_CLIENT_ID=...
#   SPOTIFY_CLIENT_SECRET=...
#   SPOTIFY_REDIRECT_URI=http://127.0.0.1:8899/callback
#   RELAY_BASE_PATH=        (empty)
docker compose up -d --build
docker compose logs -f relay
curl -s http://127.0.0.1:8899/status | jq
# then open http://127.0.0.1:8899/login in a browser on that machine
```

> The browser must be able to reach the loopback of the machine that runs the
> relay. If your browser is on a *different* host (e.g. Windows using a WSL2 /
> VM), the callback page won't load — that's fine: the URL in your address bar
> contains `?code=...`. Copy that URL and request it from the relay's machine
> instead: `curl 'http://127.0.0.1:8899/callback?code=…'` — the relay does the
> token exchange and prints "OK, session saved".

### Option B — production behind a reverse proxy

This is the layout this repo is configured for. A reverse proxy terminates TLS
(it already holds a valid certificate) and forwards one base path to the
container. Only loopback traffic reaches port 8899. See *"Running behind a
reverse proxy"* below for the exact rule.

```yaml
services:
  relay:
    image: ghcr.io/faradayff/spotify-lyrics-relay:latest
    container_name: spotify-lyrics-relay
    restart: unless-stopped
    ports:
      - "127.0.0.1:8899:8899"
    environment:
      - SPOTIFY_CLIENT_ID=${SPOTIFY_CLIENT_ID}
      - SPOTIFY_CLIENT_SECRET=${SPOTIFY_CLIENT_SECRET}
      - SPOTIFY_REDIRECT_URI=https://your-public.hostname/lyrics/callback
      - RELAY_BASE_PATH=/lyrics
    volumes:
      - ./data:/data
```

Keep `SPOTIFY_CLIENT_ID` / `SPOTIFY_CLIENT_SECRET` in `.env` (next to the
compose file), not in the image or a committed file.

### Option B2 — dedicated subdomain (root path)

If you would rather keep the relay on its own subdomain instead of a path
(e.g. `lyrics.your-domain.com`), point the whole proxy rule at the relay's
root and leave the base path empty:

```yaml
    environment:
      - SPOTIFY_REDIRECT_URI=https://lyrics.your-domain.com/callback
      - RELAY_BASE_PATH=
```

Proxy rule: `https://lyrics.your-domain.com/*` → `http://127.0.0.1:8899/*`
(the proxy must forward the URI *as-is*; the relay then serves `/login`,
`/status`, `/control`, `/callback` at the root).

| | Option B (path) | Option B2 (subdomain) |
|---|---|---|
| Login | `https://host/lyrics/login` | `https://lyrics.host/login` |
| Callback (Spotify dashboard) | `https://host/lyrics/callback` | `https://lyrics.host/callback` |
| JSON API | `https://host/lyrics/status` | `https://lyrics.host/status` |
| `RELAY_BASE_PATH` | `/lyrics` | *(empty)* |

Both modes run the same container; only `RELAY_BASE_PATH` and the registered
redirect URI differ. Pick one — the relay serves only the configured base.

## Running without Docker

```bash
go build -o spotify-lyrics-relay .
SPOTIFY_CLIENT_ID=... SPOTIFY_CLIENT_SECRET=... SPOTIFY_REDIRECT_URI=https://<your-host>/callback \
  ./spotify-lyrics-relay
```

(With a Go-only setup you still need a TLS-terminating reverse proxy in front,
or Spotify will reject the redirect.)

## Running behind a reverse proxy

This repo is set up for any server where ports 80 and 443 are already used by
a TLS-terminating proxy. The trick: **don't run a second TLS stack at all**. Let
the existing proxy (which already holds a valid certificate for your host)
forward a base path to the relay, which only listens on the machine's
loopback.

- Relay container: `127.0.0.1:8899` (nothing exposed on the router).
- Proxy rule: `https://your-public.hostname/lyrics/*` → `http://127.0.0.1:8899/*`.
- The relay serves *every* route under a configurable base path
  (`RELAY_BASE_PATH`, e.g. `/lyrics`), including `/status`, `/login`,
  `/callback`, `/control`, `/logout`.

So the public URLs are:
- Login:    `https://your-public.hostname/lyrics/login`
- Callback: `https://your-public.hostname/lyrics/callback`  ← register this
- JSON API: `https://your-public.hostname/lyrics/status`

### Checklist

1. **Reverse proxy** (nginx, Caddy, Traefik, Apache, a cloud proxy, …) → new
   rule / location:
   - Protocol: `https` (or terminate TLS with a valid cert on the host)
   - Host name: `your-public.hostname`
   - URI:      `/lyrics`   (no trailing slash)
   - Server:   `127.0.0.1`
   - Port:     `8899`
   - (Optional) add header `Host: relay:8899` if you want the relay to see
     the container name.

2. **Spotify dashboard** → redirect URI (exactly one):
   ```
   https://your-public.hostname/lyrics/callback
   ```

3. **On the host** (in the folder holding this repo):
   ```bash
   cp .env.example .env
   # edit .env:
   #   SPOTIFY_CLIENT_ID=...
   #   SPOTIFY_CLIENT_SECRET=...
   # .env already has:
   #   SPOTIFY_REDIRECT_URI=https://your-public.hostname/lyrics/callback
   #   RELAY_BASE_PATH=/lyrics
   docker compose up -d --build
   docker compose logs -f relay
   ```

4. **Log in**: open `https://your-public.hostname/lyrics/login` in a browser,
   authorize; Spotify redirects to `.../callback` and the tokens are saved to
   the `data/` volume.

5. **From any client** (car, dashboard, HA, …):
   ```bash
   curl -s https://your-public.hostname/lyrics/status | jq
   ```

> **Notes**
>
> - If the proxy already holds a valid certificate for the host, no Let's
>   Encrypt re-run and no port 80 needed for the challenge.
> - If you ever need to change the base path (e.g. from `/lyrics` to
>   `/spotify`), keep the proxy rule, `RELAY_BASE_PATH` and the Spotify
>   redirect URI all in sync; the relay has no fallback.
> - The `data/` directory is the token store — do not delete it between
>   upgrades or you will have to log in again.

## Security

- The process runs as a non-root user (`relay`) inside the container.
- Tokens are stored in a mounted volume (`data/`), not in the image.
- The image contains **no** secrets (runtime environment only).
- The relay binds to `127.0.0.1:8899` on the host only; the public surface is
  whatever your reverse proxy exposes. Add authentication in front of
  `/status` if you plan to expose it beyond the LAN.

## License

MIT
