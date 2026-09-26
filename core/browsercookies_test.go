package core

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Without this the browser navigates with its own profile — cf_clearance and
// the site's guest id — so a tracker that needs a login renders as a guest even
// though the parser logged in successfully. Measured on rutracker: login
// returned bb_session, the browser profile held only [cf_clearance bb_guid],
// and every page of the run was fetched anonymously.
func TestBrowserGetsTheCallersSession(t *testing.T) {
	defer forgetBrowserCookies("example.org")

	got := cookiesForBrowser("example.org", "bb_session=abc; bb_ssl=1")
	if len(got) != 2 {
		t.Fatalf("got %d cookies, want 2: %+v", len(got), got)
	}
	names := cookieNameList(got)
	if !strings.Contains(names, "bb_session") || !strings.Contains(names, "bb_ssl") {
		t.Errorf("names = %q", names)
	}
	for _, c := range got {
		if c.Path != "/" {
			t.Errorf("%s: path = %q, want /", c.Name, c.Path)
		}
		// Domain is deliberately empty so the library scopes it to the URL
		// being navigated.
		if c.Domain != "" {
			t.Errorf("%s: domain = %q, want empty", c.Name, c.Domain)
		}
	}
}

// The browser mints its own cf_clearance. Handing it an older one would shadow
// the fresh value — the same precedence bug stripCFManagedCookies exists to
// prevent on the HTTP path, and the one that forced a cold solve per page.
func TestCFCookiesAreNeverHandedToTheBrowser(t *testing.T) {
	defer forgetBrowserCookies("rutracker.org")

	got := cookiesForBrowser("rutracker.org", "cf_clearance=OLD; __cf_bm=OLD; bb_session=abc")
	names := cookieNameList(got)
	for _, forbidden := range []string{"cf_clearance", "__cf_bm"} {
		if strings.Contains(names, forbidden) {
			t.Errorf("%s was handed to the browser: %q", forbidden, names)
		}
	}
	if !strings.Contains(names, "bb_session") {
		t.Errorf("the auth cookie was dropped along with them: %q", names)
	}
}

// Injecting costs an extra navigation — the library sets the cookies then
// reloads — and the browser session is persistent, so re-sending them on every
// page would double the cost of every fetch for nothing.
func TestCookiesAreInjectedOncePerChange(t *testing.T) {
	defer forgetBrowserCookies("example.org")

	if got := cookiesForBrowser("example.org", "bb_session=abc"); len(got) == 0 {
		t.Fatal("the first navigation got no cookies")
	}
	if got := cookiesForBrowser("example.org", "bb_session=abc"); got != nil {
		t.Errorf("the same session was injected twice: %+v", got)
	}
	// A re-login changes the value and must reach the browser.
	if got := cookiesForBrowser("example.org", "bb_session=NEW"); len(got) == 0 {
		t.Error("a changed session was not re-injected — the browser would keep the dead one")
	}
}

// Order must not look like a change, or every navigation re-injects.
func TestCookieOrderIsNotAChange(t *testing.T) {
	defer forgetBrowserCookies("example.org")

	cookiesForBrowser("example.org", "a=1; b=2")
	if got := cookiesForBrowser("example.org", "b=2; a=1"); got != nil {
		t.Errorf("reordering counted as a change: %+v", got)
	}
}

// A cleared flare session means a fresh browser profile, which no longer holds
// anything we handed the old one.
func TestClearingTheSessionForgetsTheInjection(t *testing.T) {
	defer forgetBrowserCookies("example.org")

	cookiesForBrowser("example.org", "bb_session=abc")
	forgetBrowserCookies("example.org")
	if got := cookiesForBrowser("example.org", "bb_session=abc"); len(got) == 0 {
		t.Error("after the session was cleared the cookies were not re-injected")
	}
}

