package nnmclub

import (
	"bytes"
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

// portal_c10_guest.html is /forum/portal.php?c=10 captured live on
// 2026-11-27 with no cookie at all, so it carries no account identity — a
// no-cookie capture is the only kind that can be committed here.
func loadFixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	return raw
}

// The page is cp1251 on the wire, so the stub below serves the bytes as
// captured and only the marker tests work on the decoded text.
func loadFixture(t *testing.T, name string) string {
	t.Helper()
	return core.DecodeCP1251(loadFixtureBytes(t, name))
}

// The authorized side is derived rather than captured, by the one substitution
// the measured marker table licenses: the sidebar login form is replaced with
// the logout link the real authorized page carries. Measured on the live
// cat=10 portal within one minute on 2026-11-27 —
//
//	marker                        guest   authorized
//	login.php?logout=                 0            1
//	action="login.php"                1            0
//	NNM-Club</title>                  1            1
//	magnet:?xt                       25           25
//
// — so this derivation reproduces the authorized page in exactly the two
// respects loggedOut reads, and in no others.
func deriveAuthorized(t *testing.T, guest string) string {
	t.Helper()
	authed := strings.Replace(guest, `action="login.php"`, `href="login.php?logout=1"`, 1)
	if authed == guest {
		t.Fatal("the guest fixture no longer carries the login form this derivation depends on")
	}
	return authed
}

func deriveAuthorizedBytes(t *testing.T, guest []byte) []byte {
	t.Helper()
	authed := bytes.Replace(guest, []byte(`action="login.php"`), []byte(`href="login.php?logout=1"`), 1)
	if bytes.Equal(authed, guest) {
		t.Fatal("the guest fixture no longer carries the login form this derivation depends on")
	}
	return authed
}

func parserWithLogin(user string) *Parser {
	cfg := app.DefaultConfig()
	cfg.NNMClub.Login.U = user
	return &Parser{Config: cfg}
}

// The check was dead, and this is why: nnmclub serves its portal to guests in
// full, so a guest page carries validMarker and never reached the
// `action="login.php"` test that sat inside the !validMarker branch. That is
// the "could not fire even once" shape already paid for on animelayer,
// mazepa, anifilm and torrentby.
func TestTheGuestPortalPassesTheMarkerTheOldCheckSatBehind(t *testing.T) {
	guest := loadFixture(t, "portal_c10_guest.html")
	if !strings.Contains(guest, validMarker) {
		t.Fatalf("the guest page no longer carries %q — the fixture no longer captures why the old check was unreachable", validMarker)
	}
	if n := len(parsePageHTML("https://nnmclub.to", "10", guest, time.Now().UTC())); n == 0 {
		t.Fatal("the guest page parses no records — the fixture no longer represents a public portal")
	}
}

// Two-sided, in the direction that matters in both: a guest must be detected,
// and a real session must never be flagged as one. A one-sided check fails
// badly either way — a renamed logout path alone would put every page into a
// re-login loop.
func TestTheGuestCheckIsTwoSided(t *testing.T) {
	guest := loadFixture(t, "portal_c10_guest.html")
	authed := deriveAuthorized(t, guest)
	p := parserWithLogin("stub-user")

	if !p.loggedOut(guest) {
		t.Error("the guest portal is not detected as logged out")
	}
	if p.loggedOut(authed) {
		t.Error("an authorized portal is flagged as a guest — every page would enter a re-login loop")
	}
	// With no login configured, guest mode is the intended mode and the check
	// must stay quiet.
	if parserWithLogin("").loggedOut(guest) {
		t.Error("the check fires with no login configured")
	}

	// The case two-sidedness actually buys, and the reason the guest/authorized
	// pair above cannot demonstrate it: on the measured pages the two markers
	// never coexist, so a one-sided check answers identically on both. The
	// hazard is a page that carries the logout affordance *and* a login form —
	// a sidebar, a footer, a modal — which a bare login-form check reads as a
	// guest page and sends into a re-login loop.
	both := `<a href="login.php?logout=1">exit</a><form action="login.php">`
	if p.loggedOut(both) {
		t.Error("a page carrying both markers is read as a guest page — the check is one-sided")
	}
	// And the mirror: neither marker present is not evidence of a guest.
	if p.loggedOut(`<html><body>no account markers at all</body></html>`) {
		t.Error("a page with neither marker is read as a guest page")
	}
}

type portalStub struct {
	mu          sync.Mutex
	guest       []byte
	authed      []byte
	loginWorks  bool
	alwaysGuest bool
	logins      int
	pages       int
	authOK      bool
}

func (s *portalStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if strings.Contains(r.URL.Path, "login.php") {
			s.logins++
			if s.loginWorks {
				s.authOK = true
				w.Header().Add("Set-Cookie", "phpbb2mysql_4_sid=stub; path=/")
				w.Header().Add("Set-Cookie", "phpbb2mysql_4_data=stub; path=/")
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		s.pages++
		body := s.guest
		if s.authOK && !s.alwaysGuest {
			body = s.authed
		}
		_, _ = w.Write(body)
	})
}

