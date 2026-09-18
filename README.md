# Spotify Lyrics Relay

Relay HTTP que consume la API de Spotify (OAuth 2.0) y expone un único
endpoint JSON con la **letra sincronizada** (vía [LRClIB](https://lrclib.net))
de la canción en reproduce.

Pensado para ser consumido por una app Android 2.3.7 (API 10) que corra en la
pantalla de un coche (head unit Asteroid Smart 5.8 7144), donde TLS 1.2 / SNI
no están disponibles y una app ligera no puede hablar directo con Spotify.

```
[Móvil Spotify Premium]  ─reproduce─▶  [Spotify Web API]
                                              ▲
                                              │ OAuth 2.0
[Coche Android 2.3.7] ── GET /status ──▶  [Relay (Go, Docker)]
```

- Go stdlib únicamente (sin dependencias)
- Docker multi-stage, binario estático, usuario no-root
- Secretos solo vía variables de entorno
- `data/` volúmen para `tokens.json`

## Endpoint público

```
GET /status   →   200 JSON
```

```json
{
  "ok": true,
  "auth": true,
  "playing": true,
  "positionMs": 42000,
  "device": "Mi teléfono",
  "track": { "id":"...", "name":"...", "artist":"...", "album":"...", "uri":"..." },
  "lyricsSynced": true,
  "lyricsLines": 42,
  "line": 7
}
```

`line` es el índice (0-based) dentro de la letra sincronizada, ya calculado
con `sort.Search` sobre la lista de líneas.

Sin autenticación: `{"ok": false, "auth": false, "error": "..."}`.

## Endpoints de administración

| Ruta | Uso |
|---|---|
| `GET /` | panel HTML de estado |
| `GET /login` | redirige a Spotify (completar **desde un browser en tu PC**) |
| `GET /callback?code=...` | Spotify redirige aquí; intercambia código por tokens |
| `GET /status` | el endpoint público (lo que consume el coche) |
| `POST /control?action=next\|prev\|pause\|resume` | controles sobre la sesión |
| `GET /logout` | borra tokens |

## Variables de entorno

| Variable | Obligatorio | Descripción |
|---|---|---|
| `SPOTIFY_CLIENT_ID` | ✅ | App de developer.spotify.com (tipo: **Server-side**) |
| `SPOTIFY_CLIENT_SECRET` | ✅ | Secret de la App |
| `SPOTIFY_REDIRECT_URI` | ✅ | Debe casar con la registrada (Spotify solo acepta `http://localhost...` o `https://...`) |
| `RELAY_ADDR` | no | listen, default `:8899` |
| `STATE_DIR` | no | carpeta de tokens (por defecto `/data` en el contenedor) |

## Docker

```bash
cp .env.example .env
# editar .env con tus credenciales
docker compose up -d
docker compose logs -f relay
curl -s http://localhost:8899/status | jq
```

> **Nota**: Spotify exige `http://localhost...` o HTTPS para `redirect_uri`.
> Si el servidor no tiene dominio, usa un túnel de SSH para abrir `/login`:
> `ssh -L 8899:localhost:8899 user@server` y abre `http://localhost:8899/login`
> en el PC.

## Seguridad

- El binario corre como usuario no-root (`relay`) dentro del contenedor.
- `data/` se monta como volumen para persistir `tokens.json`.
- La imagen **no** guarda secretos (solo vía `ENV` runtime).
- Si se expone a internet: reverso proxy + TLS, y añadir `RELAY_AUTH_TOKEN`
  para autenticar `/status`.

## Licencia

MIT (ver [LICENSE](LICENSE) si se añade)
