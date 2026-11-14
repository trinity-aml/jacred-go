package bitru

import "testing"

// The production failure this guards, verbatim from the box on 2026-11-13/14:
//
//	movie pruned 2581 stale page tasks (was 2582, now 1)
//	serial pruned 714 stale page tasks (was 715, now 1)
//
// Two fetches that came back without a pager destroyed 3295 page slots — the
// entire sweep plan for both categories. maxPages is seeded at 1, so a body
// with no pager yields the same number as a real single-page category, and the
// rebuild wrote that number in.
func TestARebuildWillNotShrinkTheMapOnAnUnvouchedRead(t *testing.T) {
	tests := []struct {
		name        string
		have        int
		pagerRead   bool
		isListing   bool
		wantRefused bool
	}{
		// The production case: a challenge or error body, 715 pages on record.
		{"not a listing, large map", 715, false, false, true},
		// The same body when the map happens to be small — still refused,
		// because the body is not evidence of anything either way.
		{"not a listing, small map", 1, false, false, true},
		// A real listing whose pager markup stopped matching. Same
		// catastrophe by another route, so also refused.
		{"listing, pager unread, large map", 716, false, true, true},
		// A genuine single-page category: no pagination links, and the map is
		// already at most one page, so nothing is lost by rebuilding.
		{"listing, pager unread, map already single", 1, false, true, false},
		{"listing, pager unread, map empty", 0, false, true, false},
		// The normal path: the pager was read, so its maximum is trusted even
		// when it shrinks the map.
		{"listing, pager read, shrinks", 716, true, true, false},
		{"listing, pager read, grows", 10, true, true, false},
	}
	for _, tc := range tests {
		why := rebuildRefusal(tc.have, tc.pagerRead, tc.isListing)
		if refused := why != ""; refused != tc.wantRefused {
			t.Errorf("%s: refused=%v (%q), want refused=%v", tc.name, refused, why, tc.wantRefused)
		}
	}
}

// A refusal has to say which of the two pieces of evidence was missing: "the
// body was not a listing" and "the listing had no pager" need different fixes —
// the first is a fetch or auth problem, the second is markup drift.
func TestARefusalNamesWhichEvidenceWasMissing(t *testing.T) {
	notListing := rebuildRefusal(715, false, false)
	noPager := rebuildRefusal(715, false, true)
	if notListing == "" || noPager == "" {
		t.Fatal("both of these must be refused")
	}
	if notListing == noPager {
		t.Error("the two refusals read identically, so a log reader cannot tell a failed fetch from changed pagination markup")
	}
}
