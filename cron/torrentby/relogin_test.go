package torrentby

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"jacred/app"
	"jacred/core"
)

// torrentbyStub is the tracker as the page path meets it: a catalog served in
// full to guests and members alike, plus a login form that either hands out
// the uid/pass pair or does not.
type torrentbyStub struct {
	mu          sync.Mutex
	guest       string
	authed      string
	loginWorks  bool
	alwaysGuest bool

	logins int
	pages  int
	authOK bool
}

func (s *torrentbyStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/login") {
			s.logins++
			if s.loginWorks {
				s.authOK = true
				w.Header().Add("Set-Cookie", "uid=42; path=/")
				w.Header().Add("Set-Cookie", "pass=stub; path=/")
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		s.pages++
		body := s.guest
		if s.authOK && !s.alwaysGuest {
			body = s.authed
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	})
}

func (s *torrentbyStub) counts() (logins, pages int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logins, s.pages
}

func newStubParser(t *testing.T, stub *torrentbyStub) *Parser {
	t.Helper()
	// SaveAuth writes a live session cookie to disk; keep it out of the repo.
	core.SetSessionStoreDir(t.TempDir())
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	cfg := app.DefaultConfig()
	cfg.TorrentBy.Host = srv.URL
	cfg.TorrentBy.Alias = ""
	cfg.TorrentBy.Login.U = "stub-user"
	cfg.TorrentBy.Login.P = "stub-pass"
	cfg.TorrentBy.ParseDelay = 0
	return New(cfg, nil, t.TempDir())
}

func guestAndAuthed(t *testing.T) (guest, authed string) {
	t.Helper()
	guest = loadFixture(t, "films_guest.html")
	// The only difference a session makes to this page is the account menu;
	// the catalog itself is identical, which is the premise of the whole fix.
	authed = strings.Replace(guest, "/login/", "/logout/", 1)
	if !parserWithLogin("u").loggedOut(guest) || parserWithLogin("u").loggedOut(authed) {
		t.Fatal("the derived pair no longer reads as guest/authorized")
	}
	return guest, authed
}

// The defect, the same shape that cost mazepa 445 pages: a guest page used to
// return (nil, nil), so parsealltask read it as "this page holds nothing" and
// settled it as swept. Here that is worse than a silent zero — torrentby
// publishes its whole catalog, magnets included, to logged-out visitors, so
// the discarded page was a complete one.
func TestAGuestPageIsParsedNotDiscarded(t *testing.T) {
	guest, authed := guestAndAuthed(t)
	want := len(parsePageHTML("https://torrent.by", "films", guest, time.Now().UTC()))
	if want == 0 {
		t.Fatal("the fixture parses no rows — it no longer represents the failure")
	}

	stub := &torrentbyStub{guest: guest, authed: authed, loginWorks: false, alwaysGuest: true}
	p := newStubParser(t, stub)

	items, err := p.parsePage(context.Background(), "films", 1)
	if err != nil {
		t.Fatalf("parsePage: %v", err)
	}
	if len(items) != want {
		t.Errorf("parsed %d records from a guest page, want %d — the page was discarded", len(items), want)
	}
}

