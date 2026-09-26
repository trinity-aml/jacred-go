package rutracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"jacred/app"
	"jacred/core"
	"jacred/filedb"
)

const trackerName = "rutracker"

var (
	rowDateRe     = regexp.MustCompile(`<p>([0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2})</p>`)
	rowTopicIDRe  = regexp.MustCompile(`<a id="tt-([0-9]+)"`)
	rowTitleRe    = regexp.MustCompile(`<a id="tt-[0-9]+"[^>]+>([^\n\r]+)</a>`)
	rowSidRe      = regexp.MustCompile(`<span class="seedmed"[^>]*><b>([0-9]+)</b>`)
	rowPirRe      = regexp.MustCompile(`<span class="leechmed"[^>]*><b>([0-9]+)</b>`)
	rowSizeRe     = regexp.MustCompile(`dl-stub">([^<]+)</a>`)
	topicTimeRe   = regexp.MustCompile(`<a class="p-link small" href="viewtopic\.php\?t=[^"]+">([^<]+)</a>`)
	topicMagnetRe = regexp.MustCompile(`href="(magnet:[^"]+)" class="(?:med )?magnet-link"`)
	forumPagesRe  = regexp.MustCompile(`Страница <b>1</b> из <b>([0-9]+)</b>`)
	serialWordsRe = regexp.MustCompile(`(?i)(Сезон|Серии)`)
	firstNamePart = regexp.MustCompile(`(\[|/|\(|\|)`)
)

type Task struct {
	UpdateTime string `json:"updateTime"`
	Page       int    `json:"page"`
	// Cycle bookkeeping; embedded so the stored map stays flat.
	core.CycleFields
}

func (t Task) UpdatedToday(loc *time.Location) bool {
	tm := parseTaskTime(t.UpdateTime, loc)
	if tm.IsZero() {
		return false
	}
	now := time.Now().In(loc)
	y1, m1, d1 := tm.Date()
	y2, m2, d2 := now.Date()
	return y1 == y2 && m1 == m2 && d1 == d2
}
func (t *Task) MarkToday(loc *time.Location) {
	if loc == nil {
		loc = time.Local
	}
	now := time.Now().In(loc)
	t.UpdateTime = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).Format(time.RFC3339)
}

type Parser struct {
	Config  app.Config
	DB      *filedb.DB
	DataDir string
	Fetcher *core.Fetcher
	loc     *time.Location
	mu      sync.Mutex
	// One flag for Parse and ParseAllTask, not one each. With a guard apiece
	// both swept at once — parsealltask is scheduled every few minutes while a
	// full sweep runs for hours — so two runs drove the same session, the same
	// login path and the same rate limit at the tracker.
	busy     bool
	latestMu sync.Mutex
	tasks    map[string][]Task
	cookieMu sync.Mutex
	cookie   string
	cookieT  time.Time
	// loginBlockedUntil throttles re-login attempts. The crontab drives four
	// rutracker entrypoints, each of which calls ensureLogin, so a session
	// that cannot be established would otherwise POST the login form up to a
	// dozen times an hour — which is what rutracker's anti-bruteforce CAPTCHA
	// counts. Once it appears no automated login can pass at all, so hammering
	// the form turns a recoverable outage into a stuck one.
	loginBlockedUntil time.Time
	loginBlockReason  string
	domain            string
}

type ParseResult struct {
	Fetched, Added, Updated, Skipped, Duplicates, Failed int
	Status                                               string
	PerCategory                                          map[string]int
}

func New(cfg app.Config, db *filedb.DB, dataDir string) *Parser {
	loc, _ := time.LoadLocation("Asia/Jerusalem")
	if loc == nil {
		loc = time.Local
	}
	// Page fetches now go through Fetcher, which applies the proxy/TLS
	// transport itself — the parser no longer keeps http.Clients of its own.
	p := &Parser{Config: cfg, DB: db, DataDir: dataDir, Fetcher: core.NewFetcher(cfg), loc: loc, tasks: map[string][]Task{}, domain: core.DomainFromHost(cfg.Rutracker.Host)}
	_ = p.loadTasks()
	if saved, savedT := core.DefaultSessionStore().LoadAuth(p.domain); saved != "" && time.Since(savedT) < 2*time.Hour {
		p.cookie = saved
		p.cookieT = savedT
		log.Printf("rutracker: loaded saved cookie from disk (age=%s)", time.Since(savedT).Round(time.Second))
	}
	return p
}

func (p *Parser) getCookie() string {
	p.cookieMu.Lock()
	defer p.cookieMu.Unlock()
	if p.cookie != "" && time.Since(p.cookieT) < 2*time.Hour {
		return p.cookie
	}
	return ""
}

// invalidateCookie drops the in-memory and on-disk auth cookie so the next
// ensureLogin re-runs takeLogin. Called when a listing response comes back
// as the login form — that's a reliable signal that bb_session has expired
// server-side (rutracker often invalidates sessions well before our 2-hour
// local TTL fires).
func (p *Parser) invalidateCookie() {
	p.cookieMu.Lock()
	p.cookie = ""
	p.cookieT = time.Time{}
	p.cookieMu.Unlock()
	_ = core.DefaultSessionStore().DeleteAuth(p.domain)
}

// looksLikeRutrackerLoginForm returns true when the HTML body is the login
// page rather than a forum listing. rutracker login form posts to
// /forum/login.php with input name="login_username"; that input never
// appears on a regular topic listing (which contains class="torTopic"
// instead). Both checks together avoid false positives on edge pages.
func looksLikeRutrackerLoginForm(htmlBody string) bool {
	return strings.Contains(htmlBody, `name="login_username"`) &&
		!strings.Contains(htmlBody, `class="torTopic"`)
}

// errCFChallenge marks a response that is Cloudflare's interstitial rather
// than anything rutracker served. It is a distinct error because the body
// contains neither `class="torTopic"` nor the login form, so every downstream
// check would otherwise read it as "this category has no topics" and the run
// would report success with zero records.
var errCFChallenge = errors.New("rutracker: cloudflare challenge (bypass failed)")

// looksLikeCFChallenge recognises the Cloudflare interstitial. `cf_chl_opt` is
// the challenge script's config object and `cf-mitigated: challenge` is the
// header CF sets alongside it; the title check covers the localized variants.
//
// The bare `/cdn-cgi/challenge-platform/` path is deliberately NOT a marker: a
// *cleared* page still loads CF's JS-detection beacon from
// `/cdn-cgi/challenge-platform/scripts/jsd/main.js`, so matching on the path
// alone reports a challenge on every successful fetch. Only the interstitial's
// own `orchestrate/chl_page` script is exclusive to a challenge.
func looksLikeCFChallenge(body string) bool {
	// Topic rows mean we got the listing, whatever else rides along with it.
	if strings.Contains(body, `class="torTopic"`) {
		return false
	}
	if strings.Contains(body, "cf_chl_opt") || strings.Contains(body, "orchestrate/chl_page") {
		return true
	}
	return strings.Contains(body, "<title>Just a moment")
}

// decodeRutrackerBody picks the right charset. Origin pages are CP1251, but a
// body that came back through flaresolverr was already decoded to UTF-8 by the
// browser, and running the CP1251 mapping over that mangles every Cyrillic
// title. Decode, then fall back when the marker every rutracker page carries
// fails to appear.
func decodeRutrackerBody(data []byte) string {
	text := core.DecodeCP1251(data)
	if strings.Contains(text, "rutracker") || strings.Contains(text, "торрент") {
		return text
	}
	if raw := string(data); strings.Contains(raw, "rutracker") || strings.Contains(raw, "торрент") {
		return raw
	}
	return text
}

// loginCaptchaRe matches the anti-bruteforce CAPTCHA rutracker adds to the
// login form after repeated failed attempts. Verified against the live page on
// 2026-09-26: the form gained `cap_sid` and a per-session `cap_code_<32 hex>`
// alongside login_username/login_password.
//
// Naming it matters: a CAPTCHA needs a human, a wrong password needs a config
// edit, and a CF block needs the browser — three different fixes that
// otherwise log identically as "no bb_session".
var loginCaptchaRe = regexp.MustCompile(`(?i)name=["']cap_(?:sid|code_[0-9a-f]+)["']`)

