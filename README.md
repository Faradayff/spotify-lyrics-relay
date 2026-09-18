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
  "device": "My phone",
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

## Configuration

| Variable | Required | Description |
|---|---|---|
| `SPOTIFY_CLIENT_ID` | yes | App ID from developer.spotify.com (type: **Server-side**) |
| `SPOTIFY_CLIENT_SECRET` | yes | App secret |
| `SPOTIFY_REDIRECT_URI` | yes | Must match the registered one exactly (Spotify only allows `http://localhost...` or `https://...`) |
| `RELAY_ADDR` | no | Listen address, defaults to `:8899` |
| `STATE_DIR` | no | Token storage dir (defaults to `/data` inside the container) |

## Running with Docker

### Option A — build locally (the `docker-compose.yml` in this repo)

```bash
cp .env.example .env
# edit .env with your credentials
docker compose up -d
docker compose logs -f relay
curl -s http://localhost:8899/status | jq
```

### Option B — pull the pre-built image from GHCR

This repo publishes an image to the GitHub Container Registry on every push
to `main` (`ghcr.io/<owner>/spotify-lyrics-relay`). Example `docker-compose.yml`
on the consuming server:

```yaml
services:
  spotify-lyrics-relay:
    image: ghcr.io/yourusername/spotify-lyrics-relay:latest
    container_name: spotify-lyrics-relay
    restart: unless-stopped
    ports:
      - "8899:8899"
    environment:
      - SPOTIFY_CLIENT_ID=${SPOTIFY_CLIENT_ID}
      - SPOTIFY_CLIENT_SECRET=${SPOTIFY_CLIENT_SECRET}
      - SPOTIFY_REDIRECT_URI=${SPOTIFY_REDIRECT_URI:-http://localhost:8899/callback}
    volumes:
      - ./data:/data
    healthcheck:
      test: ["CMD", "wget", "-q", "--spider", "http://127.0.0.1:8899/"]
      interval: 30s
      timeout: 5s
      retries: 3
```

Store `SPOTIFY_CLIENT_ID` / `SPOTIFY_CLIENT_SECRET` / `SPOTIFY_REDIRECT_URI`
in a `.env` file next to the compose file (or in your Docker secrets /
environment) so they are never committed:

```bash
docker compose pull && docker compose up -d
docker compose logs -f spotify-lyrics-relay
```

To authenticate, open `http://<server>:8899/login` in a browser (see the note
below about the OSS redirect).

> **Note on the OAuth redirect**: Spotify only accepts `http://localhost...`
> or `https://...` as a redirect URI. The easiest way to log in when the relay
> runs on a remote server is an SSH tunnel:
> `ssh -L 8899:localhost:8899 user@server`, then open
> `http://localhost:8899/login` in a browser on your machine.

## Running without Docker

```bash
go build -o spotify-lyrics-relay .
SPOTIFY_CLIENT_ID=... SPOTIFY_CLIENT_SECRET=... SPOTIFY_REDIRECT_URI=http://localhost:8899/callback \
  ./spotify-lyrics-relay
```

## Security

- The process runs as a non-root user (`relay`) inside the container.
- Tokens are stored in a mounted volume (`data/`), not in the image.
- The image contains **no** secrets (runtime environment only).
- If you expose the relay beyond your LAN, put TLS behind a reverse proxy and
  add your own authentication in front of `/status`.

## License

MIT
