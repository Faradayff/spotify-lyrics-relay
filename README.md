# Spotify Lyrics Relay

An HTTP relay that fetches your **Spotify** playback state and the **synced
lyrics** (via [LRClIB](https://lrclib.net)) of the current track, exposing a
single JSON endpoint.

Built to be consumed by a lightweight Android 2.3.7 (API 10) app running on a
car head unit (Asteroid Smart 5.8 7144), where modern TLS / SNI are not
available and a small client cannot talk to Spotify directly.

```
[Spotify Mobile (your Premium)]  ─plays─▶  [Spotify Web API]
                                                ▲
                                                │ OAuth 2.0
[Car — Android 2.3.7] ─ GET /status ─────▶  [Relay (Go, Docker)]
```

- Go, standard library only (no external dependencies)
- Multi-stage Dockerfile, static binary, non-root user
- Secrets only via environment variables
- `data/` volume for `tokens.json`

## Public endpoint

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

`line` is the zero-based index into the synced lyric lines, already resolved
via `sort.Search` against the timestamped line list.

When not authenticated the response is:

```json
{ "ok": false, "auth": false, "error": "..." }
```

## Management endpoints

| Route | Purpose |
|---|---|
| `GET /` | HTML status page |
| `GET /login` | Redirects to Spotify (**complete from a browser on your PC**) |
| `GET /callback?code=...` | Spotify redirects here; the code is exchanged for tokens |
| `GET /status` | The public endpoint (what the car app consumes) |
| `POST /control?action=next\|prev\|pause\|resume` | Control the session |
| `GET /logout` | Clears stored tokens |

## Environment variables

| Variable | Required | Description |
|---|---|---|
| `SPOTIFY_CLIENT_ID` | yes | App ID from developer.spotify.com (type: **Server-side**) |
| `SPOTIFY_CLIENT_SECRET` | yes | App secret |
| `SPOTIFY_REDIRECT_URI` | yes | Must match the registered one exactly (Spotify only allows `http://localhost...` or `https://...`) |
| `RELAY_ADDR` | no | Listen address, defaults to `:8899` |
| `STATE_DIR` | no | Token storage dir (defaults to `/data` inside the container) |

## Docker

```bash
cp .env.example .env
# edit .env with your credentials
docker compose up -d
docker compose logs -f relay
curl -s http://localhost:8899/status | jq
```

> **Note**: Spotify requires `http://localhost...` or HTTPS for
> `redirect_uri`. If your server has no public domain, use an SSH tunnel to
> open `/login` in a browser:
> `ssh -L 8899:localhost:8899 user@server`, then visit
> `http://localhost:8899/login` on your PC.

## Security

- The binary runs as a non-root user (`relay`) inside the container.
- `data/` is mounted as a volume so `tokens.json` persists across restarts.
- The image **does not** bake in any secret (runtime `ENV` only).
- If you expose the relay to the internet: put a reverse proxy with TLS in
  front, and consider adding authentication to `/status`.

## License

MIT
