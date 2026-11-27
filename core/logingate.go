package core

import (
	"fmt"
	"sync"
	"time"
)

// LoginRetryCooldown is the minimum gap after a login the server refused.
// Credentials the server rejects are what earns an anti-bruteforce block, and
// a sweep has hundreds of pages to retry on.
const LoginRetryCooldown = 2 * time.Minute

// GuestRecoveryInterval bounds re-authentication driven by the page path.
const GuestRecoveryInterval = 2 * time.Minute

// LoginGate holds the two pieces of timing that every parser with a mid-run
// re-login needs. It exists because both of them have a non-obvious rule that
// is easy to get backwards, and eight hand-written copies of a rule nothing
// checks is how this codebase has lost things before:
//
//   - The failure cooldown is counted from a login the server **refused**,
//     never from one that succeeded. A successful login is immediately
//     followed by the page that needed it, so a window counted from a success
//     blocks the very recovery it is meant to protect. That mistake was made
//     here once and caught only by a test.
//
//   - The recovery window is separate from the failure cooldown, because the
//     storm case is a login that *succeeds* while the page still comes back as
//     a guest page: nothing fails, so a failure cooldown never arms, and a
//     sweep would spend one login POST per page.
//
// A parser that stops the whole run on an unrecoverable guest page (mazepa,
// korsars, rutracker) is bounded by that stop and needs only HoldOff/Note.
// One that carries on parsing the guest body (torrentby, nnmclub) has no such
// bound and must gate its recovery with BeginRecovery.
type LoginGate struct {
	mu            sync.Mutex
	lastFailure   time.Time
	lastRecovery  time.Time
	tracker       string
	failCooldown  time.Duration
	recoveryEvery time.Duration
}

// NewLoginGate returns a gate with this package's default windows. The tracker
// name only ever appears in the error text.
func NewLoginGate(tracker string) *LoginGate {
	return &LoginGate{
		tracker:       tracker,
		failCooldown:  LoginRetryCooldown,
		recoveryEvery: GuestRecoveryInterval,
	}
}

// HoldOff reports whether a login attempt is still being held off after a
// refusal, as an error naming the remaining wait.
//
// It returns an error rather than a bool deliberately: "we tried two minutes
// ago and were turned away" and "we are logged in" must not look the same to
// the caller. Several parsers here used to answer nil on this path, after
// which the run carried on unauthenticated and reported whatever a guest was
// served.
func (g *LoginGate) HoldOff() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lastFailure.IsZero() {
		return nil
	}
	wait := g.failCooldown - time.Since(g.lastFailure)
	if wait <= 0 {
		return nil
	}
	return fmt.Errorf("%s: login retry held off for %s after a refused attempt: %w",
		g.tracker, wait.Round(time.Second), ErrNotAuthorized)
}

// Note records the outcome of a login attempt and passes the error back, so a
// caller can write `return gate.Note(p.takeLogin(ctx))`.
func (g *LoginGate) Note(err error) error {
	if g == nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		g.lastFailure = time.Now()
	} else {
		g.lastFailure = time.Time{}
	}
	return err
}

// BeginRecovery reports whether the page path may spend a login attempt now,
// and stamps the window if so. The stamp is taken whatever the attempt's
// outcome turns out to be, which is the whole point: see the type's comment.
func (g *LoginGate) BeginRecovery() bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.lastRecovery.IsZero() && time.Since(g.lastRecovery) < g.recoveryEvery {
		return false
	}
	g.lastRecovery = time.Now()
	return true
}
