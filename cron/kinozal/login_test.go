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
