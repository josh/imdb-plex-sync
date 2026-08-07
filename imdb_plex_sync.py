import csv
import http.client
import json
import logging
import time
import urllib.error
import urllib.request
from collections.abc import Iterator
from pathlib import Path

import click
import polars as pl

logger = logging.getLogger("imdb-trakt-sync")

_MAX_ATTEMPTS = 4
_RETRY_STATUSES = frozenset({408, 425, 429, 500, 502, 503, 504})


def _urlopen(url: urllib.request.Request | str, timeout: float) -> bytes:
    for attempt in range(_MAX_ATTEMPTS):
        error: Exception
        try:
            with urllib.request.urlopen(url, timeout=timeout) as response:
                data: bytes = response.read()
                return data
        except urllib.error.HTTPError as e:
            if e.code not in _RETRY_STATUSES or attempt == _MAX_ATTEMPTS - 1:
                raise
            error = e
        except (urllib.error.URLError, TimeoutError, http.client.HTTPException) as e:
            if attempt == _MAX_ATTEMPTS - 1:
                raise
            error = e
        delay = 2**attempt
        logger.warning("Retrying in %is after error: %s", delay, error)
        time.sleep(delay)
    raise AssertionError("unreachable")


def _iterlines(path: Path | str) -> Iterator[str]:
    if isinstance(path, str) and path.startswith("http"):
        logger.debug("Fetching remote '%s'", path)
        yield from _urlopen(path, timeout=10).decode("utf-8").splitlines(keepends=True)
    else:
        logger.debug("Reading local file '%s'", path)
        with open(path, encoding="utf-8") as f:
            yield from f


def _fetch_imdb_watchlist(url: str) -> list[str]:
    return [row["Const"] for row in csv.DictReader(_iterlines(url))]


def _imdb_to_plex_rating_keys(imdb_ids: list[str]) -> list[str]:
    df1 = pl.LazyFrame({"imdb_id": imdb_ids}).select(
        imdb_numeric_id=pl.col("imdb_id").str.replace("tt", "").cast(pl.Int64)
    )
    df2 = pl.scan_parquet("https://josh.github.io/plex-index/plex.parquet").select(
        rating_key=pl.col("key").bin.encode("hex"),
        imdb_numeric_id=pl.col("imdb_numeric_id"),
    )
    df3 = df1.join(df2, on="imdb_numeric_id", how="left").select("rating_key")
    df4 = df3.filter(pl.col("rating_key").is_not_null())

    plex_rating_keys = df4.collect()["rating_key"].to_list()

    if len(plex_rating_keys) < len(imdb_ids):
        logger.warning("Found %i/%i IMDb IDs", len(plex_rating_keys), len(imdb_ids))
    else:
        logger.info("Found all %i IMDB IDs", len(imdb_ids))

    return plex_rating_keys


def _plex_watchlist(token: str) -> list[str]:
    keys: list[str] = []
    offset = 0
    size = 50
    while True:
        page_keys, page_size = _plex_watchlist_page(token, offset=offset, size=size)
        keys.extend(page_keys)
        if page_size < size:
            break
        offset += size
    return keys


def _plex_watchlist_page(token: str, offset: int, size: int) -> tuple[list[str], int]:
    assert size <= 100
    keys: list[str] = []
    url = "https://discover.provider.plex.tv/library/sections/watchlist/all"
    headers = {
        "Accept": "application/json",
        "X-Plex-Provider-Version": "7.2.0",
        "X-Plex-Container-Start": str(offset),
        "X-Plex-Container-Size": str(size),
        "X-Plex-Token": token,
    }
    req = urllib.request.Request(url=url, headers=headers)
    data = json.loads(_urlopen(req, timeout=30))
    metadata_items = data["MediaContainer"].get("Metadata", [])
    for metadata in metadata_items:
        if "ratingKey" in metadata:
            keys.append(metadata["ratingKey"])
    return keys, len(metadata_items)


def _plex_watchlist_add(token: str, key: str) -> None:
    url = f"https://discover.provider.plex.tv/actions/addToWatchlist?ratingKey={key}"
    headers = {
        "Accept": "application/json",
        "X-Plex-Provider-Version": "7.2.0",
        "X-Plex-Token": token,
    }
    req = urllib.request.Request(url=url, headers=headers, method="PUT")
    assert json.loads(_urlopen(req, timeout=30))


def _plex_watchlist_remove(token: str, key: str) -> None:
    url = (
        f"https://discover.provider.plex.tv/actions/removeFromWatchlist?ratingKey={key}"
    )
    headers = {
        "Accept": "application/json",
        "X-Plex-Provider-Version": "7.2.0",
        "X-Plex-Token": token,
    }
    req = urllib.request.Request(url=url, headers=headers, method="PUT")
    assert json.loads(_urlopen(req, timeout=30))


@click.command()
@click.option(
    "--imdb-watchlist-url",
    required=True,
    envvar="IMDB_WATCHLIST_URL",
)
@click.option(
    "--plex-token",
    required=True,
    envvar="PLEX_TOKEN",
)
@click.option(
    "--verbose",
    "-v",
    is_flag=True,
    help="Enable verbose logging",
    envvar="ACTIONS_RUNNER_DEBUG",
)
@click.option(
    "--dry-run",
    "-n",
    is_flag=True,
    help="Show what would be done without making changes",
)
def main(
    imdb_watchlist_url: str,
    plex_token: str,
    verbose: bool,
    dry_run: bool,
) -> None:
    logging.basicConfig(level=logging.DEBUG if verbose else logging.INFO)

    imdb_ids = _fetch_imdb_watchlist(imdb_watchlist_url)
    imdb_keys = set(_imdb_to_plex_rating_keys(imdb_ids))
    plex_keys = set(_plex_watchlist(token=plex_token))

    failures = 0

    for key in imdb_keys - plex_keys:
        if dry_run:
            logger.info("[DRY RUN] + %s", key)
        else:
            logger.info("+ %s", key)
            try:
                _plex_watchlist_add(plex_token, key)
            except Exception:
                logger.exception("Failed to add %s", key)
                failures += 1

    for key in plex_keys - imdb_keys:
        if dry_run:
            logger.info("[DRY RUN] - %s", key)
        else:
            logger.info("- %s", key)
            try:
                _plex_watchlist_remove(plex_token, key)
            except Exception:
                logger.exception("Failed to remove %s", key)
                failures += 1

    if failures:
        raise SystemExit(f"{failures} watchlist changes failed")


if __name__ == "__main__":
    main()
