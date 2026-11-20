package main

import (
	"maps"
	"slices"
	"testing"
)

func TestPlexIndex(t *testing.T) {
	tests := map[string]string{
		"tt0111161":  "5d7768248a7581001f12bc77",
		"tt0903747":  "5d9c083402391c001f583d09",
		"tt15398776": "613b69464d1516c90af83633",
	}
	keys, err := plexRatingKeys(t.Context(), slices.Sorted(maps.Keys(tests)))
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range tests {
		if got := keys[want]; got != id {
			t.Errorf("rating key %s: got IMDb ID %q, want %q", want, got, id)
		}
	}
}
