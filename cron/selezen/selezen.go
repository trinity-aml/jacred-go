
package selezen

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

const trackerName = "selezen"
const selezenUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

var (
	rowSplitRe           = regexp.MustCompile(`card overflow-hidden`)
	cardURLTitleRe       = regexp.MustCompile(`<a href="(https?://[^"]+)"><h4 class="card-title">([^<]+)</h4>`)
	createTimeRe         = regexp.MustCompile(`class="bx bx-calendar"></span>\s*([0-9]{2}\.[0-9]{2}\.[0-9]{4} [0-9]{2}:[0-9]{2})</a>`)
	sidRe                = regexp.MustCompile(`<i class="bx bx-chevrons-up"></i>([0-9 ]+)`)
	pirRe                = regexp.MustCompile(`<i class="bx bx-chevrons-down"></i>([0-9 ]+)`)
	sizeNameRe           = regexp.MustCompile(`<span class="bx bx-download"></span>([^<]+)</a>`)
	magnetRe             = regexp.MustCompile(`href="(magnet:\?xt=urn:btih:[^"]+)"`)
	itemIDRe             = regexp.MustCompile(`/relizy-ot-selezen/(\d+)-`)
	movieMainRe          = regexp.MustCompile(`^([^/\(]+) / [^/]+ / ([^/\(]+) \(([0-9]{4})\)`)
	movieShortRe         = regexp.MustCompile(`^([^/\(]+) / ([^/\(]+) \(([0-9]{4})\)`)
	serialSeasonRe       = regexp.MustCompile(`\[S\d+\]`)
	serialEpisodeRe      = regexp.MustCompile(`\[\d+[xх]\d+`)
	cleanSpaceRe         = regexp.MustCompile(`[\t\r\n\x{00A0} ]+`)
	badAnimeMarkerRe     = regexp.MustCompile(`>Аниме</a>`)
	multMarkerRe         = regexp.MustCompile(`>Мульт|>мульт`)
	loginSessionCookieRe = regexp.MustCompile(`PHPSESSID=([^;]+)(;|$)`)
	// pagination: finds highest page number in DLE pager links
	pageLinksRe = regexp.MustCompile(`/relizy-ot-selezen/page/([0-9]+)/`)
)

type Task struct {
	UpdateTime string `json:"updateTime"`
	Page       int    `json:"page"`
}

func (t Task) UpdatedToday() bool {
	if strings.TrimSpace(t.UpdateTime) == "" {
		return false
	}
	tm, _ := time.Parse(time.RFC3339, t.UpdateTime)
	if tm.IsZero() {
		tm, _ = time.Parse("2006-01-02T15:04:05", t.UpdateTime)
	}
	if tm.IsZero() {
		return false
	}
	now := time.Now()
	y1, m1, d1 := tm.Date()
	y2, m2, d2 := now.Date()
	return y1 == y2 && m1 == m2 && d1 == d2
}

func (t *Task) MarkToday() {
	now := time.Now()
	t.UpdateTime = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).Format(time.RFC3339)
}

type Parser struct {
	Config  app.Config
	DB      *filedb.DB
	DataDir string
	Fetcher *core.Fetcher

	mu               sync.Mutex
	working          bool
	allWork          bool
	latestMu         sync.Mutex
	tasks            []Task
	cookieMu         sync.Mutex
	cookie           string
	lastLoginAttempt time.Time
	domain           string
}

type ParseResult struct {
	Status  string `json:"status"`
	Parsed  int    `json:"parsed"`
	Added   int    `json:"added"`
	Updated int    `json:"updated"`
	Skipped int    `json:"skipped"`
	Failed  int    `json:"failed"`
}

func New(cfg app.Config, db *filedb.DB, dataDir string) *Parser {
	p := &Parser{Config: cfg, DB: db, DataDir: dataDir, Fetcher: core.NewFetcher(cfg), domain: core.DomainFromHost(cfg.Selezen.Host)}
	_ = p.loadTasks()
	if saved, _ := core.DefaultSessionStore().LoadAuth(p.domain); saved != "" {
		p.cookie = saved
		log.Printf("selezen: loaded saved cookie from disk")
	}
	return p
}

