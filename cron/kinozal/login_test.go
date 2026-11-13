package kinozal

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"jacred/app"
	"jacred/core"
)

// kinozal moved fully behind a Cloudflare managed challenge: measured
// 2026-09-26, takelogin.php, login.php and browse.php all answer 403 carrying
// cf_chl_opt. The login POST therefore has to be told apart from a refusal of
// the credentials, because the two need completely different fixes — and the
// marker check must not fire on a real listing, which is the mistake rutracker
// already paid for (a cleared page still loads CF's JS-detection beacon).
func TestCFChallengeIsDistinguishedFromAListing(t *testing.T) {
	challenge, err := os.ReadFile("testdata/cf_challenge.html")
	if err != nil {
		t.Fatalf("фикстура челленджа: %v", err)
	}
	if !looksLikeCFChallenge(string(challenge)) {
		t.Error("реальная заглушка Cloudflare не распознана как челлендж")
	}

	listing, err := os.ReadFile("testdata/browse_cat46.html")
	if err != nil {
		t.Fatalf("фикстура листинга: %v", err)
	}
	// The listing is CP1251 on the wire, which is what the parser decodes.
	if looksLikeCFChallenge(decodeKinozalBody(listing)) {
		t.Error("настоящий листинг принят за челлендж Cloudflare")
	}
}

// The stale literal is the bug, not the string: defaultUserAgent is pinned to
// the tls-client's Chrome impersonation profile, and a hardcoded UA 47 majors
// behind it contradicts the ClientHello on every request that carries it.
func TestNoHardcodedUserAgent(t *testing.T) {
	src, err := os.ReadFile("kinozal.go")
	if err != nil {
		t.Fatal(err)
	}
	// Match the UA literal, not the prose: the comment explaining why the old
	// one was wrong necessarily names it.
	if strings.Contains(string(src), "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/99") {
		t.Error("в kinozal.go снова зашит устаревший User-Agent Chrome/99")
	}
	if got := kinozalUA(app.TrackerSettings{}); got != "" {
		t.Errorf("без настройки UA должен быть пустым (чтобы сработал defaultUserAgent), получено %q", got)
	}
	if got := kinozalUA(app.TrackerSettings{UserAgent: "  custom-ua  "}); got != "custom-ua" {
		t.Errorf("настроенный useragent не пробрасывается: %q", got)
	}
}

// takeLogin must never report success it did not achieve. Returning nil on the
// cooldown branch made "we tried two minutes ago" look identical to "logged
// in", after which requireLogin blamed "login produced no session cookie" — a
// different failure with a different fix.
func TestCooldownIsReportedAsAnError(t *testing.T) {
	p := &Parser{}
	p.Config.Kinozal = app.TrackerSettings{
		Host:  "https://kinozal.guru",
		Login: app.LoginSettings{U: "someone", P: "secret"},
	}
	p.lastLoginAttempt = time.Now()

	err := p.takeLogin(context.Background())
	if err == nil {
		t.Fatal("кулдаун отрапортовал успех")
	}
	if !errors.Is(err, core.ErrNotAuthorized) {
		t.Errorf("ошибка кулдауна не оборачивает ErrNotAuthorized: %v", err)
	}
	// The sentinel is what lets the mid-run retry stay quiet instead of logging
	// once per page for the whole cooldown.
	if !errors.Is(err, errLoginCooldown) {
		t.Errorf("ошибка кулдауна не помечена errLoginCooldown: %v", err)
	}
}

// "login is not configured", a refused password and a Cloudflare block are
// three different problems. All of them used to surface as an empty cookie.
func TestMissingCredentialsAreNamed(t *testing.T) {
	p := &Parser{}
	p.Config.Kinozal = app.TrackerSettings{Host: "https://kinozal.guru"}

	err := p.takeLogin(context.Background())
	if err == nil {
		t.Fatal("отсутствие логина отрапортовало успех")
	}
	if !errors.Is(err, core.ErrNotAuthorized) {
		t.Errorf("не обёрнут ErrNotAuthorized: %v", err)
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("причина не названа: %v", err)
	}
}

