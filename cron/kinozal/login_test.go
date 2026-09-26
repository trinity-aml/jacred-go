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
