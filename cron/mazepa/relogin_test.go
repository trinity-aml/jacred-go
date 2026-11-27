package mazepa

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

// mazepaStub is the tracker as the re-login path meets it: a forum that serves
// its listing to guests as well as members (which is why the expired-cookie
// check has to read the account menu), plus a login form that either hands out
// a bb_ cookie or does not.
type mazepaStub struct {
	mu          sync.Mutex
	guest       string
	authed      string
	loginWorks  bool
	alwaysGuest bool

	logins int
	pages  int
	authOK bool
}

func (m *mazepaStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if r.URL.Path == "/login.php" {
			m.logins++
			if m.loginWorks {
				m.authOK = true
				w.Header().Add("Set-Cookie", "bb_session=stub-session; path=/")
				w.Header().Add("Set-Cookie", "bb_data=stub-data; path=/")
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/dl.php") {
			// A row with no inline magnet falls back to downloading the
			// attachment; serve a valid one so the test's noise is about the
			// session and nothing else.
			w.Header().Set("Content-Type", "application/x-bittorrent")
			_, _ = w.Write(minimalTorrent())
			return
		}
		m.pages++
		body := m.guest
		if m.authOK && !m.alwaysGuest {
			body = m.authed
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	})
}

// oneRow keeps a listing's header (which carries the account menu the
// expired-cookie check reads) and exactly one row. Row parsing is covered by
// the fixture tests; these tests are about the session, and every row costs an
// attachment download plus its pacing delay.
func oneRow(t *testing.T, body string) string {
	t.Helper()
	rows := rowRe.FindAllString(body, -1)
	if len(rows) < 2 {
		t.Fatalf("fixture has %d rows — nothing to trim", len(rows))
	}
	for _, row := range rows[1:] {
		body = strings.Replace(body, row, "", 1)
	}
	return body
}

// minimalTorrent is the smallest bencoded torrent whose info dict decodes to
// an infohash: one 16 KiB piece over a one-byte file.
func minimalTorrent() []byte {
	return []byte("d4:infod6:lengthi1e4:name4:test12:piece lengthi16384e6:pieces20:" +
		"01234567890123456789" + "ee")
}

func (m *mazepaStub) counts() (logins, pages int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.logins, m.pages
}

func newStubParser(t *testing.T, stub *mazepaStub) (*Parser, *httptest.Server) {
	t.Helper()
	// SaveAuth writes a live session cookie to disk; keep it out of the repo.
	core.SetSessionStoreDir(t.TempDir())
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	cfg := app.DefaultConfig()
	cfg.Mazepa.Host = srv.URL
	cfg.Mazepa.Login.U = "stub-user"
	cfg.Mazepa.Login.P = "stub-pass"
	cfg.Mazepa.ParseDelay = 0

	p := New(cfg, nil, t.TempDir())
	return p, srv
}

// The production failure, 2026-11-27: the session expired in the middle of a
// sweep, every remaining page came back as a guest page, and nothing ever
// logged in again — the login check only existed at the top of a run, and that
// run had already started. 445 pages were logged one by one as "served to a
// guest" and marked swept.
func TestAGuestPageIsRecoveredByReAuthenticating(t *testing.T) {
	stub := &mazepaStub{
		guest:      loadFixture(t, "forum_f12_guest.html"),
		authed:     oneRow(t, loadFixture(t, "forum_f12.html")),
		loginWorks: true,
	}
	p, srv := newStubParser(t, stub)

	pageURL := srv.URL + "/viewforum.php?f=12&start=0"
	if _, _, err := p.parseForumPage(context.Background(), pageURL, []string{"movie"}, srv.URL); err != nil {
		t.Fatalf("a recoverable guest page returned an error: %v", err)
	}

	logins, pages := stub.counts()
	if logins != 1 {
		t.Errorf("logins = %d, want exactly 1 (the mid-run re-authentication)", logins)
	}
	if pages != 2 {
		t.Errorf("page fetches = %d, want 2 (the guest page, then the retry)", pages)
	}
	if p.getCookie() == "" {
		t.Error("no session cookie after the recovery — the next page would be a guest page again")
	}
}

// A guest page must never read as "this page holds nothing". That return is
// what made a dead session indistinguishable from a quiet forum: the sweep
// settled unread pages as swept and the run reported ok with failed=0.
func TestAnUnrecoverableGuestPageIsAnAuthorizationError(t *testing.T) {
	for _, tc := range []struct {
		name       string
		loginWorks bool
	}{
		{"login refused", false},
		{"login accepted but the page is still a guest page", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &mazepaStub{
				guest:       loadFixture(t, "forum_f12_guest.html"),
				authed:      loadFixture(t, "forum_f12.html"),
				loginWorks:  tc.loginWorks,
				alwaysGuest: true,
			}
			p, srv := newStubParser(t, stub)

			pageURL := srv.URL + "/viewforum.php?f=12&start=0"
			items, _, err := p.parseForumPage(context.Background(), pageURL, []string{"movie"}, srv.URL)
			if err == nil {
				t.Fatalf("a guest page returned no error (items=%d) — the silent zero is back", len(items))
			}
			if !errors.Is(err, core.ErrNotAuthorized) {
				t.Errorf("error %q does not wrap core.ErrNotAuthorized, so cronErrorStatus cannot report work_login", err)
			}
		})
	}
}