// A dead session used to produce `ok fetched=0 failed=0` for all 25 categories,
// byte-identical to a quiet day — measured in production 2026-09-27 with a
// session saved the previous afternoon. Three stacked silent returns caused it:
// fetchBrowse turned a non-2xx into ("", nil), parsePage turned that into
// (nil, nil), and the mid-run guest check sat *after* the brand-title gate and
// so could never run. loggedIn is the proof authorize() demands instead.
func TestLoggedInIsProvedFromThePage(t *testing.T) {
	listing, err := os.ReadFile("testdata/browse_cat46.html")
	if err != nil {
		t.Fatal(err)
	}
	body := string(listing)

	ok, renamed := loggedIn(body)
	if !ok || renamed {
		t.Errorf("реальный листинг с маркером выхода: ok=%v renamed=%v, ожидалось true/false", ok, renamed)
	}

	// Marker renamed: the listing itself still proves the session, or every page
	// would go into a re-login loop the day kinozal renames the logout link.
	noMarker := strings.ReplaceAll(body, ">Выход</a>", ">Exit</a>")
	if ok, renamed := loggedIn(noMarker); !ok || !renamed {
		t.Errorf("листинг без маркера: ok=%v renamed=%v, ожидалось true/true", ok, renamed)
	}

	// A Cloudflare interstitial is not a session.
	challenge, err := os.ReadFile("testdata/cf_challenge.html")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := loggedIn(string(challenge)); ok {
		t.Error("заглушка Cloudflare принята за рабочую сессию")
	}

	// A branded page with no rows and no marker — the shape of login.php — is not
	// a session either.
	if ok, _ := loggedIn(`<html><head><title>Вход :: Кинозал.GURU</title></head><body></body></html>`); ok {
		t.Error("страница входа принята за рабочую сессию")
	}
}

// The three silent returns are the bug, so their shape is what is pinned.
func TestNoSilentZeroInTheFetchPath(t *testing.T) {
	src, err := os.ReadFile("kinozal.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	for name, marker := range map[string]string{
		"fetchBrowse": "func (p *Parser) fetchBrowse(",
		"parsePage":   "func (p *Parser) parsePage(",
	} {
		i := strings.Index(body, marker)
		if i < 0 {
			t.Fatalf("%s не найдена", name)
		}
		end := strings.Index(body[i:], "\nfunc ")
		fn := body[i : i+end]
		for _, silent := range []string{"return \"\", nil", "return nil, nil"} {
			if strings.Contains(fn, silent) {
				t.Errorf("%s снова содержит `%s` — отказ станет неотличим от пустой категории", name, silent)
			}
		}
	}

	// And the run must be gated up front rather than per row.
	for _, entry := range []string{") Parse(", ") UpdateTasksParse(", ") ParseAllTask(", ") ParseLatest("} {
		i := strings.Index(body, entry)
		if i < 0 {
			t.Fatalf("точка входа не найдена: %s", entry)
		}
		end := strings.Index(body[i:], "\nfunc ")
		if !strings.Contains(body[i:i+end], "p.authorize(ctx)") {
			t.Errorf("%s не проверяет авторизацию до прогона", entry)
		}
	}
}

// Issue #5 claimed the original avoids kinozal's daily view limit by using GET
// where this port used POST. The original does use GET — its own comment gives
// the reason, and it is flaresolverr's request.post being flaky in a shared
// Chromium session, not the quota. Measured against the live site on
// 2026-11-13 with one session, two ids and both methods in crossed order: GET
// and POST returned the identical limit message every time.
//
// So the request is a GET now — the id and action were always in the query
// string, so the form body was duplication, and a POST is the shape that can
// come back as the previous browse tab — but nothing here pretends that lifts
// the cap.
func TestResolveMagnetUsesGet(t *testing.T) {
	src, err := os.ReadFile("kinozal.go")
	if err != nil {
		t.Fatal(err)
	}
	fn := funcBody(string(src), "func (p *Parser) resolveMagnet(")
	if fn == "" {
		t.Fatal("resolveMagnet not found in kinozal.go")
	}
	if strings.Contains(fn, "http.MethodPost") {
		t.Error("resolveMagnet POSTs again: the params are already in the query string, " +
			"and a POST through the shared browser session can answer with the previous tab")
	}
	if !strings.Contains(fn, "http.MethodGet") {
		t.Error("resolveMagnet no longer names its method explicitly")
	}
}

