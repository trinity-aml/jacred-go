package server

import (
	"strings"
	"testing"
)

// One list for all three search paths. The shapes here are what clients
// actually send: Sonarr and AIOStreams append the episode, Radarr appends the
// year, and a few send both.
func TestSearchFallbacksCoverWhatClientsSend(t *testing.T) {
	cases := []struct {
		query string
		want  []string // expected fallback queries, in order
	}{
		{"The Boys S02E03", []string{"The Boys"}},
		{"The Matrix 1999", []string{"The Matrix"}},
		{"Пацаны 2 сезон", []string{"Пацаны"}},
		// Neither stripper alone leaves a bare title here, so all three are
		// tried: each single strip (both are plausible — the client may have
		// appended only one of the two) and then the combination.
		{"The Boys S02E03 2020", []string{"The Boys 2020", "The Boys S02E03", "The Boys"}},
		// Nothing to strip: no extra queries at all, so a working search is
		// never slowed by this.
		{"The Boys", nil},
		{"48 Hrs.", nil},
		{"tt0944947", nil},
		{"", nil},
	}
	for _, c := range cases {
		got := searchFallbacks(c.query)
		var names []string
		for _, fb := range got {
			names = append(names, fb.Query)
		}
		if strings.Join(names, "|") != strings.Join(c.want, "|") {
			t.Errorf("searchFallbacks(%q) = %v, want %v", c.query, names, c.want)
		}
	}
}

// The numbers are carried, not discarded: two of the three endpoints filter on
// them, and dropping the season turns a request for one into a request for all.
func TestSearchFallbacksCarryTheNumbers(t *testing.T) {
	got := searchFallbacks("The Boys S02E03")
	if len(got) != 1 || got[0].Season != 2 || got[0].Episode != 3 {
		t.Fatalf("season/episode lost: %+v", got)
	}
	got = searchFallbacks("The Matrix 1999")
	if len(got) != 1 || got[0].Year != 1999 {
		t.Fatalf("year lost: %+v", got)
	}
	got = searchFallbacks("The Boys S02E03 2020")
	if len(got) < 2 {
		t.Fatalf("expected a combined fallback: %+v", got)
	}
	last := got[len(got)-1]
	if last.Season != 2 || last.Episode != 3 || last.Year != 2020 {
		t.Errorf("the combined fallback lost numbers: %+v", last)
	}
}

// A fruitless query must not be able to cause unbounded work.
func TestSearchFallbacksAreBounded(t *testing.T) {
	if got := searchFallbacks("Show S01E01 2020"); len(got) > maxSearchFallbacks {
		t.Errorf("%d fallbacks, cap is %d", len(got), maxSearchFallbacks)
	}
	// And no duplicates, which would mean running the same query twice.
	seen := map[string]bool{}
	for _, fb := range searchFallbacks("Show S01E01 2020") {
		if seen[fb.Query] {
			t.Errorf("duplicate fallback %q", fb.Query)
		}
		seen[fb.Query] = true
	}
}
