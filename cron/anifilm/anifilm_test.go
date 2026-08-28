package anifilm

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// releases_anime_guest.html is /releases/page/1?category=anime captured from
// anifilm.pro on 2026-08-28 without a session. anifilm serves the catalog to
// guests, so this page renders in full — which is exactly why the expired-cookie
// check cannot be written against the login form.

func loadFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	return string(b)
}

var releaseCardRe = regexp.MustCompile(`/releases/[0-9]+-[a-z0-9-]+`)

func TestGuestPageIsDetected(t *testing.T) {
	guest := loadFixture(t, "releases_anime_guest.html")

	// A guest gets real content, so the presence of a listing proves nothing
	// about authorization.
	if n := len(releaseCardRe.FindAllString(guest, -1)); n == 0 {
		t.Fatal("guest page carries no release cards — fixture no longer represents the failure")
	}
	if !loggedOut(guest) {
		t.Error("guest page not detected as logged out")
	}

	// Regression guard. The check used to look for the login form's action
	// attribute, which lives only on /account/login — never on a listing.
	// Upstream's AnifilmSyncService.LooksLikeLoginForm still has this gap.
	if strings.Contains(guest, `action="/account/login"`) {
		t.Error("guest page now carries action=\"/account/login\"; the old detection would have worked after all")
	}
}

// The check is two-sided so that a renamed logout path cannot by itself put
// every page into a re-login loop.
func TestAuthorizedPageIsNotFlagged(t *testing.T) {
	guest := loadFixture(t, "releases_anime_guest.html")
	authed := strings.Replace(guest, `/account/login`, `/account/logout`, 1)
	if loggedOut(authed) {
		t.Error("a page carrying /account/logout was flagged as logged out")
	}
}

func TestLoggedOutNeedsBothSides(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"guest header", `<a href="/account/login">Вход</a>`, true},
		{"account menu", `<a href="/account/logout">Выход</a>`, false},
		{"neither marker", `<div>no header at all</div>`, false},
		{"both present", `<a href="/account/login">x</a><a href="/account/logout">y</a>`, false},
	}
	for _, c := range cases {
		if got := loggedOut(c.body); got != c.want {
			t.Errorf("%s: loggedOut = %v, want %v", c.name, got, c.want)
		}
	}
}