// How long a failed login is not retried. A CAPTCHA gets a longer window,
// since retrying before a human clears it cannot succeed and each attempt
// refreshes the block.
const (
	loginCooldown        = 15 * time.Minute
	loginCaptchaCooldown = 2 * time.Hour
)

func (p *Parser) noteLoginFailure(d time.Duration, reason string) {
	p.cookieMu.Lock()
	p.loginBlockedUntil = time.Now().Add(d)
	p.loginBlockReason = reason
	p.cookieMu.Unlock()
}

func (p *Parser) loginBlocked() (time.Duration, string, bool) {
	p.cookieMu.Lock()
	defer p.cookieMu.Unlock()
	if remaining := time.Until(p.loginBlockedUntil); remaining > 0 {
		return remaining, p.loginBlockReason, true
	}
	return 0, "", false
}

func (p *Parser) takeLogin(ctx context.Context) bool {
	host := strings.TrimRight(p.Config.Rutracker.Host, "/")
	if host == "" || p.Config.Rutracker.Login.U == "" {
		log.Println("rutracker: login skipped — no host or login configured")
		return false
	}
	log.Printf("rutracker: attempting login to %s as %s", host, p.Config.Rutracker.Login.U)
	loginURL := host + "/forum/login.php"

	// /forum/login.php sits behind the same Cloudflare challenge as the rest
	// of the forum, so the credentials POST needs cf_clearance and the exact
	// User-Agent that earned it.
	//
	// It goes through Fetcher, not a raw net/http client. That matters more
	// than it looks: cf_clearance is minted by a real Chrome and CF fingerprints
	// the ClientHello, so replaying it over a Go handshake is a visible
	// mismatch — measured here as an intermittent
	// `status=403 cf-mitigated="challenge"` on a clearance seconds old, with
	// the credentials never reaching phpBB. Fetcher's tls-client impersonates
	// Chrome_146, which is the whole reason it exists. The two things that
	// used to require the raw client — Set-Cookie off the 302, and not
	// following that redirect — are now FetchResult.Header and
	// FetchOptions.NoRedirect.
	ua := strings.TrimSpace(p.Config.Rutracker.UserAgent)
	postCookie := strings.TrimSpace(p.Config.Rutracker.Cookie)
	if flareCookie, flareUA := p.Fetcher.GetFlareCookies(loginURL); flareCookie != "" {
		postCookie = core.MergeCookieStrings(postCookie, flareCookie)
		if ua == "" {
			ua = flareUA
		}
		log.Printf("rutracker: login using cf_clearance from flaresolverr")
	} else if !strings.Contains(postCookie, "cf_clearance") {
		log.Printf("rutracker: login has no cf_clearance — the CF challenge on %s was not solved; "+
			"paste a browser cf_clearance (plus a matching useragent) into init.yaml Rutracker", loginURL)
	}

	form := url.Values{
		"login_username": {p.Config.Rutracker.Login.U},
		"login_password": {p.Config.Rutracker.Login.P},
		"login":          {"\xc2\xf5\xee\xe4"}, // "Вход" in CP1251
	}
	tracker := p.Config.Rutracker
	tracker.Cookie = postCookie
	// Asks for standard mode without relying on it: Do re-promotes any domain
	// in the CF auto-detect registry, and rutracker.org is in it. Harmless —
	// for a non-GET, Do's flare branch merges the cached clearance and then
	// issues a plain POST over the impersonating client, which is the same
	// request. The point is that it is not the raw net/http client.
	tracker.FetchMode = "standard"
	res, err := p.Fetcher.Do(loginURL, tracker, core.FetchOptions{
		Method:      http.MethodPost,
		Body:        []byte(form.Encode()),
		ContentType: "application/x-www-form-urlencoded",
		UserAgent:   ua,
		NoRedirect:  true,
		ExtraHeaders: map[string]string{
			"Referer": loginURL,
			"Origin":  host,
		},
	})
	if err != nil {
		log.Printf("rutracker: login HTTP error: %v", err)
		return false
	}
	body := res.Body
	log.Printf("rutracker: login response status=%d cf-mitigated=%q", res.StatusCode, res.Header.Get("Cf-Mitigated"))

	var parts []string
	for _, line := range res.Header.Values("Set-Cookie") {
		parts = append(parts, strings.SplitN(line, ";", 2)[0])
	}
	cookieStr := strings.Join(parts, "; ")
	if p.acceptLoginCookies(cookieStr, postCookie) {
		return true
	}
	// Distinguish "Cloudflare never let us reach phpBB" from "rutracker
	// rejected these credentials" — they need completely different fixes.
	text := decodeRutrackerBody(body)
	if res.Header.Get("Cf-Mitigated") == "challenge" || looksLikeCFChallenge(text) {
		// CF refused the POST itself. A GET replay of the same clearance works
		// at ~140ms, so this is specific to the credentials POST, and it is
		// intermittent — measured on rutracker, 302 on one attempt and
		// 403 cf-mitigated=challenge on the next. Submitting the form in the
		// browser sidesteps it by construction: that is the client that earned
		// the clearance.
		log.Printf("rutracker: login POST was challenged by cloudflare — retrying through the browser")
		if cookieStr, ok := p.loginViaBrowser(ctx, loginURL, form.Encode(), postCookie); ok {
			return p.acceptLoginCookies(cookieStr, postCookie)
		}
		log.Printf("rutracker: login BLOCKED by cloudflare challenge (credentials were never checked)")
		p.noteLoginFailure(loginCooldown, "cloudflare challenge on the login form")
		return false
	}
	if loginCaptchaRe.MatchString(text) {
		log.Printf("rutracker: login form is showing a CAPTCHA — rutracker's anti-bruteforce kicked in "+
			"after repeated login attempts. No automated login can pass it, and every further attempt "+
			"refreshes the block. Wait it out, or log in once in a browser and paste that bb_session "+
			"into init.yaml Rutracker.cookie. Not retrying for %s", loginCaptchaCooldown)
		p.noteLoginFailure(loginCaptchaCooldown, "login form is showing a CAPTCHA")
		return false
	}
	log.Printf("rutracker: login FAILED — no bb_session; cookies set: [%s]", core.CookieNames(cookieStr))
	p.noteLoginFailure(loginCooldown, "credentials rejected")
	return false
}

// acceptLoginCookies stores a session if the response actually carried one.
// Shared by both login routes — the HTTP POST and the browser form — so they
// cannot drift on what counts as success or on what gets persisted.
func (p *Parser) acceptLoginCookies(cookieStr, postCookie string) bool {
	if !strings.Contains(cookieStr, "bb_session") {
		return false
	}
	// Keep cf_clearance alongside bb_session: listing fetches reuse this saved
	// string, and an auth-only cookie would get bounced at the edge.
	merged := cookieStr
	if postCookie != "" {
		merged = core.MergeCookieStrings(postCookie, cookieStr)
	}
	p.cookieMu.Lock()
	p.cookie = merged
	p.cookieT = time.Now()
	p.loginBlockedUntil = time.Time{}
	p.loginBlockReason = ""
	p.cookieMu.Unlock()
	_ = core.DefaultSessionStore().SaveAuth(p.domain, merged)
	log.Printf("rutracker: login OK, got bb_session")
	return true
}

