package core

import (
	"errors"
	"fmt"
	"testing"
)

// TestACooldownRefusalIsRecognisableByCallers pins the contract the parsers
// depend on: the message stays human-readable and the sentinel survives the
// wrapping, because a parser has to tell "the tracker refused this page" from
// "we refused it ourselves and never left the process".
func TestACooldownRefusalIsRecognisableByCallers(t *testing.T) {
	fromFetcher := fmt.Errorf("flaresolverr: %s in cooldown for %s after recent failure: %w",
		"bitru.org", "2m39s", ErrFlareCooldown)
	fromParser := fmt.Errorf("rutracker: flaresolverr cooldown %s: %w", "3m0s", ErrFlareCooldown)

	for _, err := range []error{fromFetcher, fromParser} {
		if !errors.Is(err, ErrFlareCooldown) {
			t.Errorf("a cooldown refusal is not recognisable through %q", err)
		}
	}
	if errors.Is(fmt.Errorf("rutracker: status 403"), ErrFlareCooldown) {
		t.Error("an ordinary fetch failure is being read as a cooldown refusal")
	}
}

// TestThreeRefusalsWouldSettleAPageThatWasNeverFetched is why the guard in the
// parsers exists, stated as behaviour rather than prose. The budget is there
// so a permanently broken page cannot hold a cycle open — but a page refused
// without a network request is not broken, and settling it means the sweep
// rotates having skipped it. Production lost 3 169 rutracker pages this way in
// one night, every one refused in ~0 ms.
func TestThreeRefusalsWouldSettleAPageThatWasNeverFetched(t *testing.T) {
	cycle := &ParseAllCycle{CycleID: "test-cycle"}
	slot := &CycleFields{}

	for attempt := 1; attempt < ParseAllFailBudget; attempt++ {
		if NoteParseAllAttempt("rutracker", slot, cycle, false) {
			t.Fatalf("page settled after %d failures, before the budget was spent", attempt)
		}
	}
	if !NoteParseAllAttempt("rutracker", slot, cycle, false) {
		t.Fatalf("page did not settle after %d failures — the budget is not %d", ParseAllFailBudget, ParseAllFailBudget)
	}
	if slot.CycleID() != cycle.CycleID {
		t.Error("a settled page does not carry the cycle id, so this test is not measuring what it claims")
	}
}
