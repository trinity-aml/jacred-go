package core

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestARecycleIsRateLimited pins the floor that keeps a bad hour from thrashing
// the service. Closing it tears down every browser, including one another
// domain is mid-render on, and a failed cold solve is an everyday event.
func TestARecycleIsRateLimited(t *testing.T) {
	flareSvcMu.Lock()
	savedSvc, savedAt := flareSvc, flareLastRecycle
	flareSvc, flareLastRecycle = nil, time.Now()
	flareSvcMu.Unlock()
	t.Cleanup(func() {
		flareSvcMu.Lock()
		flareSvc, flareLastRecycle = savedSvc, savedAt
		flareSvcMu.Unlock()
	})

	if recycleFlareService("test") {
		t.Error("a recycle fired inside the rate-limit window")
	}

	flareSvcMu.Lock()
	flareLastRecycle = time.Now().Add(-2 * flareRecycleInterval)
	flareSvcMu.Unlock()
	// Still false here because there is no service to replace, but the
	// rate-limit must no longer be what stops it: the window has to have been
	// consulted and passed.
	recycleFlareService("test")
	flareSvcMu.Lock()
	fired := time.Since(flareLastRecycle) < time.Minute
	flareSvcMu.Unlock()
	if !fired {
		t.Error("past the window the recycle did not even record an attempt")
	}
}

// TestOnlyAColdSolveFailureRecyclesTheService keeps the trigger narrow. A failed
// render means the clearance is already held and the page simply did not come
// back — a new browser cannot fix that, and recycling on it would churn the
// service for nothing. Measured: a Service is spent after one *cold* solve.
func TestOnlyAColdSolveFailureRecyclesTheService(t *testing.T) {
	flareSvcMu.Lock()
	savedAt := flareLastRecycle
	flareLastRecycle = time.Now().Add(-2 * flareRecycleInterval)
	flareSvcMu.Unlock()
	t.Cleanup(func() {
		flareSvcMu.Lock()
		flareLastRecycle = savedAt
		flareSvcMu.Unlock()
	})

	before := func() time.Time {
		flareSvcMu.Lock()
		defer flareSvcMu.Unlock()
		return flareLastRecycle
	}()

	// haveClearance=true is a render, and must not touch the service.
	noteColdSolveFailure("example.org", true)

	if after := func() time.Time {
		flareSvcMu.Lock()
		defer flareSvcMu.Unlock()
		return flareLastRecycle
	}(); !after.Equal(before) {
		t.Error("a failed render recycled the browser service; only a cold solve may")
	}
}

// TestTheRecycleKeepsTheCookieCache pins the other half of the trade: a
// clearance already minted is still replayable, so dropping it would turn a
// recycle into a cold start for every domain at once.
func TestTheRecycleKeepsTheCookieCache(t *testing.T) {
	src := readFetcherSource(t)
	body := funcBodyWithoutComments(t, src, "func recycleFlareService(")
	// Only ClearCFAuto is reachable from this scope, so it is the one this
	// guard was verified against; flareCache and clearFlareSession are a
	// Fetcher field and a Fetcher method and would not compile here anyway.
	for _, forbidden := range []string{"ClearCFAuto", "flareCache", "clearFlareSession"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("recycleFlareService touches %s — a recycle must replace the browser, not the clearances", forbidden)
		}
	}
	if !strings.Contains(body, "flareLastUsed") {
		t.Error("recycleFlareService does not clear the per-session bookkeeping, so the idle reaper will chase dead session ids")
	}
}

func readFetcherSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("fetcher.go")
	if err != nil {
		t.Fatalf("read fetcher.go: %v", err)
	}
	return string(raw)
}

func funcBodyWithoutComments(t *testing.T, src, signature string) string {
	t.Helper()
	at := strings.Index(src, signature)
	if at < 0 {
		t.Fatalf("%s not found", signature)
	}
	rest := src[at:]
	if end := strings.Index(rest, "\n}\n"); end >= 0 {
		rest = rest[:end]
	}
	var kept []string
	for _, line := range strings.Split(rest, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// TestAColdSolveFailureReachesTheRecycle closes the one link the live run could
// not exercise: the end-to-end bitru parse made a single cold solve and it
// succeeded, so nothing ever failed and the recycle was never called. The
// remedy itself is measured (a new Service restores cold solving) and the
// trigger's guards are above; this asserts the wiring between them.
func TestAColdSolveFailureReachesTheRecycle(t *testing.T) {
	flareSvcMu.Lock()
	savedAt := flareLastRecycle
	flareLastRecycle = time.Now().Add(-2 * flareRecycleInterval)
	flareSvcMu.Unlock()
	t.Cleanup(func() {
		flareSvcMu.Lock()
		flareLastRecycle = savedAt
		flareSvcMu.Unlock()
	})

	noteColdSolveFailure("example.org", false)

	flareSvcMu.Lock()
	age := time.Since(flareLastRecycle)
	flareSvcMu.Unlock()
	if age > time.Minute {
		t.Error("a failed cold solve never reached recycleFlareService — the rate-limit stamp was not touched")
	}
}