// With a usable login the page path must restore the authorized view, and
// spend exactly one login doing it.
func TestAGuestPageTriggersOneReAuthentication(t *testing.T) {
	guest, authed := guestAndAuthed(t)
	stub := &torrentbyStub{guest: guest, authed: authed, loginWorks: true}
	p := newStubParser(t, stub)

	items, err := p.parsePage(context.Background(), "films", 1)
	if err != nil {
		t.Fatalf("parsePage: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("no records after the recovery")
	}
	logins, pages := stub.counts()
	if logins != 1 {
		t.Errorf("logins = %d, want exactly 1", logins)
	}
	if pages != 2 {
		t.Errorf("page fetches = %d, want 2 (the guest page, then the retry)", pages)
	}
	if p.cookie() == "" {
		t.Error("no session cookie after the recovery")
	}
}

// The storm case is a login that *succeeds* while the page still comes back as
// a guest page: nothing fails, so the login-failure cooldown never arms. Only
// the guest-recovery window bounds it.
func TestGuestRecoveryIsRateLimited(t *testing.T) {
	guest, authed := guestAndAuthed(t)
	stub := &torrentbyStub{guest: guest, authed: authed, loginWorks: true, alwaysGuest: true}
	p := newStubParser(t, stub)

	const pages = 8
	for i := 0; i < pages; i++ {
		items, err := p.parsePage(context.Background(), "films", i)
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		if len(items) == 0 {
			t.Fatalf("page %d returned no records", i)
		}
	}
	if logins, _ := stub.counts(); logins != 1 {
		t.Errorf("logins = %d over %d guest pages, want 1 — a succeeding login is not bounded by lastLoginFail", logins, pages)
	}
}

// CLAUDE.md's rule, which this parser used to break: the cooldown branch must
// not return nil. "We tried two minutes ago and were turned away" and "we are
// logged in" are different answers, and the caller acted on the second.
func TestTheLoginCooldownBranchReturnsAnError(t *testing.T) {
	guest, authed := guestAndAuthed(t)
	stub := &torrentbyStub{guest: guest, authed: authed, loginWorks: false}
	p := newStubParser(t, stub)

	first := p.ensureLogin(context.Background())
	if first == nil || !errors.Is(first, core.ErrNotAuthorized) {
		t.Fatalf("a refused login returned %v, want an error wrapping core.ErrNotAuthorized", first)
	}
	second := p.ensureLogin(context.Background())
	if second == nil {
		t.Fatal("the cooldown branch returned nil — indistinguishable from being logged in")
	}
	if !errors.Is(second, core.ErrNotAuthorized) {
		t.Errorf("cooldown error %q does not wrap core.ErrNotAuthorized", second)
	}
	if logins, _ := stub.counts(); logins != 1 {
		t.Errorf("logins = %d, want 1 — the cooldown is not holding", logins)
	}

	// A login that succeeds must leave no cooldown behind.
	stub.mu.Lock()
	stub.loginWorks = true
	stub.mu.Unlock()
	p.cookieMu.Lock()
	p.lastLoginFail = time.Time{}
	p.cookieMu.Unlock()
	if err := p.ensureLogin(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
	p.invalidateCookie()
	if err := p.ensureLogin(context.Background()); err != nil {
		t.Errorf("a login right after a successful one was held off: %v", err)
	}
}

// The login result must not be dropped on the floor. It used to be, twice
// over: ensureLogin did `_ = p.takeLogin(ctx)` and returned nothing, so all
// four entrypoints ran on regardless and no cause was ever reported.
func TestEveryEntrypointActsOnTheLoginError(t *testing.T) {
	s := sourceWithoutComments(t, "torrentby.go")
	if strings.Contains(s, "_ = p.takeLogin(") {
		t.Error("takeLogin's error is discarded again")
	}
	for _, decl := range []string{
		"func (p *Parser) Parse(",
		"func (p *Parser) UpdateTasksParse(",
		"func (p *Parser) ParseAllTask(",
		"func (p *Parser) ParseLatest(",
	} {
		i := strings.Index(s, decl)
		if i < 0 {
			t.Fatalf("%s not found", decl)
		}
		body := s[i+len(decl):]
		if j := strings.Index(body, "\nfunc "); j >= 0 {
			body = body[:j]
		}
		if !strings.Contains(body, "if err := p.ensureLogin(ctx); err != nil {") {
			t.Errorf("%s does not read ensureLogin's error", decl)
		}
	}
}

// sourceWithoutComments is what makes the guard above mean anything: the
// explanation beside each of these calls quotes the very code it forbids, so a
// scan over the raw file fails against correct code — and, the other way
// round, a comment left in place could satisfy a guard whose call had been
// deleted. That mistake is on record in this tree.
func sourceWithoutComments(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var out strings.Builder
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}
