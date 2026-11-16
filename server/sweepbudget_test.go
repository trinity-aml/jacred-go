package server

import (
	"os"
	"strings"
	"testing"
)

// TestASelfInflictedRefusalDoesNotSpendTheSweepBudget guards both parsers that
// can be handed `core.ErrFlareCooldown` by the fetcher.
//
// `NoteParseAllAttempt` settles a page once it has failed ParseAllFailBudget
// times, so counting a refusal this process made itself marks pages as visited
// that never left the box — and the cycle then rotates having skipped them.
// The save path in the same loops already draws this line ("a save error is
// our own"); the browser cooldown belongs on the same side.
//
// Measured on production over the night of 2026-11-15/16: rutracker lost 3 295
// of 4 257 page attempts and 3 169 of those (96.2%) were refused in ~0 ms with
// no network at all, against 124 real solve failures. bitru produced zero
// records across the same window on the same path.
func TestASelfInflictedRefusalDoesNotSpendTheSweepBudget(t *testing.T) {
	for _, path := range []string{
		"../cron/rutracker/rutracker.go",
		"../cron/bitru/bitru.go",
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		src := stripGoComments(string(raw))

		settle := strings.Index(src, "p.settle(cycle, cat, task.Page, false)")
		if settle < 0 {
			t.Errorf("%s: the sweep no longer settles a failed page — if that is deliberate, retire this guard", path)
			continue
		}
		// The guard has to sit immediately above the settle call, inside the
		// same error branch; anywhere else and the budget is still spent.
		window := src[max(0, settle-400):settle]
		if !strings.Contains(window, "ErrFlareCooldown") {
			t.Errorf("%s: a failed page is settled without asking whether the refusal was ours — a browser cooldown will spend the sweep's budget on pages that were never fetched", path)
		}
	}
}

// TestTheCooldownRefusalIsWrappedNotRetyped keeps the two parsers from
// reporting the condition in a way `errors.Is` cannot see. A plain
// `fmt.Errorf("... cooldown ...")` reads identically in a log and is invisible
// to the guard above.
func TestTheCooldownRefusalIsWrappedNotRetyped(t *testing.T) {
	for path, marker := range map[string]string{
		"../cron/rutracker/rutracker.go": "flaresolverr cooldown",
		"../cron/bitru/bitru.go":         "flaresolverr unavailable",
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		src := stripGoComments(string(raw))
		at := strings.Index(src, marker)
		if at < 0 {
			t.Errorf("%s: %q is gone — if the message was renamed, update this guard", path, marker)
			continue
		}
		line := src[at:min(len(src), at+220)]
		if !strings.Contains(line, "core.ErrFlareCooldown") {
			t.Errorf("%s: %q is reported without wrapping core.ErrFlareCooldown, so no caller can tell it from a real fetch failure", path, marker)
		}
	}
}

// stripGoComments removes // lines so a guard cannot be satisfied by the prose
// that explains it — this codebase has already had a scan pass against deleted
// code for exactly that reason.
func stripGoComments(src string) string {
	var kept []string
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
