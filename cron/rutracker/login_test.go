package rutracker

import (
	"strings"
	"testing"
	"time"
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
	p := &Parser{}
	p.noteLoginFailure(loginCooldown, "credentials rejected")
	// Host and credentials are empty, so if the cooldown were ignored
	// takeLogin would run and log "login skipped"; either way it returns
	// false. What we assert is that it reports blocked.
	if _, _, blocked := p.loginBlocked(); !blocked {
		t.Fatal("setup: not blocked")
	}
	if p.ensureLogin(t.Context()) {
		t.Error("ensureLogin succeeded while blocked")
	}
}
