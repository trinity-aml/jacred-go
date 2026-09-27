package toloka

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"jacred/core"
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
	// Must be >1: a single login page is not evidence the session died — one
	// succeeded page and one refused page eight seconds apart, on the same sid,
	// were measured in production 2026-09-27.
	if sessionRefusalsBeforeRelogin < 2 {
		t.Errorf("порог %d сбросит живую сессию на первом же случайном отказе", sessionRefusalsBeforeRelogin)
	}
	// And must stay small: past the threshold every page is another credentials
	// POST, which is how a tracker's anti-bruteforce gets tripped.
	if sessionRefusalsBeforeRelogin > 5 {
		t.Errorf("порог %d слишком велик: мёртвая сессия будет замечена слишком поздно", sessionRefusalsBeforeRelogin)
	}
}

// A refusal that is *not* yet conclusive must keep the session and report an
// ordinary page error, so the sweep skips one page instead of stalling for the
// login cooldown. Only a run of them is treated as authorization.
func TestIsolatedRefusalKeepsTheSession(t *testing.T) {
	src, err := os.ReadFile("toloka.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	i := strings.Index(body, "func (p *Parser) parsePage(")
	fn := body[i : i+strings.Index(body[i:], "\nfunc ")]

	below := fn[strings.Index(fn, "if n < sessionRefusalsBeforeRelogin"):]
	below = below[:strings.Index(below, "\n\t\t}")]
	if strings.Contains(below, "invalidateCookie") {
		t.Error("одиночный отказ снова сбрасывает сессию")
	}
	if strings.Contains(below, "ErrNotAuthorized") {
		t.Error("одиночный отказ помечен как ошибка авторизации — проход остановится на ровном месте")
	}
	// The counter only means "consecutive" if a good page clears it.
	if !strings.Contains(fn, "p.sessionRejects = 0") {
		t.Error("успешная страница не обнуляет счётчик — «подряд» перестанет значить подряд")
	}
}

// The login storm came from invalidateCookie zeroing the cooldown: every
// refused page bought a fresh credentials POST, so production logged in twice
// in fifteen seconds and never stopped. Dropping the session must not also drop
// the rate limit — a genuinely expired session still recovers on the next cron
// pass, which is minutes away, not seconds.
func TestInvalidateKeepsTheLoginCooldown(t *testing.T) {
	core.SetSessionStoreDir(t.TempDir())
	p := &Parser{domain: "toloka.to"}
	p.cookie = "toloka_sid=x; toloka_ssl=1; toloka_data=y;"
	p.lastLoginAttempt = time.Now()

	p.invalidateCookie()

	if p.cookie != "" {
		t.Error("сессия не сброшена")
	}
	if p.lastLoginAttempt.IsZero() {
		t.Fatal("кулдаун обнулён — это и есть шторм логинов")
	}
	_, err := p.ensureCookie(context.Background())
	if err == nil {
		t.Fatal("после сброса сессии вход пошёл сразу, минуя кулдаун")
	}
	if !errors.Is(err, core.ErrNotAuthorized) {
		t.Errorf("ошибка кулдауна не обёрнута в ErrNotAuthorized: %v", err)
	}
}

// Both halves of the login storm were the same one-line shape: zeroing the
// attempt time, once in invalidateCookie ("allow immediate re-login") and once
// in ensureCookie ("clear cooldown on success"). Either one alone disables the
// rate limit on credentials POSTs, so the shape itself is what is pinned.
func TestAttemptTimeIsNeverZeroed(t *testing.T) {
	src, err := os.ReadFile("toloka.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "lastLoginAttempt = time.Time{}") {
		t.Error("отметка о попытке входа снова обнуляется — кулдаун перестаёт ограничивать логины")
	}
}

// Parse and ParseAllTask share one flag. With a guard each they swept
// concurrently — the deployed schedule fires parsealltask every 5 minutes and
// parse every 40, while a full sweep runs for hours — so two runs drove the
// same session and the same download.php quota at once.
func TestParseAndParseAllTaskShareOneFlag(t *testing.T) {
	p := &Parser{}

	if !p.acquire("parse") {
		t.Fatal("первый захват флага не удался")
	}
	if p.acquire("parsealltask") {
		t.Error("parsealltask захватил флаг, пока идёт parse — прогоны снова параллельны")
	}
	p.release()
	if !p.acquire("parsealltask") {
		t.Error("после release флаг не отдан")
	}
	p.release()

	// The other direction, so neither op is privileged.
	if !p.acquire("parsealltask") {
		t.Fatal("захват parsealltask не удался")
	}
	if p.acquire("parse") {
		t.Error("parse захватил флаг, пока идёт parsealltask")
	}
	p.release()
}

// The entrypoints must go through the shared flag, not keep private ones.
func TestEntrypointsUseTheSharedFlag(t *testing.T) {
	src, err := os.ReadFile("toloka.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if strings.Contains(body, "p.allWork") || strings.Contains(body, "p.working") {
		t.Error("вернулся отдельный флаг для одного из прогонов")
	}
	for _, entry := range []string{"func (p *Parser) Parse(", "func (p *Parser) ParseAllTask("} {
		i := strings.Index(body, entry)
		if i < 0 {
			t.Fatalf("точка входа не найдена: %s", entry)
		}
		e := strings.Index(body[i:], "\nfunc ")
		if !strings.Contains(body[i:i+e], "p.acquire(") {
			t.Errorf("%s не берёт общий флаг", entry)
		}
	}
}

// An authorization failure must stop the sweep, not be counted as one bad page.
// The refusal budget in parsePage cannot bound a loop that swallows its errors:
// production 2026-09-27 logged `login on cooldown` once per page from page 194
// to 232 and reported `session refused, 4/2` — twice the budget.
func TestSweepStopsOnAnAuthorizationError(t *testing.T) {
	src, err := os.ReadFile("toloka.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	for _, entry := range []string{"func (p *Parser) ParseAllTask(", "func (p *Parser) ParseLatest("} {
		i := strings.Index(body, entry)
		if i < 0 {
			t.Fatalf("точка входа не найдена: %s", entry)
		}
		fn := body[i : i+strings.Index(body[i:], "\nfunc ")]
		if !strings.Contains(fn, "errors.Is(err, core.ErrNotAuthorized)") {
			t.Errorf("%s не прерывает проход на ошибке авторизации — цикл снова будет сыпать ошибку на каждой странице", entry)
		}
	}
}
