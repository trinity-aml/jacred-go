package core

import (
	"os"
	"strings"
	"testing"
)

// A login must not be vetoed by the browser cooldown, and a page fetch still
// must be.
//
// Measured on the production box 2026-11-13: two WebDriver stalls
// (`The browser stopped responding after 44.1 seconds`, then 52.0) opened the
// 3-minute cooldown, and seventeen minutes later a challenged login POST could
// not fall back to the browser — `browser login failed: rutracker.org in
// cooldown for 59s` — so /trackers reported `work_login — cloudflare challenge
// on the login form` for the next three hours. The credentials were fine and so
// was the login form; a browser stall was the whole cause, and the message
// named the wrong thing entirely.
//
// The cooldown exists to stop a parser burning hundreds of page renders on a
// browser that just failed. A login is one request and the gate for a whole
// tracker, so refusing it saves the browser nothing and loses the run.
//
// This reads the source rather than calling PostViaBrowser, deliberately. With
// the check gone there is no decision left to drive, and calling it in a unit
// test launches a real browser — the first version of this test did exactly
// that and sat for 19 seconds before skipping.
func TestOnlyPageFetchesAreHeldOffByTheBrowserCooldown(t *testing.T) {
	src, err := os.ReadFile("fetcher.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	// All three login helpers, not just the one the production failure came
	// through: PostViaBrowser is rutracker's and kinozal's, SolveForLogin is
	// nnmclub's, and LoginViaFlare has no caller yet but is the same kind of
	// thing. Enumerating them here means the roster is checked rather than
	// described in a comment nobody re-reads.
	for _, fn := range []string{
		"func (f *Fetcher) PostViaBrowser(",
		"func (f *Fetcher) SolveForLogin(",
		"func (f *Fetcher) LoginViaFlare(",
	} {
		src := funcSource(body, fn)
		if src == "" {
			t.Errorf("%s not found in fetcher.go", fn)
			continue
		}
		if strings.Contains(src, "flareCooldownRemaining(") {
			t.Errorf("%s consults the browser cooldown again: a login is one request "+
				"and the gate for a whole tracker, so a cooldown opened by an unrelated "+
				"page stall must not turn it away", fn)
		}
		// A failure on these paths still has to mark the domain, or the other
		// half of the cooldown — keeping page fetches off a dead browser —
		// stops working.
		if !strings.Contains(src, "markFlareFailure(") {
			t.Errorf("%s no longer marks a failure, so a dead browser would keep "+
				"being handed page fetches", fn)
		}
	}

	// And the page path must keep the check. solveFlare is where it lives —
	// fetchViaFlare delegates to it — which the first version of this test got
	// wrong and failed on correct code for it.
	page := funcSource(body, "func (f *Fetcher) solveFlare(")
	if page == "" {
		t.Fatal("solveFlare not found in fetcher.go")
	}
	if !strings.Contains(page, "flareCooldownRemaining(") {
		t.Error("the page path stopped checking the cooldown, so one stalled browser " +
			"would be handed a whole sweep")
	}
}

// funcSource returns the body of the function starting at marker, up to the
// next top-level func.
func funcSource(src, marker string) string {
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

// A challenge on a domain already known to refuse the replay must not be read
// as a stale cookie. The two cases are distinguished only by their order in
// one switch — Go evaluates the cases top to bottom — so what this guards is
// that order, which no type or signature can express.
//
// Getting it wrong is expensive and quiet: the bare `case challenged` calls
// clearFlareSession, so the next browser trip is a cold solve (8 s and a
// browser launch) in place of a render (2 s), and a cold solve is the shape
// that stalls the browser and opens the 3-minute cooldown that loses the rest
// of the sweep. Verified by swapping the two cases.
func TestAProbeChallengeDoesNotDropTheSession(t *testing.T) {
	src, err := os.ReadFile("fetcher.go")
	if err != nil {
		t.Fatalf("read fetcher.go: %v", err)
	}
	body := string(src)
	hostile := strings.Index(body, "case challenged && replayHostile(domain):")
	if hostile < 0 {
		t.Fatal("fetchViaFlare no longer has a case for a challenge on a replay-hostile domain — a condemned domain's probe will drop its session and pay a cold solve")
	}
	bare := strings.Index(body, "\n\t\t\tcase challenged:")
	if bare < 0 {
		t.Fatal(`cannot find the bare "case challenged:" arm — adjust this guard rather than deleting it`)
	}
	if hostile > bare {
		t.Fatalf("the replay-hostile case is at %d, after the bare challenged case at %d — Go takes the first match, so a condemned domain's probe falls into the stale-cookie branch and re-solves", hostile, bare)
	}
	// And the hostile arm must not be the one that clears the session.
	arm := body[hostile:bare]
	if strings.Contains(arm, "clearFlareSession") {
		t.Error("the replay-hostile arm clears the session — that is the behaviour it exists to avoid")
	}
	if !strings.Contains(arm, "sessionWasValid = true") {
		t.Error("the replay-hostile arm does not keep the session valid, so the next browser trip is a cold solve rather than a render")
	}
}
