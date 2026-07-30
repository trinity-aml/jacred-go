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
