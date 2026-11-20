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
		if !slices.Contains(keys[id], want) {
			t.Errorf("%s: got rating keys %v, want %s", id, keys[id], want)
		}
	}
}