// loginViaBrowser submits the credentials form in the browser, for when CF
// refuses the POST from Go.
func (p *Parser) loginViaBrowser(ctx context.Context, loginURL, postData, cookie string) (string, bool) {
	cookieStr, body, err := p.Fetcher.PostViaBrowser(ctx, loginURL, postData, cookie)
	if err != nil {
		log.Printf("rutracker: browser login failed: %v", err)
		return "", false
	}
	if loginCaptchaRe.MatchString(body) {
		log.Printf("rutracker: login form is showing a CAPTCHA — rutracker's anti-bruteforce kicked in "+
			"after repeated login attempts. No automated login can pass it, and every further attempt "+
			"refreshes the block. Wait it out, or log in once in a browser and paste that bb_session "+
			"into init.yaml Rutracker.cookie. Not retrying for %s", loginCaptchaCooldown)
		p.noteLoginFailure(loginCaptchaCooldown, "login form is showing a CAPTCHA")
		return "", false
	}
	return cookieStr, strings.Contains(cookieStr, "bb_session")
}

func (p *Parser) ensureLogin(ctx context.Context) bool {
	if p.getCookie() != "" {
		return true
	}
	if remaining, reason, blocked := p.loginBlocked(); blocked {
		log.Printf("rutracker: not retrying login for %s — %s", remaining.Round(time.Second), reason)
		return false
	}
	return p.takeLogin(ctx)
}

func (p *Parser) Parse(ctx context.Context, page int) (ParseResult, error) {
	p.mu.Lock()
	if p.busy {
		p.mu.Unlock()
		return ParseResult{Status: "work"}, nil
	}
	p.busy = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.busy = false; p.mu.Unlock() }()
	if isDisabled(p.Config.DisableTrackers, trackerName) {
		return ParseResult{Status: "disabled"}, nil
	}
	if !p.ensureLogin(ctx) {
		return ParseResult{Status: core.StatusWorkLogin}, fmt.Errorf("rutracker: login failed: %w", core.ErrNotAuthorized)
	}
	res := ParseResult{Status: "ok", PerCategory: map[string]int{}}
	seenURLs := map[string]struct{}{} // cross-category duplicate tracking
	log.Printf("rutracker: starting parse, %d categories, masterDb=%d entries", len(firstPageCats), len(p.DB.MasterEntries()))
	for i, cat := range firstPageCats {
		items, err := p.parsePage(ctx, cat, page)
		if errors.Is(err, errCFChallenge) {
			// Every remaining category would hit the same wall, so stop after
			// one instead of grinding through 60+ futile fetches.
			log.Printf("rutracker: aborting after %d/%d cats — %v", i, len(firstPageCats), err)
			res.Status = "cf-challenge"
			return res, err
		}
		if err != nil {
			log.Printf("rutracker: cat %s error: %v (continuing)", cat, err)
			continue // don't abort all categories on single failure
		}
		res.Fetched += len(items)
		res.PerCategory[cat] = len(items)
		if len(items) == 0 {
			continue
		}
		a, u, s, d, f, err := p.saveTorrents(ctx, items, seenURLs)
		if err != nil {
			log.Printf("rutracker: cat %s save error: %v (continuing)", cat, err)
			continue
		}
		res.Added += a
		res.Updated += u
		res.Skipped += s
		res.Duplicates += d
		res.Failed += f
		if (i+1)%10 == 0 {
			log.Printf("rutracker: progress %d/%d cats, fetched=%d added=%d dup=%d", i+1, len(firstPageCats), res.Fetched, res.Added, res.Duplicates)
		}
	}
	log.Printf("rutracker: parse done, fetched=%d added=%d updated=%d skipped=%d duplicates=%d failed=%d", res.Fetched, res.Added, res.Updated, res.Skipped, res.Duplicates, res.Failed)
	log.Printf("rutracker: done fetched=%d added=%d skipped=%d failed=%d", res.Fetched, res.Added, res.Skipped, res.Failed)
	return res, nil
}

func (p *Parser) UpdateTasksParse(ctx context.Context) (map[string][]Task, error) {
	if !p.ensureLogin(ctx) {
		return nil, fmt.Errorf("rutracker: login failed: %w", core.ErrNotAuthorized)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tasks == nil {
		p.tasks = map[string][]Task{}
	}
	for _, cat := range allTaskCats {
		htmlBody, err := p.fetchCategoryRoot(ctx, cat)
		if errors.Is(err, errCFChallenge) {
			log.Printf("rutracker: updatetasksparse aborted — %v", err)
			return nil, err
		}
		if err != nil {
			continue
		}
		// Same expiry guard as parsePage: pagination regex on a login page
		// silently matches nothing and we'd save a degenerate task list.
		if looksLikeRutrackerLoginForm(htmlBody) {
			log.Printf("rutracker: cat=%s root returned login form (cookie expired, invalidating)", cat)
			p.invalidateCookie()
			break
		}
		maxPages := 0
		if m := forumPagesRe.FindStringSubmatch(htmlBody); len(m) > 1 {
			maxPages, _ = strconv.Atoi(strings.TrimSpace(m[1]))
		}
		pages := map[int]Task{}
		for _, t := range p.tasks[cat] {
			pages[t.Page] = t
		}
		for page := 0; page <= maxPages; page++ {
			if _, ok := pages[page]; !ok {
				pages[page] = Task{Page: page, UpdateTime: "0001-01-01T00:00:00"}
			}
		}
		merged := make([]Task, 0, len(pages))
		for _, t := range pages {
			merged = append(merged, t)
		}
		sort.Slice(merged, func(i, j int) bool { return merged[i].Page < merged[j].Page })
		// The map was additive only, so a category that shrank kept its old
		// slots and ParseAllTask re-fetched each one every sweep. maxPages is
		// what this loop just read off the live pager; when it could not be
		// read it is 0 and PruneTaskPages deliberately changes nothing.
		merged, pruned := core.PruneTaskPages(merged, func(t Task) int { return t.Page }, maxPages)
		if pruned > 0 {
			log.Printf("rutracker: updatetasksparse cat=%s maxPage=%d pruned=%d remaining=%d", cat, maxPages, pruned, len(merged))
		}
		p.tasks[cat] = merged
	}
	if err := p.saveTasksLocked(); err != nil {
		return nil, err
	}
	return cloneTasks(p.tasks), nil
}

// beginCycleLocked opens (or rotates) the sweep cycle. Caller holds p.mu.
func (p *Parser) beginCycleLocked() (*core.ParseAllCycle, int, int) {
	slots, keys := core.CycleSlotsFromMap[Task](p.tasks, func(t Task) int { return t.Page })
	cycle, pending := core.BeginParseAllCycle(
		core.ParseAllCyclePath(p.DataDir, trackerName),
		core.MapFingerprint(keys),
		slots,
		func(i int) bool { return slots[i].(*Task).UpdatedToday(p.loc) },
		true,
	)
	if pending != len(slots) {
		_ = p.saveTasksLocked()
	}
	return cycle, pending, len(slots)
}

// settle records one page's outcome. A failure spends its budget instead of
// settling the page, so a transient error is retried while a permanently
// broken page cannot hold the cycle open forever.
func (p *Parser) settle(cycle *core.ParseAllCycle, cat string, page int, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if list, found := p.tasks[cat]; found {
		for i := range list {
			if list[i].Page == page {
				if core.NoteParseAllAttempt(trackerName, &list[i], cycle, ok) && ok {
					list[i].MarkToday(p.loc)
				}
			}
		}
		p.tasks[cat] = list
	}
	_ = p.saveTasksLocked()
}

func (p *Parser) ParseAllTask(ctx context.Context, force bool) (string, error) {
	if !p.ensureLogin(ctx) {
		return "", fmt.Errorf("rutracker: login failed: %w", core.ErrNotAuthorized)
	}
	p.mu.Lock()
	if p.busy {
		p.mu.Unlock()
		return "work", nil
	}
	p.busy = true
	snapshot := cloneTasks(p.tasks)
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.busy = false; p.mu.Unlock() }()

	if len(snapshot) == 0 {
		log.Printf("rutracker: parsealltask — tasks empty, running updatetasksparse first")
		if _, err := p.UpdateTasksParse(ctx); err != nil {
			return "", err
		}
		p.mu.Lock()
		snapshot = cloneTasks(p.tasks)
		p.mu.Unlock()
	}

	// Opened against p.tasks rather than the snapshot: the first run stamps
	// the day's existing progress into the cycle, and that has to be
	// persisted before the snapshot is taken.
	p.mu.Lock()
	cycle, pendingAtStart, cycleTotal := p.beginCycleLocked()
	snapshot = cloneTasks(p.tasks)
	p.mu.Unlock()

	totalPages := 0
	for _, list := range snapshot {
		totalPages += len(list)
	}
	processed, fetched, added, updated, skipped, failed, errs := 0, 0, 0, 0, 0, 0, 0
	for cat, list := range snapshot {
		for _, task := range list {
			if !force && !core.PendingInCycle(&task, cycle) {
				continue
			}
			if p.Config.Rutracker.ParseDelay > 0 {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(time.Duration(p.Config.Rutracker.ParseDelay) * time.Millisecond):
				}
			}
			items, err := p.parsePage(ctx, cat, task.Page)
			if errors.Is(err, errCFChallenge) {
				log.Printf("rutracker: parsealltask aborted at cat=%s page=%d — %v", cat, task.Page, err)
				return "cf-challenge", err
			}
			if err != nil {
				log.Printf("rutracker: parsealltask cat=%s page=%d error: %v", cat, task.Page, err)
				p.settle(cycle, cat, task.Page, false)
				errs++
				continue
			}
			processed++
			if len(items) == 0 {
				log.Printf("rutracker: parsealltask cat=%s page=%d empty (marking today)", cat, task.Page)
				p.settle(cycle, cat, task.Page, true)
				continue
			}
			a, u, s, _, f, err := p.saveTorrents(ctx, items, nil)
			if err != nil {
				log.Printf("rutracker: parsealltask cat=%s page=%d save error: %v", cat, task.Page, err)
				errs++
				continue
			}
			fetched += len(items)
			added += a
			updated += u
			skipped += s
			failed += f
			log.Printf("rutracker: parsealltask cat=%s page=%d fetched=%d added=%d skipped=%d failed=%d", cat, task.Page, len(items), a, s, f)
			p.settle(cycle, cat, task.Page, true)
		}
	}
	p.mu.Lock()
	slots, _ := core.CycleSlotsFromMap[Task](p.tasks, func(t Task) int { return t.Page })
	pendingLeft := core.CountPendingInCycle(slots, cycle)
	p.mu.Unlock()
	log.Printf("rutracker: parsealltask done processed=%d/%d fetched=%d added=%d updated=%d skipped=%d failed=%d errors=%d", processed, totalPages, fetched, added, updated, skipped, failed, errs)
	log.Printf("rutracker: parsealltask cycle=%s pending=%d->%d/%d", cycle.CycleID, pendingAtStart, pendingLeft, cycleTotal)
	return "ok", nil
}

