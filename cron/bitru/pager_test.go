package bitru

import (
	"os"
	"strings"
	"testing"
)

// The pager is read from two different bodies and they are not the same text.
//
// bitru is browser-only whenever its clearance cannot be replayed, and a
// browser body is the serialised DOM, where an href's `&` comes back as
// `&amp;`. The old regex demanded a literal `&`, so it matched nothing on a
// rendered body and maxPages fell back to its seed of 1 — which is how
// `movie pruned 2581 stale page tasks (was 2582, now 1)` happened with a pager
// sitting right there advertising 2584 pages.
//
// Measured 2026-11-14 on the live page: the rendered body carries
// `browse.php?tmp=movie&amp;page=` 17 times and `…&page=` zero times.
func TestThePagerIsReadFromBothBodyForms(t *testing.T) {
	rendered, err := os.ReadFile("testdata/pager_movie_rendered.html")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	// The raw-HTTP form of the same markup: bitru's own output escapes nothing.
	raw := strings.ReplaceAll(string(rendered), "&amp;", "&")

	for _, tc := range []struct {
		name string
		body string
	}{
		{"browser-rendered DOM (&amp;)", string(rendered)},
		{"raw HTTP (&)", raw},
	} {
		max, links := pagerMaxPage(tc.body, "movie")
		if max != 2584 {
			t.Errorf("%s: maxPages = %d, want 2584 — the pager in this body is unreadable, so the map would collapse to one page", tc.name, max)
		}
		// 2,3,2581,2582,2583,2584 — six numeric page links. The "»" next link
		// and the "..." ellipsis must not count: one has attributes between the
		// href and the '>', the other has no digits.
		if links != 6 {
			t.Errorf("%s: matched %d links, want 6 (the » next link and the ... ellipsis must not match)", tc.name, links)
		}
	}
}

// A body with no pager must report zero links, so rebuildRefusal can tell it
// apart from a real single-page category. Without that the seed of 1 is
// indistinguishable from a read.
func TestABodyWithNoPagerReportsNoLinks(t *testing.T) {
	max, links := pagerMaxPage(`<html><body><a href="details.php?id=7">x</a></body></html>`, "movie")
	if links != 0 {
		t.Errorf("matched %d links in a body with no pager", links)
	}
	if max != 1 {
		t.Errorf("maxPages = %d, want the seed 1", max)
	}
	if why := rebuildRefusal(2582, links > 0, true); why == "" {
		t.Error("a listing with no pager links and 2582 pages on record must be refused, not rebuilt")
	}
}

// The scoping the original regex existed for: a link to another category must
// not inflate this one.
func TestThePagerIgnoresOtherCategories(t *testing.T) {
	body := `<a href="browse.php?tmp=serial&amp;page=716#content">716</a>` +
		`<a href="browse.php?tmp=movie&amp;page=3#content">3</a>`
	if max, links := pagerMaxPage(body, "movie"); max != 3 || links != 1 {
		t.Errorf("movie read maxPages=%d links=%d, want 3 and 1 — a serial link leaked in", max, links)
	}
}
