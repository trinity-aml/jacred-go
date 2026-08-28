package torrentby

import (
	"os"
	"strings"
	"testing"
	"time"

	"jacred/app"
)

// films_guest.html is /films/?page=1 captured from torrent.by on 2026-08-28
// without a session. torrentby's catalog is fully public — the parser reads 900+
// records with no session at all — so a guest page is a complete listing, and
// the expired-cookie check cannot be written against the login form.

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
	cfg.TorrentBy.Login.U = user
	return &Parser{Config: cfg}
}

func TestGuestListingIsDetected(t *testing.T) {
	guest := loadFixture(t, "films_guest.html")

	if n := len(parsePageHTML("https://torrent.by", "films", guest, time.Now().UTC())); n == 0 {
		t.Fatal("guest listing parsed no rows — fixture no longer represents the failure")
	}
	if !parserWithLogin("someuser").loggedOut(guest) {
		t.Error("guest listing not detected as logged out")
	}

	// Regression guard: the check used to key off action="/login/", which lives
	// only on the login page and could therefore never fire on a listing.
	if strings.Contains(guest, `action="/login/"`) {
		t.Error("guest listing now carries action=\"/login/\"; the old detection would have worked after all")
	}
}

func TestAuthorizedListingIsNotFlagged(t *testing.T) {
	authed := strings.Replace(loadFixture(t, "films_guest.html"), `/login/`, `/logout/`, 1)
	if parserWithLogin("someuser").loggedOut(authed) {
		t.Error("a page carrying /logout was flagged as logged out")
	}
}

// Guest mode is a supported configuration here — torrentby parses fine without
// credentials, so the check must stay off when none are set.
func TestGuestModeSkipsTheCheck(t *testing.T) {
	if parserWithLogin("").loggedOut(loadFixture(t, "films_guest.html")) {
		t.Error("check fired with no credentials configured")
	}
}
