package rutracker

import (
	"os"
	"strings"
	"testing"
)

// cf_challenge.html is the verbatim 403 body rutracker.org served for
// /forum/viewforum.php?f=2090 on 2026-07-29, when the forum was put behind a
// Cloudflare managed challenge.
func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	return b
}

// The regression this guards: the challenge body contains neither
// `class="torTopic"` nor the login form, so before it was recognised
// explicitly every category "parsed" into zero rows and the run reported
// success — indistinguishable from a quiet day on the tracker.
func TestCFChallengeIsNotMistakenForAnEmptyListing(t *testing.T) {
	body := decodeRutrackerBody(loadFixture(t, "cf_challenge.html"))

	if !looksLikeCFChallenge(body) {
		t.Fatal("real challenge page not recognised")
	}
	if looksLikeRutrackerLoginForm(body) {
		t.Error("challenge page misread as the login form — would wrongly drop the session cookie")
	}
	if rows := strings.Split(body, `class="torTopic"`); len(rows) != 1 {
		t.Errorf("challenge page yielded %d topic rows, want 0", len(rows)-1)
	}
}

// cleared_listing.html is the page CloakBrowser got back from
// /forum/viewforum.php?f=2090 once the challenge was solved (anonymous view,
// browser-decoded to UTF-8). It still carries CF's JS-detection beacon, which
// is exactly what made the first challenge check misfire on a good page.
func TestClearedPageIsNotMistakenForAChallenge(t *testing.T) {
	body := decodeRutrackerBody(loadFixture(t, "cleared_listing.html"))

	if looksLikeCFChallenge(body) {
		t.Error("cleared listing read as a challenge — every successful run would abort")
	}
	if rows := strings.Count(body, `class="torTopic"`); rows != 50 {
		t.Errorf("cleared listing has %d topic rows, want 50", rows)
	}
	if !strings.Contains(body, "/cdn-cgi/challenge-platform/") {
		t.Skip("fixture no longer carries the CF beacon — the regression it guards is gone")
	}
}

