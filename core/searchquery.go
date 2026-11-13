package core

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	// "S01E05", "s1e1", "S01", "E05", "1x05" — the shapes Sonarr, Prowlarr and
	// AIOStreams append to a title in a plain `q=` parameter.
	seasonEpisodeTokenRe = regexp.MustCompile(`(?i)\b(s(\d{1,2})e(\d{1,3})|s(\d{1,2})|e(\d{1,3})|(\d{1,2})x(\d{1,3}))\b`)

	// "2 сезон", "1-2 сезон" — and everything after it, which is usually a
	// quality or voice tail the stored name does not carry either.
	// No \b around the Cyrillic: Go's \b is an ASCII word boundary, so
	// `\bсезон\b` never matches at all — the pattern looked right and silently
	// did nothing until a live query proved it.
	russianSeasonSuffixRe = regexp.MustCompile(`(?i)\s*(\d{1,2})(?:\s*-\s*\d{1,2})?\s*сезон.*$`)

	// "Season 2", "Сезон 2" — the same thing written the other way round.
	seasonWordSuffixRe = regexp.MustCompile(`(?i)(?:^|\s)(?:season|сезон)\s*(\d{1,2})(?:\D.*)?$`)

	// An id lookup is not a title and must never be rewritten.
	idQueryRe = regexp.MustCompile(`(?i)^\s*(tt\d{5,}|kp\d+|\d{5,})\s*$`)

	multiSpaceRe = regexp.MustCompile(`\s{2,}`)
)

// StripSeasonEpisode removes inline season/episode tokens from a free-text
// search query and reports what they said.
//
// A stored record's name is the bare title — "Пацаны", "The Boys" — while a
// client that wants one episode asks for `q=The Boys S02E03`. `SearchName`
// reduces both to letters and digits, so the query becomes "theboyss02e03" and
// matches nothing at all. Measured against a live index: "Пацаны" and
// "The Boys" each return a record, while "Пацаны S01E05", "Пацаны 2 сезон",
// "Пацаны Season 2", "The Boys S02E03" and "The Boys 1x05" all return zero.
//
// stripped is empty when the query carries no such tokens, so a caller can tell
// "nothing to do" from "here is another thing to try" without comparing strings.
// season and episode are 0 when that part was absent; they are worth returning
// rather than discarding because `/api/v1.0/torrents` can filter by season, and
// dropping the number would turn a request for one season into a request for
// all of them.
func StripSeasonEpisode(query string) (stripped string, season, episode int) {
	original := strings.TrimSpace(query)
	if original == "" || idQueryRe.MatchString(original) {
		return "", 0, 0
	}

	out := original
	matched := false

	// The word forms carry a tail ("Season 2 1080p"), so run them first — the
	// token form would otherwise leave that tail behind.
	if m := russianSeasonSuffixRe.FindStringSubmatch(out); m != nil {
		season = atoiSafe(m[1])
		out = russianSeasonSuffixRe.ReplaceAllString(out, "")
		matched = true
	}
	if m := seasonWordSuffixRe.FindStringSubmatch(out); m != nil {
		if season == 0 {
			season = atoiSafe(m[1])
		}
		out = seasonWordSuffixRe.ReplaceAllString(out, "")
		matched = true
	}
	for _, m := range seasonEpisodeTokenRe.FindAllStringSubmatch(out, -1) {
		matched = true
		switch {
		case m[2] != "": // S01E05
			if season == 0 {
				season = atoiSafe(m[2])
			}
			if episode == 0 {
				episode = atoiSafe(m[3])
			}
		case m[4] != "": // S01
			if season == 0 {
				season = atoiSafe(m[4])
			}
		case m[5] != "": // E05
			if episode == 0 {
				episode = atoiSafe(m[5])
			}
		case m[6] != "": // 1x05
			if season == 0 {
				season = atoiSafe(m[6])
			}
			if episode == 0 {
				episode = atoiSafe(m[7])
			}
		}
	}
	if !matched {
		// Nothing was a season or an episode. Returning early matters: the
		// cleanup below exists to tidy what a removal left behind, and running
		// it unconditionally would rewrite ordinary titles — measured against
		// 694 real names, it turned "48 Hrs." into "48 Hrs" and "Не покидай..."
		// into "Не покидай", neither of which is a season query.
		return "", 0, 0
	}

	out = seasonEpisodeTokenRe.ReplaceAllString(out, "")
	out = multiSpaceRe.ReplaceAllString(out, " ")
	out = strings.Trim(out, " -–—,.")

	// Nothing left, or nothing changed: there is no second query worth running.
	if out == "" || strings.EqualFold(out, original) {
		return "", 0, 0
	}
	return out, season, episode
}

func atoiSafe(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// A trailing release year, bare or parenthesised: "The Matrix 1999",
// "Матрица (1999)". Anchored at the end so a year inside the title — "2001: A
// Space Odyssey", "1917" — is not a match, and bounded to plausible release
// years rather than any four digits.
var trailingYearRe = regexp.MustCompile(`(?:^|\s)[(\[]?((?:19|20)\d{2})[)\]]?\s*$`)

// StripTrailingYear removes a trailing release year from a free-text query and
// reports it.
//
// Clients append the year to disambiguate — Radarr and AIOStreams send
// "The Matrix 1999" — while a stored record's name is the bare title and the
// year lives in its own `relased` field. `SearchName` keeps digits, so the
// query becomes "thematrix1999" and matches nothing.
//
// Like StripSeasonEpisode this returns an empty string when there is nothing to
// strip, so a caller can tell "nothing to do" from "here is another thing to
// try", and it returns the year rather than discarding it: both search paths
// can filter on it, and dropping the number would widen a request for the 1999
// film into a request for every remake.
//
// It is only ever a **fallback**, run when the original query found nothing.
// That is what makes it safe on a title that ends in a year of its own: a
// search for "Blade Runner 2049" finds its record and the year is never
// touched. Stripping unconditionally would turn that query into one for the
// 1982 film.
func StripTrailingYear(query string) (stripped string, year int) {
	original := strings.TrimSpace(query)
	if original == "" || idQueryRe.MatchString(original) {
		return "", 0
	}
	m := trailingYearRe.FindStringSubmatch(original)
	if m == nil {
		// No year, so no cleanup either. This is load-bearing, not
		// decorative: the trim below would eat a trailing comma or dash from
		// a query that has no year in it at all, turning "Я, робот," into
		// "Я, робот". That is the same mistake that once rewrote "48 Hrs." —
		// cleanup must only tidy what a removal actually left behind.
		return "", 0
	}
	out := strings.TrimSpace(trailingYearRe.ReplaceAllString(original, ""))
	out = multiSpaceRe.ReplaceAllString(out, " ")
	out = strings.Trim(out, " -–—,")
	// A query that was nothing but a year leaves no title to search for.
	if out == "" || strings.EqualFold(out, original) {
		return "", 0
	}
	return out, atoiSafe(m[1])
}
