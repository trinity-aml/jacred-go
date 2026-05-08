package korsars

import (
	"context"
	"encoding/json"
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

const trackerName = "korsars"

// Forum IDs, hand-picked by the user.
var movieCats = []string{"282", "31", "33", "125", "146", "270"}
var serialCats = []string{"287", "286", "267", "303", "288", "39", "40", "300", "41", "121", "144", "271"}
var cartoonCats = []string{"43", "44", "277", "46", "272", "273"}

var allCats = func() []string {
	out := make([]string, 0, len(movieCats)+len(serialCats)+len(cartoonCats))
	out = append(out, movieCats...)
	out = append(out, serialCats...)
	out = append(out, cartoonCats...)
	return out
}()

// Listing row regexes. The site uses phpBB-mod markup similar to rutracker:
// each torrent is a <tr id="tr-{topicID}"> block containing the title link,
// magnet anchor, dl.php size link, seedmed/leechmed counters, and a
// <p>YYYY-MM-DD HH:MM</p> column.
var (
	rowDateRe    = regexp.MustCompile(`<p>([0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2})</p>`)
	rowTopicIDRe = regexp.MustCompile(`<a id="tt-([0-9]+)"`)
	rowTitleRe   = regexp.MustCompile(`<a id="tt-[0-9]+"[^>]+>\s*<b>([^<]+)</b>\s*</a>`)
	rowSidRe     = regexp.MustCompile(`<span class="seedmed"[^>]*><b>([0-9]+)</b>`)
	rowPirRe     = regexp.MustCompile(`<span class="leechmed"[^>]*><b>([0-9]+)</b>`)
	rowSizeRe    = regexp.MustCompile(`href="\./dl\.php\?id=[0-9]+"[^>]*>([^<]+)</a>`)
	rowMagnetRe  = regexp.MustCompile(`href="(magnet:[^"]+)"`)

	// Pagination link with the largest start= value tells us the last page;
	// listings paginate in steps of 50 topics.
	pagerStartRe = regexp.MustCompile(`viewforum\.php\?f=[0-9]+(?:&amp;|&)start=([0-9]+)`)

	// Title patterns. Korsars titles follow a single shape:
	//   "RUS [/ ALT-RUS] / EN [Sxx[-yy]] (YEAR[-YEAR]) ..."
	// Series carry a [Sxx] / [Sxx-yy] block before the year; films don't.
	yearRe         = regexp.MustCompile(`\(([0-9]{4})`)
	titleSerial3Re = regexp.MustCompile(`^([^/\[\(]+) / [^/\[\(]+ / ([^/\[\(]+) \[S[0-9]`)
	titleSerial2Re = regexp.MustCompile(`^([^/\[\(]+) / ([^/\[\(]+) \[S[0-9]`)
	titleSerial1Re = regexp.MustCompile(`^([^/\[\(]+) \[S[0-9]`)
	titleMovie3Re  = regexp.MustCompile(`^([^/\(]+) / [^/\(]+ / ([^/\(]+) \(`)
	titleMovie2Re  = regexp.MustCompile(`^([^/\(]+) / ([^/\(]+) \(`)
	titleMovie1Re  = regexp.MustCompile(`^([^/\(]+) \(`)

	firstNamePart  = regexp.MustCompile(`(\[|/|\(|\|)`)
	spaceCleanupRe = regexp.MustCompile(`[\n\r\t\x{00A0} ]+`)
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
	loc, _ := time.LoadLocation("Europe/Moscow")
	if loc == nil {
		loc = time.Local
	}
	p := &Parser{
		Config:  cfg,
		DB:      db,
		DataDir: dataDir,
		Fetcher: core.NewFetcher(cfg),
		loc:     loc,
		tasks:   map[string][]Task{},
		domain:  core.DomainFromHost(cfg.Korsars.Host),
	}
	_ = p.loadTasks()
	if saved, savedT := core.DefaultSessionStore().LoadAuth(p.domain); saved != "" && time.Since(savedT) < 24*time.Hour {
		p.cookie = saved
		p.cookieT = savedT
		log.Printf("korsars: loaded saved cookie from disk (age=%s)", time.Since(savedT).Round(time.Second))
	}
	return p
}

func (p *Parser) UpdateConfig(cfg app.Config) {
	p.Config = cfg
	p.domain = core.DomainFromHost(cfg.Korsars.Host)
}

// invalidateCookie drops the in-memory + on-disk auth cookie so the next
// fetch triggers a fresh takeLogin. Called when the listing returns the
// login form (session expired).
func (p *Parser) invalidateCookie() {
	p.cookieMu.Lock()
	p.cookie = ""
	p.cookieT = time.Time{}
	p.cookieMu.Unlock()
	if p.domain != "" {
		_ = core.DefaultSessionStore().DeleteAuth(p.domain)
	}
}

func (p *Parser) getCookie() string {
	p.cookieMu.Lock()
	defer p.cookieMu.Unlock()
	if p.cookie != "" && time.Since(p.cookieT) < 24*time.Hour {
		return p.cookie
	}
	return ""
}

// takeLogin POSTs login_username/login_password to /login.php. Korsars uses
// phpBB-style sessions; the response sets a single bb_data cookie carrying
// uid+sid+uk in URL-encoded PHP serialize. No CSRF token — just the form.
func (p *Parser) takeLogin(ctx context.Context) bool {
	host := strings.TrimRight(p.Config.Korsars.Host, "/")
	if host == "" || p.Config.Korsars.Login.U == "" {
		log.Println("korsars: login skipped — no host or login configured")
		return false
	}
	log.Printf("korsars: attempting login to %s as %s", host, p.Config.Korsars.Login.U)
	loginClient := &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	form := url.Values{
		"login_username": {p.Config.Korsars.Login.U},
		"login_password": {p.Config.Korsars.Login.P},
		"autologin":      {"1"},
		"login":          {"Вход"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/login.php", strings.NewReader(form.Encode()))
	if err != nil {
		log.Printf("korsars: login request error: %v", err)
		return false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Referer", host+"/")
	resp, err := loginClient.Do(req)
	if err != nil {
		log.Printf("korsars: login HTTP error: %v", err)
		return false
	}
	defer resp.Body.Close()
	log.Printf("korsars: login response status=%d", resp.StatusCode)

	var parts []string
	for _, line := range resp.Header.Values("Set-Cookie") {
		parts = append(parts, strings.SplitN(line, ";", 2)[0])
	}
	cookieStr := strings.Join(parts, "; ")
	if !strings.Contains(cookieStr, "bb_data") {
		log.Printf("korsars: login FAILED — no bb_data in cookies: %s", cookieStr)
		return false
	}
	p.cookieMu.Lock()
	p.cookie = cookieStr
	p.cookieT = time.Now()
	p.cookieMu.Unlock()
	_ = core.DefaultSessionStore().SaveAuth(p.domain, cookieStr)
	log.Printf("korsars: login OK, got bb_data")
	return true
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
	seenURLs := map[string]struct{}{}
	log.Printf("korsars: starting parse, %d categories, page=%d", len(allCats), page)
	for _, cat := range allCats {
		items, err := p.parsePage(ctx, cat, page)
		if err != nil {
			log.Printf("korsars: cat %s error: %v (continuing)", cat, err)
			continue
		}
		res.Fetched += len(items)
		res.PerCategory[cat] = len(items)
		if len(items) == 0 {
			continue
		}
		a, u, s, d, f, err := p.saveTorrents(ctx, items, seenURLs)
		if err != nil {
			log.Printf("korsars: cat %s save error: %v (continuing)", cat, err)
			continue
		}
		res.Added += a
		res.Updated += u
		res.Skipped += s
		res.Duplicates += d
		res.Failed += f
	}
	log.Printf("korsars: done fetched=%d added=%d updated=%d skipped=%d duplicates=%d failed=%d", res.Fetched, res.Added, res.Updated, res.Skipped, res.Duplicates, res.Failed)
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
	for _, cat := range allCats {
		htmlBody, err := p.fetchCategoryRoot(ctx, cat)
		if err != nil {
			continue
		}
		maxStart := 0
		for _, m := range pagerStartRe.FindAllStringSubmatch(htmlBody, -1) {
			if v, _ := strconv.Atoi(m[1]); v > maxStart {
				maxStart = v
			}
		}
		maxPages := maxStart / 50
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
		log.Printf("korsars: parsealltask — tasks empty, running updatetasksparse first")
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
			if p.Config.Korsars.ParseDelay > 0 {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(time.Duration(p.Config.Korsars.ParseDelay) * time.Millisecond):
				}
			}
			items, err := p.parsePage(ctx, cat, task.Page)
			if err != nil {
				log.Printf("korsars: parsealltask cat=%s page=%d error: %v", cat, task.Page, err)
				errs++
				continue
			}
			processed++
			if len(items) == 0 {
				log.Printf("korsars: parsealltask cat=%s page=%d empty (marking today)", cat, task.Page)
				p.markTaskToday(cat, task.Page)
				continue
			}
			a, u, s, _, f, err := p.saveTorrents(ctx, items, nil)
			if err != nil {
				log.Printf("korsars: parsealltask cat=%s page=%d save error: %v", cat, task.Page, err)
				errs++
				continue
			}
			fetched += len(items)
			added += a
			updated += u
			skipped += s
			failed += f
			log.Printf("korsars: parsealltask cat=%s page=%d fetched=%d added=%d skipped=%d failed=%d", cat, task.Page, len(items), a, s, f)
			p.markTaskToday(cat, task.Page)
		}
	}
	log.Printf("korsars: parsealltask done processed=%d/%d fetched=%d added=%d updated=%d skipped=%d failed=%d errors=%d", processed, totalPages, fetched, added, updated, skipped, failed, errs)
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
			if p.Config.Korsars.ParseDelay > 0 {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(time.Duration(p.Config.Korsars.ParseDelay) * time.Millisecond):
				}
			}
			items, err := p.parsePage(ctx, cat, task.Page)
			if err != nil {
				log.Printf("korsars: parselatest cat=%s page=%d error: %v", cat, task.Page, err)
				errs++
				continue
			}
			processed++
			if len(items) == 0 {
				log.Printf("korsars: parselatest cat=%s page=%d empty (marking today)", cat, task.Page)
				p.markTaskToday(cat, task.Page)
				continue
			}
			a, u, s, _, f, err := p.saveTorrents(ctx, items, nil)
			if err != nil {
				log.Printf("korsars: parselatest cat=%s page=%d save error: %v", cat, task.Page, err)
				errs++
				continue
			}
			fetched += len(items)
			added += a
			updated += u
			skipped += s
			failed += f
			log.Printf("korsars: parselatest cat=%s page=%d fetched=%d added=%d skipped=%d failed=%d", cat, task.Page, len(items), a, s, f)
			p.markTaskToday(cat, task.Page)
			lines = append(lines, fmt.Sprintf("%s - %d", cat, task.Page))
		}
	}
	log.Printf("korsars: parselatest done processed=%d fetched=%d added=%d updated=%d skipped=%d failed=%d errors=%d", processed, fetched, added, updated, skipped, failed, errs)
	if len(lines) == 0 {
		return "ok", nil
	}
	return strings.Join(lines, "\n"), nil
}

