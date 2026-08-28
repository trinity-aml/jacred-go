package animelayer

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Fixtures were captured anonymously from animelayer.ru on 2026-08-28:
// listing_anime_anon.html is /torrents/anime/ page 1 as served to a logged-out
// visitor, login_page.html is /auth/login/. There is deliberately no captured
// authorized page — it would carry the account's identity, which must not enter
// the repo — so the authorized case is derived from the anonymous capture by
// dropping the two header blocks the site swaps out on login (see authorized()).

func load(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	return string(b)
}

var anonBlockRe = regexp.MustCompile(`(?s)<!-- noindex -->.*?<!--/noindex-->`)

// authorized simulates a logged-in catalog page. animelayer serves the same
// markup either way and only swaps the header, so removing the two
// <!-- noindex --> blocks that hold the login and registration links is the
// whole difference as far as this parser is concerned.
func authorized(t *testing.T, body string) string {
	t.Helper()
	out := anonBlockRe.ReplaceAllString(body, `<div class="profile-menu"><a rel="nofollow" href="/auth/logout/">Выход</a></div>`)
	if hasAnonymousMarkers(out) {
		t.Fatalf("fixture still looks anonymous after stripping the header blocks")
	}
	return out
}

// The catalog is public: an unauthorized request returns a complete listing,
// so "did the page render" cannot distinguish the two and the header must.
func TestAnonymousListingIsDetected(t *testing.T) {
	body := load(t, "listing_anime_anon.html")

	if !strings.Contains(body, `id="wrapper"`) {
		t.Fatal("fixture lacks id=\"wrapper\" — it would be rejected before the marker check")
	}
	if n := len(parseListing(body, "https://animelayer.ru", 1)); n == 0 {
		t.Fatal("anonymous page parsed zero rows — fixture no longer represents the failure")
	}
	if !hasAnonymousMarkers(body) {
		t.Error("anonymous catalog page not detected as anonymous")
	}

	// Regression guard. The parser used to look for the login form's action
	// attribute, which only ever appears on /auth/login/ — never on a listing.
	// The check could not fire, so an expired cookie was never invalidated and
	// every run scored parsed=N failed=N until someone re-authenticated by hand.
	if strings.Contains(body, `action="/auth/login/"`) {
		t.Error("listing now carries action=\"/auth/login/\"; the old detection would have worked after all")
	}
}

func TestAuthorizedListingIsNotFlagged(t *testing.T) {
	body := authorized(t, load(t, "listing_anime_anon.html"))
	if hasAnonymousMarkers(body) {
		t.Error("authorized page flagged as anonymous — the parser would re-login on every page")
	}
	if len(parseListing(body, "https://animelayer.ru", 1)) == 0 {
		t.Error("authorized page parsed zero rows")
	}
}

func TestParseListing(t *testing.T) {
	items := parseListing(load(t, "listing_anime_anon.html"), "https://animelayer.ru", 1)
	if len(items) < 30 {
		t.Fatalf("parsed %d rows, expected the full page", len(items))
	}
	for i, it := range items {
		for _, k := range []string{"url", "title", "name", "createTime", "sizeName"} {
			if strings.TrimSpace(asString(it[k])) == "" {
				t.Errorf("row %d: empty %s (title=%q)", i, k, asString(it["title"]))
			}
		}
		if !strings.HasPrefix(asString(it["url"]), "https://animelayer.ru/torrent/") {
			t.Errorf("row %d: unexpected url %q", i, asString(it["url"]))
		}
		if asString(it["relased"]) == "0" {
			t.Errorf("row %d: no release year", i)
		}
	}
}

// The size is printed in the listing next to the leecher count. Reading it there
// means a row is not worthless when the attachment download fails, and it is the
// only size available before the login-gated .torrent is fetched.
func TestRowSizeName(t *testing.T) {
	items := parseListing(load(t, "listing_anime_anon.html"), "https://animelayer.ru", 1)
	sizeRe := regexp.MustCompile(`^[0-9]+(\.[0-9]+)? (KB|MB|GB|TB|КБ|МБ|ГБ|ТБ)$`)
	for i, it := range items {
		got := asString(it["sizeName"])
		if !sizeRe.MatchString(got) {
			t.Errorf("row %d: sizeName = %q", i, got)
		}
	}
}

func TestLooksLikeHTML(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"torrent", []byte("d8:announce35:http://tracker.example/announce"), false},
		{"html", []byte("<!DOCTYPE html>\n<html>"), true},
		{"html after whitespace", []byte("\r\n\t  <html>"), true},
		{"empty", nil, false},
	}
	for _, c := range cases {
		if got := looksLikeHTML(c.data); got != c.want {
			t.Errorf("%s: looksLikeHTML = %v, want %v", c.name, got, c.want)
		}
	}
}

// takeLogin posts credentials straight at this form; if the fields are renamed
// the login silently yields no session and the tracker stops parsing.
func TestLoginFormContract(t *testing.T) {
	body := load(t, "login_page.html")
	for _, want := range []string{`action="/auth/login/"`, `name="login"`, `name="password"`, `method="post"`} {
		if !strings.Contains(body, want) {
			t.Errorf("login page no longer contains %s", want)
		}
	}
}