// A refused login must not be retried once per page. The window is counted
// from the failure, never from a success: a successful login is immediately
// followed by the page that needed it.
func TestARefusedLoginIsHeldOffButASuccessfulOneIsNot(t *testing.T) {
	stub := &mazepaStub{
		guest:       loadFixture(t, "forum_f12_guest.html"),
		authed:      loadFixture(t, "forum_f12.html"),
		alwaysGuest: true,
	}
	p, srv := newStubParser(t, stub)

	pageURL := srv.URL + "/viewforum.php?f=12&start=0"
	for i := 0; i < 5; i++ {
		if _, _, err := p.parseForumPage(context.Background(), pageURL, []string{"movie"}, srv.URL); err == nil {
			t.Fatalf("attempt %d: guest page returned no error", i)
		}
	}
	if logins, _ := stub.counts(); logins != 1 {
		t.Errorf("logins = %d over 5 guest pages, want 1 — the cooldown is not holding", logins)
	}

	// A login that succeeds must leave no cooldown behind, or the next page
	// whose session dies cannot recover.
	stub.mu.Lock()
	stub.loginWorks = true
	stub.mu.Unlock()
	p.mu.Lock()
	p.lastLoginFail = time.Time{}
	p.mu.Unlock()

	if err := p.ensureLogin(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
	p.invalidateCookie()
	if err := p.ensureLogin(context.Background()); err != nil {
		t.Fatalf("a login right after a successful one was held off: %v", err)
	}
}

// The sweep must stop on an authorization failure rather than walk the rest of
// its map. Each unauthorized page used to spend the cycle's failure budget,
// and after ParseAllFailBudget attempts a page settles as swept without ever
// having been read — a dead session consuming the whole sweep plan.
func TestTheSweepStopsInsteadOfSettlingUnauthorizedPages(t *testing.T) {
	stub := &mazepaStub{
		guest:       loadFixture(t, "forum_f12_guest.html"),
		authed:      loadFixture(t, "forum_f12.html"),
		loginWorks:  true,
		alwaysGuest: true,
	}
	p, _ := newStubParser(t, stub)

	p.mu.Lock()
	p.tasks = map[string][]Task{"12": {
		{Page: 0, UpdateTime: "0001-01-01T00:00:00"},
		{Page: 1, UpdateTime: "0001-01-01T00:00:00"},
		{Page: 2, UpdateTime: "0001-01-01T00:00:00"},
	}}
	p.mu.Unlock()

	// Run the sweep ParseAllFailBudget times: a page settles after that many
	// failed attempts, so this is the number of runs it takes for "the fetch
	// failed" to turn into "the page is done". A single run cannot show the
	// difference.
	for run := 0; run < core.ParseAllFailBudget; run++ {
		_, err := p.ParseAllTask(context.Background(), true)
		if err == nil {
			t.Fatalf("run %d: parsealltask reported success over a sweep that read nothing", run)
		}
		if !errors.Is(err, core.ErrNotAuthorized) {
			t.Errorf("run %d: error %q does not wrap core.ErrNotAuthorized", run, err)
		}
	}

	// Each run must have stopped at the first page rather than marching
	// through the map.
	if _, pages := stub.counts(); pages > 2*core.ParseAllFailBudget {
		t.Errorf("fetched %d pages after losing the session, want at most %d (one page plus its retry per run)",
			pages, 2*core.ParseAllFailBudget)
	}

	// And no page may have been settled as swept.
	p.mu.Lock()
	tasks := cloneTasks(p.tasks)
	p.mu.Unlock()
	for _, task := range tasks["12"] {
		if task.UpdatedToday() {
			t.Errorf("page %d was marked done although it was never read", task.Page)
		}
	}
	cycle := core.LoadParseAllCycle(core.ParseAllCyclePath(p.DataDir, trackerName))
	if cycle == nil {
		t.Fatal("no cycle checkpoint was written — this test cannot see what the sweep settled")
	}
	p.mu.Lock()
	slots, _ := core.CycleSlotsFromMap[Task](p.tasks, func(t Task) int { return t.Page })
	p.mu.Unlock()
	if pending := core.CountPendingInCycle(slots, cycle); pending != len(tasks["12"]) {
		t.Errorf("cycle %s has %d of %d pages still pending — pages it never read were settled as swept",
			cycle.CycleID, pending, len(tasks["12"]))
	}
}

// The login must not be rebuilt per entrypoint. Four copies of the same check
// are what let a sweep run for an hour with no session: each one could only
// ever fire before the run started.
func TestEveryEntrypointLogsInThroughOneHelper(t *testing.T) {
	src := readSource(t, "mazepa.go")
	if n := strings.Count(src, "p.takeLogin("); n != 1 {
		t.Errorf("takeLogin has %d callers, want 1 (ensureLogin) — a second copy of the login check cannot be reached mid-run", n)
	}
	for _, entry := range []string{
		"func (p *Parser) Parse(",
		"func (p *Parser) ParseAllTask(",
		"func (p *Parser) ParseLatest(",
		"func (p *Parser) UpdateTasksParse(",
	} {
		body := funcBody(t, src, entry)
		if !strings.Contains(body, "p.ensureLogin(ctx)") {
			t.Errorf("%s does not authorize through ensureLogin", entry)
		}
	}
}

func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// funcBody returns the source of the function whose declaration starts with
// decl, up to the next top-level declaration.
func funcBody(t *testing.T, src, decl string) string {
	t.Helper()
	i := strings.Index(src, decl)
	if i < 0 {
		t.Fatalf("%s not found", decl)
	}
	rest := src[i+len(decl):]
	if j := strings.Index(rest, "\nfunc "); j >= 0 {
		return rest[:j]
	}
	return rest
}