func (p *Parser) Parse(ctx context.Context, parseFrom, parseTo int) (ParseResult, error) {
	p.mu.Lock()
	if p.working {
		p.mu.Unlock()
		return ParseResult{Status: "work"}, nil
	}
	p.working = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.working = false
		p.mu.Unlock()
	}()

	if isDisabled(p.Config.DisableTrackers, trackerName) {
		return ParseResult{Status: "disabled"}, nil
	}
	host := strings.TrimRight(strings.TrimSpace(p.Config.Selezen.Host), "/")
	if host == "" {
		return ParseResult{Status: "conf"}, nil
	}

	startPage, endPage := 1, 1
	if parseFrom > 0 {
		startPage = parseFrom
		endPage = parseFrom
	}
	if parseTo > 0 {
		endPage = parseTo
	}
	if startPage > endPage {
		startPage, endPage = endPage, startPage
	}

	res := ParseResult{Status: "ok"}
	for page := startPage; page <= endPage; page++ {
		if page > startPage && p.Config.Selezen.ParseDelay > 0 {
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-time.After(time.Duration(p.Config.Selezen.ParseDelay) * time.Millisecond):
			}
		}
		parsed, added, updated, skipped, failed, err := p.parsePage(ctx, page)
		if err != nil {
			return res, err
		}
		res.Parsed += parsed
		res.Added += added
		res.Updated += updated
		res.Skipped += skipped
		res.Failed += failed
	}
	log.Printf("selezen: done parsed=%d added=%d skipped=%d failed=%d", res.Parsed, res.Added, res.Skipped, res.Failed)
	return res, nil
}

func (p *Parser) parsePage(ctx context.Context, page int) (int, int, int, int, int, error) {
	cookie, err := p.ensureCookie(ctx)
	if err != nil || strings.TrimSpace(cookie) == "" {
		return 0, 0, 0, 0, 0, err
	}
	host := strings.TrimRight(strings.TrimSpace(p.Config.Selezen.Host), "/")
	listURL := host + "/relizy-ot-selezen/"
	if page > 1 {
		listURL = fmt.Sprintf("%s/relizy-ot-selezen/page/%d/", host, page)
	}
	body, err := p.fetchText(ctx, listURL, cookie, host+"/")
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}
	if body == "" || !strings.Contains(body, "dle_root") {
		return 0, 0, 0, 0, 0, nil
	}
	if loginUser := strings.TrimSpace(p.Config.Selezen.Login.U); loginUser != "" && !strings.Contains(body, ">"+loginUser+"<") {
		log.Printf("selezen: page=%d missing user marker (cookie expired, invalidating)", page)
		p.invalidateCookie()
		return 0, 0, 0, 0, 0, nil
	}
	torrents := parsePageHTML(body)
	if len(torrents) == 0 {
		return 0, 0, 0, 0, 0, nil
	}
	added, updated, skipped, failed, err := p.saveTorrents(ctx, cookie, torrents)
	return len(torrents), added, updated, skipped, failed, err
}

