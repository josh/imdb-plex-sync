# imdb-plex-sync

Sync IMDb watchlist to Plex watchlist

Reads an IMDb watchlist CSV export, maps each title to its Plex rating key using a Plex index parquet file you provide (build one with [josh/plex-index](https://github.com/josh/plex-index)), then adds and removes items so the Plex watchlist matches.

## Usage

```sh
go install github.com/josh/imdb-plex-sync@latest
IMDB_WATCHLIST_URL=https://example.com/watchlist.csv \
  PLEX_INDEX_URL=https://example.com/plex.parquet \
  PLEX_TOKEN=... \
  imdb-plex-sync --dry-run
```

## Configuration

| Environment variable | Flag | Description |
| --- | --- | --- |
| `IMDB_WATCHLIST_URL` | `-imdb-watchlist-url` | IMDb watchlist CSV URL or local path. Required. |
| `PLEX_INDEX_URL` | `-plex-index-url` | URL or local path of a Plex index parquet file built with [josh/plex-index](https://github.com/josh/plex-index). Required. |
| `PLEX_TOKEN` | | Plex account token, sent as `X-Plex-Token`. Required unless `PLEX_DISCOVER_URL` points at a proxy that adds it. |
| `PLEX_DISCOVER_URL` | `-plex-discover-url` | Alternate Plex Discover base URL, such as an authenticating proxy. Defaults to `https://discover.provider.plex.tv`. |
| `DRY_RUN` | `-dry-run`, `-n` | Show what would change without changing anything. |
| | `-v` | Enable verbose logging. |
| | `-version`, `-V` | Print the version. |

## See Also

[josh/imdb-data](https://github.com/josh/imdb-data) and [josh/trakt-plex-sync](https://github.com/josh/trakt-plex-sync).
