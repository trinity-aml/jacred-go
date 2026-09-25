package lostfilm

import (
	"errors"
	"strings"
	"testing"

	"jacred/app"
	"jacred/core"
)

// The check is deliberately stricter than "is the value non-empty", which is
// what the C# original tests. The deployed init.yaml here carried a short note
// in `cookie` with no '=' in it: non-empty, sent as a Cookie header, useless —
// and the parser ran anonymously for as long as nobody looked.
func TestHasAuthCookie(t *testing.T) {
	cases := []struct {
		name   string
		cookie string
		want   bool
	}{
		{"real session", "lf_session=abc; lf_udv=1; PHPSESSID=xyz", true},
		{"single pair", "PHPSESSID=abc", true},
		{"padded", "  lf_session=abc  ", true},
		{"empty", "", false},
		{"whitespace", "   ", false},
		{"prose, no '='", "надо обновить", false},
		{"name with no value", "lf_session=", false},
		{"value with no name", "=abc", false},
		{"separators only", " ; ; ", false},
	}
	for _, c := range cases {
		if got := hasAuthCookie(c.cookie); got != c.want {
			t.Errorf("%s: hasAuthCookie = %v, want %v", c.name, got, c.want)
		}
	}
}

// lostfilm serves its listings to anyone and gates only the magnets, so an
// unauthenticated run parses every page and then fails per episode — reported
// as failed=N/withoutMag=N with nothing naming the cause. The gate turns that
// into one named failure before the first fetch.
func TestAuthorizeRefusesAnUnusableCookie(t *testing.T) {
	cfg := app.DefaultConfig()

	for _, bad := range []string{"", "   ", "надо обновить"} {
		cfg.Lostfilm.Cookie = bad
		err := (&Parser{Config: cfg}).authorize()
		if err == nil {
			t.Errorf("cookie %q accepted", bad)
			continue
		}
		// The shared contract: handlers derive HTTP 500 + work_login from this.
		if !errors.Is(err, core.ErrNotAuthorized) {
			t.Errorf("cookie %q: error does not wrap core.ErrNotAuthorized: %v", bad, err)
		}
		if !strings.Contains(err.Error(), "lostfilm") {
			t.Errorf("cookie %q: error does not name the tracker: %v", bad, err)
		}
	}

	cfg.Lostfilm.Cookie = "lf_session=abc; PHPSESSID=xyz"
	if err := (&Parser{Config: cfg}).authorize(); err != nil {
		t.Errorf("a usable cookie was rejected: %v", err)
	}
}

// A refused session looks identical on every episode, so the diagnosis is worth
// one line per run, not one per row — and it has to come back on the next run.
func TestVPageRejectionIsReportedOncePerRun(t *testing.T) {
	p := &Parser{}
	p.noteVPageWithoutMagnets("https://example/v1")
	if !p.vPageReported {
		t.Fatal("first rejection was not recorded")
	}
	p.noteVPageWithoutMagnets("https://example/v2")
	if !p.vPageReported {
		t.Error("flag cleared by a second call")
	}
	// Each run resets it; parseRange and ParseSeasonPacks do this after taking
	// the lock, so a later run reports again instead of staying quiet forever.
	p.vPageReported = false
	p.noteVPageWithoutMagnets("https://example/v3")
	if !p.vPageReported {
		t.Error("a new run does not report again")
	}
}
