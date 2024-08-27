# imdb-plex-sync

Sync IMDb watchlist to Plex watchlist

## Setup

Design to run via GitHub Actions. To get started, Fork this repository.

Then set a bunch of Repository secrets for the following:

- `IMDB_WATCHLIST_URL`: Your IMDb watchlist CSV URL
- `PLEX_USERNAME`: Plex email or username
- `PLEX_PASSWORD`: Plex password
- `PLEX_TOKEN`: Plex server token

## See Also

[josh/imdb-data](https://github.com/josh/imdb-data) and [josh/trakt-plex-sync](https://github.com/josh/trakt-plex-sync).
