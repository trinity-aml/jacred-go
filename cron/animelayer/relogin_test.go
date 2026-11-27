package animelayer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"jacred/app"
	"jacred/core"
)

// The give-up path of the retry loop must report an authorization failure, not
// an empty page.
//
// It used to `return lastCookie, "", nil`. For this tracker that is the
// expensive silent zero: the catalog is public, so a logged-out request still
// returns a full listing and only the .torrent attachment is gated — which is
// how a dead session once produced `parsed=35 added=0 failed=35` with nothing
// naming authorization. The AST guard in server/parserguards_test.go cannot
// see this shape: the invalidate happens inside the loop and the silent return
// is the function's own tail.
func TestTheGiveUpPathIsAnAuthorizationFailure(t *testing.T) {
	core.SetSessionStoreDir(t.TempDir())
	// A page that is unmistakably the catalog (id="wrapper") and unmistakably
	// anonymous: it offers login and registration instead of an account menu.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><body><div id="wrapper">` +
			`<a href="/auth/login/">вход</a><a href="/auth/register/">регистрация</a>` +
			`</div></body></html>`))
	}))
	defer srv.Close()

	cfg := app.DefaultConfig()
	cfg.Animelayer.Host = srv.URL
	cfg.Animelayer.Alias = srv.URL
	cfg.Animelayer.Login.U = "stub-user"
	// A configured cookie short-circuits ensureCookie, so the loop takes its
	// give-up branch on the first pass instead of spending a login.
	cfg.Animelayer.Cookie = "stub=cookie"
	p := New(cfg, nil)

	_, body, err := p.fetchListing(context.Background(), srv.URL+"/torrents/anime/", 1)
	if err == nil {
		t.Fatalf("an anonymous catalog page returned no error (body=%d bytes) — the silent zero is back", len(body))
	}
	if !errors.Is(err, core.ErrNotAuthorized) {
		t.Errorf("error %q does not wrap core.ErrNotAuthorized, so cronErrorStatus cannot report work_login", err)
	}
}
