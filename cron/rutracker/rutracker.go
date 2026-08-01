package rutracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
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

var firstPageCats = []string{"549", "22", "1666", "941", "1950", "2090", "2221", "2091", "2092", "2093", "2200", "2540", "934", "505", "252", "124", "1213", "2343", "930", "2365", "208", "539", "209", "921", "815", "1460", "1457", "2199", "313", "312", "1247", "2201", "2339", "140", "842", "235", "242", "819", "1531", "721", "1102", "1120", "1214", "489", "387", "9", "81", "915", "1939", "119", "1803", "266", "193", "1690", "1459", "825", "1248", "1288", "325", "534", "694", "704", "1105", "2491", "1389"}
var allTaskCats = []string{"549", "22", "1666", "941", "1950", "2090", "2221", "2091", "2092", "2093", "2200", "2540", "934", "505", "252", "124", "1213", "2343", "930", "2365", "208", "539", "209", "921", "815", "1460", "1457", "2199", "313", "312", "1247", "2201", "2339", "140", "842", "235", "242", "819", "1531", "721", "1102", "1120", "1214", "489", "387", "9", "81", "915", "1939", "119", "1803", "266", "193", "1690", "1459", "825", "1248", "1288", "325", "534", "694", "704", "1105", "2491", "1389", "709", "2109", "46", "671", "2177", "2538", "251", "98", "97", "851", "2178", "821", "2076", "56", "2123", "876", "2139", "1467", "1469", "249", "552", "500", "2112", "1327", "1468", "2168", "2160", "314", "1281", "2110", "979", "2169", "2164", "2166", "2163", "24", "1959", "939", "1481", "113", "115", "882", "1482", "393", "2537", "532", "827", "1392", "2475", "2493", "2113", "2482", "2103", "2522", "2485", "2486", "2479", "2089", "1794", "845", "2312", "343", "2111", "1527", "2069", "1323", "2009", "2000", "2010", "2006", "2007", "2005", "259", "2004", "1999", "2001", "2002", "283", "1997", "2003", "1608", "1609", "2294", "1229", "1693", "2532", "136", "592", "2533", "1952", "1621", "2075", "1668", "1613", "1614", "1623", "1615", "1630", "2425", "2514", "1616", "2014", "1442", "1491", "1987", "1617", "1620", "1998", "1343", "751", "1697", "255", "260", "261", "256", "1986", "660", "1551", "626", "262", "1326", "978", "1287", "1188", "1667", "1675", "257", "875", "263", "2073", "550", "2124", "1470", "528", "486", "854", "2079", "1336", "2171", "1339", "2455", "1434", "2350", "1472", "2068", "2016"}

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
	Config   app.Config
	DB       *filedb.DB
	DataDir  string
	Fetcher  *core.Fetcher
	loc      *time.Location
	mu       sync.Mutex
	working  bool
	allWork  bool
	latestMu sync.Mutex
	tasks    map[string][]Task
	cookieMu sync.Mutex
	cookie   string
	cookieT  time.Time
	domain   string
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
	// User-Agent that earned it. The POST itself stays on a raw http.Client
	// (not Fetcher) because bb_session arrives in Set-Cookie on the 302 and
	// FetchResult carries only body + status.
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
	if ua == "" {
		ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"
	}

	loginClient := &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	form := url.Values{
		"login_username": {p.Config.Rutracker.Login.U},
		"login_password": {p.Config.Rutracker.Login.P},
		"login":          {"\xc2\xf5\xee\xe4"}, // "Вход" in CP1251
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, loginURL, strings.NewReader(form.Encode()))
	if err != nil {
		log.Printf("rutracker: login request error: %v", err)
		return false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", ua)
	if postCookie != "" {
		req.Header.Set("Cookie", postCookie)
	}
	req.Header.Set("Referer", loginURL)
	req.Header.Set("Origin", host)
	resp, err := loginClient.Do(req)
	if err != nil {
		log.Printf("rutracker: login HTTP error: %v", err)
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	log.Printf("rutracker: login response status=%d cf-mitigated=%q", resp.StatusCode, resp.Header.Get("Cf-Mitigated"))

	var parts []string
	for _, line := range resp.Header.Values("Set-Cookie") {
		parts = append(parts, strings.SplitN(line, ";", 2)[0])
	}
	cookieStr := strings.Join(parts, "; ")
	if strings.Contains(cookieStr, "bb_session") {
		// Keep cf_clearance alongside bb_session: listing fetches reuse this
		// saved string, and an auth-only cookie would get bounced at the edge.
		merged := cookieStr
		if postCookie != "" {
			merged = core.MergeCookieStrings(postCookie, cookieStr)
		}
		p.cookieMu.Lock()
		p.cookie = merged
		p.cookieT = time.Now()
		p.cookieMu.Unlock()
		_ = core.DefaultSessionStore().SaveAuth(p.domain, merged)
		log.Printf("rutracker: login OK, got bb_session")
		return true
	}
	// Distinguish "Cloudflare never let us reach phpBB" from "rutracker
	// rejected these credentials" — they need completely different fixes.
	if resp.Header.Get("Cf-Mitigated") == "challenge" || looksLikeCFChallenge(decodeRutrackerBody(body)) {
		log.Printf("rutracker: login BLOCKED by cloudflare challenge (credentials were never checked)")
		return false
	}
	log.Printf("rutracker: login FAILED — no bb_session in cookies: %s", cookieStr)
	return false
}

func (p *Parser) ensureLogin(ctx context.Context) bool {
	if p.getCookie() != "" {
		return true
	}
	return p.takeLogin(ctx)
}

func (p *Parser) Parse(ctx context.Context, page int) (ParseResult, error) {
	p.mu.Lock()
	if p.working {
		p.mu.Unlock()
		return ParseResult{Status: "work"}, nil
	}
	p.working = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.working = false; p.mu.Unlock() }()
	if isDisabled(p.Config.DisableTrackers, trackerName) {
		return ParseResult{Status: "disabled"}, nil
	}
	if !p.ensureLogin(ctx) {
		return ParseResult{Status: "login failed"}, nil
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
		return nil, fmt.Errorf("login failed")
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
		p.tasks[cat] = merged
	}
	if err := p.saveTasksLocked(); err != nil {
		return nil, err
	}
	return cloneTasks(p.tasks), nil
}

