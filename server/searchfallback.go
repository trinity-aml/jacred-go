package server

import (
	"strings"

	"jacred/core"
)

// Query fallbacks, in one place because there are three search paths and they
// must not drift: /api/v1.0/torrents, the Jackett JSON endpoint and the Torznab
// XML endpoint. Each used to carry its own copy of the season/episode retry —
// two copies when this was written, with the third endpoint about to add a
// third — and a hand-maintained duplicate in this codebase has a record of
// going stale.
//
// Every fallback is attempted **only when the original query found nothing**.
// That is not an optimisation, it is what makes them safe:
//
//   - A query that already works is never diluted, reordered or slowed.
//   - A title that genuinely ends in a year keeps working. "Blade Runner 2049"
//     finds its own record, so the year is never stripped; doing it
//     unconditionally would answer with the 1982 film instead.
//
// Upstream merges its extra variants into every search (`mergeV1: auto`); this
// deliberately does not.

// searchFallback is one more query worth trying, with whatever the stripped
// tokens said. Season, Episode and Year are 0 when that part was absent. They
// are carried rather than discarded because two of the three endpoints can
// filter on them, and throwing the number away turns a request for one season
// into a request for all of them.
type searchFallback struct {
	Query   string
	Season  int
	Episode int
	Year    int
}

// maxSearchFallbacks bounds the work a fruitless query can cause. A query with
// both a season and a year yields three candidates; the cap is here so a future
// stripper cannot quietly turn one miss into a dozen index walks.
const maxSearchFallbacks = 3

// searchFallbacks lists the queries to try after `query` returned nothing, best
// first. Empty when there is nothing to strip.
func searchFallbacks(query string) []searchFallback {
	var out []searchFallback
	seen := map[string]bool{strings.ToLower(strings.TrimSpace(query)): true}
	add := func(q string, season, episode, year int) {
		q = strings.TrimSpace(q)
		key := strings.ToLower(q)
		if q == "" || seen[key] || len(out) >= maxSearchFallbacks {
			return
		}
		seen[key] = true
		out = append(out, searchFallback{Query: q, Season: season, Episode: episode, Year: year})
	}

	// Season and episode first: it is the commonest shape by far, since Sonarr
	// and AIOStreams append the episode to every query they make.
	noSE, season, episode := core.StripSeasonEpisode(query)
	if noSE != "" {
		add(noSE, season, episode, 0)
	}
	noYear, year := core.StripTrailingYear(query)
	if noYear != "" {
		add(noYear, 0, 0, year)
	}
	// Then both, for "The Boys S02E03 2020" — neither stripper alone leaves a
	// bare title there.
	if noSE != "" {
		if both, y := core.StripTrailingYear(noSE); both != "" {
			add(both, season, episode, y)
		}
	}
	if noYear != "" {
		if both, s, e := core.StripSeasonEpisode(noYear); both != "" {
			add(both, s, e, year)
		}
	}
	return out
}