func parsePageHTML(htmlBody string) []filedb.TorrentDetails {
	rows := rowSplitRe.Split(replaceBadNames(htmlBody), -1)
	out := make([]filedb.TorrentDetails, 0, len(rows))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, row := range rows[1:] {
		if strings.TrimSpace(row) == "" || badAnimeMarkerRe.MatchString(row) {
			continue
		}
		createRaw := matchDecode(createTimeRe, row)
		createTime, err := time.ParseInLocation("02.01.2006 15:04", createRaw, time.UTC)
		if err != nil || createTime.IsZero() {
			continue
		}
		m := cardURLTitleRe.FindStringSubmatch(row)
		if len(m) < 3 {
			continue
		}
		urlv := strings.TrimSpace(m[1])
		title := strings.TrimSpace(html.UnescapeString(m[2]))
		if urlv == "" || !strings.Contains(strings.ToLower(urlv), ".html") || title == "" {
			continue
		}
		sidRaw := strings.ReplaceAll(matchDecode(sidRe, row), " ", "")
		pirRaw := strings.ReplaceAll(matchDecode(pirRe, row), " ", "")
		sizeName := strings.TrimSpace(strings.ReplaceAll(matchDecode(sizeNameRe, row), "&nbsp;", " "))
		if sidRaw == "" || pirRaw == "" || sizeName == "" {
			continue
		}
		name, original, relased := parseNames(title)
		if strings.TrimSpace(name) == "" {
			name = fallbackName(title)
		}
		if strings.TrimSpace(name) == "" {
			continue
		}
		sid, _ := strconv.Atoi(sidRaw)
		pir, _ := strconv.Atoi(pirRaw)
		out = append(out, filedb.TorrentRecord{
			TrackerName: trackerName,
			Types: typesForRow(row, title, urlv),
			URL: urlv,
			Title: title,
			Sid: sid,
			Pir: pir,
			SizeName: sizeName,
			CreateTime: createTime.UTC().Format(time.RFC3339Nano),
			UpdateTime: now,
			Name: strings.TrimSpace(name),
			OriginalName: strings.TrimSpace(original),
			Relased: relased,
			SearchName: core.SearchName(name),
			SearchOrig: core.SearchName(firstNonEmpty(original, name)),
		}.ToMap())
	}
	return out
}

func (p *Parser) saveTorrents(ctx context.Context, cookie string, torrents []filedb.TorrentDetails) (int, int, int, int, error) {
	added, updated, skipped, failed := 0, 0, 0, 0
	plog := core.NewParserLog(trackerName, filepath.Join(p.DB.DataDir, "log"), p.Config.LogParsers && p.Config.Selezen.Log)
	bucketCache := map[string]map[string]filedb.TorrentDetails{}
	changed := map[string]time.Time{}

	for _, incoming := range torrents {
		key := p.DB.KeyDb(asString(incoming["name"]), asString(incoming["originalname"]))
		if strings.TrimSpace(key) == "" || key == ":" {
			skipped++
			continue
		}
		bucket, ok := bucketCache[key]
		if !ok {
			loaded, err := p.DB.OpenReadOrEmpty(key)
			if err != nil {
				return added, updated, skipped, failed, err
			}
			bucket = loaded
			bucketCache[key] = bucket
		}
		urlv := asString(incoming["url"])
		if strings.TrimSpace(urlv) == "" {
			skipped++
			continue
		}
		existing, exists := bucket[urlv]
		if !exists {
			if id := itemID(urlv); id != "" {
				for oldURL, oldItem := range bucket {
					if itemID(oldURL) == id {
						existing, exists = oldItem, true
						if strings.TrimSpace(asString(existing["magnet"])) != "" {
							incoming["magnet"] = existing["magnet"]
						}
						break
					}
				}
			}
		}
		if strings.TrimSpace(asString(incoming["magnet"])) == "" {
			magnet, err := p.fetchMagnet(ctx, cookie, urlv)
			if err != nil {
				failed++
				continue
			}
			if strings.TrimSpace(magnet) == "" {
				failed++
				continue
			}
			incoming["magnet"] = magnet
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
			return added, updated, skipped, failed, err
		}
	}
	return added, updated, skipped, failed, nil
}

// invalidateCookie clears the in-memory and on-disk cookie and resets the
// login-attempt cooldown so the next ensureCookie call re-authenticates
// immediately. Used when a fetched page lacks the username marker — the only
// reliable signal that the saved cookie has expired server-side.
func (p *Parser) invalidateCookie() {
	p.cookieMu.Lock()
	p.cookie = ""
	p.lastLoginAttempt = time.Time{}
	p.cookieMu.Unlock()
	_ = core.DefaultSessionStore().DeleteAuth(p.domain)
}

