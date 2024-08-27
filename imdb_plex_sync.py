import csv
import logging
import re
from collections.abc import Iterator
from pathlib import Path
from typing import Any

import click
import requests
from plexapi.myplex import MyPlexAccount  # type: ignore
from plexapi.video import Video  # type: ignore

logger = logging.getLogger("imdb-trakt-sync")


@click.command()
@click.option(
    "--imdb-watchlist-url",
    required=True,
    envvar="IMDB_WATCHLIST_URL",
)
@click.option(
    "--plex-username",
    required=True,
    envvar="PLEX_USERNAME",
)
@click.option(
    "--plex-password",
    required=True,
    envvar="PLEX_PASSWORD",
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
    plex_username: str,
    plex_password: str,
    plex_token: str | None,
    verbose: bool,
) -> None:
    logging.basicConfig(level=logging.DEBUG if verbose else logging.INFO)

    imdb_ids = _fetch_imdb_watchlist(imdb_watchlist_url)
    imdb_keys = set(_imdb_to_plex_ids(imdb_ids))

    account = MyPlexAccount(
        username=plex_username,
        password=plex_password,
        token=plex_token,
    )
    plex_keys: set[str] = set(
        item.key.replace("/library/metadata/", "") for item in account.watchlist()
    )

    for key in imdb_keys - plex_keys:
        video = _find_by_plex_guid(account, key)
        logger.info("+ %s", video.title)
        video.addToWatchlist()

    for key in plex_keys - imdb_keys:
        video = _find_by_plex_guid(account, key)
        logger.info("- %s", video.title)
        video.removeFromWatchlist()


def _fetch_imdb_watchlist(url: str) -> list[str]:
    return [row["Const"] for row in csv.DictReader(_iterlines(url))]


def _iterlines(path: Path | str) -> Iterator[str]:
    if isinstance(path, str) and path.startswith("http"):
        logger.debug("Fetching remote '%s'", path)
        response = requests.get(path)
        response.raise_for_status()
        yield from response.iter_lines(decode_unicode=True)
    else:
        logger.debug("Reading local file '%s'", path)
        with open(path) as f:
            yield from f


def _sparql(query: str) -> Any:
    r = requests.post(
        "https://query.wikidata.org/sparql",
        data={"query": query},
        headers={
            "Accept": "application/json",
            "User-Agent": "IMDbPlexBot/0.0 (https://github.com/josh/imdb-plex-sync)",
        },
        timeout=(1, 90),
    )
    r.raise_for_status()
    data = r.json()
    return data


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


def _find_by_plex_guid(account: MyPlexAccount, ratingkey: str) -> Video:
    return account.fetchItem(
        f"https://metadata.provider.plex.tv/library/metadata/{ratingkey}"
    )


if __name__ == "__main__":
    main()