// The daily cap is an account fact that looks identical on every row, so it is
// named once and then stops the lookups: 842 doomed requests in one measured
// run is 842 requests at the tracker for nothing.
func TestViewLimitIsRecognisedAndStopsAsking(t *testing.T) {
	for _, body := range []string{
		"Увы, вы Зритель. Вам доступно к просмотру всего 100 раздач в сутки.",
		"Увы, вы Опытный Зритель. Вам доступно к просмотру всего 150 раздач в сутки",
	} {
		if !viewLimitRe.MatchString(body) {
			t.Errorf("not recognised as the view limit: %q", body)
		}
	}
	// It must key on the cap, not on a rank name that changes as the account
	// moves up, and it must not fire on an ordinary details page.
	for _, body := range []string{
		"Инфо хеш: 0123456789abcdef0123456789abcdef01234567",
		"Раздача удалена",
		"",
	} {
		if viewLimitRe.MatchString(body) {
			t.Errorf("false positive on %q", body)
		}
	}

	p := &Parser{}
	if p.viewLimitInForce() {
		t.Fatal("the limit is in force before anything happened")
	}
	if !p.noteViewLimit() {
		t.Error("the first sighting should report itself, so the explanation is logged")
	}
	if p.noteViewLimit() {
		t.Error("a second sighting logged the explanation again")
	}
	if !p.viewLimitInForce() {
		t.Error("the window did not open, so the next row would ask again")
	}
	// The window has to expire on its own: the cap is daily, and nothing resets
	// this per run.
	p.quotaUntil = time.Now().Add(-time.Second)
	if p.viewLimitInForce() {
		t.Error("the window did not expire")
	}
	if !p.noteViewLimit() {
		t.Error("after expiry the next sighting should log again")
	}
}

// A CP1251 browse page, a UTF-8 interstitial and a UTF-8 details body all have
// to decode correctly. The old sniff kept a CP1251 decode only when the result
// looked like a browse page, so every other body — including every failure
// message — came out mangled.
func TestBodyCharsetIsSniffedOnTheBytes(t *testing.T) {
	utf8Body := "Увы, вы Зритель. Вам доступно к просмотру всего 100 раздач в сутки."
	if got := decodeKinozalBody([]byte(utf8Body)); got != utf8Body {
		t.Errorf("a UTF-8 body was mangled:\n got %q\nwant %q", got, utf8Body)
	}
	cp1251 := cp1251Bytes(t, "Кинозал.GURU — Инфо хеш")
	if got := decodeKinozalBody(cp1251); !strings.Contains(got, "Кинозал") {
		t.Errorf("a CP1251 body was not decoded: %q", got)
	}
	// And a details body in CP1251, which carries neither browse marker — the
	// case the old sniff got wrong in the other direction.
	cp1251Details := cp1251Bytes(t, "Инфо хеш: ABCDEF")
	if got := decodeKinozalBody(cp1251Details); !strings.Contains(got, "Инфо хеш") {
		t.Errorf("a CP1251 details body was not decoded: %q", got)
	}
}

func cp1251Bytes(t *testing.T, s string) []byte {
	t.Helper()
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r < 128:
			out = append(out, byte(r))
		case r >= 'А' && r <= 'я':
			out = append(out, byte(r-'А'+0xC0))
		case r == '—':
			out = append(out, 0x97)
		default:
			out = append(out, '?')
		}
	}
	return out
}

// funcBody returns the source of the function starting at marker, up to the
// next top-level func. Scanning the one function matters here: the file as a
// whole still mentions http.MethodPost for the login, which is correct.
func funcBody(src, marker string) string {
	start := strings.Index(src, marker)
	if start < 0 {
		return ""
	}
	rest := src[start+len(marker):]
	if end := strings.Index(rest, "\nfunc "); end >= 0 {
		return rest[:end]
	}
	return rest
}

// A run that lost its magnets to the daily cap must not report a bare "ok":
// on /trackers that is indistinguishable from rows that are genuinely broken.
func TestParseReportsTheViewLimitAsAStatus(t *testing.T) {
	src, err := os.ReadFile("kinozal.go")
	if err != nil {
		t.Fatal(err)
	}
	fn := funcBody(string(src), "func (p *Parser) Parse(")
	if fn == "" {
		t.Fatal("Parse not found")
	}
	if !strings.Contains(fn, "viewLimitInForce()") || !strings.Contains(fn, `"rate_limited"`) {
		t.Error("Parse no longer names the view limit in its status, so a capped run " +
			"reports ok with a large failed and nothing says why")
	}
}
