package selezen

import (
	"os"
	"strings"
	"testing"

	"jacred/app"
)

// relizy_guest.html is /relizy-ot-selezen/ captured from selezen.top on
// 2026-08-28 without a session. selezen serves the catalog to guests, so the
// page carries dle_root and a full set of cards either way — which is why the
// account name in the header is the only usable signal.

func loadFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	return string(b)
}

func parserWithLogin(user string) *Parser {
	cfg := app.DefaultConfig()
	cfg.Selezen.Login.U = user
	cfg.Selezen.Login.P = "x"
	return &Parser{Config: cfg}
}

func TestGuestPageIsDetected(t *testing.T) {
	guest := loadFixture(t, "relizy_guest.html")

	// The guest page renders content, so neither dle_root nor a non-empty row
	// set can stand in for an authorization check.
	if !strings.Contains(guest, "dle_root") {
		t.Fatal("guest page lacks dle_root — it would be rejected before the marker check")
	}
	if n := len(parsePageHTML(guest)); n == 0 {
		t.Fatal("guest page parsed no rows — fixture no longer represents the failure")
	}
	if !parserWithLogin("someuser").loggedOut(guest) {
		t.Error("guest page not detected as logged out")
	}
}

func TestAuthorizedPageIsNotFlagged(t *testing.T) {
	authed := loadFixture(t, "relizy_guest.html") + `<span class="user">someuser</span>`
	if parserWithLogin("someuser").loggedOut(authed) {
		t.Error("page carrying the account name was flagged as logged out")
	}
}

// The regression this guards: parsePage used to return (0,0,0,0,0,nil) whenever
// ensureCookie handed back an empty cookie, so a missing or dead session
// produced "parsed=0 added=0 skipped=0 failed=0" with status "ok" — identical
// to a quiet day, and with no log line naming a cause. Parse now refuses to
// start without a session.
func TestAuthorizeRefusesWithoutCredentials(t *testing.T) {
	p := parserWithLogin("")
	p.Config.Selezen.Login.P = ""
	if p.loginConfigured() {
		t.Fatal("loginConfigured true with neither cookie nor credentials")
	}
	err := p.authorize(nil)
	if err == nil {
		t.Fatal("authorize returned nil with no credentials — the silent zero is back")
	}
	if !strings.Contains(err.Error(), "no cookie and no login credentials") {
		t.Errorf("authorize error = %v", err)
	}
}

func TestLoginConfigured(t *testing.T) {
	cfg := app.DefaultConfig()
	cfg.Selezen.Login.U, cfg.Selezen.Login.P = "", ""
	cfg.Selezen.Cookie = "PHPSESSID=abc"
	if !(&Parser{Config: cfg}).loginConfigured() {
		t.Error("a configured cookie should count as configured login")
	}
}
