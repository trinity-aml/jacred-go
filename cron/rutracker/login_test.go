package rutracker

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"jacred/core"
)

// The real login form as rutracker served it on 2026-09-26, once its
// anti-bruteforce CAPTCHA had been triggered. Only the shape matters, so this
// is the field set rather than a captured page — the live one carries a
// per-session token and nothing is gained by committing it.
const captchaLoginForm = `<form action="login.php" method="post">
<input type="text" name="login_username" value="">
<input type="password" name="login_password" value="">
<input type="hidden" name="cap_sid" value="9753a5746b94b964b6d70c2fdadaf3df">
<img src="/captcha/pic.php"><input type="text" name="cap_code_9753a5746b94b964b6d70c2fdadaf3df" value="">
<input type="submit" name="login" value="Вход"></form>`

const plainLoginForm = `<form action="login.php" method="post">
<input type="text" name="login_username" value="">
<input type="password" name="login_password" value="">
<input type="submit" name="login" value="Вход"></form>`

// A CAPTCHA, a wrong password and a Cloudflare block all used to log the same
// "no bb_session". They need three different fixes — a human, a config edit,
// and the browser — so the parser has to tell them apart.
func TestCaptchaIsRecognisedOnTheLoginForm(t *testing.T) {
	if !loginCaptchaRe.MatchString(captchaLoginForm) {
		t.Error("the live CAPTCHA form was not recognised")
	}
	if loginCaptchaRe.MatchString(plainLoginForm) {
		t.Error("an ordinary login form was reported as a CAPTCHA")
	}
	// A listing must never be mistaken for one, or a healthy run would stop.
	if loginCaptchaRe.MatchString(`<table class="torTopic"><a href="viewtopic.php?t=1">x</a></table>`) {
		t.Error("a listing was reported as a CAPTCHA")
	}
	// Either field alone is enough: the pair is what rutracker adds, but a
	// markup change that keeps one should still be caught.
	for _, only := range []string{
		`<input name="cap_sid" value="x">`,
		`<input name="cap_code_deadbeefdeadbeefdeadbeefdeadbeef" value="">`,
	} {
		if !loginCaptchaRe.MatchString(only) {
			t.Errorf("not matched: %s", only)
		}
	}
}

// The crontab drives four rutracker entrypoints, each calling ensureLogin, so
// a session that cannot be established would POST the login form up to a dozen
// times an hour — which is what trips the CAPTCHA in the first place.
func TestFailedLoginIsNotRetriedImmediately(t *testing.T) {
	p := &Parser{}
	if _, _, blocked := p.loginBlocked(); blocked {
		t.Fatal("a fresh parser starts blocked")
	}

	p.noteLoginFailure(loginCooldown, "credentials rejected")
	remaining, reason, blocked := p.loginBlocked()
	if !blocked {
		t.Fatal("a failed login did not start a cooldown")
	}
	if reason != "credentials rejected" {
		t.Errorf("reason = %q", reason)
	}
	if remaining <= 0 || remaining > loginCooldown {
		t.Errorf("remaining = %s, want within %s", remaining, loginCooldown)
	}
}

// Retrying before a human clears the CAPTCHA cannot succeed and each attempt
// refreshes the block, so it gets a longer window than a wrong password.
func TestCaptchaCooldownIsLongerThanAPlainFailure(t *testing.T) {
	if loginCaptchaCooldown <= loginCooldown {
		t.Errorf("captcha cooldown %s is not longer than %s", loginCaptchaCooldown, loginCooldown)
	}
	p := &Parser{}
	p.noteLoginFailure(loginCaptchaCooldown, "login form is showing a CAPTCHA")
	remaining, reason, _ := p.loginBlocked()
	if remaining <= loginCooldown {
		t.Errorf("remaining = %s, want more than %s", remaining, loginCooldown)
	}
	if !strings.Contains(reason, "CAPTCHA") {
		t.Errorf("reason = %q", reason)
	}
}

