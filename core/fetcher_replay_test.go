package core

import (
	"testing"
	"time"
)

func resetReplayHostile(t *testing.T, domain string) {
	t.Helper()
	clear := func() {
		flareReplayMu.Lock()
		delete(flareReplayHostile, domain)
		flareReplayMu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

func TestReplayHostileRegistry(t *testing.T) {
	const domain = "replay.example"
	resetReplayHostile(t, domain)

	if replayHostile(domain) {
		t.Fatal("clean domain reported as replay-hostile")
	}
	markReplayHostile(domain)
	if !replayHostile(domain) {
		t.Fatal("domain not flagged after markReplayHostile")
	}
	if replayHostile("other.example") {
		t.Error("flag leaked to an unrelated domain")
	}

	// Ages out, so a domain (or an IP reputation) that stops being hostile is
	// retried rather than pinned to the browser forever.
	flareReplayMu.Lock()
	flareReplayHostile[domain] = time.Now().Add(-flareReplayHostileTTL - time.Minute)
	flareReplayMu.Unlock()
	if replayHostile(domain) {
		t.Error("expired flag still reported")
	}
}

// The distinction the whole fix rests on: a clearance challenged seconds after
// being issued was never valid for our client, while one challenged after an
// hour has genuinely expired. Only the second warrants dropping the session.
func TestReplayFreshWindowSeparatesUnusableFromExpired(t *testing.T) {
	if flareReplayFreshWindow >= flareSessionTTL {
		t.Fatalf("fresh window (%s) must be far below the session TTL (%s), or an expired cookie would be misread as unusable",
			flareReplayFreshWindow, flareSessionTTL)
	}
	// Production measured ~1s between solve and the challenged replay.
	if flareReplayFreshWindow <= time.Second {
		t.Errorf("fresh window (%s) is too tight to catch the observed ~1s case", flareReplayFreshWindow)
	}
	// cf_clearance lives 30–120 min; a challenge that late is a real expiry.
	for _, age := range []time.Duration{30 * time.Minute, time.Hour} {
		if age < flareReplayFreshWindow {
			t.Errorf("age %s would be misclassified as unusable-replay", age)
		}
	}
}
