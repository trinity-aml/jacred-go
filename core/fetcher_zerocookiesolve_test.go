package core

import (
	"os"
	"strings"
	"testing"
	"time"
)

// A solve that issues no cookies must not be reported as a browser failure.
//
// Cloudflare issues no cookies at all when it decides a visitor needs no
// clearance — the documented `clearance=false` state, where the page rendered
// fine and the browser is simply the only way to fetch the domain. Calling
// markFlareFailure on that opens the 3-minute flareFailCooldown, after which
// every page fetch for the domain answers 503 in ~0ms and re-arms the window,
// so the tracker reports a Cloudflare problem it does not have.
//
// Measured on the production box 2026-11-14, nine seconds after the solve
// began: `solved kinozal.guru cookies=0 [] clearance=false`, immediately
// followed by `solve failed … solved but returned 0 cookies`, a 503 on the
// authorization probe — and, on the very same second, `login OK — got uid+pass
// cookies`. The login worked while the run reported the tracker unreachable.
//
// This reads the source for the same reason TestOnlyPageFetchesAreHeldOffByThe
// BrowserCooldown does: driving solveFlare for real launches a browser.
func TestAZeroCookieSolveDoesNotCondemnTheBrowser(t *testing.T) {
	src, err := os.ReadFile("fetcher.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	const marker = `if cookieStr == "" {`
	at := strings.Index(body, marker)
	if at < 0 {
		t.Fatalf("the zero-cookie branch is gone from solveFlare — if it moved, move this test with it")
	}
	// The branch body, up to the closing brace of its else.
	rest := body[at:]
	end := strings.Index(rest, "\n\t// When the browser actually navigated")
	if end < 0 {
		end = min(len(rest), 1200)
	}
	branch := rest[:end]

	if strings.Contains(branch, "markFlareFailure") {
		t.Error("a solve that issued no cookies opens the browser cooldown again — " +
			"the browser did its job, and the cooldown makes every page fetch for the domain answer 503")
	}
	if strings.Contains(branch, "return nil, nil,") {
		t.Error("a solve that issued no cookies returns an error again — " +
			"the caller then never sees the rendered body, which is the only way to fetch a browser-only domain")
	}
	if !strings.Contains(branch, "f.flareCache[domain] = sess") {
		t.Error("the non-empty case no longer caches the session")
	}
	// The session must be cached only in the else branch: a cookie-less
	// session on disk is what the old code was right to avoid.
	beforeBranch := body[:at]
	if strings.Count(beforeBranch, "f.flareCache[domain] = sess") != 0 {
		t.Error("the session is cached before the empty-cookie check, so a cookie-less session reaches the cache")
	}
}

// The guarantee the old branch was really protecting: a cached session with no
// cookies must read as absent, so no caller is ever wedged into sending an
// empty Cookie header. That is what makes it safe for solveFlare to stop
// treating the condition as a failure.
func TestACookielessSessionReadsAsNoSession(t *testing.T) {
	f := &Fetcher{flareCache: map[string]*flareSession{}}

	f.flareCache["example.org"] = &flareSession{
		cookies:   "   ",
		userAgent: "UA",
		obtained:  time.Now(),
	}
	if got := f.getFlareSession("example.org"); got != nil {
		t.Errorf("a session with blank cookies was handed out: %+v", got)
	}

	f.flareCache["example.net"] = &flareSession{
		cookies:   "cf_clearance=x",
		userAgent: "UA",
		obtained:  time.Now(),
	}
	if got := f.getFlareSession("example.net"); got == nil {
		t.Error("a session with real cookies was withheld")
	}
}