// A caller with no session, or one carrying only CF cookies, must not trigger
// the extra navigation.
func TestNothingToInjectSendsNothing(t *testing.T) {
	for _, cookie := range []string{"", "   ", "cf_clearance=x", "cf_clearance=x; __cf_bm=y", "novalue"} {
		if got := cookiesForBrowser("nothing.example", cookie); got != nil {
			t.Errorf("cookie %q produced %+v", cookie, got)
		}
		forgetBrowserCookies("nothing.example")
	}
}

// Values must never reach a log line.
func TestCookieNameListHasNoValues(t *testing.T) {
	got := cookieNameList(parseBrowserCookies("bb_session=SECRET; bb_ssl=ALSOSECRET"))
	if strings.Contains(got, "SECRET") {
		t.Errorf("a cookie value leaked into the log line: %q", got)
	}
	if !strings.Contains(got, "bb_session") {
		t.Errorf("names missing: %q", got)
	}
}

// Condemning a domain to the browser is expensive in one direction only: it
// costs every page a render for six hours, while one extra doomed replay costs
// a single request. Measured on rutracker 2026-09-26 — a single fresh-clearance
// challenge right after login condemned the domain, the run then rendered every
// page, and a probe in a fresh process fetched the same two category pages over
// plain HTTP in 630ms and 141ms with 50 listing rows each. The replay had been
// working the whole time.
func TestOneStrikeDoesNotCondemnADomain(t *testing.T) {
	const d = "strike.example"
	defer clearReplayStrike(d)
	defer func() {
		flareReplayMu.Lock()
		delete(flareReplayHostile, d)
		flareReplayMu.Unlock()
	}()

	if markReplayHostile(d) {
		t.Fatal("a single fresh-clearance challenge condemned the domain")
	}
	if replayHostile(d) {
		t.Error("the domain is hostile after one strike")
	}
	if !markReplayHostile(d) {
		t.Fatal("a second strike did not condemn the domain")
	}
	if !replayHostile(d) {
		t.Error("the domain is not hostile after two strikes")
	}
}

// A replay that works clears the pending strike, so two unrelated transients
// never add up to a condemnation.
func TestAWorkingReplayClearsTheStrike(t *testing.T) {
	const d = "recover.example"
	defer clearReplayStrike(d)
	defer func() {
		flareReplayMu.Lock()
		delete(flareReplayHostile, d)
		flareReplayMu.Unlock()
	}()

	markReplayHostile(d)
	clearReplayStrike(d)
	if markReplayHostile(d) {
		t.Error("a strike survived a successful replay and condemned on the next failure")
	}
	if replayHostile(d) {
		t.Error("domain condemned despite a successful replay in between")
	}
}

// Two hiccups far apart are not a pattern.
func TestAnOldStrikeDoesNotCount(t *testing.T) {
	const d = "stale.example"
	defer clearReplayStrike(d)
	defer func() {
		flareReplayMu.Lock()
		delete(flareReplayHostile, d)
		flareReplayMu.Unlock()
	}()

	flareReplayMu.Lock()
	flareReplayStrike[d] = time.Now().Add(-2 * flareReplayStrikeWindow)
	flareReplayMu.Unlock()

	if markReplayHostile(d) {
		t.Error("a strike older than the window still condemned the domain")
	}
}

// A condemned domain used to stay on the browser for the whole six-hour TTL.
// Measured on rutracker 2026-09-26: the replay is intermittent — rejected
// immediately after a solve and again 79s later, while a saved session fetched
// the same pages over plain HTTP at ~140ms minutes earlier. One probe every
// few minutes is one wasted request against hundreds of saved renders.
func TestCondemnedDomainIsReprobed(t *testing.T) {
	const d = "probe.example"
	defer clearReplayHostile(d)

	markReplayHostile(d)
	markReplayHostile(d)
	if !replayHostile(d) {
		t.Fatal("setup: domain not condemned")
	}

	// The first call after condemnation is the probe itself.
	if skipReplay(d) {
		t.Error("the first request after condemnation did not probe")
	}
	// Immediately after, the replay is skipped again.
	if !skipReplay(d) {
		t.Error("every request probed — the condemnation does nothing")
	}
	// Once the interval elapses, one more probe is allowed.
	flareReplayMu.Lock()
	flareReplayProbe[d] = time.Now().Add(-flareReplayRetryInterval - time.Second)
	flareReplayMu.Unlock()
	if skipReplay(d) {
		t.Error("no probe after the retry interval elapsed")
	}
}

