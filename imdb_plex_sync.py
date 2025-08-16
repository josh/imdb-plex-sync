import csv
import json
import logging
import re
import urllib.parse
import urllib.request
from collections.abc import Iterator
from pathlib import Path
from typing import Any

import click

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


def _sparql(query: str) -> Any:
    headers = {
        "Accept": "application/json",
        "User-Agent": "IMDbPlexBot/0.0 (https://github.com/josh/imdb-plex-sync)",
    }
    data = urllib.parse.urlencode({"query": query}).encode("utf-8")
    req = urllib.request.Request(
        "https://query.wikidata.org/sparql",
        data=data,
        headers=headers,
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=90) as response:
        return json.load(response)


_SPARQL_QUERY = """
SELECT DISTINCT ?imdb_id ?plex_id WHERE {
  VALUES ?imdb_id { ?imdb_ids }
  ?item wdt:P345 ?imdb_id; wdt:P11460 ?plex_id.
}
"""


def _imdb_to_plex_ids(imdb_ids: list[str]) -> list[str]:
    values_str = " ".join([f'"{v}"' for v in imdb_ids])
    query = _SPARQL_QUERY.replace("?imdb_ids", values_str)
    data = _sparql(query)

    imdb_to_plex: dict[str, str] = {}
    for result in data["results"]["bindings"]:
        imdb_id = result["imdb_id"]["value"]
        plex_id = result["plex_id"]["value"]
        if re.match(r"^[a-f0-9]{24}$", plex_id):
            if imdb_id in imdb_to_plex:
                logger.warning("Duplicate IMDb ID %s", imdb_id)
            imdb_to_plex[imdb_id] = plex_id

    plex_ids = [
        imdb_to_plex[imdb_id] for imdb_id in imdb_ids if imdb_id in imdb_to_plex
    ]
    if len(plex_ids) < len(imdb_ids):
        logger.warning("Found %i/%i IMDb IDs", len(plex_ids), len(imdb_ids))
    else:
        logger.info("Found all %i IMDB IDs", len(imdb_ids))

    return plex_ids


def _plex_watchlist(token: str) -> list[str]:
    keys: list[str] = []
    url = "https://discover.provider.plex.tv/library/sections/watchlist/all"
    headers = {
        "Accept": "application/json",
        "X-Plex-Provider-Version": "7.2.0",
        "X-Plex-Container-Size": "300",
        "X-Plex-Token": token,
    }
    req = urllib.request.Request(url=url, headers=headers)
    with urllib.request.urlopen(req, timeout=30) as response:
        data = json.load(response)
        for metadata in data["MediaContainer"]["Metadata"]:
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
def main(
    imdb_watchlist_url: str,
    plex_token: str,
    verbose: bool,
) -> None:
    logging.basicConfig(level=logging.DEBUG if verbose else logging.INFO)

    imdb_ids = _fetch_imdb_watchlist(imdb_watchlist_url)
    imdb_keys = set(_imdb_to_plex_ids(imdb_ids))
    plex_keys = set(_plex_watchlist(token=plex_token))

    for key in imdb_keys - plex_keys:
        logger.info("+ %s", key)
        _plex_watchlist_add(plex_token, key)

    for key in plex_keys - imdb_keys:
        logger.info("- %s", key)
        _plex_watchlist_remove(plex_token, key)


if __name__ == "__main__":
    main()
