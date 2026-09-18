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

## Spotify Developer Dashboard setup

Before the first `/login` works you need an app in <https://developer.spotify.com/dashboard>.
Fill the form like this:

1. Click **Create app**.
2. **Type**: **Server-side** (not "Native / Browser-based").
3. **App name**: anything, e.g. `spotify-lyrics-relay`.
4. **Description**: free text.

**Redirect URIs** (this is the field you asked about — enter exactly one, and
it must be *identical* to the one you put in `SPOTIFY_REDIRECT_URI` / used in
the `Caddyfile`):

```
https://<your-public-hostname>/callback
```

`<your-public-hostname>` is the DNS name that resolves to the box running this
stack — e.g. a subdomain of your DDNS such as `relay.yourdomain.com`.

> **Why HTTPS?** Spotify rejects insecure `http://` redirect URIs for
> self-hosted apps (the dashboard shows *"This redirect URI is not secure"*).
> For a `localhost` URI it allows the exception — but only for *local*
> development, not for a relay that lives behind a DDNS / NAT.
> The cleanest fix is to terminate TLS with a reverse proxy. This repo ships a
> **Caddy** configuration ([`Caddyfile`](Caddyfile) + the `caddy` service in
> the compose file) that obtains a free Let's Encrypt certificate
> automatically. See *"Running with Caddy"* below.
>
> Keep all three in sync at all times, otherwise Spotify will reject the
> callback with `redirect_uri_mismatch`:
>
> 1. The URI you paste into the Spotify dashboard
> 2. `SPOTIFY_REDIRECT_HOST` in `.env`
> 3. The hostname at the top of `Caddyfile`

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
| `SPOTIFY_REDIRECT_URI` | yes | Must match the registered one exactly. Spotify rejects insecure `http://` URIs for self-hosted apps — use `https://<host>/callback`. |
| `SPOTIFY_REDIRECT_HOST` | yes (in this repo's compose) | Public hostname (your DDNS). Used to build `SPOTIFY_REDIRECT_URI` and must match the host in `Caddyfile`. |
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
to `main` (`ghcr.io/<owner>/spotify-lyrics-relay`).

**With Caddy (recommended, gives you HTTPS for Spotify login):**

```yaml
services:
  relay:
    image: ghcr.io/yourusername/spotify-lyrics-relay:latest
    restart: unless-stopped
    environment:
      - SPOTIFY_CLIENT_ID=${SPOTIFY_CLIENT_ID}
      - SPOTIFY_CLIENT_SECRET=${SPOTIFY_CLIENT_SECRET}
      - SPOTIFY_REDIRECT_URI=https://${SPOTIFY_REDIRECT_HOST}/callback
      - RELAY_ADDR=:8899
    volumes:
      - ./data:/data
    depends_on:
      - caddy

  caddy:
    image: caddy:2-alpine
    restart: unless-stopped
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy_data:/data
      - caddy_config:/config
    depends_on:
      - relay

volumes:
  caddy_data:
  caddy_config:
```

**Without a public hostname (pure LAN dev):**

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
      - SPOTIFY_REDIRECT_URI=http://localhost:8899/callback
    volumes:
      - ./data:/data
```

> This mode **only** works if you complete the login from the same machine
> that runs Docker (since the `localhost` redirect URI is validated by Spotify
> in the browser). On a remote host you need a public hostname or an SSH
> tunnel (see below).

Store `SPOTIFY_CLIENT_ID` / `SPOTIFY_CLIENT_SECRET` / `SPOTIFY_REDIRECT_HOST`
in a `.env` file next to the compose file (or in your Docker secrets /
environment) so they are never committed:

```bash
docker compose up -d
docker compose logs -f relay
curl -s https://<your-host>/status | jq
```

> **Note on the OAuth redirect**: if you do **not** have a public hostname,
> the last resort is an SSH tunnel to the box running the relay:
> `ssh -N -L 8899:localhost:8899 user@server`, then open
> `http://localhost:8899/login` on your own PC while `relay`'s
> `SPOTIFY_REDIRECT_URI` is `http://localhost:8899/callback`. After that the
> tunnel can be closed and the relay keeps working normally. This only affects
> the one-time login.

## Running without Docker

```bash
go build -o spotify-lyrics-relay .
SPOTIFY_CLIENT_ID=... SPOTIFY_CLIENT_SECRET=... SPOTIFY_REDIRECT_URI=https://<your-host>/callback \
  ./spotify-lyrics-relay
```

(With a Go-only setup you still need to put a TLS-terminating reverse proxy
in front, or Spotify will reject the redirect.)

## Running with Caddy (public HTTPS for Spotify login)

The repo ships [`Caddyfile`](Caddyfile) and a `caddy` service in
[`docker-compose.yml`](docker-compose.yml). Caddy handles TLS end-to-end
(Let's Encrypt, auto-renewed) and proxies to the relay, so:

- You **don't** forward port 8899 on your router (only 80 and 443).
- Spotify sees a valid `https://` redirect URI.
- The `/status` endpoint is reachable at `https://<your-host>/status`
  (great for remote dashboards too).

### Checklist

1. **DNS**: a hostname (e.g. `relay.yourdomain.com`) whose A / AAAA record
   points to the **public IP of the box running this stack**. If you use a
   DDNS, the hostname can be a subdomain of it, as long as the final name
   resolves to that box.
2. **Router ports**: forward **80/tcp** and **443/tcp** to that box (Caddy
   needs 80 for the Let's Encrypt HTTP-01 challenge).
3. **Edit files** so the three values match:
   - `Caddyfile` → replace `your-hostname.tld` with your hostname.
   - `.env` → set `SPOTIFY_REDIRECT_HOST=<your-host>`
     (the relay's `SPOTIFY_REDIRECT_URI` is built as
     `https://<your-host>/callback`).
   - Spotify dashboard → add the **same** `https://<your-host>/callback`.
4. **Run**:
   ```bash
   cp .env.example .env
   # fill SPOTIFY_CLIENT_ID / SPOTIFY_CLIENT_SECRET / SPOTIFY_REDIRECT_HOST
   docker compose up -d
   docker compose logs -f caddy     # first start obtains the certificate
   docker compose logs -f relay
   ```
5. **Log in**: open `https://<your-host>/login` in a browser, authorize, and
   the callback lands on `.../callback`. Done.
6. **From the car / LAN**: `curl -s https://<your-host>/status | jq`.

## Security

- The process runs as a non-root user (`relay`) inside the container.
- Tokens are stored in a mounted volume (`data/`), not in the image.
- The image contains **no** secrets (runtime environment only).
- If you expose the relay beyond your LAN, put TLS behind a reverse proxy and
  add your own authentication in front of `/status`.

## License

MIT