// A probe that succeeds puts the domain back on the fast path.
func TestASuccessfulProbeLiftsTheSentence(t *testing.T) {
	const d = "lift.example"
	defer clearReplayHostile(d)

	markReplayHostile(d)
	markReplayHostile(d)
	if !replayHostile(d) {
		t.Fatal("setup: not condemned")
	}
	clearReplayHostile(d)
	if replayHostile(d) {
		t.Error("the sentence survived a successful probe")
	}
	if skipReplay(d) {
		t.Error("replay still skipped after the sentence was lifted")
	}
}

// A domain that was never condemned always uses the replay.
func TestHealthyDomainAlwaysReplays(t *testing.T) {
	for i := 0; i < 3; i++ {
		if skipReplay("healthy.example") {
			t.Fatal("a healthy domain skipped the replay")
		}
	}
}

// The caller-clearance retry only makes sense when the caller actually brought
// one; otherwise the second attempt would send the identical cookie set.
func TestCallerClearanceDetection(t *testing.T) {
	for _, c := range []struct {
		cookie string
		want   bool
	}{
		{"cf_clearance=abc; bb_session=x", true},
		{"bb_session=x; cf_clearance=abc", true},
		{"bb_session=x", false},
		{"", false},
		{"cf_clearance", false}, // a name with no value is not a cookie
	} {
		if got := callerHasClearance(c.cookie); got != c.want {
			t.Errorf("callerHasClearance(%q) = %v, want %v", c.cookie, got, c.want)
		}
	}
}

// The session's clearance must still go first. That ordering is what keeps the
// original bug fixed: an hours-old clearance persisted into a saved auth cookie
// must not shadow one a solve just minted — which once forced a cold solve per
// page and buried the browser.
func TestSessionClearanceIsStillTriedFirst(t *testing.T) {
	merged := mergeCookies("cf_clearance=FROM_SESSION; bb_guid=g", stripCFManagedCookies("cf_clearance=FROM_CALLER; bb_session=s"))
	if !strings.Contains(merged, "FROM_SESSION") {
		t.Errorf("the session's clearance did not win the first attempt: %s", merged)
	}
	if strings.Contains(merged, "FROM_CALLER") {
		t.Errorf("the caller's clearance leaked into the first attempt: %s", merged)
	}
	if !strings.Contains(merged, "bb_session=s") {
		t.Errorf("the caller's auth cookie was dropped: %s", merged)
	}

}

// The retry must send the caller's cookie *on its own*. Merging the session
// back in reproduces the request that was just challenged — which is exactly
// what the first version of this retry did, and why it changed nothing live.
// The configuration that was measured to work was the caller's cookie alone
// with the impersonation profile's own UA.
func TestRetryUsesTheCallerCookieAlone(t *testing.T) {
	src, err := os.ReadFile("fetcher.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	code := string(src)
	const want = `retry, rerr := f.doHTTP(http.MethodGet, rawURL, cookie, defaultUserAgent, "", nil, extraHeaders, profile)`
	if !strings.Contains(code, want) {
		t.Error("the caller-clearance retry no longer sends the caller's cookie alone with defaultUserAgent")
	}
	if strings.Contains(code, "retry, rerr := f.doHTTP(http.MethodGet, rawURL, mergeCookies(") {
		t.Error("the retry merges the session's cookies back in, reproducing the challenged request")
	}
}