func (p *Parser) ParseLatest(ctx context.Context, pages int) (string, error) {
	if !p.latestMu.TryLock() {
		return "work", nil
	}
	defer p.latestMu.Unlock()
	if !p.ensureLogin(ctx) {
		return "", fmt.Errorf("rutracker: login failed: %w", core.ErrNotAuthorized)
	}
	if pages <= 0 {
		pages = 5
	}
	p.mu.Lock()
	snapshot := cloneTasks(p.tasks)
	p.mu.Unlock()
	if len(snapshot) == 0 {
		if _, err := p.UpdateTasksParse(ctx); err != nil {
			return "", err
		}
		p.mu.Lock()
		snapshot = cloneTasks(p.tasks)
		p.mu.Unlock()
	}
	var lines []string
	processed, fetched, added, updated, skipped, failed, errs := 0, 0, 0, 0, 0, 0, 0
	for cat, list := range snapshot {
		sort.Slice(list, func(i, j int) bool { return list[i].Page < list[j].Page })
		if len(list) > pages {
			list = list[:pages]
		}
		for _, task := range list {
			if p.Config.Rutracker.ParseDelay > 0 {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(time.Duration(p.Config.Rutracker.ParseDelay) * time.Millisecond):
				}
			}
			items, err := p.parsePage(ctx, cat, task.Page)
			if errors.Is(err, errCFChallenge) {
				log.Printf("rutracker: parselatest aborted at cat=%s page=%d — %v", cat, task.Page, err)
				return "cf-challenge", err
			}
			if err != nil {
				log.Printf("rutracker: parselatest cat=%s page=%d error: %v", cat, task.Page, err)
				errs++
				continue
			}
			processed++
			if len(items) == 0 {
				log.Printf("rutracker: parselatest cat=%s page=%d empty (marking today)", cat, task.Page)
				p.mu.Lock()
				if list2, ok := p.tasks[cat]; ok {
					for i := range list2 {
						if list2[i].Page == task.Page {
							list2[i].MarkToday(p.loc)
						}
					}
					p.tasks[cat] = list2
				}
				_ = p.saveTasksLocked()
				p.mu.Unlock()
				continue
			}
			a, u, s, _, f, err := p.saveTorrents(ctx, items, nil)
			if err != nil {
				log.Printf("rutracker: parselatest cat=%s page=%d save error: %v", cat, task.Page, err)
				errs++
				continue
			}
			fetched += len(items)
			added += a
			updated += u
			skipped += s
			failed += f
			log.Printf("rutracker: parselatest cat=%s page=%d fetched=%d added=%d skipped=%d failed=%d", cat, task.Page, len(items), a, s, f)
			p.mu.Lock()
			if list2, ok := p.tasks[cat]; ok {
				for i := range list2 {
					if list2[i].Page == task.Page {
						list2[i].MarkToday(p.loc)
					}
				}
				p.tasks[cat] = list2
			}
			if err := p.saveTasksLocked(); err != nil {
				p.mu.Unlock()
				return "", err
			}
			p.mu.Unlock()
			lines = append(lines, fmt.Sprintf("%s - %d", cat, task.Page))
		}
	}
	log.Printf("rutracker: parselatest done processed=%d fetched=%d added=%d updated=%d skipped=%d failed=%d errors=%d", processed, fetched, added, updated, skipped, failed, errs)
	if len(lines) == 0 {
		return "ok", nil
	}
	return strings.Join(lines, "\n"), nil
}

func (p *Parser) parsePage(ctx context.Context, cat string, page int) ([]filedb.TorrentDetails, error) {
	htmlBody, err := p.fetchPage(ctx, cat, page)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(htmlBody) == "" {
		return nil, nil
	}
	// Detect session expiry: rutracker server-side TTL is often shorter than
	// our 2-hour local cookieT — without this check the parser would keep
	// hitting the login wall until the local TTL eventually rolled over.
	if looksLikeRutrackerLoginForm(htmlBody) {
		log.Printf("rutracker: cat=%s page=%d returned login form (cookie expired, invalidating)", cat, page)
		p.invalidateCookie()
		return nil, nil
	}
	rows := strings.Split(replaceBadNames(htmlBody), `class="torTopic"`)
	out := make([]filedb.TorrentDetails, 0, len(rows))
	for _, row := range rows[1:] {
		createTime, _ := time.ParseInLocation("2006-01-02 15:04", match1(rowDateRe, row), time.Local)
		if createTime.IsZero() {
			continue
		}
		id := match1(rowTopicIDRe, row)
		title := core.StripTagsAndCollapseSpaces(html.UnescapeString(match1(rowTitleRe, row)))
		sid, _ := strconv.Atoi(match1(rowSidRe, row))
		pir, _ := strconv.Atoi(match1(rowPirRe, row))
		sizeName := strings.TrimSpace(strings.ReplaceAll(html.UnescapeString(match1(rowSizeRe, row)), "&nbsp;", " "))
		if id == "" || title == "" || sizeName == "" {
			continue
		}
		name, original, year := parseTitle(cat, title)
		if strings.TrimSpace(name) == "" {
			name = firstTokenTitle(title)
		}
		types := categoryTypes(cat)
		if len(types) == 0 || strings.TrimSpace(name) == "" {
			continue
		}
		out = append(out, filedb.TorrentRecord{TrackerName: trackerName, Types: types, URL: strings.TrimRight(p.Config.Rutracker.Host, "/") + "/forum/viewtopic.php?t=" + id, Title: title, Sid: sid, Pir: pir, SizeName: sizeName, CreateTime: createTime.UTC().Format(time.RFC3339Nano), Name: name, OriginalName: original, Relased: year}.ToMap())
	}
	return out, nil
}

