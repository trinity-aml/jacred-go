package toloka

import (
	"os"
	"strings"
	"testing"
)

// The login form served in place of a listing is how an unusable session is
// detected, so its signature is pinned against a real capture rather than a
// hand-written snippet.
func TestLoginFormIsRecognisedOnTheLivePage(t *testing.T) {
	page, err := os.ReadFile("testdata/login_form.html")
	if err != nil {
		t.Fatalf("фикстура: %v", err)
	}
	if !looksLikeTolokaLoginForm(string(page)) {
		t.Error("реальная страница входа не распознана")
	}
	// Two-sided: the check must not fire on an ordinary listing, or a working
	// session would be thrown away on every page.
	listing := `<html lang="uk"><body><table class="forumline">` +
		`<a href="viewtopic.php?t=1">Тема</a></table></body></html>`
	if looksLikeTolokaLoginForm(listing) {
		t.Error("листинг принят за форму входа")
	}
}

// The parser must not invent a User-Agent of its own: the login POST and the
// page fetches have to agree, and in flaresolverr mode the browser's UA is the
// one that reaches the site. Whether toloka actually binds the session to it is
// unconfirmed (see takeLogin), but a hand-written UA next to a Chrome
// ClientHello is wrong regardless.
func TestParserInventsNoUserAgent(t *testing.T) {
	if got := defaultUA(); got != "" {
		t.Errorf("defaultUA должен быть пустым, чтобы UA задавал Fetcher/браузер, получено %q", got)
	}
	src, err := os.ReadFile("toloka.go")
	if err != nil {
		t.Fatal(err)
	}
	// The comments explain the old literal, so match the literal's own shape:
	// a Mozilla string being returned or assigned as a UA value.
	if strings.Contains(string(src), `return "Mozilla/5.0`) {
		t.Error("в toloka.go снова зашит собственный User-Agent")
	}
}

// A session refused on the very next request after it was issued will not be
// fixed by logging in again. Unbounded, that was one credentials POST per
// category — four in 45 seconds in production — which is how a tracker's
// anti-bruteforce gets tripped.
func TestReloginLoopIsBounded(t *testing.T) {
	if maxSessionRejects < 1 {
		t.Fatalf("граница перелогинов должна быть положительной, получено %d", maxSessionRejects)
	}
	if maxSessionRejects > 3 {
		t.Errorf("граница перелогинов %d слишком велика: каждая попытка — отдельный POST с паролем", maxSessionRejects)
	}
}
