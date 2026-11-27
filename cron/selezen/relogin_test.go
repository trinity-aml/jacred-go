package selezen

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
// It used to `return lastCookie, "", nil`, and parsePage turned that into
// `parsed == 0`, which ParseAllTask logs as `empty (settling)` and settles as
// swept — so the cycle rotated having never read the page, and the run
// answered ok. The AST guard in server/parserguards_test.go cannot see this
// shape: the invalidate happens inside the loop and the silent return is the
// function's own tail.
func TestTheGiveUpPathIsAnAuthorizationFailure(t *testing.T) {
	core.SetSessionStoreDir(t.TempDir())
	// A catalog page that is unmistakably a catalog (dle_root) and just as
	// unmistakably served to a guest: the configured account name is absent.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><body id="dle_root">a catalog page with no account menu</body></html>`))
	}))
	defer srv.Close()

	cfg := app.DefaultConfig()
	cfg.Selezen.Host = srv.URL
	cfg.Selezen.Login.U = "stub-user"
	// A configured cookie short-circuits ensureCookie, so the loop takes its
	// give-up branch on the first pass instead of spending a login.
	cfg.Selezen.Cookie = "stub=cookie"
	p := New(cfg, nil, t.TempDir())

	_, body, err := p.fetchListing(context.Background(), srv.URL+"/relizy-ot-selezen/", srv.URL, 1)
	if err == nil {
		t.Fatalf("a page served without the account marker returned no error (body=%d bytes) — the silent zero is back", len(body))
	}
	if !errors.Is(err, core.ErrNotAuthorized) {
		t.Errorf("error %q does not wrap core.ErrNotAuthorized, so cronErrorStatus cannot report work_login", err)
	}
}
