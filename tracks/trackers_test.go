package tracks

import (
	"net/url"
	"os"
	"strings"
	"testing"
)

// The production crash, reproduced as a unit test.
//
// `panic: http: != http://` killed jacred 12 times in 14 days. The stack ran
// anacrolix/torrent's initTrackerClient → panicif.NotEq(u.String(), urlStr),
// reached from our own Analyze via AddTorrentSpec. The offending announce was
// the literal string "http://": url.Parse accepts it and String() returns
// "http:", because an empty Host drops the "//".
func TestTheAnnounceThatCrashedProductionIsDropped(t *testing.T) {
	if trackerIsUsable("http://") {
		t.Fatal(`"http://" is still accepted — this is the exact value that panicked the process 12 times`)
	}
	// Prove the mechanism rather than trusting the claim: this is the library's
	// own assertion, run here.
	u, err := url.Parse("http://")
	if err != nil {
		t.Fatalf("url.Parse unexpectedly failed: %v", err)
	}
	if u.String() == "http://" {
		t.Fatal("url.Parse now round-trips \"http://\" — the premise of this guard changed, re-check the library assertion")
	}
	if u.String() != "http:" {
		t.Errorf("round-trip gave %q, the crash message said \"http:\"", u.String())
	}
}

func TestTrackerIsUsable(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
		why  string
	}{
		// Real announces must survive, or the probe loses its peers.
		{"http://tr.example.org:80/announce", true, "ordinary http announce"},
		{"udp://tr.example.org:6969", true, "udp announce, no path"},
		{"https://a.b/ann?x=1", true, "query string is fine"},
		{"wss://t.example/ws", true, "websocket announce"},
		{"http://[::1]:80/a", true, "ipv6 host"},
		// Fails the round-trip: the library's assertion.
		{"http://", false, "the production crash"},
		{"udp://", false, "same shape, udp"},
		{"not a url", false, "space is escaped on round-trip"},
		// Round-trips cleanly but names no tracker, so a round-trip-only
		// check would wrongly keep it.
		{"http:", false, "scheme with no host"},
		{"", false, "empty"},
		{"   ", false, "whitespace only"},
		// Non-empty host, so the host check passes — only the round-trip
		// catches these, and each would trip the library's assertion.
		// Measured: String() escapes them and the result differs from the input.
		{"http://tracker.example/annoünce", false, "non-ASCII in the path is escaped on round-trip"},
		{"http://tracker.example/a b", false, "unescaped space in the path"},
		{"http://tracker.example/a|b", false, "unescaped pipe in the path"},
		// These DO round-trip and must survive, so the check is not just
		// rejecting anything unusual.
		{"http://TRACKER.Example/a", true, "uppercase host round-trips unchanged"},
		{"http://tracker.example/announce#frag", true, "a fragment round-trips"},
	}
	for _, tc := range tests {
		if got := trackerIsUsable(tc.raw); got != tc.want {
			t.Errorf("trackerIsUsable(%q) = %v, want %v (%s)", tc.raw, got, tc.want, tc.why)
		}
	}
}

// Tier structure has to survive, and a tier left empty must not be carried.
func TestSanitizeTrackersKeepsTiersAndReportsDrops(t *testing.T) {
	kept, dropped := sanitizeTrackers([][]string{
		{"http://good.example/announce", "http://"},
		{"udp://"},
		{"udp://also.good:6969"},
	})
	if len(kept) != 2 {
		t.Fatalf("kept %d tiers, want 2 (the all-bad tier must be gone): %v", len(kept), kept)
	}
	if len(kept[0]) != 1 || kept[0][0] != "http://good.example/announce" {
		t.Errorf("first tier = %v, want just the good announce", kept[0])
	}
	if len(kept[1]) != 1 || kept[1][0] != "udp://also.good:6969" {
		t.Errorf("second tier = %v", kept[1])
	}
	if len(dropped) != 2 {
		t.Errorf("reported %d drops, want 2: %v", len(dropped), dropped)
	}
	// A clean list must be left exactly alone and report nothing.
	clean := [][]string{{"http://a.b/ann"}, {"udp://c.d:1"}}
	k, d := sanitizeTrackers(clean)
	if len(d) != 0 {
		t.Errorf("a clean list reported drops: %v", d)
	}
	if len(k) != 2 || k[0][0] != "http://a.b/ann" || k[1][0] != "udp://c.d:1" {
		t.Errorf("a clean list was altered: %v", k)
	}
}