func (p *Parser) ParseAllTask(ctx context.Context, force bool) (string, error) {
	if !p.ensureLogin(ctx) {
		return "login failed", nil
	}
	p.mu.Lock()
	if p.allWork {
		p.mu.Unlock()
		return "work", nil
	}
	p.allWork = true
	snapshot := cloneTasks(p.tasks)
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.allWork = false; p.mu.Unlock() }()

	if len(snapshot) == 0 {
		log.Printf("rutracker: parsealltask — tasks empty, running updatetasksparse first")
		if _, err := p.UpdateTasksParse(ctx); err != nil {
			return "", err
		}
		p.mu.Lock()
		snapshot = cloneTasks(p.tasks)
		p.mu.Unlock()
	}

	totalPages := 0
	for _, list := range snapshot {
		totalPages += len(list)
	}
	processed, fetched, added, updated, skipped, failed, errs := 0, 0, 0, 0, 0, 0, 0
	for cat, list := range snapshot {
		for _, task := range list {
			if !force && task.UpdatedToday(p.loc) {
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
				errs++
				continue
			}
			processed++
			if len(items) == 0 {
				log.Printf("rutracker: parsealltask cat=%s page=%d empty (marking today)", cat, task.Page)
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
		}
	}
	log.Printf("rutracker: parsealltask done processed=%d/%d fetched=%d added=%d updated=%d skipped=%d failed=%d errors=%d", processed, totalPages, fetched, added, updated, skipped, failed, errs)
	return "ok", nil
}

func (p *Parser) ParseLatest(ctx context.Context, pages int) (string, error) {
	if !p.latestMu.TryLock() {
		return "work", nil
	}
	defer p.latestMu.Unlock()
	if !p.ensureLogin(ctx) {
		return "login failed", nil
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

var movieCats, serialCats, otherNamedCats = map[string]bool{}, map[string]bool{}, map[string]bool{}
var categoryTypeMap = map[string][]string{}

func init() {
	for _, cat := range []string{"549", "22", "1666", "941", "1950", "2090", "2221", "2091", "2092", "2093", "2200", "2540", "934", "505", "124", "1457", "2199", "313", "312", "1247", "2201", "2339", "140", "252"} {
		movieCats[cat] = true
		categoryTypeMap[cat] = []string{"movie"}
	}
	for _, cat := range []string{"2343", "930", "2365", "208", "539", "209", "1213"} {
		movieCats[cat] = true
		categoryTypeMap[cat] = []string{"multfilm"}
	}
	for _, cat := range []string{"921", "815", "1460"} {
		serialCats[cat] = true
		categoryTypeMap[cat] = []string{"multserial"}
	}
	for _, cat := range []string{"842", "235", "242", "819", "1531", "721", "1102", "1120", "1214", "489", "387", "9", "81", "119", "1803", "266", "193", "1690", "1459", "825", "1248", "1288", "325", "534", "694", "704", "915", "1939"} {
		serialCats[cat] = true
		categoryTypeMap[cat] = []string{"serial"}
	}
	for _, cat := range []string{"1105", "2491", "1389"} {
		otherNamedCats[cat] = true
		categoryTypeMap[cat] = []string{"anime"}
	}
	for _, cat := range []string{"709", "2109"} {
		movieCats[cat] = true
		categoryTypeMap[cat] = []string{"documovie"}
	}
	for _, cat := range []string{"46", "671", "2177", "2538", "251", "98", "97", "851", "2178", "821", "2076", "56", "2123", "876", "2139", "1467", "1469", "249", "552", "500", "2112", "1327", "1468", "2168", "2160", "314", "1281", "2110", "979", "2169", "2164", "2166", "2163"} {
		otherNamedCats[cat] = true
		categoryTypeMap[cat] = []string{"docuserial", "documovie"}
	}
	for _, cat := range []string{"24", "1959", "939", "1481", "113", "115", "882", "1482", "393", "2537", "532", "827"} {
		otherNamedCats[cat] = true
		categoryTypeMap[cat] = []string{"tvshow"}
	}
	for _, cat := range []string{"1392", "2475", "2493", "2113", "2482", "2103", "2522", "2485", "2486", "2479", "2089", "1794", "845", "2312", "343", "2111", "1527", "2069", "1323", "2009", "2000", "2010", "2006", "2007", "2005", "259", "2004", "1999", "2001", "2002", "283", "1997", "2003", "1608", "1609", "2294", "1229", "1693", "2532", "136", "592", "2533", "1952", "1621", "2075", "1668", "1613", "1614", "1623", "1615", "1630", "2425", "2514", "1616", "2014", "1442", "1491", "1987", "1617", "1620", "1998", "1343", "751", "1697", "255", "260", "261", "256", "1986", "660", "1551", "626", "262", "1326", "978", "1287", "1188", "1667", "1675", "257", "875", "263", "2073", "550", "2124", "1470", "528", "486", "854", "2079", "1336", "2171", "1339", "2455", "1434", "2350", "1472", "2068", "2016"} {
		otherNamedCats[cat] = true
		categoryTypeMap[cat] = []string{"sport"}
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
