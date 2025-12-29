import csv
import json
import logging
import urllib.parse
import urllib.request
from collections.abc import Iterator
from pathlib import Path

import click
import polars as pl

logger = logging.getLogger("imdb-trakt-sync")


def _iterlines(path: Path | str) -> Iterator[str]:
    if isinstance(path, str) and path.startswith("http"):
        logger.debug("Fetching remote '%s'", path)
        with urllib.request.urlopen(path, timeout=10) as response:
            for line in response:
                yield line.decode("utf-8")
    else:
        logger.debug("Reading local file '%s'", path)
        with open(path) as f:
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
    df3 = (
        df1.join(df2, on="imdb_numeric_id", how="left")
        .select("rating_key")
        .filter(pl.col("rating_key").is_not_null())
    )

    plex_rating_keys = df3.collect()["rating_key"].to_list()

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
        page_keys = _plex_watchlist_page(token, offset=offset, size=size)
        keys.extend(page_keys)
        if len(page_keys) < size:
            break
        offset += size
    return keys


def _plex_watchlist_page(token: str, offset: int, size: int) -> list[str]:
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
    with urllib.request.urlopen(req, timeout=30) as response:
        data = json.load(response)
        for metadata in data["MediaContainer"].get("Metadata", []):
            if "ratingKey" in metadata:
                keys.append(metadata["ratingKey"])
    return keys


def _plex_watchlist_add(token: str, key: str) -> None:
    url = f"https://discover.provider.plex.tv/actions/addToWatchlist?ratingKey={key}"
    headers = {
        "Accept": "application/json",
        "X-Plex-Provider-Version": "7.2.0",
        "X-Plex-Token": token,
    }
    req = urllib.request.Request(url=url, headers=headers, method="PUT")
    with urllib.request.urlopen(req, timeout=30) as response:
        data = json.load(response)
        assert data


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
    with urllib.request.urlopen(req, timeout=30) as response:
        data = json.load(response)
        assert data


@click.command()
@click.option(
    "--imdb-watchlist-url",
    required=True,
    envvar="IMDB_WATCHLIST_URL",
)
@click.option(
    "--plex-token",
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

    for key in imdb_keys - plex_keys:
        if dry_run:
            logger.info("[DRY RUN] + %s", key)
        else:
            logger.info("+ %s", key)
            _plex_watchlist_add(plex_token, key)

    for key in plex_keys - imdb_keys:
        if dry_run:
            logger.info("[DRY RUN] - %s", key)
        else:
            logger.info("- %s", key)
            _plex_watchlist_remove(plex_token, key)


if __name__ == "__main__":
    main()