// An announce path can carry the account's passkey and Data/log is retained,
// so the log line must name scheme and host only — the same rule the cookie
// logging follows.
func TestADroppedTrackerIsLoggedWithoutItsPath(t *testing.T) {
	_, dropped := sanitizeTrackers([][]string{{"http://tracker.example/announce/SECRETPASSKEY123"}})
	if len(dropped) != 0 {
		t.Fatal("that announce is valid and must not be dropped at all")
	}
	// Force the redaction path with a value that IS dropped but still carries
	// a path, which is the case worth guarding.
	got := redactTracker("http://tracker.example/announce/SECRETPASSKEY123?pk=SECRET2")
	if strings.Contains(got, "SECRETPASSKEY123") || strings.Contains(got, "SECRET2") {
		t.Errorf("redactTracker leaked the announce path or query: %q", got)
	}
	if got != "http://tracker.example" {
		t.Errorf("redactTracker = %q, want scheme://host only", got)
	}
	if v := redactTracker("http://"); strings.Contains(v, "//") && !strings.Contains(v, "no host") {
		t.Errorf("redactTracker(%q) = %q, should say it has no host", "http://", v)
	}
}

// The second production panic, as a test.
//
// `panic: piece 0 has relative availability 1` killed jacred 3 times in 14
// days from anacrolix/torrent's assertAllPiecesRelativeAvailabilityZero,
// reached through (*Torrent).close <- (*Torrent).Drop <- our Analyze. The
// assertion is the library's own invariant about its own peer bookkeeping, so
// the only thing a caller can do is not die with it.
func TestDroppingAProbeTorrentSurvivesTheLibraryAssertion(t *testing.T) {
	// The real message, verbatim from the production journal.
	dropProbeTorrent("c268b36eb250d931d6a7b7e34a2c6ee95e8356da", func() {
		panic("piece 0 has relative availability 1")
	})
	// Reaching here at all is the assertion: before this wrapper, that panic
	// unwound out of Analyze and took the process with it.

	// A runtime panic (not a string) must be survived too — the library also
	// panics with a bare int in decPieceAvailability.
	dropProbeTorrent("deadbeef", func() { panic(1) })
	dropProbeTorrent("deadbeef", func() { var p *int; _ = *p })
}

// A normal drop must still happen, exactly once, and must not be swallowed
// into a no-op by the recover wrapper.
func TestANormalDropStillRuns(t *testing.T) {
	calls := 0
	dropProbeTorrent("abc", func() { calls++ })
	if calls != 1 {
		t.Errorf("drop ran %d times, want exactly 1", calls)
	}
}

// The recover must not hide a panic raised anywhere else in Analyze's
// teardown: it wraps the Drop call only. Guarded by reading the source,
// because the alternative — a recover sitting in Analyze itself — would
// silently swallow real bugs in the probe path.
func TestTheRecoverWrapsOnlyTheDrop(t *testing.T) {
	src, err := os.ReadFile("native.go")
	if err != nil {
		t.Fatalf("read native.go: %v", err)
	}
	body := string(src)
	if n := strings.Count(body, "recover()"); n != 1 {
		t.Errorf("native.go has %d recover() calls, want exactly 1 (in dropProbeTorrent)", n)
	}
	fn := body[strings.Index(body, "func dropProbeTorrent("):]
	if !strings.Contains(fn[:strings.Index(fn, "\n}")], "recover()") {
		t.Error("the single recover() is not inside dropProbeTorrent")
	}
	if !strings.Contains(body, "defer dropProbeTorrent(") {
		t.Error("Analyze no longer defers dropProbeTorrent — a raw t.Drop() would kill the process again")
	}
	if strings.Contains(body, "defer t.Drop()") {
		t.Error("a raw `defer t.Drop()` is back; it must go through dropProbeTorrent")
	}
}
