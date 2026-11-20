package main

import (
	"slices"
	"testing"
)

func TestPlexIndex(t *testing.T) {
	tests := map[string]string{
		"tt0111161":  "5d7768248a7581001f12bc77",
		"tt0903747":  "5d9c083402391c001f583d09",
		"tt15398776": "613b69464d1516c90af83633",
	}
	ids := slices.Sorted(func(yield func(string) bool) {
		for id := range tests {
			if !yield(id) {
				return
			}
		}
	})
	keys, err := plexRatingKeys(t.Context(), ids)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range tests {
		if !slices.Contains(keys, want) {
			t.Errorf("%s: missing rating key %s in %v", id, want, keys)
		}
	}
}
