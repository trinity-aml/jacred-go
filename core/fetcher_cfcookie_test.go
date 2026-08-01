package core

import (
	"strings"
	"testing"
)

func TestStripCFManagedCookies(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"bb_session=SECRET", "bb_session=SECRET"},
		{"cf_clearance=X", ""},
		{"cf_clearance=X; bb_session=S", "bb_session=S"},
		{"bb_session=S; cf_clearance=X; bb_guid=G", "bb_session=S; bb_guid=G"},
		{"__cf_bm=B; bb_session=S", "bb_session=S"},
		{"  bb_session=S ;  cf_clearance=X  ", "bb_session=S"},
		// Only exact names go. A cookie that merely contains "cf" stays.
		{"cfduid_legacy=1; my_cf_clearance=2", "cfduid_legacy=1; my_cf_clearance=2"},
	}
	for _, c := range cases {
		if got := stripCFManagedCookies(c.in); got != c.want {
			t.Errorf("stripCFManagedCookies(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The bug this exists to prevent, reproduced end to end through the same
// helpers the parsers use.
//
// takeLogin persists a cf_clearance snapshot next to bb_session so the
// standard non-flare path isn't bounced at the edge. An hour later a solve
// mints a fresh clearance — and mergeCookies, which lets the caller win on
// same-name conflicts, used to throw that fresh one away and replay the stale
// one. CF then challenges within a second, the challenge reads as "cookies
// stale", and the domain re-solves once per page until the browser dies.
func TestFreshClearanceBeatsTheOneStoredWithAuth(t *testing.T) {
	// What takeLogin ends up saving.
	postCookie := MergeCookieStrings("", "cf_clearance=OLD_FROM_LOGIN; bb_guid=g1")
	savedAuth := MergeCookieStrings(postCookie, "bb_session=SECRET")
	if !strings.Contains(savedAuth, "cf_clearance=OLD_FROM_LOGIN") {
		t.Fatalf("precondition changed — saved auth no longer carries a clearance: %s", savedAuth)
	}

	// What a later solve produces, and what fetchWithCookies now sends.
	sessCookies := "cf_clearance=FRESH_FROM_SOLVE; bb_guid=g2"
	sent := mergeCookies(sessCookies, stripCFManagedCookies(savedAuth))

	if strings.Contains(sent, "OLD_FROM_LOGIN") {
		t.Errorf("stale clearance still replayed: %s", sent)
	}
	if !strings.Contains(sent, "cf_clearance=FRESH_FROM_SOLVE") {
		t.Errorf("fresh clearance missing: %s", sent)
	}
	// The auth cookie must survive — stripping CF names must not cost the login.
	if !strings.Contains(sent, "bb_session=SECRET") {
		t.Errorf("auth cookie dropped: %s", sent)
	}
	// Exactly one clearance goes out.
	if n := strings.Count(sent, "cf_clearance="); n != 1 {
		t.Errorf("cf_clearance appears %d times: %s", n, sent)
	}
}

// A caller cookie with no CF names must come through byte-identical, so the
// strip cannot disturb trackers that never store a clearance.
func TestStripLeavesPlainAuthCookiesAlone(t *testing.T) {
	const auth = "PHPSESSID=abc; uid=42; pass=deadbeef"
	if got := stripCFManagedCookies(auth); got != auth {
		t.Errorf("got %q, want %q", got, auth)
	}
}