func (p *Parser) saveTorrents(ctx context.Context, torrents []filedb.TorrentDetails, seenURLs map[string]struct{}) (int, int, int, int, int, error) {
	added, updated, skipped, duplicates, failed := 0, 0, 0, 0, 0
	skipCached, skipSame, skipEmpty := 0, 0, 0
	plog := core.NewParserLog(trackerName, filepath.Join(p.DB.DataDir, "log"), p.Config.LogParsers && p.Config.Rutracker.Log)
	bucketCache := make(map[string]map[string]filedb.TorrentDetails, len(torrents))
	changed := make(map[string]time.Time, len(torrents))
	if seenURLs == nil {
		seenURLs = map[string]struct{}{}
	}
	for _, incoming := range torrents {
		select {
		case <-ctx.Done():
			return added, updated, skipped, duplicates, failed, ctx.Err()
		default:
		}
		key := p.DB.KeyDb(asString(incoming["name"]), asString(incoming["originalname"]))
		if strings.TrimSpace(key) == "" || key == ":" {
			skipEmpty++
			skipped++
			continue
		}
		bucket, ok := bucketCache[key]
		if !ok {
			loaded, err := p.DB.OpenReadOrEmpty(key)
			if err != nil {
				return added, updated, skipped, duplicates, failed, err
			}
			bucket = loaded
			bucketCache[key] = bucket
		}
		urlv := strings.TrimSpace(asString(incoming["url"]))
		if urlv == "" {
			skipEmpty++
			skipped++
			continue
		}
		// Check if we already processed this URL in this parse run (cross-category duplicate)
		if _, seen := seenURLs[urlv]; seen {
			duplicates++
			continue
		}
		seenURLs[urlv] = struct{}{}
		existing, exists := bucket[urlv]
		needMagnet := !exists || asString(existing["title"]) != asString(incoming["title"]) || strings.TrimSpace(asString(existing["magnet"])) == ""
		if needMagnet && strings.TrimSpace(asString(incoming["magnet"])) == "" {
			topic, err := p.fetchTopic(ctx, urlv)
			if err == nil && topic != "" {
				if tm := parseTopicCreateTime(match1(topicTimeRe, topic)); !tm.IsZero() {
					incoming["createTime"] = tm
				}
				if magnet := match1(topicMagnetRe, topic); magnet != "" {
					incoming["magnet"] = magnet
				}
			}
		}
		if needMagnet && strings.TrimSpace(asString(incoming["magnet"])) == "" {
			plog.WriteFailed(urlv, asString(incoming["title"]))
			failed++
			continue
		}
		var ex filedb.TorrentDetails
		if exists {
			ex = existing
		}
		result := filedb.MergeTorrent(ex, incoming, p.Config.TracksAttempt)
		if !result.Changed {
			skipSame++
			skipped++
			continue
		}
		bucket[urlv] = result.Torrent
		changed[key] = fileTime(result.Torrent)
		if !result.IsNew {
			plog.WriteUpdated(urlv, asString(incoming["title"]))
			updated++
		} else {
			plog.WriteAdded(urlv, asString(incoming["title"]))
			added++
		}
	}
	if skipCached > 0 || skipSame > 0 || skipEmpty > 0 {
		log.Printf("rutracker: save detail — skipCached=%d skipSame=%d skipEmpty=%d", skipCached, skipSame, skipEmpty)
	}
	for key, when := range changed {
		if err := p.DB.SaveBucket(key, bucketCache[key], when); err != nil {
			return added, updated, skipped, duplicates, failed, err
		}
	}
	return added, updated, skipped, duplicates, failed, nil
}

func (p *Parser) fetchCategoryRoot(ctx context.Context, cat string) (string, error) {
	return p.fetch(ctx, fmt.Sprintf("%s/forum/viewforum.php?f=%s", requestHost(p.Config.Rutracker), cat))
}
func (p *Parser) fetchPage(ctx context.Context, cat string, page int) (string, error) {
	url := fmt.Sprintf("%s/forum/viewforum.php?f=%s", requestHost(p.Config.Rutracker), cat)
	if page > 0 {
		url += fmt.Sprintf("&start=%d", page*50)
	}
	return p.fetch(ctx, url)
}
func (p *Parser) fetchTopic(ctx context.Context, url string) (string, error) {
	return p.fetch(ctx, url)
}

// fetch retrieves a page through the shared Fetcher. Going through Fetcher
// (rather than a bare http.Client, as this parser used to) is what puts
// rutracker on the same footing as every other tracker: CF auto-detect can
// promote the domain to flaresolverr routing, solved cf_clearance cookies come
// from the shared session store, and a `useragent` from init.yaml is honoured
// so a hand-copied cf_clearance stays valid.
func (p *Parser) fetch(ctx context.Context, rawURL string) (string, error) {
	cookie := ""
	if c := p.getCookie(); c != "" {
		cookie = c
	} else {
		cookie = p.Config.Rutracker.Cookie
	}
	isListing := strings.Contains(rawURL, "viewforum.php")
	delays := []time.Duration{0, 5 * time.Second, 10 * time.Second, 15 * time.Second}
	for attempt, delay := range delays {
		if delay > 0 {
			// The whole ladder spans 30s while a flare failure puts the domain
			// in a 3-minute cooldown, so once we are in one every remaining
			// attempt returns 503 in ~0ms without touching the network. Stop
			// instead of burning the budget on guaranteed failures.
			if remaining, blocked := core.FlareCooldown(rawURL); blocked {
				log.Printf("rutracker: flaresolverr in cooldown for %s — abandoning retries for %s", remaining.Round(time.Second), rawURL)
				return "", fmt.Errorf("rutracker: flaresolverr cooldown %s", remaining.Round(time.Second))
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(delay):
			}
		}
		last := attempt == len(delays)-1
		started := time.Now()
		data, status, err := p.Fetcher.DownloadExt(rawURL, p.Config.Rutracker, cookie, p.Config.Rutracker.UserAgent)
		elapsed := time.Since(started).Round(time.Millisecond)
		if err != nil {
			log.Printf("rutracker: fetch err elapsed=%s attempt=%d url=%s err=%v", elapsed, attempt+1, rawURL, err)
			if last {
				return "", err
			}
			continue
		}
		text := decodeRutrackerBody(data)
		// A challenge is not a transient error — retrying just burns the
		// browser and the rate limit. Report it so the caller can stop.
		if looksLikeCFChallenge(text) || status == http.StatusForbidden && strings.TrimSpace(text) == "" {
			log.Printf("rutracker: fetch %d elapsed=%s attempt=%d url=%s — cloudflare challenge", status, elapsed, attempt+1, rawURL)
			return "", errCFChallenge
		}
		if status >= 500 {
			log.Printf("rutracker: fetch %d elapsed=%s attempt=%d bytes=%d url=%s", status, elapsed, attempt+1, len(data), rawURL)
			if last {
				return "", fmt.Errorf("rutracker returned HTTP %d", status)
			}
			continue
		}
		if isListing || elapsed > 5*time.Second {
			log.Printf("rutracker: fetch ok elapsed=%s status=%d bytes=%d url=%s", elapsed, status, len(data), rawURL)
		}
		return text, nil
	}
	return "", fmt.Errorf("rutracker fetch exhausted retries")
}

