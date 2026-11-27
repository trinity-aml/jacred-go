package core

import (
	"errors"
	"testing"
	"time"
)

// The rule that is easy to get backwards, and was: the window is counted from
// a refusal, never from a success. A successful login is immediately followed
// by the page that needed it.
func TestTheCooldownFollowsARefusalAndNotASuccess(t *testing.T) {
	g := NewLoginGate("stub")

	if err := g.HoldOff(); err != nil {
		t.Fatalf("a fresh gate holds off a login: %v", err)
	}

	_ = g.Note(errors.New("refused"))
	err := g.HoldOff()
	if err == nil {
		t.Fatal("a refused login is not held off — the next page would retry immediately")
	}
	if !errors.Is(err, ErrNotAuthorized) {
		t.Errorf("hold-off error %q does not wrap ErrNotAuthorized, so cronErrorStatus cannot report work_login", err)
	}

	_ = g.Note(nil)
	if err := g.HoldOff(); err != nil {
		t.Errorf("a login right after a successful one is held off: %v", err)
	}
}

// HoldOff must answer with an error, not a bool or a nil. Several parsers here
// returned nil on this path, so "we tried and were turned away" was
// indistinguishable from "we are logged in" and the run carried on as a guest.
func TestTheHoldOffBranchIsNeverSilent(t *testing.T) {
	g := NewLoginGate("stub")
	_ = g.Note(errors.New("refused"))
	for i := 0; i < 3; i++ {
		if g.HoldOff() == nil {
			t.Fatalf("attempt %d: hold-off returned nil while the cooldown was open", i)
		}
	}
}

// Note passes the error through, so a caller can write
// `return gate.Note(p.takeLogin(ctx))` without dropping the cause.
func TestNotePassesTheCauseBack(t *testing.T) {
	g := NewLoginGate("stub")
	want := errors.New("the cause")
	if got := g.Note(want); !errors.Is(got, want) {
		t.Errorf("Note returned %v, want the error it was given", got)
	}
	if got := g.Note(nil); got != nil {
		t.Errorf("Note(nil) returned %v", got)
	}
}

// The recovery window is independent of the failure cooldown, because the
// storm case is a login that *succeeds* while the page still comes back as a
// guest page: nothing fails, so the failure cooldown never arms.
func TestTheRecoveryWindowIsIndependentOfTheFailureCooldown(t *testing.T) {
	g := NewLoginGate("stub")

	if !g.BeginRecovery() {
		t.Fatal("the first recovery was refused")
	}
	for i := 0; i < 5; i++ {
		if g.BeginRecovery() {
			t.Fatalf("recovery %d was allowed inside the window — a succeeding login is unbounded", i+2)
		}
	}

	// Every one of those logins succeeded, so the failure cooldown is open on
	// nothing and must not be what is holding anything off.
	_ = g.Note(nil)
	if err := g.HoldOff(); err != nil {
		t.Errorf("the failure cooldown is armed after successes only: %v", err)
	}

	g.mu.Lock()
	g.lastRecovery = time.Now().Add(-GuestRecoveryInterval - time.Second)
	g.mu.Unlock()
	if !g.BeginRecovery() {
		t.Error("recovery is still refused after the window elapsed")
	}
}

// A nil gate must behave as "no limits" rather than panic: it is a field on a
// parser struct and a zero-valued parser is constructed in tests.
func TestANilGateImposesNoLimits(t *testing.T) {
	var g *LoginGate
	if err := g.HoldOff(); err != nil {
		t.Errorf("nil gate holds off: %v", err)
	}
	if !g.BeginRecovery() {
		t.Error("nil gate refuses recovery")
	}
	want := errors.New("x")
	if got := g.Note(want); !errors.Is(got, want) {
		t.Errorf("nil gate dropped the cause: %v", got)
	}
}
