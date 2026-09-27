package lostfilm

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"jacred/app"
	"jacred/core"
)

// The check is deliberately stricter than "is the value non-empty", which is
// what the C# original tests. The deployed init.yaml here carried a short note
// in `cookie` with no '=' in it: non-empty, sent as a Cookie header, useless —
// and the parser ran anonymously for as long as nobody looked.
func TestHasAuthCookie(t *testing.T) {
	cases := []struct {
		name   string
		cookie string
		want   bool
	}{
		{"real session", "lf_session=abc; lf_udv=1; PHPSESSID=xyz", true},
		{"single pair", "PHPSESSID=abc", true},
		{"padded", "  lf_session=abc  ", true},
		{"empty", "", false},
		{"whitespace", "   ", false},
		{"prose, no '='", "надо обновить", false},
		{"name with no value", "lf_session=", false},
		{"value with no name", "=abc", false},
		{"separators only", " ; ; ", false},
	}
	for _, c := range cases {
		if got := hasAuthCookie(c.cookie); got != c.want {
			t.Errorf("%s: hasAuthCookie = %v, want %v", c.name, got, c.want)
		}
	}
}

// lostfilm serves its listings to anyone and gates only the magnets, so an
// unauthenticated run parses every page and then fails per episode — reported
// as failed=N/withoutMag=N with nothing naming the cause. The gate turns that
// into one named failure before the first fetch.
func TestAuthorizeRefusesAnUnusableCookie(t *testing.T) {
	cfg := app.DefaultConfig()

	for _, bad := range []string{"", "   ", "надо обновить"} {
		cfg.Lostfilm.Cookie = bad
		err := (&Parser{Config: cfg}).authorize(t.Context())
		if err == nil {
			t.Errorf("cookie %q accepted", bad)
			continue
		}
		// The shared contract: handlers derive HTTP 500 + work_login from this.
		if !errors.Is(err, core.ErrNotAuthorized) {
			t.Errorf("cookie %q: error does not wrap core.ErrNotAuthorized: %v", bad, err)
		}
		if !strings.Contains(err.Error(), "lostfilm") {
			t.Errorf("cookie %q: error does not name the tracker: %v", bad, err)
		}
	}

	// A cookie of the right *shape* is no longer enough — that is the change.
	// An expired one used to sail through here and then produce thousands of
	// anonymous failed++/noMagnet++ rows instead of one named refusal.
	cfg.Lostfilm.Cookie = "lf_session=abc; PHPSESSID=xyz"

	serve := func(body string) *Parser {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(srv.Close)
		c := cfg
		c.Lostfilm.Host = srv.URL
		return &Parser{Config: c, Fetcher: core.NewFetcher(c)}
	}

	if err := serve(guestUserPane).authorize(t.Context()); err == nil {
		t.Error("гостевая панель принята за рабочую сессию")
	} else if !errors.Is(err, core.ErrNotAuthorized) {
		t.Errorf("отказ не оборачивает ErrNotAuthorized: %v", err)
	}

	authorized := strings.NewReplacer(
		`<a href="/login" class="link">Вход</a>`, `<a href="/my" class="link">аккаунт</a>`,
		`<a href="/reg" class="link gray-color">Регистрация</a>`, `<a href="/logout" class="link">Выход</a>`,
	).Replace(guestUserPane)
	if err := serve(authorized).authorize(t.Context()); err != nil {
		t.Errorf("рабочая сессия отвергнута: %v", err)
	}
}

// A refused session looks identical on every episode, so the diagnosis is worth
// one line per run, not one per row — and it has to come back on the next run.
func TestVPageRejectionIsReportedOncePerRun(t *testing.T) {
	p := &Parser{}
	p.noteVPageWithoutMagnets("https://example/v1")
	if !p.vPageReported {
		t.Fatal("first rejection was not recorded")
	}
	p.noteVPageWithoutMagnets("https://example/v2")
	if !p.vPageReported {
		t.Error("flag cleared by a second call")
	}
	// Each run resets it; parseRange and ParseSeasonPacks do this after taking
	// the lock, so a later run reports again instead of staying quiet forever.
	p.vPageReported = false
	p.noteVPageWithoutMagnets("https://example/v3")
	if !p.vPageReported {
		t.Error("a new run does not report again")
	}
}

// The account pane as lostfilm served it to a guest on 2026-09-27. Inline
// rather than a 70 KB fixture: this snippet is the whole contract.
const guestUserPane = `<div id="main-rightt-side">
<div class="user-pane">
	<a href="/login" class="link">Вход</a>
	<div class="divider">|</div>
	<a href="/reg" class="link gray-color">Регистрация</a>
	<div class="tail"></div>
</div>`

func TestLooksLoggedOut(t *testing.T) {
	if !looksLoggedOut(guestUserPane) {
		t.Error("гостевая панель аккаунта не распознана")
	}
	// An authorized page offers neither. Derived, because there is no working
	// lostfilm cookie on this box to capture a real one with.
	authorized := strings.NewReplacer(
		`<a href="/login" class="link">Вход</a>`, `<a href="/my" class="link">trinity1980</a>`,
		`<a href="/reg" class="link gray-color">Регистрация</a>`, `<a href="/logout" class="link">Выход</a>`,
	).Replace(guestUserPane)
	if looksLoggedOut(authorized) {
		t.Error("авторизованная панель принята за гостевую — трекер встал бы на ровном месте")
	}
	// No pane at all (an error page, a partial body) is not evidence of a guest.
	if looksLoggedOut(`<html><body>что-то другое</body></html>`) {
		t.Error("тело без панели аккаунта принято за гостевое")
	}
	// The pane markers must be read from the pane, not from anywhere on the page:
	// a footer link to /login elsewhere must not condemn a live session.
	far := authorized + strings.Repeat("x", 2000) + `<a href="/login">Вход</a><a href="/reg">Регистрация</a>`
	if looksLoggedOut(far) {
		t.Error("ссылка на вход вне панели принята за признак гостя")
	}
}