func (p *Parser) ensureCookie(ctx context.Context) (string, error) {
	if cfg := strings.TrimSpace(p.Config.Selezen.Cookie); cfg != "" {
		return cfg, nil
	}
	p.cookieMu.Lock()
	if strings.TrimSpace(p.cookie) != "" {
		cookie := p.cookie
		p.cookieMu.Unlock()
		return cookie, nil
	}
	if time.Since(p.lastLoginAttempt) < 2*time.Minute {
		p.cookieMu.Unlock()
		return "", nil
	}
	p.lastLoginAttempt = time.Now()
	p.cookieMu.Unlock()

	cookie, err := p.takeLogin(ctx)
	if err != nil {
		return "", err
	}
	p.cookieMu.Lock()
	p.cookie = cookie
	p.cookieMu.Unlock()
	if cookie != "" {
		_ = core.DefaultSessionStore().SaveAuth(p.domain, cookie)
	}
	return cookie, nil
}

func (p *Parser) takeLogin(ctx context.Context) (string, error) {
	host := strings.TrimRight(strings.TrimSpace(p.Config.Selezen.Host), "/")
	if host == "" || strings.TrimSpace(p.Config.Selezen.Login.U) == "" || strings.TrimSpace(p.Config.Selezen.Login.P) == "" {
		return "", nil
	}
	vals := url.Values{}
	vals.Set("login_name", p.Config.Selezen.Login.U)
	vals.Set("login_password", p.Config.Selezen.Login.P)
	vals.Set("login_not_save", "1")
	vals.Set("login", "submit")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host, strings.NewReader(vals.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", selezenUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Referer", host+"/")
	req.Header.Set("Origin", host)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Use separate client with redirect disabled to capture Set-Cookie from 302
	loginClient := &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := loginClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	for _, setCookie := range resp.Header.Values("Set-Cookie") {
		if m := loginSessionCookieRe.FindStringSubmatch(setCookie); len(m) > 1 && strings.TrimSpace(m[1]) != "" {
			return fmt.Sprintf("PHPSESSID=%s; _ym_isad=2;", strings.TrimSpace(m[1])), nil
		}
	}
	return "", nil
}

// fetchText routes through the shared Fetcher so the tracker's fetchmode
// (standard / flaresolverr) takes effect. The login cookie from takeLogin is
// merged with any config-side cookie inside Fetcher.GetExt.
func (p *Parser) fetchText(ctx context.Context, urlv, cookie, _ string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	body, status, err := p.Fetcher.GetStringExt(urlv, p.Config.Selezen, cookie, selezenUA)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", nil
	}
	return body, nil
}

func (p *Parser) fetchMagnet(ctx context.Context, cookie, urlv string) (string, error) {
	host := strings.TrimRight(strings.TrimSpace(p.Config.Selezen.Host), "/")
	body, err := p.fetchText(ctx, urlv, cookie, host+"/")
	if err != nil || body == "" {
		return "", err
	}
	return html.UnescapeString(matchDecode(magnetRe, body)), nil
}