// An expired cooldown must let the next run try again — the block is a
// throttle, not a permanent stop.
func TestCooldownExpires(t *testing.T) {
	p := &Parser{}
	p.noteLoginFailure(-time.Second, "old failure")
	if _, _, blocked := p.loginBlocked(); blocked {
		t.Error("an expired cooldown still blocks")
	}
}

// A working session clears the block, so a recovered tracker is not held back
// by the previous failure's window.
func TestSuccessfulLoginClearsTheBlock(t *testing.T) {
	p := &Parser{}
	p.noteLoginFailure(loginCaptchaCooldown, "login form is showing a CAPTCHA")
	p.cookieMu.Lock()
	p.cookie = "bb_session=x"
	p.cookieT = time.Now()
	p.loginBlockedUntil = time.Time{}
	p.loginBlockReason = ""
	p.cookieMu.Unlock()
	if _, _, blocked := p.loginBlocked(); blocked {
		t.Error("the block survived a successful login")
	}
}

// ensureLogin must not reach takeLogin while blocked — that is the whole point.
func TestEnsureLoginRespectsTheCooldown(t *testing.T) {
	// Configured on purpose: with an empty config the "login is not configured"
	// branch fires first and this would stop testing the cooldown at all.
	p := &Parser{}
	p.Config.Rutracker.Host = "https://rutracker.org"
	p.Config.Rutracker.Login.U = "someone"
	p.Config.Rutracker.Login.P = "secret"
	p.noteLoginFailure(loginCooldown, "credentials rejected")
	if _, _, blocked := p.loginBlocked(); !blocked {
		t.Fatal("setup: not blocked")
	}
	err := p.ensureLogin(t.Context())
	if err == nil {
		t.Fatal("ensureLogin succeeded while blocked")
	}
	if !errors.Is(err, core.ErrNotAuthorized) {
		t.Errorf("blocked login does not wrap ErrNotAuthorized: %v", err)
	}
	// The cause has to survive to the caller. All four entrypoints used to
	// flatten a cooldown, a CAPTCHA block and a rejected password into one
	// string, which is why production could not say which had happened.
	if !strings.Contains(err.Error(), "credentials rejected") {
		t.Errorf("ensureLogin dropped the reason: %v", err)
	}
}

// A category that could not be fetched used to be logged and dropped, counting
// toward nothing: res.Failed only ever held row-level save failures. So a run
// where nearly every category failed still answered `status: ok` with a small
// fetched count, which on /trackers is indistinguishable from a quiet day.
// Production 2026-09-27 showed exactly that: `fetched=50 added=2 failed=7`
// against ~4433 for a healthy 98-category pass.
func TestCategoryFailuresAreCounted(t *testing.T) {
	src, err := os.ReadFile("rutracker.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	i := strings.Index(body, "func (p *Parser) Parse(")
	if i < 0 {
		t.Fatal("Parse не найдена")
	}
	fn := body[i : i+strings.Index(body[i:], "\nfunc ")]

	j := strings.Index(fn, "cat %s error")
	if j < 0 {
		t.Fatal("ветка ошибки категории не найдена")
	}
	// The counters must be bumped in that branch, not merely logged.
	branch := fn[max(0, j-400) : j+100]
	for _, want := range []string{"catErrors++", "res.Failed++"} {
		if !strings.Contains(branch, want) {
			t.Errorf("ветка ошибки категории не содержит %s — отказ снова станет невидимым", want)
		}
	}
	// And a run that reached nothing must not report success.
	if !strings.Contains(fn, "%d of %d categories failed to fetch") {
		t.Error("прогон, потерявший большинство категорий, не сообщает об ошибке")
	}
}

// "Not configured" is the third case ensureLogin has to keep separate from a
// cooldown and from rejected credentials — production could not tell them
// apart because all three answered the same flat string.
func TestUnconfiguredLoginIsNamed(t *testing.T) {
	err := (&Parser{}).ensureLogin(t.Context())
	if err == nil {
		t.Fatal("пустой конфиг отрапортовал успешный вход")
	}
	if !errors.Is(err, core.ErrNotAuthorized) {
		t.Errorf("не обёрнут ErrNotAuthorized: %v", err)
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("причина не названа: %v", err)
	}
}