func (p *Parser) loadTasks() error {
	path := filepath.Join(p.DataDir, "temp", "rutracker_taskParse.json")
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			p.tasks = map[string][]Task{}
			return nil
		}
		return err
	}
	var raw map[string][]Task
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	p.tasks = raw
	return nil
}
func (p *Parser) saveTasksLocked() error {
	if p.tasks == nil {
		p.tasks = map[string][]Task{}
	}
	path := filepath.Join(p.DataDir, "temp", "rutracker_taskParse.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(p.tasks)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
func cloneTasks(in map[string][]Task) map[string][]Task {
	out := make(map[string][]Task, len(in))
	for k, v := range in {
		vv := make([]Task, len(v))
		copy(vv, v)
		out[k] = vv
	}
	return out
}
func parseTaskTime(v string, loc *time.Location) time.Time {
	if strings.TrimSpace(v) == "" {
		return time.Time{}
	}
	if loc == nil {
		loc = time.Local
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if tm, err := time.ParseInLocation(layout, v, loc); err == nil {
			return tm
		}
	}
	return time.Time{}
}
func match1(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}
func firstTokenTitle(title string) string {
	return strings.TrimSpace(strings.ReplaceAll(firstNamePart.Split(title, 2)[0], "в 3Д", ""))
}

func parseTitle(cat, title string) (string, string, int) {
	if movieCats[cat] {
		return parseMovieTitle(title)
	}
	if serialCats[cat] {
		return parseSerialTitle(title)
	}
	if otherNamedCats[cat] {
		name := strings.TrimSpace(matchString(title, `^([^/\(\[]+) `, 1))
		year, _ := strconv.Atoi(matchString(title, ` \[([0-9]{4})(,|-) `, 1))
		if serialWordsRe.MatchString(name) {
			return "", "", 0
		}
		return name, "", year
	}
	return "", "", 0
}
func parseMovieTitle(title string) (string, string, int) {
	pats := []string{`^([^/\(\[]+) / [^/\(\[]+ / ([^/\(\[]+) \([^\)]+\) \[([0-9]+), `, `^([^/\(\[]+) / ([^/\(\[]+) \([^\)]+\) \[([0-9]+), `, `^([^/\(\[]+) \([^\)]+\) \[([0-9]+), `}
	for i, pat := range pats {
		m := regexp.MustCompile(pat).FindStringSubmatch(title)
		if len(m) == 0 {
			continue
		}
		switch i {
		case 0, 1:
			y, _ := strconv.Atoi(m[3])
			return strings.TrimSpace(strings.ReplaceAll(m[1], "в 3Д", "")), strings.TrimSpace(strings.NewReplacer(" in 3D", "", " 3D", "").Replace(m[2])), y
		case 2:
			y, _ := strconv.Atoi(m[2])
			return strings.TrimSpace(strings.ReplaceAll(m[1], "в 3Д", "")), "", y
		}
	}
	return "", "", 0
}
func parseSerialTitle(title string) (string, string, int) {
	if !serialWordsRe.MatchString(title) {
		return "", "", 0
	}
	pats := []string{`^([^/\(\[]+) / [^/\(\[]+ / [^/\(\[]+ / ([^/\(\[]+) / Сезон: [^/]+ / [^\(\[]+ \([^\)]+\) \[([0-9]+)(,|-)`, `^([^/\(\[]+) / [^/\(\[]+ / ([^/\(\[]+) / Сезон: [^/]+ / [^\(\[]+ \([^\)]+\) \[([0-9]+)(,|-)`, `^([^/\(\[]+) / ([^/\(\[]+) / Сезон: [^/]+ / [^\(\[]+ \([^\)]+\) \[([0-9]+)(,|-)`, `^([^/\(\[]+) / Сезон: [^/]+ / [^\(\[]+ \([^\)]+\) \[([0-9]+)(,|-)`, `^([^/\(\[]+) / [^/\(\[]+ / ([^/\(\[]+) / [^\(\[]+ \([^\)]+\) \[([0-9]+)(,|-)`, `^([^/\(\[]+) / ([^/\(\[]+) / [^\(\[]+ \([^\)]+\) \[([0-9]+)(,|-)`, `^([^/\(\[]+) / [^\(\[]+ \([^\)]+\) \[([0-9]+)(,|-)`}
	for idx, pat := range pats {
		m := regexp.MustCompile(pat).FindStringSubmatch(title)
		if len(m) == 0 {
			continue
		}
		switch idx {
		case 0, 1, 2, 4, 5:
			y, _ := strconv.Atoi(m[3])
			n, o := strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
			if serialWordsRe.MatchString(n) || serialWordsRe.MatchString(o) {
				return "", "", 0
			}
			return n, o, y
		case 3, 6:
			y, _ := strconv.Atoi(m[2])
			n := strings.TrimSpace(m[1])
			if serialWordsRe.MatchString(n) {
				return "", "", 0
			}
			return n, "", y
		}
	}
	return "", "", 0
}
func matchString(s, pat string, idx int) string {
	m := regexp.MustCompile(pat).FindStringSubmatch(s)
	if len(m) > idx {
		return strings.TrimSpace(m[idx])
	}
	return ""
}
func parseTopicCreateTime(v string) time.Time {
	v = strings.TrimSpace(strings.ReplaceAll(v, "-", " "))
	if v == "" {
		return time.Time{}
	}
	tm, _ := time.ParseInLocation("02.01.06 15:04", v, time.Local)
	return tm
}
func requestHost(cfg app.TrackerSettings) string {
	if strings.TrimSpace(cfg.Alias) != "" {
		return strings.TrimSpace(cfg.Alias)
	}
	return strings.TrimSpace(cfg.Host)
}
func replaceBadNames(s string) string {
	return strings.NewReplacer("Ё", "Е", "ё", "е").Replace(s)
}

// titleKind selects which grammar parseTitle applies to a forum's titles.
type titleKind uint8

const (
	kindMovie titleKind = iota
	kindSerial
	kindOther
)

// rutrackerCategory is one forum: the types its records carry, how titles are
// written there, and whether the hourly first-page pass visits it.
//
// One table replaces the five structures this used to be — two id slices plus
// three kind sets assembled in init(). Spreading it out is how 35 forums went
// missing relative to upstream: a slice could gain an id without gaining types,
// and categoryTypes returning nil makes parsePage drop every row of that forum
// without a word. Here a forum cannot exist without both.
//
// Order is meaningful and preserved deliberately. A rutracker run that hits the
// flaresolverr cooldown abandons whatever it has not reached yet, so the head of
// this table keeps its priority; forums added later are appended rather than
// interleaved, so existing coverage cannot regress in a truncated run.
type rutrackerCategory struct {
	id    string
	types []string
	kind  titleKind
	quick bool // visited by the hourly pass too, not only ParseAllTask
}

var rutrackerCategories = []rutrackerCategory{
	{"549", []string{"movie"}, kindMovie, true},
	{"22", []string{"movie"}, kindMovie, true},
	{"1666", []string{"movie"}, kindMovie, true},
	{"941", []string{"movie"}, kindMovie, true},
	{"1950", []string{"movie"}, kindMovie, true},
	{"2090", []string{"movie"}, kindMovie, true},
	{"2221", []string{"movie"}, kindMovie, true},
	{"2091", []string{"movie"}, kindMovie, true},
	{"2092", []string{"movie"}, kindMovie, true},
	{"2093", []string{"movie"}, kindMovie, true},
	{"2200", []string{"movie"}, kindMovie, true},
	{"2540", []string{"movie"}, kindMovie, true},
	{"934", []string{"movie"}, kindMovie, true},
	{"505", []string{"movie"}, kindMovie, true},
	{"252", []string{"movie"}, kindMovie, true},
	{"124", []string{"movie"}, kindMovie, true},
	{"1213", []string{"multfilm"}, kindMovie, true},
	{"2343", []string{"multfilm"}, kindMovie, true},
	{"930", []string{"multfilm"}, kindMovie, true},
	{"2365", []string{"multfilm"}, kindMovie, true},
	{"208", []string{"multfilm"}, kindMovie, true},
	{"539", []string{"multfilm"}, kindMovie, true},
	{"209", []string{"multfilm"}, kindMovie, true},
	{"921", []string{"multserial"}, kindSerial, true},
	{"815", []string{"multserial"}, kindSerial, true},
	{"1460", []string{"multserial"}, kindSerial, true},
	{"1457", []string{"movie"}, kindMovie, true},
	{"2199", []string{"movie"}, kindMovie, true},
	{"313", []string{"movie"}, kindMovie, true},
	{"312", []string{"movie"}, kindMovie, true},
	{"1247", []string{"movie"}, kindMovie, true},
	{"2201", []string{"movie"}, kindMovie, true},
	{"2339", []string{"movie"}, kindMovie, true},
	{"140", []string{"movie"}, kindMovie, true},
	{"842", []string{"serial"}, kindSerial, true},
	{"235", []string{"serial"}, kindSerial, true},
	{"242", []string{"serial"}, kindSerial, true},
	{"819", []string{"serial"}, kindSerial, true},
	{"1531", []string{"serial"}, kindSerial, true},
	{"721", []string{"serial"}, kindSerial, true},
	{"1102", []string{"serial"}, kindSerial, true},
	{"1120", []string{"serial"}, kindSerial, true},
	{"1214", []string{"serial"}, kindSerial, true},
	{"489", []string{"serial"}, kindSerial, true},
	{"387", []string{"serial"}, kindSerial, true},
	{"9", []string{"serial"}, kindSerial, true},
	{"81", []string{"serial"}, kindSerial, true},
	{"915", []string{"serial"}, kindSerial, true},
	{"1939", []string{"serial"}, kindSerial, true},
	{"119", []string{"serial"}, kindSerial, true},
	{"1803", []string{"serial"}, kindSerial, true},
	{"266", []string{"serial"}, kindSerial, true},
	{"193", []string{"serial"}, kindSerial, true},
	{"1690", []string{"serial"}, kindSerial, true},
	{"1459", []string{"serial"}, kindSerial, true},
	{"825", []string{"serial"}, kindSerial, true},
	{"1248", []string{"serial"}, kindSerial, true},
	{"1288", []string{"serial"}, kindSerial, true},
	{"325", []string{"serial"}, kindSerial, true},
	{"534", []string{"serial"}, kindSerial, true},
	{"694", []string{"serial"}, kindSerial, true},
	{"704", []string{"serial"}, kindSerial, true},
	{"1105", []string{"anime"}, kindOther, true},
	{"2491", []string{"anime"}, kindOther, true},
	{"1389", []string{"anime"}, kindOther, true},
	{"709", []string{"documovie"}, kindMovie, false},
	{"2109", []string{"documovie"}, kindMovie, false},
	{"46", []string{"docuserial", "documovie"}, kindOther, false},
	{"671", []string{"docuserial", "documovie"}, kindOther, false},
	{"2177", []string{"docuserial", "documovie"}, kindOther, false},
	{"2538", []string{"docuserial", "documovie"}, kindOther, false},
	{"251", []string{"docuserial", "documovie"}, kindOther, false},
	{"98", []string{"docuserial", "documovie"}, kindOther, false},
	{"97", []string{"docuserial", "documovie"}, kindOther, false},
	{"851", []string{"docuserial", "documovie"}, kindOther, false},
	{"2178", []string{"docuserial", "documovie"}, kindOther, false},
	{"821", []string{"docuserial", "documovie"}, kindOther, false},
	{"2076", []string{"docuserial", "documovie"}, kindOther, false},
	{"56", []string{"docuserial", "documovie"}, kindOther, false},
	{"2123", []string{"docuserial", "documovie"}, kindOther, false},
	{"876", []string{"docuserial", "documovie"}, kindOther, false},
	{"2139", []string{"docuserial", "documovie"}, kindOther, false},
	{"1467", []string{"docuserial", "documovie"}, kindOther, false},
	{"1469", []string{"docuserial", "documovie"}, kindOther, false},
	{"249", []string{"docuserial", "documovie"}, kindOther, false},
	{"552", []string{"docuserial", "documovie"}, kindOther, false},
	{"500", []string{"docuserial", "documovie"}, kindOther, false},
	{"2112", []string{"docuserial", "documovie"}, kindOther, false},
	{"1327", []string{"docuserial", "documovie"}, kindOther, false},
	{"1468", []string{"docuserial", "documovie"}, kindOther, false},
	{"2168", []string{"docuserial", "documovie"}, kindOther, false},
	{"2160", []string{"docuserial", "documovie"}, kindOther, false},
	{"314", []string{"docuserial", "documovie"}, kindOther, false},
	{"1281", []string{"docuserial", "documovie"}, kindOther, false},
	{"2110", []string{"docuserial", "documovie"}, kindOther, false},
	{"979", []string{"docuserial", "documovie"}, kindOther, false},
	{"2169", []string{"docuserial", "documovie"}, kindOther, false},
	{"2164", []string{"docuserial", "documovie"}, kindOther, false},
	{"2166", []string{"docuserial", "documovie"}, kindOther, false},
	{"2163", []string{"docuserial", "documovie"}, kindOther, false},
	{"24", []string{"tvshow"}, kindOther, false},
	{"1959", []string{"tvshow"}, kindOther, false},
	{"939", []string{"tvshow"}, kindOther, false},
	{"1481", []string{"tvshow"}, kindOther, false},
	{"113", []string{"tvshow"}, kindOther, false},
	{"115", []string{"tvshow"}, kindOther, false},
	{"882", []string{"tvshow"}, kindOther, false},
	{"1482", []string{"tvshow"}, kindOther, false},
	{"393", []string{"tvshow"}, kindOther, false},
	{"2537", []string{"tvshow"}, kindOther, false},
	{"532", []string{"tvshow"}, kindOther, false},
	{"827", []string{"tvshow"}, kindOther, false},
	{"1392", []string{"sport"}, kindOther, false},
	{"2475", []string{"sport"}, kindOther, false},
	{"2493", []string{"sport"}, kindOther, false},
	{"2113", []string{"sport"}, kindOther, false},
	{"2482", []string{"sport"}, kindOther, false},
	{"2103", []string{"sport"}, kindOther, false},
	{"2522", []string{"sport"}, kindOther, false},
	{"2485", []string{"sport"}, kindOther, false},
	{"2486", []string{"sport"}, kindOther, false},
	{"2479", []string{"sport"}, kindOther, false},
	{"2089", []string{"sport"}, kindOther, false},
	{"1794", []string{"sport"}, kindOther, false},
	{"845", []string{"sport"}, kindOther, false},
	{"2312", []string{"sport"}, kindOther, false},
	{"343", []string{"sport"}, kindOther, false},
	{"2111", []string{"sport"}, kindOther, false},
	{"1527", []string{"sport"}, kindOther, false},
	{"2069", []string{"sport"}, kindOther, false},
	{"1323", []string{"sport"}, kindOther, false},
	{"2009", []string{"sport"}, kindOther, false},
	{"2000", []string{"sport"}, kindOther, false},
	{"2010", []string{"sport"}, kindOther, false},
	{"2006", []string{"sport"}, kindOther, false},
	{"2007", []string{"sport"}, kindOther, false},
	{"2005", []string{"sport"}, kindOther, false},
	{"259", []string{"sport"}, kindOther, false},
	{"2004", []string{"sport"}, kindOther, false},
	{"1999", []string{"sport"}, kindOther, false},
	{"2001", []string{"sport"}, kindOther, false},
	{"2002", []string{"sport"}, kindOther, false},
	{"283", []string{"sport"}, kindOther, false},
	{"1997", []string{"sport"}, kindOther, false},
	{"2003", []string{"sport"}, kindOther, false},
	{"1608", []string{"sport"}, kindOther, false},
	{"1609", []string{"sport"}, kindOther, false},
	{"2294", []string{"sport"}, kindOther, false},
	{"1229", []string{"sport"}, kindOther, false},
	{"1693", []string{"sport"}, kindOther, false},
	{"2532", []string{"sport"}, kindOther, false},
	{"136", []string{"sport"}, kindOther, false},
	{"592", []string{"sport"}, kindOther, false},
	{"2533", []string{"sport"}, kindOther, false},
	{"1952", []string{"sport"}, kindOther, false},
	{"1621", []string{"sport"}, kindOther, false},
	{"2075", []string{"sport"}, kindOther, false},
	{"1668", []string{"sport"}, kindOther, false},
	{"1613", []string{"sport"}, kindOther, false},
	{"1614", []string{"sport"}, kindOther, false},
	{"1623", []string{"sport"}, kindOther, false},
	{"1615", []string{"sport"}, kindOther, false},
	{"1630", []string{"sport"}, kindOther, false},
	{"2425", []string{"sport"}, kindOther, false},
	{"2514", []string{"sport"}, kindOther, false},
	{"1616", []string{"sport"}, kindOther, false},
	{"2014", []string{"sport"}, kindOther, false},
	{"1442", []string{"sport"}, kindOther, false},
	{"1491", []string{"sport"}, kindOther, false},
	{"1987", []string{"sport"}, kindOther, false},
	{"1617", []string{"sport"}, kindOther, false},
	{"1620", []string{"sport"}, kindOther, false},
	{"1998", []string{"sport"}, kindOther, false},
	{"1343", []string{"sport"}, kindOther, false},
	{"751", []string{"sport"}, kindOther, false},
	{"1697", []string{"sport"}, kindOther, false},
	{"255", []string{"sport"}, kindOther, false},
	{"260", []string{"sport"}, kindOther, false},
	{"261", []string{"sport"}, kindOther, false},
	{"256", []string{"sport"}, kindOther, false},
	{"1986", []string{"sport"}, kindOther, false},
	{"660", []string{"sport"}, kindOther, false},
	{"1551", []string{"sport"}, kindOther, false},
	{"626", []string{"sport"}, kindOther, false},
	{"262", []string{"sport"}, kindOther, false},
	{"1326", []string{"sport"}, kindOther, false},
	{"978", []string{"sport"}, kindOther, false},
	{"1287", []string{"sport"}, kindOther, false},
	{"1188", []string{"sport"}, kindOther, false},
	{"1667", []string{"sport"}, kindOther, false},
	{"1675", []string{"sport"}, kindOther, false},
	{"257", []string{"sport"}, kindOther, false},
	{"875", []string{"sport"}, kindOther, false},
	{"263", []string{"sport"}, kindOther, false},
	{"2073", []string{"sport"}, kindOther, false},
	{"550", []string{"sport"}, kindOther, false},
	{"2124", []string{"sport"}, kindOther, false},
	{"1470", []string{"sport"}, kindOther, false},
	{"528", []string{"sport"}, kindOther, false},
	{"486", []string{"sport"}, kindOther, false},
	{"854", []string{"sport"}, kindOther, false},
	{"2079", []string{"sport"}, kindOther, false},
	{"1336", []string{"sport"}, kindOther, false},
	{"2171", []string{"sport"}, kindOther, false},
	{"1339", []string{"sport"}, kindOther, false},
	{"2455", []string{"sport"}, kindOther, false},
	{"1434", []string{"sport"}, kindOther, false},
	{"2350", []string{"sport"}, kindOther, false},
	{"1472", []string{"sport"}, kindOther, false},
	{"2068", []string{"sport"}, kindOther, false},
	{"2016", []string{"sport"}, kindOther, false},
	{"4", []string{"multfilm"}, kindMovie, true},      // added 2026-09-25
	{"7", []string{"movie"}, kindMovie, true},         // added 2026-09-25
	{"33", []string{"anime"}, kindOther, true},        // added 2026-09-25
	{"84", []string{"multfilm"}, kindMovie, true},     // added 2026-09-25
	{"100", []string{"movie"}, kindMovie, true},       // added 2026-09-25
	{"101", []string{"movie"}, kindMovie, true},       // added 2026-09-25
	{"173", []string{"serial"}, kindSerial, true},     // added 2026-09-25
	{"189", []string{"serial"}, kindSerial, true},     // added 2026-09-25
	{"271", []string{"movie"}, kindMovie, true},       // added 2026-09-25
	{"272", []string{"movie"}, kindMovie, true},       // added 2026-09-25
	{"498", []string{"multserial"}, kindSerial, true}, // added 2026-09-25
	{"572", []string{"movie"}, kindMovie, true},       // added 2026-09-25
	{"625", []string{"serial"}, kindSerial, true},     // added 2026-09-25
	{"717", []string{"serial"}, kindOther, true},      // added 2026-09-25
	{"718", []string{"movie"}, kindMovie, true},       // added 2026-09-25
	{"775", []string{"movie"}, kindMovie, true},       // added 2026-09-25
	{"812", []string{"serial"}, kindSerial, true},     // added 2026-09-25
	{"820", []string{"serial"}, kindOther, true},      // added 2026-09-25
	{"911", []string{"serial"}, kindSerial, true},     // added 2026-09-25
	{"920", []string{"serial"}, kindSerial, true},     // added 2026-09-25
	{"1106", []string{"anime"}, kindOther, true},      // added 2026-09-25
	{"1171", []string{"serial"}, kindSerial, true},    // added 2026-09-25
	{"1202", []string{"documovie"}, kindMovie, false}, // added 2026-09-25
	{"1242", []string{"serial"}, kindOther, true},     // added 2026-09-25
	{"1463", []string{"serial"}, kindSerial, true},    // added 2026-09-25
	{"1543", []string{"movie"}, kindMovie, true},      // added 2026-09-25
	{"1577", []string{"multfilm"}, kindMovie, true},   // added 2026-09-25
	{"1669", []string{"serial"}, kindSerial, true},    // added 2026-09-25
	{"1940", []string{"movie"}, kindMovie, true},      // added 2026-09-25
	{"1949", []string{"serial"}, kindSerial, true},    // added 2026-09-25
	{"1985", []string{"documovie"}, kindMovie, false}, // added 2026-09-25
	{"2100", []string{"serial"}, kindSerial, true},    // added 2026-09-25
	{"2366", []string{"serial"}, kindSerial, true},    // added 2026-09-25
	{"2393", []string{"serial"}, kindSerial, true},    // added 2026-09-25
	{"2412", []string{"serial"}, kindOther, true},     // added 2026-09-25
}

var (
	firstPageCats []string // forums the hourly Parse visits
	allTaskCats   []string // every forum, for ParseAllTask
)

var movieCats, serialCats, otherNamedCats = map[string]bool{}, map[string]bool{}, map[string]bool{}
var categoryTypeMap = map[string][]string{}

func init() {
	for _, c := range rutrackerCategories {
		if _, dup := categoryTypeMap[c.id]; dup {
			panic("rutracker: duplicate category " + c.id)
		}
		if len(c.types) == 0 {
			panic("rutracker: category " + c.id + " has no types — every row would be dropped")
		}
		allTaskCats = append(allTaskCats, c.id)
		if c.quick {
			firstPageCats = append(firstPageCats, c.id)
		}
		categoryTypeMap[c.id] = c.types
		switch c.kind {
		case kindMovie:
			movieCats[c.id] = true
		case kindSerial:
			serialCats[c.id] = true
		default:
			otherNamedCats[c.id] = true
		}
	}
}

func categoryTypes(cat string) []string {
	v := categoryTypeMap[cat]
	if len(v) == 0 {
		return nil
	}
	out := make([]string, len(v))
	copy(out, v)
	return out
}

func fileTime(t filedb.TorrentDetails) time.Time {
	if tm, ok := t["updateTime"].(time.Time); ok {
		return tm
	}
	return time.Now()
}
func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case fmt.Stringer:
		return x.String()
	case nil:
		return ""
	default:
		if v == nil {
			return ""
		}
		return fmt.Sprint(v)
	}
}
func asInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case json.Number:
		i, _ := x.Int64()
		return int(i)
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(x))
		return i
	default:
		return 0
	}
}
func isDisabled(list []string, name string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), name) {
			return true
		}
	}
	return false
}