func (p *Parser) markTaskToday(cat string, page int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if list, ok := p.tasks[cat]; ok {
		for i := range list {
			if list[i].Page == page {
				list[i].MarkToday(p.loc)
			}
		}
		p.tasks[cat] = list
	}
	_ = p.saveTasksLocked()
}

func (p *Parser) parsePage(ctx context.Context, cat string, page int) ([]filedb.TorrentDetails, error) {
	htmlBody, err := p.fetchPage(ctx, cat, page)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(htmlBody) == "" {
		return nil, nil
	}
	// Detect session expiry: if listings come back as the login form, drop
	// our cookie so the next ensureLogin re-authenticates.
	if strings.Contains(htmlBody, `name="login_username"`) && !strings.Contains(htmlBody, `id="tt-`) {
		log.Printf("korsars: cat=%s page=%d returned login form — invalidating session", cat, page)
		p.invalidateCookie()
		return nil, nil
	}
	host := strings.TrimRight(p.Config.Korsars.Host, "/")
	rows := strings.Split(htmlBody, `id="tt-`)
	out := make([]filedb.TorrentDetails, 0, len(rows))
	for _, row := range rows[1:] {
		// Re-prefix so `rowTopicIDRe` (which expects the full marker) matches.
		row = `<a id="tt-` + row
		id := match1(rowTopicIDRe, row)
		title := strings.TrimSpace(spaceCleanupRe.ReplaceAllString(html.UnescapeString(stripTags(match1(rowTitleRe, row))), " "))
		if id == "" || title == "" {
			continue
		}
		createTime, _ := time.ParseInLocation("2006-01-02 15:04", match1(rowDateRe, row), p.loc)
		if createTime.IsZero() {
			continue
		}
		sid, _ := strconv.Atoi(match1(rowSidRe, row))
		pir, _ := strconv.Atoi(match1(rowPirRe, row))
		sizeName := strings.TrimSpace(strings.ReplaceAll(html.UnescapeString(match1(rowSizeRe, row)), "&nbsp;", " "))
		magnet := html.UnescapeString(match1(rowMagnetRe, row))
		if sizeName == "" || magnet == "" {
			continue
		}
		types := categoryTypes(cat)
		if len(types) == 0 {
			continue
		}
		name, original, year := parseTitle(title)
		if strings.TrimSpace(name) == "" {
			name = firstTokenTitle(title)
		}
		if strings.TrimSpace(name) == "" {
			continue
		}
		out = append(out, filedb.TorrentRecord{
			TrackerName:  trackerName,
			Types:        types,
			URL:          host + "/viewtopic.php?t=" + id,
			Title:        title,
			Sid:          sid,
			Pir:          pir,
			SizeName:     sizeName,
			CreateTime:   createTime.UTC().Format(time.RFC3339Nano),
			Name:         name,
			OriginalName: original,
			Relased:      year,
			Magnet:       magnet,
		}.ToMap())
	}
	return out, nil
}