func TestLooksLikeCFChallenge(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"challenge script config", `<html><script>window._cf_chl_opt={cType:'managed'}</script></html>`, true},
		{"challenge platform script", `<script src="/cdn-cgi/challenge-platform/h/b/orchestrate/chl_page/v1"></script>`, true},
		{"interstitial title", `<html><head><title>Just a moment...</title></head></html>`, true},
		{"real listing", `<table><tr><td class="torTopic">…</td></tr></table>`, false},
		{"login form", `<form action="/forum/login.php"><input name="login_username"></form>`, false},
		{"empty body", ``, false},
		// CF injects its JS-detection beacon into *cleared* pages too. Keying
		// off the challenge-platform path alone reported a challenge on every
		// successful fetch, which aborted the run as "cf-challenge".
		{"jsd beacon on a cleared page", `<script>a.src='/cdn-cgi/challenge-platform/scripts/jsd/main.js'</script>`, false},
		// A topic that merely discusses Cloudflare must not trip the check —
		// a false positive here aborts the whole run.
		{"topic mentioning cloudflare", `<a id="tt-1" href="x">Just a moment (2019)</a><td class="torTopic">x</td>`, false},
	}
	for _, tc := range tests {
		if got := looksLikeCFChallenge(tc.body); got != tc.want {
			t.Errorf("%s: looksLikeCFChallenge = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Listing pages are CP1251, but anything fetched through flaresolverr comes
// back already decoded to UTF-8 by the browser. Running the CP1251 mapping
// over UTF-8 mangles every Cyrillic title, so the decoder has to detect which
// one it was handed.
func TestDecodeRutrackerBodyHandlesBothCharsets(t *testing.T) {
	const want = "торрент"

	cp1251 := []byte{0xf2, 0xee, 0xf0, 0xf0, 0xe5, 0xed, 0xf2} // "торрент"
	if got := decodeRutrackerBody(cp1251); got != want {
		t.Errorf("CP1251 body decoded to %q, want %q", got, want)
	}

	if got := decodeRutrackerBody([]byte(want)); got != want {
		t.Errorf("UTF-8 body decoded to %q, want %q — CP1251 mapping applied twice", got, want)
	}
}

// The category table is the single source for five derived values, and the
// invariants below are the ones whose violation is silent rather than loud.
func TestCategoryTableIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for i, c := range rutrackerCategories {
		if c.id == "" {
			t.Errorf("row %d has no id", i)
			continue
		}
		if seen[c.id] {
			t.Errorf("category %s appears twice", c.id)
		}
		seen[c.id] = true

		// parsePage drops a row whose category resolves to no types, so a forum
		// listed without them is parsed and thrown away in silence.
		if len(c.types) == 0 {
			t.Errorf("category %s has no types; every row of that forum would be dropped", c.id)
		}
		for _, ty := range c.types {
			switch ty {
			case "movie", "serial", "multfilm", "multserial", "anime",
				"documovie", "docuserial", "tvshow", "sport":
			default:
				t.Errorf("category %s carries unknown type %q", c.id, ty)
			}
		}
		if c.kind != kindMovie && c.kind != kindSerial && c.kind != kindOther {
			t.Errorf("category %s has an unknown title kind %d", c.id, c.kind)
		}
	}
}

// Each derived value has to stay consistent with the table it comes from —
// these five used to be maintained by hand, which is how they drifted apart.
func TestDerivedCategoryListsMatchTheTable(t *testing.T) {
	if len(allTaskCats) != len(rutrackerCategories) {
		t.Errorf("allTaskCats has %d ids for %d categories", len(allTaskCats), len(rutrackerCategories))
	}
	if len(categoryTypeMap) != len(rutrackerCategories) {
		t.Errorf("categoryTypeMap has %d entries for %d categories", len(categoryTypeMap), len(rutrackerCategories))
	}

	inAll := map[string]bool{}
	for _, id := range allTaskCats {
		inAll[id] = true
	}
	for _, id := range firstPageCats {
		if !inAll[id] {
			t.Errorf("%s is in the hourly pass but not in the full sweep", id)
		}
	}

	for _, c := range rutrackerCategories {
		// parseTitle branches on exactly these three sets; a category in none of
		// them gets no title grammar, in two of them gets the wrong one.
		n := 0
		for _, in := range []bool{movieCats[c.id], serialCats[c.id], otherNamedCats[c.id]} {
			if in {
				n++
			}
		}
		if n != 1 {
			t.Errorf("category %s belongs to %d title-kind sets, want exactly 1", c.id, n)
		}
		if got := categoryTypes(c.id); len(got) == 0 {
			t.Errorf("categoryTypes(%s) is empty", c.id)
		}
	}
}

// categoryTypes hands its slice to a record, so it must not expose the table's
// own backing array — a caller appending to it would rewrite the category.
func TestCategoryTypesReturnsACopy(t *testing.T) {
	const id = "549"
	got := categoryTypes(id)
	if len(got) == 0 {
		t.Fatalf("no types for %s", id)
	}
	got[0] = "mutated"
	if again := categoryTypes(id); again[0] == "mutated" {
		t.Error("categoryTypes exposes the table's slice; a caller can rewrite a category")
	}
}

// Coverage pin. 35 forums were missing relative to the C# original — 17 serial,
// 10 movie, 3 multfilm, 2 anime, 2 documovie, 1 multserial — and the gap was
// invisible because nothing reports a forum that is simply never visited.
// Lowering these numbers should be a deliberate act, not a merge accident.
func TestCategoryCoverageDoesNotShrink(t *testing.T) {
	const (
		wantAll   = 246
		wantQuick = 98
	)
	if len(allTaskCats) < wantAll {
		t.Errorf("full sweep covers %d forums, was %d — coverage regressed", len(allTaskCats), wantAll)
	}
	if len(firstPageCats) < wantQuick {
		t.Errorf("hourly pass covers %d forums, was %d — coverage regressed", len(firstPageCats), wantQuick)
	}

	// A sample of the forums that were missing, one per type they brought in.
	for _, id := range []string{"7", "33", "84", "498", "1202", "1463"} {
		if len(categoryTypes(id)) == 0 {
			t.Errorf("forum %s is not covered again", id)
		}
	}
}
