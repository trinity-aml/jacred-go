package rutracker

import (
	"os"
	"strings"
	"testing"
)

// A browser-form login that reached rutracker and came back without a session
// must not be reported, or backed off, as a Cloudflare challenge.
//
// Production 2026-11-15 is the whole reason this exists. The trace was:
//
//	flaresolverr: submitting a form to rutracker.org in the browser
//	flaresolverr: form submitted to rutracker.org, cookies=0 []
//	rutracker: login BLOCKED by cloudflare challenge (credentials were never checked)
//
// Nothing between the second and third line: loginViaBrowser returned a bare
// false and the caller named the failure after the only branch it had. The day
// totalled 167 × "no cf_clearance", 40 × 403 and 39 blocks against **one**
// successful login, and the CF ladder's fast first steps (30 s, 2 min) are
// what turned one refusal into that volume — which is how rutracker's own
// anti-bruteforce gets armed and kept armed.
//
// This reads the source rather than driving loginViaBrowser, deliberately:
// calling it launches a real browser, the trap `TestOnlyPageFetchesAreHeldOff`
// already records paying for once.
func TestABrowserFormRefusalIsNotCalledACloudflareChallenge(t *testing.T) {
	body := readRutrackerSource(t)

	fn := funcBody(body, "func (p *Parser) loginViaBrowser(")
	if fn == "" {
		t.Fatal("loginViaBrowser not found")
	}
	if strings.Contains(fn, "(string, bool)") {
		t.Error("loginViaBrowser went back to a bare bool, so its three distinct failures " +
			"share one answer again and the caller can only guess which happened")
	}
	for _, want := range []string{"browserLoginOK", "browserLoginNoSession", "browserLoginChallenged", "browserLoginCaptcha"} {
		if !strings.Contains(fn, want) {
			t.Errorf("loginViaBrowser no longer reports %s", want)
		}
	}
	// The no-session path must say so. A silent return is what made the
	// production trace unreadable.
	if !strings.Contains(fn, "set no session") {
		t.Error("the no-session path stopped logging, so a browser form that reached rutracker " +
			"and was refused is invisible again")
	}

	// And the caller must not arm the CF ladder on it.
	caller := funcBody(body, "func (p *Parser) takeLogin(")
	if caller == "" {
		t.Fatal("takeLogin not found")
	}
	arm := strings.Index(caller, "noteCFLoginFailure()")
	noSession := strings.Index(caller, "case browserLoginNoSession:")
	if noSession < 0 {
		t.Fatal("takeLogin no longer handles browserLoginNoSession separately — it is back to " +
			"treating every browser-form failure as a cloudflare challenge")
	}
	if arm < 0 {
		t.Fatal("takeLogin no longer arms the CF backoff at all; the challenged case still needs it")
	}
	arm1 := caller[noSession:]
	if end := strings.Index(arm1, "\n\t\t}"); end > 0 {
		arm1 = arm1[:end]
	}
	if strings.Contains(arm1, "noteCFLoginFailure") {
		t.Error("the no-session arm arms the cloudflare ladder, which is the exact bug: " +
			"cloudflare did not refuse that request, the browser reached rutracker")
	}
	if !strings.Contains(arm1, "noteLoginFailure(loginCooldown") {
		t.Error("the no-session arm does not record a block of its own, so a refusal would be " +
			"retried immediately and forever")
	}
}

// The "paste a cf_clearance" advice was wrong on this tracker and printed 167
// times in a day. Measured against the live site 2026-11-15:
// /forum/login.php answers "Challenge not detected!" and sets no cookies at
// all, while /forum/index.php sets bb_guid — so an empty cookie set on the
// login page is rutracker having none to give, not a challenge we lost.
func TestAnEmptyLoginCookieSetIsNotCalledAFailedChallenge(t *testing.T) {
	body := readRutrackerSource(t)
	fn := funcBody(body, "func (p *Parser) takeLogin(")
	if fn == "" {
		t.Fatal("takeLogin not found")
	}
	if strings.Contains(fn, "the CF challenge on %s was not solved") {
		t.Error("takeLogin again asserts the challenge was not solved when the login page " +
			"simply issues no cookies")
	}
	if strings.Contains(fn, "paste a browser cf_clearance") && strings.Contains(fn, "was not solved") {
		t.Error("takeLogin again tells the operator to paste a clearance for a page that is not challenged")
	}
}

func readRutrackerSource(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("rutracker.go")
	if err != nil {
		t.Fatalf("read rutracker.go: %v", err)
	}
	return string(src)
}

// funcBody returns the body of the function starting at marker, up to the next
// top-level func.
func funcBody(src, marker string) string {
	start := strings.Index(src, marker)
	if start < 0 {
		return ""
	}
	rest := src[start+len(marker):]
	if end := strings.Index(rest, "\nfunc "); end >= 0 {
		return rest[:end]
	}
	return rest
}
