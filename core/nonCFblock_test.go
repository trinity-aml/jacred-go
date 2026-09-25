package core

import (
	"net/http"
	"testing"
	"time"
)

func resetNonCFBlock() {
	nonCFBlockMu.Lock()
	nonCFBlockSeen = map[string]time.Time{}
	nonCFBlockMu.Unlock()
}

// Only a refusal is worth the line. CF auto-detect already handles a body that
// is an interstitial; this covers the case that takes no branch at all.
func TestNonCFBlockReportsOnlyRefusals(t *testing.T) {
	resetNonCFBlock()
	now := time.Now()
	for _, status := range []int{200, 204, 301, 404, 429, 500, 502} {
		if shouldReportNonCFBlock("example.org", status, now) {
			t.Errorf("status %d was reported as a block", status)
		}
	}
	for _, status := range []int{http.StatusForbidden, http.StatusServiceUnavailable} {
		resetNonCFBlock()
		if !shouldReportNonCFBlock("example.org", status, now) {
			t.Errorf("status %d was not reported", status)
		}
	}
}

// A parser retries a refused host many times in a run; the diagnosis is the same
// every time, so it is worth one line — but a block that outlives the window
// should show up again rather than go quiet forever.
func TestNonCFBlockIsRateLimitedPerHost(t *testing.T) {
	resetNonCFBlock()
	start := time.Now()

	if !shouldReportNonCFBlock("a.example", 403, start) {
		t.Fatal("first refusal not reported")
	}
	for i := 1; i < 50; i++ {
		if shouldReportNonCFBlock("a.example", 403, start.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("refusal %d reported again inside the window", i)
		}
	}
	// A different host is tracked separately — one noisy site must not silence
	// the diagnosis for another.
	if !shouldReportNonCFBlock("b.example", 503, start) {
		t.Error("a second host was silenced by the first")
	}
	if !shouldReportNonCFBlock("a.example", 403, start.Add(nonCFBlockWindow+time.Second)) {
		t.Error("the window never re-arms; a lasting block would go unreported")
	}
}

func TestNonCFBlockIgnoresAnEmptyHost(t *testing.T) {
	resetNonCFBlock()
	for _, d := range []string{"", "   "} {
		if shouldReportNonCFBlock(d, 403, time.Now()) {
			t.Errorf("reported a block for host %q", d)
		}
	}
}
