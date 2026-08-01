package core

import (
	"testing"
	"time"
)

// A cold solve has to sit through slow anti-bot JS; a render, where the
// browser profile already holds clearance, has nothing to wait for. Charging
// renders the cold-solve wait is what made every rutracker page cost 11–17 s.
func TestFlareWaitSeconds(t *testing.T) {
	if got := flareWaitSeconds(false); got != flareSolveWait {
		t.Errorf("cold solve wait = %d, want %d", got, flareSolveWait)
	}
	if got := flareWaitSeconds(true); got != flareRenderWait {
		t.Errorf("render wait = %d, want %d", got, flareRenderWait)
	}
	if flareRenderWait >= flareSolveWait {
		t.Errorf("render wait (%d) must be shorter than solve wait (%d)", flareRenderWait, flareSolveWait)
	}
	if flareRenderWait <= 0 {
		t.Errorf("render wait must stay positive, got %d", flareRenderWait)
	}
}

func TestFlareCooldown(t *testing.T) {
	const domain = "cooldown.example"
	const url = "https://cooldown.example/forum/viewtopic.php?t=1"
	clearFlareFailure(domain)
	t.Cleanup(func() { clearFlareFailure(domain) })

	if _, blocked := FlareCooldown(url); blocked {
		t.Fatal("clean domain reported as in cooldown")
	}

	markFlareFailure(domain)
	remaining, blocked := FlareCooldown(url)
	if !blocked {
		t.Fatal("domain not in cooldown after a failure")
	}
	if remaining <= 0 || remaining > flareFailCooldown {
		t.Errorf("remaining = %s, want (0, %s]", remaining, flareFailCooldown)
	}

	// The point of exporting this: a parser retry ladder is far shorter than
	// the cooldown, so every attempt after the first failure is guaranteed to
	// fail locally. Retrying inside the window can never help.
	if longestRetryLadder := 30 * time.Second; remaining <= longestRetryLadder {
		t.Errorf("cooldown %s is within a retry ladder (%s) — the skip would be pointless",
			remaining, longestRetryLadder)
	}

	clearFlareFailure(domain)
	if _, blocked := FlareCooldown(url); blocked {
		t.Error("cooldown survived clearFlareFailure")
	}
}

// FlareCooldown keys on the domain, so every URL on a failing host is covered.
func TestFlareCooldownIsPerDomain(t *testing.T) {
	const domain = "cooldown2.example"
	clearFlareFailure(domain)
	t.Cleanup(func() { clearFlareFailure(domain) })

	markFlareFailure(domain)
	for _, u := range []string{
		"https://cooldown2.example/",
		"https://cooldown2.example/forum/viewforum.php?f=549",
		"http://COOLDOWN2.EXAMPLE/forum/viewtopic.php?t=2",
	} {
		if _, blocked := FlareCooldown(u); !blocked {
			t.Errorf("%s not covered by the domain cooldown", u)
		}
	}
	if _, blocked := FlareCooldown("https://other.example/x"); blocked {
		t.Error("unrelated domain reported as in cooldown")
	}
}
