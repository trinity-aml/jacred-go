package server

import (
	"errors"
	"fmt"
	"testing"

	"jacred/core"
)

// A failed run used to answer HTTP 500 with `"status":"ok"`, because parsers
// initialise ParseResult to "ok" and most return that same value alongside the
// error. A monitor keying on the field read the failure as a success.
func TestCronErrorStatusReplacesStaleOK(t *testing.T) {
	plain := errors.New("boom")
	cases := map[string]string{
		"ok":           "error",
		"":             "error",
		"  ":           "error",
		"cf-challenge": "cf-challenge",
		"canceled":     "canceled",
	}
	for in, want := range cases {
		if got := cronErrorStatus(plain, in); got != want {
			t.Errorf("cronErrorStatus(plain, %q) = %q, want %q", in, got, want)
		}
	}
}

// Every authorization failure reports one status and one HTTP code, whatever the
// parser left in its result struct. Three spellings used to coexist —
// "login failed" with HTTP 200, a free-text "login error: <message>", and
// "work_login" with HTTP 500.
func TestCronErrorStatusIsAuthoritativeForAuthFailures(t *testing.T) {
	authErr := fmt.Errorf("korsars: login failed: %w", core.ErrNotAuthorized)
	for _, stale := range []string{"ok", "", "login failed", "cf-challenge"} {
		if got := cronErrorStatus(authErr, stale); got != core.StatusWorkLogin {
			t.Errorf("with result status %q, got %q, want %q", stale, got, core.StatusWorkLogin)
		}
	}
	// A wrapped sentinel must still be recognised through several layers.
	nested := fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", core.ErrNotAuthorized))
	if got := cronErrorStatus(nested, "ok"); got != core.StatusWorkLogin {
		t.Errorf("nested wrap: got %q", got)
	}
}

// The handlers that take a plain (string, error) result emitted no status at all
// on failure; they now pass an empty one and rely on the error.
func TestCronErrorStatusWithNoResultStatus(t *testing.T) {
	if got := cronErrorStatus(errors.New("boom"), ""); got != "error" {
		t.Errorf("got %q, want error", got)
	}
	if got := cronErrorStatus(fmt.Errorf("x: %w", core.ErrNotAuthorized), ""); got != core.StatusWorkLogin {
		t.Errorf("got %q, want %q", got, core.StatusWorkLogin)
	}
}
