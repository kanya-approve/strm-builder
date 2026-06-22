# strm-builder

Mirrors the media files on one or more WebDAV or HTTP(S) directory-index servers
into a tree of `.strm` files, each containing the source file's HTTP(S) URL. Pair
it with [plex-strm-assistant](https://github.com/liveinaus/plex-strm-assistant)
(Plex), or point Jellyfin / Emby / Kodi straight at the output — no FUSE mount in
the playback path.

Single static Go binary, stdlib only, built with [ko](https://ko.build) (no
Dockerfile). Concurrent crawl — WebDAV PROPFIND or HTML autoindex, auto-detected
per source — with idempotent writes and optional pruning.

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
| `-embed-creds` / `EMBED_CREDENTIALS` | `false` | Embed the URL's `user:pass@` into the written `.strm` URLs |
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

Or run the published image straight from ghcr.io (no build needed), passing config
as env vars and mounting the output directory:

```bash
docker run --rm \
  -e SOURCE_URLS=https://user:pass@host/movies \
  -v "$PWD/out:/strm" \
  ghcr.io/kanya-approve/strm-builder:latest
```

`make run` wraps that `docker run` and forwards the env-var knobs:
`make run SOURCE_URLS=https://user:pass@host/movies DRY_RUN=true`.
