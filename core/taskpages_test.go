package core

import "testing"

type tp struct {
	page int
	seen bool
}

func pages(ts []tp) []int {
	out := make([]int, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.page)
	}
	return out
}

func eq(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPruneTaskPagesDropsGhosts(t *testing.T) {
	in := []tp{{page: 0}, {page: 1}, {page: 2}, {page: 3}, {page: 4}}
	got, n := PruneTaskPages(in, func(x tp) int { return x.page }, 2)
	if n != 2 {
		t.Errorf("pruned %d, want 2", n)
	}
	if !eq(pages(got), []int{0, 1, 2}) {
		t.Errorf("kept %v, want [0 1 2]", pages(got))
	}
}

// The whole reason this lives in one place. maxPage comes from parsing a live
// page, and a Cloudflare interstitial, a maintenance notice or a changed layout
// all arrive as HTTP 200 with no pager — yielding 0. Pruning to 0 would wipe the
// sweep plan, so a non-positive maxPage must change nothing at all.
func TestPruneTaskPagesRefusesANonPositiveMax(t *testing.T) {
	in := []tp{{page: 0}, {page: 1}, {page: 7}}
	for _, max := range []int{0, -1, -100} {
		got, n := PruneTaskPages(in, func(x tp) int { return x.page }, max)
		if n != 0 {
			t.Errorf("maxPage=%d pruned %d slots; a map must never be wiped by an unread pager", max, n)
		}
		if !eq(pages(got), []int{0, 1, 7}) {
			t.Errorf("maxPage=%d changed the map to %v", max, pages(got))
		}
	}
}

func TestPruneTaskPagesKeepsEverythingInRange(t *testing.T) {
	in := []tp{{page: 0}, {page: 1}, {page: 2}}
	got, n := PruneTaskPages(in, func(x tp) int { return x.page }, 5)
	if n != 0 || !eq(pages(got), []int{0, 1, 2}) {
		t.Errorf("pruned %d, got %v; nothing was past the tail", n, pages(got))
	}
}

// Slots carry per-page state (last update time), so pruning must keep the
// surviving values intact rather than rebuild them.
func TestPruneTaskPagesPreservesSurvivingValues(t *testing.T) {
	in := []tp{{page: 0, seen: true}, {page: 1}, {page: 9, seen: true}}
	got, _ := PruneTaskPages(in, func(x tp) int { return x.page }, 1)
	if len(got) != 2 || !got[0].seen || got[1].seen {
		t.Errorf("surviving values not preserved: %+v", got)
	}
}

// The caller keeps its own slice; pruning must not scribble over it, or a
// parser holding the pre-prune value would see a corrupted map.
func TestPruneTaskPagesDoesNotAliasTheInput(t *testing.T) {
	in := []tp{{page: 0}, {page: 1}, {page: 2}, {page: 3}}
	got, _ := PruneTaskPages(in, func(x tp) int { return x.page }, 1)
	got[0].page = 99
	if in[0].page != 0 {
		t.Error("pruning wrote through to the caller's slice")
	}
}

func TestPruneTaskPagesHandlesAnEmptyMap(t *testing.T) {
	got, n := PruneTaskPages(nil, func(x tp) int { return x.page }, 5)
	if n != 0 || len(got) != 0 {
		t.Errorf("empty map: pruned %d, got %v", n, got)
	}
}

// A parser numbering pages from 1 seeds maxPage with 1, so the number alone
// cannot say whether the pager was read. Without separate evidence the whole
// section would be trimmed to its first page by any body that is not a listing.
func TestPruneTaskPagesIfReadNeedsEvidence(t *testing.T) {
	in := []tp{{page: 1}, {page: 2}, {page: 9}}

	// No evidence: a Cloudflare page, a login wall, a changed layout — the map
	// must survive all of them untouched, even though maxPage looks usable.
	for _, max := range []int{1, 5, 100} {
		got, n := PruneTaskPagesIfRead(in, func(x tp) int { return x.page }, max, false)
		if n != 0 || !eq(pages(got), []int{1, 2, 9}) {
			t.Errorf("maxPage=%d without evidence pruned %d slots", max, n)
		}
	}
}

// With evidence a genuine single-page section prunes correctly — which is the
// case the plain guard has to refuse and the reason this variant exists.
func TestPruneTaskPagesIfReadTrimsASinglePageSection(t *testing.T) {
	in := []tp{{page: 1}, {page: 2}, {page: 3}}
	got, n := PruneTaskPagesIfRead(in, func(x tp) int { return x.page }, 1, true)
	if n != 2 {
		t.Errorf("pruned %d, want 2", n)
	}
	if !eq(pages(got), []int{1}) {
		t.Errorf("kept %v, want [1]", pages(got))
	}
}

// Page 0 is not a valid slot for a 1-based parser, so a max below 1 is still
// treated as unread however confident the caller is.
func TestPruneTaskPagesIfReadRejectsAnImpossibleMax(t *testing.T) {
	in := []tp{{page: 1}, {page: 2}}
	for _, max := range []int{0, -3} {
		got, n := PruneTaskPagesIfRead(in, func(x tp) int { return x.page }, max, true)
		if n != 0 || !eq(pages(got), []int{1, 2}) {
			t.Errorf("maxPage=%d pruned %d slots", max, n)
		}
	}
}