func (p *Parser) saveTorrents(ctx context.Context, torrents []filedb.TorrentDetails, seenURLs map[string]struct{}) (int, int, int, int, int, error) {
	added, updated, skipped, duplicates, failed := 0, 0, 0, 0, 0
	plog := core.NewParserLog(trackerName, filepath.Join(p.DB.DataDir, "log"), p.Config.LogParsers && p.Config.Korsars.Log)
	bucketCache := map[string]map[string]filedb.TorrentDetails{}
	changed := map[string]time.Time{}
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
			skipped++
			continue
		}
		if _, seen := seenURLs[urlv]; seen {
			duplicates++
			continue
		}
		seenURLs[urlv] = struct{}{}
		existing, exists := bucket[urlv]
		if !exists {
			if oldURL, found := filedb.FindByTrackerID(bucket, trackerName, urlv); found {
				existing = bucket[oldURL]
				delete(bucket, oldURL)
				exists = true
			}
		}
		if strings.TrimSpace(asString(incoming["magnet"])) == "" {
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
	for key, when := range changed {
		if err := p.DB.SaveBucket(key, bucketCache[key], when); err != nil {
			return added, updated, skipped, duplicates, failed, err
		}
	}
	return added, updated, skipped, duplicates, failed, nil
}

func (p *Parser) fetchCategoryRoot(ctx context.Context, cat string) (string, error) {
	return p.fetch(ctx, fmt.Sprintf("%s/viewforum.php?f=%s", requestHost(p.Config.Korsars), cat))
}

