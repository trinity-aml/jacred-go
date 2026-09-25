package core

// PruneTaskPages drops task slots whose page is past maxPage, and returns the
// trimmed slice together with how many went.
//
// Parsers keep a per-category map of pages to sweep in ParseAllTask. Until this
// existed the maps were additive only: UpdateTasksParse read the live pager,
// added every page up to it, and never removed one. A category that shrank —
// or a single bad read that once reported a huge pager — left slots behind
// forever, and ParseAllTask fetches every slot it finds. At the deployed
// parseDelay of 7 s that is seven seconds of every full sweep, per ghost page,
// for as long as the file survives.
//
// **The guard is the point of putting this in one place.** Pruning is
// destructive, and the number it trusts comes from parsing a live page — which
// can come back as a Cloudflare interstitial, a maintenance page or a changed
// layout, all of them HTTP 200 with no pager in them. A pager that did not parse
// yields maxPage 0, and pruning to 0 would wipe the map. So a non-positive
// maxPage prunes **nothing**: the worst case is that a map stays too long, never
// that a sweep loses its plan. The C# original guards only against a failed
// fetch (`html == null`) and would still trim a 200 that carries no pager.
//
// The cost of being this careful is that a category which genuinely shrinks to
// its first page alone is never pruned. That is the rarer event by far on these
// trackers, and it is the one whose failure mode is merely wasteful.
func PruneTaskPages[T any](tasks []T, pageOf func(T) int, maxPage int) ([]T, int) {
	if maxPage <= 0 || len(tasks) == 0 {
		return tasks, 0
	}
	kept := tasks[:0:0]
	for _, t := range tasks {
		if pageOf(t) <= maxPage {
			kept = append(kept, t)
		}
	}
	return kept, len(tasks) - len(kept)
}

// PruneTaskPagesIfRead is PruneTaskPages for parsers whose pages are numbered
// from 1 rather than 0.
//
// There the shared guard cannot work: those parsers seed maxPage with 1, so an
// unread pager and a genuine single-page section produce the same number, and
// trusting it would trim a whole section to its first page on any body that is
// not a listing. The caller therefore has to supply separate evidence that the
// page it parsed really was one — a marker it already trusts elsewhere, never
// the pager itself, or the check would be circular.
//
// With that evidence a single-page section prunes correctly; without it nothing
// is touched.
func PruneTaskPagesIfRead[T any](tasks []T, pageOf func(T) int, maxPage int, pageIsAListing bool) ([]T, int) {
	if !pageIsAListing || maxPage < 1 {
		return tasks, 0
	}
	return PruneTaskPages(tasks, pageOf, maxPage)
}
