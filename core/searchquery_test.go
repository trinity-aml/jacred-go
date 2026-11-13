package core

import "testing"

// Every shape below was measured against a live index first: the bare title
// returned a record and the same title with the tokens appended returned zero,
// because SearchName reduces "The Boys S02E03" to "theboyss02e03".
func TestStripSeasonEpisode(t *testing.T) {
	cases := []struct {
		query   string
		want    string
		season  int
		episode int
	}{
		{"The Boys S02E03", "The Boys", 2, 3},
		{"The Boys s2e3", "The Boys", 2, 3},
		{"The Boys S02", "The Boys", 2, 0},
		{"The Boys E05", "The Boys", 0, 5},
		{"The Boys 1x05", "The Boys", 1, 5},
		{"Пацаны 2 сезон", "Пацаны", 2, 0},
		{"Пацаны 1-2 сезон", "Пацаны", 1, 0},
		{"Пацаны Сезон 3", "Пацаны", 3, 0},
		{"Пацаны Season 2", "Пацаны", 2, 0},
		// A tail after the season is quality or voice, which a stored name has
		// no more than it has the season itself.
		{"Пацаны 2 сезон 1080p", "Пацаны", 2, 0},
		{"The Boys Season 2 WEB-DL", "The Boys", 2, 0},
	}
	for _, c := range cases {
		got, season, episode := StripSeasonEpisode(c.query)
		if got != c.want || season != c.season || episode != c.episode {
			t.Errorf("%q -> (%q, s%d, e%d), want (%q, s%d, e%d)",
				c.query, got, season, episode, c.want, c.season, c.episode)
		}
	}
}

// Go's \b is an ASCII word boundary, so `\bсезон\b` matches nothing at all.
// The first version of these patterns had it and silently did nothing to any
// Russian query — the unit tests would have agreed with the mistake; a live
// query is what exposed it.
func TestStripSeasonEpisodeHandlesCyrillic(t *testing.T) {
	// Exact season numbers are pinned in the table above; what this asserts is
	// only that the Cyrillic form is recognised at all.
	for _, q := range []string{"Пацаны 2 сезон", "Пацаны Сезон 2", "Пацаны 1-2 сезон"} {
		got, season, _ := StripSeasonEpisode(q)
		if got != "Пацаны" {
			t.Errorf("%q -> %q, want the title alone", q, got)
		}
		if season == 0 {
			t.Errorf("%q gave up the season number", q)
		}
	}
}

// An empty result means "nothing to retry", which is how the callers tell a
// season query from an ordinary one without comparing strings themselves.
func TestStripSeasonEpisodeLeavesOrdinaryQueriesAlone(t *testing.T) {
	cases := []string{
		"The Boys", "Пацаны", "",
		"   ",
		// Trailing punctuation is not a season. Both of these were rewritten by
		// an earlier version whose cleanup ran even when nothing matched —
		// caught by sweeping 694 real names out of the index.
		"48 Hrs.", "Не покидай...",
		// Id lookups are not titles and must never be rewritten.
		"tt0944947", "kp301", "1234567",
		// A bare trailing number is a sequel, not a season.
		"Стражи Галактики 2", "Терминатор 2",
	}
	for _, q := range cases {
		if got, _, _ := StripSeasonEpisode(q); got != "" {
			t.Errorf("%q was rewritten to %q", q, got)
		}
	}
}

// A query that is nothing but a season token has no title left to search for.
func TestStripSeasonEpisodeRejectsAnEmptyRemainder(t *testing.T) {
	for _, q := range []string{"S01E05", "2 сезон", "1x05"} {
		if got, _, _ := StripSeasonEpisode(q); got != "" {
			t.Errorf("%q left %q, which is not a title", q, got)
		}
	}
}

func TestStripTrailingYear(t *testing.T) {
	cases := []struct {
		query, want string
		year        int
	}{
		{"The Matrix 1999", "The Matrix", 1999},
		{"Матрица (1999)", "Матрица", 1999},
		{"Матрица [1999]", "Матрица", 1999},
		{"The Matrix   2003 ", "The Matrix", 2003},
		{"ATP Cincinnati 2007", "ATP Cincinnati", 2007},
	}
	for _, c := range cases {
		got, year := StripTrailingYear(c.query)
		if got != c.want || year != c.year {
			t.Errorf("StripTrailingYear(%q) = %q, %d; want %q, %d", c.query, got, year, c.want, c.year)
		}
	}
}

// The property that makes this safe to add: it must not touch a query that does
// not end in a year. Swept over 6767 real names out of the production index —
// 86 fired, every one of them genuinely ending in a year, and **zero** fired
// without one. That sweep is the standard to re-run after touching the pattern;
// these are the shapes it is worth keeping in the suite.
func TestStripTrailingYearLeavesOrdinaryQueriesAlone(t *testing.T) {
	for _, q := range []string{
		"The Boys", "Пацаны", "", "   ",
		// A year inside the title is not a trailing year.
		"2001: A Space Odyssey", "1917", "Жизнь 2.0",
		// Not a plausible release year.
		"Omega 1899", "Omega 2100", "Rush 99",
		// Trailing punctuation is not a year, and the cleanup must not run
		// when nothing matched — this is how "48 Hrs." became "48 Hrs".
		"48 Hrs.", "Не покидай...",
		// Id lookups are never titles.
		"tt0944947", "kp301", "1234567",
		// Nothing but a year leaves no title to search for.
		"1999", "(2026)",
		// These are what make the early return load-bearing rather than
		// decorative: with no year matched, the trim below would still eat a
		// trailing comma or dash and quietly rewrite the query. Verified by
		// removing that return and watching these fail.
		"The Matrix,", "Mad Max -", "Волк, ", "Я, робот,",
	} {
		if got, year := StripTrailingYear(q); got != "" {
			t.Errorf("%q was rewritten to %q (year %d)", q, got, year)
		}
	}
}