func (p *Parser) UpdateTasksParse(ctx context.Context) ([]Task, error) {
	cookie, err := p.ensureCookie(ctx)
	if err != nil {
		return nil, err
	}
	host := strings.TrimRight(strings.TrimSpace(p.Config.Selezen.Host), "/")
	if host == "" {
		return nil, nil
	}
	body, err := p.fetchText(ctx, host+"/relizy-ot-selezen/", cookie, host+"/")
	if err != nil {
		return nil, err
	}
	maxPage := 1
	for _, m := range pageLinksRe.FindAllStringSubmatch(body, -1) {
		if n, err2 := strconv.Atoi(m[1]); err2 == nil && n > maxPage {
			maxPage = n
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tasks == nil {
		p.tasks = []Task{}
	}
	pages := map[int]Task{}
	for _, t := range p.tasks {
		pages[t.Page] = t
	}
	for pg := 1; pg <= maxPage; pg++ {
		if _, ok := pages[pg]; !ok {
			pages[pg] = Task{Page: pg, UpdateTime: "0001-01-01T00:00:00"}
		}
	}
	merged := make([]Task, 0, len(pages))
	for _, t := range pages {
		merged = append(merged, t)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Page < merged[j].Page })
	p.tasks = merged
	if err := p.saveTasksLocked(); err != nil {
		return nil, err
	}
	return cloneTasks(p.tasks), nil
}

func (p *Parser) ParseAllTask(ctx context.Context, force bool) (string, error) {
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
		log.Printf("selezen: parsealltask — tasks empty, running updatetasksparse first")
		if _, err := p.UpdateTasksParse(ctx); err != nil {
			return "", err
		}
		p.mu.Lock()
		snapshot = cloneTasks(p.tasks)
		p.mu.Unlock()
	}

	totalPages := len(snapshot)
	processed, fetched, totalAdded, totalUpdated, totalSkipped, totalFailed, errs := 0, 0, 0, 0, 0, 0, 0
	for _, task := range snapshot {
		if !force && task.UpdatedToday() {
			continue
		}
		if p.Config.Selezen.ParseDelay > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(p.Config.Selezen.ParseDelay) * time.Millisecond):
			}
		}
		parsed, added, updated, skipped, failed, err := p.parsePage(ctx, task.Page)
		if err != nil {
			log.Printf("selezen: parsealltask page=%d error: %v", task.Page, err)
			errs++
			continue
		}
		processed++
		if parsed == 0 {
			log.Printf("selezen: parsealltask page=%d empty (marking today)", task.Page)
			p.mu.Lock()
			for i := range p.tasks {
				if p.tasks[i].Page == task.Page {
					p.tasks[i].MarkToday()
					break
				}
			}
			_ = p.saveTasksLocked()
			p.mu.Unlock()
			continue
		}
		fetched += parsed
		totalAdded += added
		totalUpdated += updated
		totalSkipped += skipped
		totalFailed += failed
		log.Printf("selezen: parsealltask page=%d fetched=%d added=%d skipped=%d failed=%d", task.Page, parsed, added, skipped, failed)
		p.mu.Lock()
		for i := range p.tasks {
			if p.tasks[i].Page == task.Page {
				p.tasks[i].MarkToday()
				break
			}
		}
		if err2 := p.saveTasksLocked(); err2 != nil {
			p.mu.Unlock()
			return "", err2
		}
		p.mu.Unlock()
	}
	log.Printf("selezen: parsealltask done processed=%d/%d fetched=%d added=%d updated=%d skipped=%d failed=%d errors=%d", processed, totalPages, fetched, totalAdded, totalUpdated, totalSkipped, totalFailed, errs)
	return "ok", nil
}