func (p *Parser) fetchPage(ctx context.Context, cat string, page int) (string, error) {
	u := fmt.Sprintf("%s/viewforum.php?f=%s", requestHost(p.Config.Korsars), cat)
	if page > 0 {
		u += fmt.Sprintf("&start=%d", page*50)
	}
	return p.fetch(ctx, u)
}

func (p *Parser) fetch(ctx context.Context, rawURL string) (string, error) {
	ts := p.Config.Korsars
	if c := p.getCookie(); c != "" {
		ts.Cookie = c
	}
	data, _, err := p.Fetcher.Download(rawURL, ts)
	if err != nil {
		return "", err
	}
	// Korsars serves UTF-8 (per <meta charset>); no CP1251 decode here.
	return string(data), nil
}

func (p *Parser) loadTasks() error {
	path := filepath.Join(p.DataDir, "temp", "korsars_taskParse.json")
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
	path := filepath.Join(p.DataDir, "temp", "korsars_taskParse.json")
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

func stripTags(s string) string {
	for {
		start := strings.IndexByte(s, '<')
		if start < 0 {
			return s
		}
		end := strings.IndexByte(s[start:], '>')
		if end < 0 {
			return s[:start]
		}
		s = s[:start] + s[start+end+1:]
	}
}

func firstTokenTitle(title string) string {
	return strings.TrimSpace(firstNamePart.Split(title, 2)[0])
}

// parseTitle peels the leading "RUS / [ALT-RUS / ] EN" name block off a
// listing title and returns (russian, english/original, year). Series and
// films share the same title shape; the optional `[Sxx[-yy]]` block sits
// between the names and the `(YEAR)` parenthesis.
func parseTitle(title string) (string, string, int) {
	year := 0
	if m := yearRe.FindStringSubmatch(title); len(m) > 1 {
		year, _ = strconv.Atoi(m[1])
	}
	if m := titleSerial3Re.FindStringSubmatch(title); len(m) == 3 {
		return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), year
	}
	if m := titleSerial2Re.FindStringSubmatch(title); len(m) == 3 {
		return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), year
	}
	if m := titleSerial1Re.FindStringSubmatch(title); len(m) == 2 {
		return strings.TrimSpace(m[1]), "", year
	}
	if m := titleMovie3Re.FindStringSubmatch(title); len(m) == 3 {
		return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), year
	}
	if m := titleMovie2Re.FindStringSubmatch(title); len(m) == 3 {
		return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), year
	}
	if m := titleMovie1Re.FindStringSubmatch(title); len(m) == 2 {
		return strings.TrimSpace(m[1]), "", year
	}
	return "", "", year
}

func requestHost(cfg app.TrackerSettings) string {
	if strings.TrimSpace(cfg.Alias) != "" {
		return strings.TrimSpace(cfg.Alias)
	}
	return strings.TrimSpace(cfg.Host)
}

var categoryTypeMap = func() map[string][]string {
	m := map[string][]string{}
	for _, c := range movieCats {
		m[c] = []string{"movie"}
	}
	for _, c := range serialCats {
		m[c] = []string{"serial"}
	}
	// Cartoon forums on korsars mix both films and series — emit both types
	// so search hits work either way.
	for _, c := range cartoonCats {
		m[c] = []string{"multfilm", "multserial"}
	}
	return m
}()

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
		return fmt.Sprint(v)
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
