# strm-builder

Mirrors the media files on one or more WebDAV or HTTP(S) directory-index servers
into a tree of `.strm` files, each containing the source file's HTTP(S) URL. Pair
it with [plex-strm-assistant](https://github.com/liveinaus/plex-strm-assistant)
(Plex), or point Jellyfin / Emby / Kodi straight at the output — no FUSE mount in
the playback path.

Single static Go binary built with [ko](https://ko.build) (no Dockerfile). The
crawler is stdlib-only; the [serve mode](#serve-mode--request-driven-stremio-bridge)
adds one dependency ([ptt-go](https://github.com/itsrenoria/ptt-go), for
release-name parsing). Concurrent crawl — WebDAV PROPFIND or HTML autoindex,
auto-detected per source — with idempotent writes and optional pruning.

## Sources

Point each source at a normal `http://` or `https://` URL. It's probed once at
its root — a WebDAV `PROPFIND` if the server supports it, otherwise its HTML
directory index (autoindex) is parsed — so WebDAV and plain file-listing servers
both work with no extra configuration.

## Layout

Output is `<root>/<host>/<path-from-server-root>/<name>.strm`. A source URL may
include a subfolder — only that subfolder is crawled, but the path is still
mirrored from the server root:

```
-url https://host/movies
  ->  <root>/host/movies/Title (2024)/Title (2024).strm
```

## Configuration

Each flag has a matching environment variable:

| Flag / Env | Default | Description |
|------------|---------|-------------|
| `-url` / `SOURCE_URLS` | — (required) | Source URL(s), WebDAV or HTTP directory-index (auto-detected); put credentials in the URL as `user:pass@host`. Flag repeatable, env comma/space-separated, positional args also accepted |
| `-root` / `ROOT_FOLDER` | `/strm` | Where the `.strm` trees are written |
| `-embed-creds` / `EMBED_CREDENTIALS` | `true` | Embed the URL's `user:pass@` into the written `.strm` URLs (set `false` to keep them out) |
| `-concurrency` / `CONCURRENCY` | `8` | Parallel PROPFINDs — lower it for rate-limited servers |
| `-ext` / `MEDIA_EXTENSIONS` | common video set | Comma-separated extensions, or `*` for all |
| `-prune` / `PRUNE` | `false` | Delete `.strm` whose source no longer exists |
| `-dry-run` / `DRY_RUN` | `false` | Log without writing |
| `-timeout` / `TIMEOUT` | `30s` | Per-request timeout |

## Build

```bash
make image    # ko build + push to KO_DOCKER_REPO (default ghcr.io/kanya-approve/strm-builder)
```

Built with [ko](https://ko.build) — no Dockerfile. CI publishes automatically:
pushes to `main` build a `:latest` / `:sha-<sha>` snapshot; tagging `vX.Y.Z` cuts a
signed multi-arch release.

## Run

```bash
./strm-builder -url https://user:pass@host/movies -url https://user:pass@host/tvs \
  -root ./out -concurrency 2
```

For local development, `make run` runs it from source, forwarding the env-var
knobs: `make run SOURCE_URLS=https://user:pass@host/movies DRY_RUN=true`.

Or run the published image from ghcr.io — no checkout or build needed — passing
config as env vars and mounting the output directory:

```bash
docker run --rm \
  -e SOURCE_URLS=https://user:pass@host/movies \
  -v "$PWD/out:/strm" \
  ghcr.io/kanya-approve/strm-builder:latest
```

## Serve mode — request-driven Stremio bridge

The crawler above needs a browsable server. When your source is a **Stremio
addon** instead (it answers `manifest`/`stream` queries but has nothing to walk),
run `strm-builder serve`: a small webhook bridge that turns approved media
requests into `.strm` files.

```
request approved (Overseerr / Jellyseerr / Seerr)
  → webhook → strm-builder serve
  → resolve via ANY Stremio stream addon  → pick a direct URL
  → Movies/Title (Year)/Title (Year).strm
    TV Shows/Title (Year)/Season 01/Title - S01E01.strm
```

The addon is configurable — nothing is hardcoded. It must return streams that
carry a direct `http(s)` `url` (debrid-backed addons do); torrent-only
(`infoHash`) streams have no URL and are skipped. TMDB supplies titles/years and
expands a series into episodes, so a TMDB API key is required (the same one your
Seerr instance already uses). `-target` selects the folder/naming convention.

Output is **not** a mirror of the source; it's a clean library built from TMDB
metadata, split into top-level folders: `Movies/`, `TV Shows/`, `Anime/` (series)
and `Anime Movies/`. Anime is detected exactly the way Overseerr/Jellyseerr
separate it — the TMDB `anime` keyword (210024) — so the split matches what you see
in Seerr. Anime movies get their own folder rather than mixing with anime series,
since a Plex library is one type (movies *or* shows). Turn the whole split off with
`-anime=false`; rename the buckets with `-anime-folder` / `-anime-movies-folder`;
or set `-anime-movies-folder=` (empty) to file anime movies under `Movies/`.

### Quality ranking and versions

The addon returns a shuffled random subset of releases per request, so there's no
usable order to trust. Instead each release name is parsed with
[ptt-go](https://github.com/itsrenoria/ptt-go) for its resolution, and selection
is quality-ranked: **the largest file of each resolution**, resolutions ordered
best-first. `-versions 1` (default) keeps only the largest file of the highest
resolution, with a clean name. `-versions N` keeps the top N resolutions as
separate `.strm` files in the same folder, which Plex groups as **Versions**:

```
Movies/The Matrix (1999)/The Matrix (1999) - 2160p.strm
Movies/The Matrix (1999)/The Matrix (1999) - 1080p.strm
```

Because only the largest release of a resolution is guaranteed to appear in one
call, raise `-sample` (extra addon calls, unioned) when keeping several versions
so the lower resolutions are reliably found. Note: Plex does not fail over between
Versions — they are a manual quality choice, not a fallback chain.

### Generic URL

You don't need an addon to create a properly-named entry. Give any direct video
URL plus its matching TMDB id and it builds the folder/file for you:

```bash
# a movie
curl 'http://host:8080/fulfill?type=movie&tmdb=45745&url=https://cdn.example/sintel.mkv'
# one episode
curl 'http://host:8080/fulfill?type=tv&tmdb=2316&season=2&episode=1&url=https://cdn.example/office-s02e01.mkv'
```

```bash
strm-builder serve \
  -addon 'https://addon.example/config/<cfg>/manifest.json' \
  -tmdb-key "$TMDB_API_KEY" \
  -root ./out -target plex -listen :8080
```

Then in Overseerr/Jellyseerr add a **Webhook** notification agent posting to
`http://<host>:8080/webhook` on *Request Approved* / *Request Automatically
Approved* (set the Authorization header to your `-webhook-secret` if you use one).
Approvals then fulfil into the library your Plex/Jellyfin points at. Test without
Seerr via `GET /fulfill?type=movie&tmdb=278` or `?type=tv&tmdb=1396&season=1`.

Because fulfilment bypasses Sonarr/Radarr, Seerr never sees the request complete.
Set `-seerr-url` and `-seerr-api-key` and, after writing the `.strm`, the bridge
calls Seerr back to mark the request **Available** (`POST /media/{id}/available`),
closing the loop. Left unset, it just writes files and Seerr relies on its own
library scan.

| Flag / Env | Default | Description |
|------------|---------|-------------|
| `-addon` / `STREMIO_ADDON` | — (required) | Stremio addon URL (its `manifest.json`, or the base) |
| `-tmdb-key` / `TMDB_API_KEY` | — (required) | TMDB v3 API key, for titles/years and episode lists |
| `-seerr-url` / `SEERR_URL` | — | Overseerr/Jellyseerr base URL; set with the API key to mark requests available |
| `-seerr-api-key` / `SEERR_API_KEY` | — | Overseerr/Jellyseerr API key |
| `-target` / `TARGET` | `plex` | Layout preset: `plex`, `jellyfin`, `emby`, `kodi` |
| `-anime` / `ANIME_SPLIT` | `true` | Route anime (TMDB `anime` keyword) into its own top folders |
| `-anime-folder` / `ANIME_FOLDER` | `Anime` | Folder for anime series |
| `-anime-movies-folder` / `ANIME_MOVIES_FOLDER` | `Anime Movies` | Folder for anime movies; empty puts them in `Movies` |
| `-root` / `ROOT_FOLDER` | `/strm` | Where the `.strm` trees are written |
| `-listen` / `LISTEN_ADDR` | `:8080` | HTTP listen address for the webhook |
| `-versions` / `VERSIONS` | `1` | Resolutions to keep per item (largest of each), best-first, as separate `.strm` |
| `-pick` / `PICK` | `largest` | Which release to keep within a resolution: `largest` or `smallest` |
| `-sample` / `SAMPLE` | `1` | Addon calls to union before ranking; raise for `-versions` >1 |
| `-webhook-secret` / `WEBHOOK_SECRET` | — | If set, required in the webhook `Authorization` header or `?secret=` |
| `-concurrency` / `CONCURRENCY` | `2` | Parallel episode resolutions (addons rate-limit; kept low, with 429 back-off) |
| `-dry-run` / `DRY_RUN` | `false` | Log without writing |
| `-timeout` / `TIMEOUT` | `30s` | Per-request timeout |