func (p *Parser) ParseLatest(ctx context.Context, pages int) (string, error) {
	if !p.latestMu.TryLock() {
		return "work", nil
	}
	defer p.latestMu.Unlock()
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
	sort.Slice(snapshot, func(i, j int) bool { return snapshot[i].Page < snapshot[j].Page })
	if len(snapshot) > pages {
		snapshot = snapshot[:pages]
	}
	var lines []string
	processed, fetched, totalAdded, totalUpdated, totalSkipped, totalFailed, errs := 0, 0, 0, 0, 0, 0, 0
	for _, task := range snapshot {
		if p.Config.Selezen.ParseDelay > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(p.Config.Selezen.ParseDelay) * time.Millisecond):
			}
		}
		parsed, added, updated, skipped, failed, err := p.parsePage(ctx, task.Page)
		if err != nil {
			log.Printf("selezen: parselatest page=%d error: %v", task.Page, err)
			errs++
			continue
		}
		processed++
		if parsed == 0 {
			log.Printf("selezen: parselatest page=%d empty (marking today)", task.Page)
			p.mu.Lock()
			for i := range p.tasks {
				if p.tasks[i].Page == task.Page {
					p.tasks[i].MarkToday()
					break
				}
			}
			_ = p.saveTasksLocked()
			p.mu.Unlock()
			continue
		}
		fetched += parsed
		totalAdded += added
		totalUpdated += updated
		totalSkipped += skipped
		totalFailed += failed
		log.Printf("selezen: parselatest page=%d fetched=%d added=%d skipped=%d failed=%d", task.Page, parsed, added, skipped, failed)
		p.mu.Lock()
		for i := range p.tasks {
			if p.tasks[i].Page == task.Page {
				p.tasks[i].MarkToday()
				break
			}
		}
		if err2 := p.saveTasksLocked(); err2 != nil {
			p.mu.Unlock()
			return "", err2
		}
		p.mu.Unlock()
		lines = append(lines, fmt.Sprintf("page=%d", task.Page))
	}
	log.Printf("selezen: parselatest done processed=%d fetched=%d added=%d updated=%d skipped=%d failed=%d errors=%d", processed, fetched, totalAdded, totalUpdated, totalSkipped, totalFailed, errs)
	if len(lines) == 0 {
		return "ok", nil
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func (p *Parser) loadTasks() error {
	path := filepath.Join(p.DataDir, "temp", "selezen_taskParse.json")
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			p.tasks = []Task{}
			return nil
		}
		return err
	}
	var raw []Task
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	p.tasks = raw
	return nil
}

func (p *Parser) saveTasksLocked() error {
	if p.tasks == nil {
		p.tasks = []Task{}
	}
	path := filepath.Join(p.DataDir, "temp", "selezen_taskParse.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(p.tasks)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func cloneTasks(src []Task) []Task {
	out := make([]Task, len(src))
	copy(out, src)
	return out
}

func parseNames(title string) (string, string, int) {
	if m := movieMainRe.FindStringSubmatch(title); len(m) > 3 {
		year, _ := strconv.Atoi(strings.TrimSpace(m[3]))
		return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), year
	}
	if m := movieShortRe.FindStringSubmatch(title); len(m) > 3 {
		year, _ := strconv.Atoi(strings.TrimSpace(m[3]))
		return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), year
	}
	return "", "", 0
}

func fallbackName(title string) string {
	parts := regexp.MustCompile(`(\[|/|\(|\|)`).Split(title, 2)
	if len(parts) == 0 {
		return ""
	}
	return strings.TrimSpace(parts[0])
}

func typesForRow(row, title, urlv string) []string {
	if multMarkerRe.MatchString(row) {
		return []string{"multfilm"}
	}
	if strings.Contains(strings.ToLower(title), "tvshows") || strings.Contains(strings.ToLower(urlv), "tvshows") || serialSeasonRe.MatchString(title) || serialEpisodeRe.MatchString(title) {
		return []string{"serial"}
	}
	return []string{"movie"}
}

func itemID(urlv string) string {
	m := itemIDRe.FindStringSubmatch(urlv)
	if len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func replaceBadNames(s string) string {
	s = strings.ReplaceAll(s, "\u00a0", " ")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	return s
}

func matchDecode(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(cleanSpaceRe.ReplaceAllString(html.UnescapeString(m[1]), " "))
}

func fileTime(t filedb.TorrentDetails) time.Time {
	for _, key := range []string{"updateTime", "createTime"} {
		s := strings.TrimSpace(asString(t[key]))
		if s == "" {
			continue
		}
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.9999999Z07:00", "2006-01-02T15:04:05Z07:00", time.RFC3339} {
			if tm, err := time.Parse(layout, s); err == nil {
				return tm.UTC()
			}
		}
	}
	return time.Now().UTC()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func isDisabled(list []string, tracker string) bool {
	for _, item := range list {
		if strings.EqualFold(strings.TrimSpace(item), strings.TrimSpace(tracker)) {
			return true
		}
	}
	return false
}

func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
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
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	default:
		n, _ := strconv.Atoi(strings.TrimSpace(fmt.Sprint(v)))
		return n
	}
}