func newStubParser(t *testing.T, stub *portalStub) *Parser {
	t.Helper()
	core.SetSessionStoreDir(t.TempDir())
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	cfg := app.DefaultConfig()
	cfg.NNMClub.Host = srv.URL
	cfg.NNMClub.Alias = ""
	cfg.NNMClub.Login.U = "stub-user"
	cfg.NNMClub.Login.P = "stub-pass"
	cfg.NNMClub.ParseDelay = 0
	return New(cfg, nil, t.TempDir())
}

// A guest page must not be discarded. The measured table above is why: it
// holds the same 25 records with the same magnets as an authorized page, so
// returning (nil, nil) for it lost a whole page — and parsealltask then read
// the empty result as "this page holds nothing" and settled it as swept.
func TestAGuestPageIsParsedNotDiscarded(t *testing.T) {
	guest := loadFixture(t, "portal_c10_guest.html")
	want := len(parsePageHTML("https://nnmclub.to", "10", guest, time.Now().UTC()))
	raw := loadFixtureBytes(t, "portal_c10_guest.html")
	stub := &portalStub{guest: raw, authed: deriveAuthorizedBytes(t, raw), alwaysGuest: true}
	p := newStubParser(t, stub)

	// No login is attempted here, deliberately: nnmclub authenticates through
	// the browser (SolveForLogin), and a unit test that lets it try launches a
	// real Chrome — a first version of this file sat for 66 seconds doing
	// exactly that. Arming the gate's failure window makes ensureLogin answer
	// with its hold-off error instead, which is the branch that decides
	// whether the guest page is kept.
	armGateFailure(p)

	items, err := p.parsePage(context.Background(), "10", 0)
	if err != nil {
		t.Fatalf("parsePage: %v", err)
	}
	if len(items) != want {
		t.Errorf("parsed %d records from a guest page, want %d — the page was discarded", len(items), want)
	}
	if logins, _ := stub.counts(); logins != 0 {
		t.Errorf("the test attempted %d logins — it is no longer isolated from the browser", logins)
	}
}

// armGateFailure puts the parser's login gate into its post-refusal cooldown,
// so ensureLogin returns without touching the network.
func armGateFailure(p *Parser) {
	_ = p.gate.Note(errors.New("refused, for the test"))
}

// The recovery must be gated by the window, not attempted per page. The storm
// case is a login that *succeeds* while the page still comes back as a guest
// page: nothing fails, so the gate's failure cooldown never arms and only its
// recovery window bounds the attempts. nnmclub's sweep is hundreds of pages.
func TestTheRecoveryIsGatedByTheWindow(t *testing.T) {
	raw := loadFixtureBytes(t, "portal_c10_guest.html")
	stub := &portalStub{guest: raw, authed: deriveAuthorizedBytes(t, raw), alwaysGuest: true}
	p := newStubParser(t, stub)

	// Spend the window, so every page below must find it closed and fall
	// straight through to parsing the guest body — again without a login,
	// which is what keeps this test out of a real browser.
	if !p.gate.BeginRecovery() {
		t.Fatal("a fresh gate refused the first recovery")
	}
	for i := 0; i < 6; i++ {
		items, err := p.parsePage(context.Background(), "10", i)
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		if len(items) == 0 {
			t.Fatalf("page %d returned no records", i)
		}
	}
	logins, pages := stub.counts()
	if logins != 0 {
		t.Errorf("logins = %d while the recovery window was closed, want 0", logins)
	}
	if pages != 6 {
		t.Errorf("page fetches = %d, want 6 — a refetch happened inside the closed window", pages)
	}
}

func (s *portalStub) counts() (logins, pages int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logins, s.pages
}

// A body that is not an nnmclub page must be named, not reported as an empty
// page. nnmclub is CF-gated, so an interstitial is a realistic body here, and
// the caller would otherwise settle the page as swept.
func TestANonNNMClubBodyIsAnError(t *testing.T) {
	stub := &portalStub{guest: []byte("<html><body>not an nnmclub page at all</body></html>")}
	p := newStubParser(t, stub)
	armGateFailure(p)

	items, err := p.parsePage(context.Background(), "10", 0)
	if err == nil {
		t.Fatalf("a body that is not an nnmclub page returned no error (items=%d)", len(items))
	}
	if !strings.Contains(err.Error(), "not an nnmclub page") {
		t.Errorf("error %q does not name the cause", err)
	}
}

// The login result must not be dropped on the floor. ensureLogin used to be
// `_ = p.takeLogin(ctx)` returning nothing, and takeLogin's own cooldown
// branch answered nil, so "we tried two minutes ago" and "we are logged in"
// were one answer.
func TestEveryEntrypointActsOnTheLoginError(t *testing.T) {
	s := sourceWithoutComments(t, "nnmclub.go")
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

// Any scan of the source strips comments first: the explanation beside each of
// these calls quotes the very code it forbids, so a scan over the raw file
// fails against correct code — and a comment left behind could satisfy a guard
// whose call had been deleted. Both mistakes are on record in this tree.
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
