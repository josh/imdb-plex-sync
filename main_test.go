package main

import (
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestPlexIndex(t *testing.T) {
	source := os.Getenv("PLEX_INDEX_URL")
	if source == "" {
		t.Skip("PLEX_INDEX_URL not set")
	}
	tests := map[string]string{
		"tt0111161":  "5d7768248a7581001f12bc77",
		"tt0903747":  "5d9c083402391c001f583d09",
		"tt15398776": "613b69464d1516c90af83633",
	}
	keys, err := plexRatingKeys(t.Context(), source, slices.Sorted(maps.Keys(tests)))
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range tests {
		if !slices.Contains(keys[id], want) {
			t.Errorf("%s: got rating keys %v, want %s", id, keys[id], want)
		}
	}
}

func TestIMDbWatchlist(t *testing.T) {
	source := os.Getenv("IMDB_WATCHLIST_URL")
	if source == "" {
		t.Skip("IMDB_WATCHLIST_URL not set")
	}
	titles, err := fetchIMDbWatchlist(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	if len(titles) == 0 {
		t.Fatal("got empty watchlist")
	}
	ids := make([]string, 0, len(titles))
	for _, title := range titles {
		if !strings.HasPrefix(title.ID, "tt") {
			t.Errorf("invalid IMDb ID %q", title.ID)
		}
		ids = append(ids, title.ID)
	}

	index := os.Getenv("PLEX_INDEX_URL")
	if index == "" {
		return
	}
	keys, err := plexRatingKeys(t.Context(), index, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) == 0 {
		t.Error("no watchlist titles mapped to Plex rating keys")
	}
}
