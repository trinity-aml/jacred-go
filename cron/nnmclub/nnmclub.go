package nnmclub

import (
	"context"
	"encoding/json"
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

const trackerName = "nnmclub"
const validMarker = "NNM-Club</title>"

var categories = []string{"10", "13", "6", "4", "3", "22", "23", "1", "7", "11"}
var firstNamePart = regexp.MustCompile(`(\[|/|\(|\|)`)

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
	t.UpdateTime = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).Format("2006-01-02T15:04:05Z07:00")
}

type Parser struct {
	Config  app.Config
	DB      *filedb.DB
	DataDir string
	Client  *http.Client
	loc     *time.Location

	mu               sync.Mutex
	working          bool
	allWork          bool
	latest           sync.Mutex
	tasks            map[string][]Task
	cookieMu         sync.Mutex
	cookie           string
	lastLoginAttempt time.Time
}

type ParseResult struct {
	Fetched, Added, Updated, Skipped, Failed int
	Status                                   string
	PerCategory                              map[string]int
}

func New(cfg app.Config, db *filedb.DB, dataDir string) *Parser {
	loc, err := time.LoadLocation("Asia/Jerusalem")
	if err != nil {
		loc = time.Local
	}
	p := &Parser{Config: cfg, DB: db, DataDir: dataDir, Client: &http.Client{Timeout: 35 * time.Second}, loc: loc, tasks: map[string][]Task{}}
	_ = p.loadTasks()
	return p
}

func (p *Parser) getCookie() string {
	p.cookieMu.Lock()
	defer p.cookieMu.Unlock()
	if strings.TrimSpace(p.cookie) != "" {
		return p.cookie
	}
	if strings.TrimSpace(p.Config.NNMClub.Cookie) != "" {
		return strings.TrimSpace(p.Config.NNMClub.Cookie)
	}
	return ""
}

func (p *Parser) takeLogin(ctx context.Context) error {
	p.cookieMu.Lock()
	if time.Since(p.lastLoginAttempt) < 2*time.Minute {
		p.cookieMu.Unlock()
		return nil
	}
	p.lastLoginAttempt = time.Now()
	p.cookieMu.Unlock()

	host := strings.TrimRight(p.Config.NNMClub.Host, "/")
	if host == "" || strings.TrimSpace(p.Config.NNMClub.Login.U) == "" {
		return fmt.Errorf("nnmclub: no host or login configured")
	}
	log.Printf("nnmclub: attempting login to %s as %s", host, p.Config.NNMClub.Login.U)

	form := url.Values{}
	form.Set("username", p.Config.NNMClub.Login.U)
	form.Set("password", p.Config.NNMClub.Login.P)
	form.Set("autologin", "on")
	form.Set("redirect", "")
	form.Set("login", "\xc2\xf5\xee\xe4") // "Вход" in CP1251

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/forum/login.php", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	loginClient := &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := loginClient.Do(req)
	if err != nil {
		log.Printf("nnmclub: login error: %v", err)
		return err
	}
	defer resp.Body.Close()
	log.Printf("nnmclub: login response status=%d", resp.StatusCode)

	var parts []string
	for _, line := range resp.Header.Values("Set-Cookie") {
		parts = append(parts, strings.SplitN(line, ";", 2)[0])
	}
	cookieStr := strings.Join(parts, "; ")
	if strings.Contains(cookieStr, "phpbb2mysql") || strings.Contains(cookieStr, "sid=") {
		p.cookieMu.Lock()
		p.cookie = cookieStr
		p.cookieMu.Unlock()
		log.Printf("nnmclub: login OK")
		return nil
	}
	log.Printf("nnmclub: login FAILED — cookies: %s", cookieStr)
	return fmt.Errorf("nnmclub: login failed")
}

func (p *Parser) ensureLogin(ctx context.Context) {
	if p.getCookie() != "" {
		return
	}
	_ = p.takeLogin(ctx)
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
	p.ensureLogin(ctx)
	res := ParseResult{Status: "ok", PerCategory: map[string]int{}}
	for _, cat := range categories {
		items, err := p.parsePage(ctx, cat, page)
		if err != nil {
			return res, err
		}
		res.Fetched += len(items)
		res.PerCategory[cat] = len(items)
		if len(items) == 0 {
			continue
		}
		added, updated, skipped, failed, err := p.saveTorrents(items)
		if err != nil {
			return res, err
		}
		res.Added += added
		res.Updated += updated
		res.Skipped += skipped
		res.Failed += failed
	}
	return res, nil
}

func (p *Parser) UpdateTasksParse(ctx context.Context) (map[string][]Task, error) {
	p.ensureLogin(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tasks == nil {
		p.tasks = map[string][]Task{}
	}
	for _, cat := range categories {
		htmlBody, err := p.fetchCategoryRoot(ctx, cat)
		if err != nil {
			return nil, err
		}
		maxPages := 0
		if m := regexp.MustCompile(`<a href="[^"]+">([0-9]+)</a>[^<\n\r]+<a href="[^"]+">След\.</a>`).FindStringSubmatch(htmlBody); len(m) > 1 {
			maxPages, _ = strconv.Atoi(strings.TrimSpace(m[1]))
		}
		existing := p.tasks[cat]
		pages := map[int]Task{}
		for _, t := range existing {
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

func (p *Parser) ParseAllTask(ctx context.Context) (string, error) {
	p.ensureLogin(ctx)
	p.mu.Lock()
	if p.allWork {
		p.mu.Unlock()
		return "work", nil
	}
	p.allWork = true
	snapshot := cloneTasks(p.tasks)
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.allWork = false; p.mu.Unlock() }()

	for cat, list := range snapshot {
		for _, task := range list {
			if task.UpdatedToday(p.loc) {
				continue
			}
			if p.Config.NNMClub.ParseDelay > 0 {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(time.Duration(p.Config.NNMClub.ParseDelay) * time.Millisecond):
				}
			}
			items, err := p.parsePage(ctx, cat, task.Page)
			if err != nil {
				return "", err
			}
			if len(items) > 0 {
				if _, _, _, _, err := p.saveTorrents(items); err != nil {
					return "", err
				}
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
	}
	return "ok", nil
}

func (p *Parser) ParseLatest(ctx context.Context, pages int) (string, error) {
	if !p.latest.TryLock() {
		return "work", nil
	}
	defer p.latest.Unlock()
	p.ensureLogin(ctx)
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
	for cat, list := range snapshot {
		sort.Slice(list, func(i, j int) bool { return list[i].Page < list[j].Page })
		if len(list) > pages {
			list = list[:pages]
		}
		for _, task := range list {
			if p.Config.NNMClub.ParseDelay > 0 {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(time.Duration(p.Config.NNMClub.ParseDelay) * time.Millisecond):
				}
			}
			items, err := p.parsePage(ctx, cat, task.Page)
			if err != nil {
				return "", err
			}
			if len(items) == 0 {
				continue
			}
			if _, _, _, _, err := p.saveTorrents(items); err != nil {
				return "", err
			}
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
	if len(lines) == 0 {
		return "ok", nil
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func (p *Parser) parsePage(ctx context.Context, cat string, page int) ([]filedb.TorrentDetails, error) {
	rawURL := strings.TrimRight(requestHost(p.Config.NNMClub), "/") + "/forum/portal.php?c=" + cat + "&start=" + strconv.Itoa(page*20)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if cookie := p.getCookie(); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	htmlBody := core.DecodeCP1251(body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !strings.Contains(htmlBody, validMarker) {
		return nil, nil
	}
	return parsePageHTML(strings.TrimRight(p.Config.NNMClub.Host, "/"), cat, htmlBody, time.Now().UTC()), nil
}

func (p *Parser) fetchCategoryRoot(ctx context.Context, cat string) (string, error) {
	rawURL := strings.TrimRight(requestHost(p.Config.NNMClub), "/") + "/forum/portal.php?c=" + cat
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	if cookie := p.getCookie(); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	htmlBody := core.DecodeCP1251(body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("nnmclub status %d", resp.StatusCode)
	}
	return htmlBody, nil
}

func parsePageHTML(host, cat, htmlBody string, now time.Time) []filedb.TorrentDetails {
	htmlBody = strings.ReplaceAll(htmlBody, "\n", "")
	htmlBody = strings.ReplaceAll(htmlBody, "\r", "")
	htmlBody = strings.ReplaceAll(htmlBody, "\t", "")
	if m := regexp.MustCompile(`(?is)<td valign="top" width="[0-9]+%">(.*)<div class="paginport nav">`).FindStringSubmatch(htmlBody); len(m) > 1 {
		htmlBody = m[1]
	}
	rows := strings.Split(replaceBadNames(htmlBody), `<table width="100%" class="pline">`)
	out := make([]filedb.TorrentDetails, 0, len(rows))
	for _, row := range rows[1:] {
		match := func(pattern string, group ...int) string {
			idx := 1
			if len(group) > 0 {
				idx = group[0]
			}
			re := regexp.MustCompile(`(?is)` + pattern)
			m := re.FindStringSubmatch(row)
			if len(m) <= idx {
				return ""
			}
			s := html.UnescapeString(strings.TrimSpace(m[idx]))
			s = regexp.MustCompile(`[\n\r\t\x{00A0} ]+`).ReplaceAllString(s, " ")
			return strings.TrimSpace(s)
		}
		magnet := regexp.MustCompile(`"(magnet:[^"]+)"`).FindStringSubmatch(row)
		if len(magnet) < 2 {
			continue
		}
		createTime := parseCreateTime(match(`\| ([0-9]+ [^ ]+ [0-9]{4} [^<]+)</span> \| <span class="tit"`), "02.01.2006 15:04:05")
		if createTime.IsZero() {
			continue
		}
		urlPath := match(`<a class="pgenmed" href="(viewtopic\.php[^"]+)"`)
		title := match(`>([^<]+)</a></h2></td>`)
		sidRaw := match(`title="Раздаюших">&nbsp;([0-9]+)</span>`)
		pirRaw := match(`title="Качают">&nbsp;([0-9]+)</span>`)
		sizeName := match(`<span class="pcomm bold">([^<]+)</span>`)
		if urlPath == "" || title == "" || sidRaw == "" || pirRaw == "" || sizeName == "" {
			continue
		}
		name, original, relased := parseTitle(cat, title, row)
		if strings.TrimSpace(name) == "" {
			name = fallbackName(title)
		}
		types := typesForCategory(cat)
		if name == "" || len(types) == 0 {
			continue
		}
		sid, _ := strconv.Atoi(sidRaw)
		pir, _ := strconv.Atoi(pirRaw)
		out = append(out, filedb.TorrentDetails{
			"trackerName":  trackerName,
			"types":        types,
			"url":          strings.TrimRight(host, "/") + "/forum/" + strings.TrimLeft(urlPath, "/"),
			"title":        title,
			"sid":          sid,
			"pir":          pir,
			"sizeName":     sizeName,
			"magnet":       magnet[1],
			"createTime":   createTime.UTC().Format(time.RFC3339Nano),
			"updateTime":   now.UTC().Format(time.RFC3339Nano),
			"name":         name,
			"originalname": original,
			"relased":      relased,
			"_sn":          core.SearchName(name),
			"_so":          core.SearchName(firstNonEmpty(original, name)),
		})
	}
	return out
}

func (p *Parser) saveTorrents(torrents []filedb.TorrentDetails) (int, int, int, int, error) {
	added, updated, skipped, failed := 0, 0, 0, 0
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
		urlv := strings.TrimSpace(asString(incoming["url"]))
		if urlv == "" {
			skipped++
			continue
		}
		existing, exists := bucket[urlv]
		if exists && samePrimary(existing, incoming) {
			skipped++
			continue
		}
		bucket[urlv] = mergeTorrent(existing, exists, incoming)
		changed[key] = fileTime(bucket[urlv])
		if exists {
			updated++
		} else {
			added++
		}
	}
	for key, when := range changed {
		if err := p.DB.SaveBucket(key, bucketCache[key], when); err != nil {
			return added, updated, skipped, failed, err
		}
	}
	if len(changed) > 0 {
		if err := p.DB.SaveChangesToFile(); err != nil {
			return added, updated, skipped, failed, err
		}
	}
	return added, updated, skipped, failed, nil
}

func (p *Parser) loadTasks() error {
	path := filepath.Join(p.DataDir, "temp", "nnmclub_taskParse.json")
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
	path := filepath.Join(p.DataDir, "temp", "nnmclub_taskParse.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(p.tasks)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func cloneTasks(src map[string][]Task) map[string][]Task {
	out := make(map[string][]Task, len(src))
	for k, list := range src {
		vv := make([]Task, len(list))
		copy(vv, list)
		out[k] = vv
	}
	return out
}

func parseTaskTime(v string, loc *time.Location) time.Time {
	if strings.TrimSpace(v) == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if tm, err := time.Parse(layout, v); err == nil {
			return tm.In(loc)
		}
	}
	return time.Time{}
}

func parseTitle(cat, title, row string) (string, string, int) {
	parseYear := func(v string) int { n, _ := strconv.Atoi(strings.TrimSpace(v)); return n }
	var name, original string
	relased := 0
	try := func(pattern string, nameIdx, originalIdx, yearIdx int) bool {
		m := regexp.MustCompile(pattern).FindStringSubmatch(title)
		if len(m) == 0 || len(m) <= yearIdx || nameIdx < 0 || len(m) <= nameIdx || strings.TrimSpace(m[nameIdx]) == "" || strings.TrimSpace(m[yearIdx]) == "" {
			return false
		}
		name = strings.TrimSpace(m[nameIdx])
		if originalIdx >= 0 && len(m) > originalIdx {
			original = strings.TrimSpace(m[originalIdx])
		}
		relased = parseYear(m[yearIdx])
		return true
	}
	switch cat {
	case "10", "6", "3", "22", "23", "11":
		if try(`^([^/\(\|]+) \([^\)]+\) / [^/\(\|]+ / ([^/\(\|]+) \(([0-9]{4})(-[0-9]{4})?\)`, 1, 2, 3) ||
			try(`^([^/\(\|]+) / [^/\(\|]+ / [^/\(\|]+ / ([^/\(\|]+) \(([0-9]{4})(-[0-9]{4})?\)`, 1, 2, 3) ||
			try(`^([^/\(\|]+) / [^/\(\|]+ / ([^/\(\|]+) \(([0-9]{4})(-[0-9]{4})?\)`, 1, 2, 3) ||
			try(`^([^/\(\|]+) \([^\)]+\) / ([^/\(\|]+) \(([0-9]{4})(-[0-9]{4})?\)`, 1, 2, 3) ||
			try(`^([^/\(\|]+) / ([^/\(\|]+) \(([0-9]{4})(-[0-9]{4})?\)`, 1, 2, 3) {
			return name, original, relased
		}
		if m := regexp.MustCompile(`^([^/\(\|]+) \([^\)]+\) \(([0-9]{4})(-[0-9]{4})?\)`).FindStringSubmatch(title); len(m) > 2 {
			return strings.TrimSpace(m[1]), "", parseYear(m[2])
		}
		if m := regexp.MustCompile(`^([^/\(\|]+) \(([0-9]{4})(-[0-9]{4})?\)`).FindStringSubmatch(title); len(m) > 2 {
			return strings.TrimSpace(m[1]), "", parseYear(m[2])
		}
	case "13":
		if m := regexp.MustCompile(`^([^/\(\|]+) \(([0-9]{4})\)`).FindStringSubmatch(title); len(m) > 2 {
			return strings.TrimSpace(m[1]), "", parseYear(m[2])
		}
	case "4":
		if try(`^([^/\(\|]+) / [^/\(\|]+ \(([0-9]{4})(-[0-9]{4})?\)`, 1, -1, 2) {
			return name, original, relased
		}
		if m := regexp.MustCompile(`^([^/\(\|]+) \(([0-9]{4})(-[0-9]{4})?\)`).FindStringSubmatch(title); len(m) > 2 {
			return strings.TrimSpace(m[1]), "", parseYear(m[2])
		}
	case "1":
		patterns := []struct {
			p          string
			ni, oi, yi int
		}{
			{`^([^/\[\(]+) \([0-9]{4}\) \| ([^/\[\(]+) \([^\)]+\) \[([0-9]{4})(-[0-9]{4})?,`, 2, 1, 3},
			{`^([^/\[\(]+) \([0-9]{4}\) \| ([^/\[\(]+) \[([0-9]{4})(-[0-9]{4})?,`, 2, 1, 3},
			{`^([^/\[\(]+) \| [^/\[\(]+ \| [^/\[\(]+ \| ([^/\[\(]+) (\[(ТВ|TV)-[0-9]+\] )?\[([0-9]{4})(-[0-9]{4})?,`, 2, 1, 5},
			{`^([^/\[\(]+) \| [^/\[\(]+ \| ([^/\[\(]+) \([^\)]+\) (\[(ТВ|TV)-[0-9]+\] )?\[([0-9]{4})(-[0-9]{4})?,`, 2, 1, 5},
			{`^([^/\[\(]+) \| [^/\[\(]+ \| ([^/\[\(]+) (\[(ТВ|TV)-[0-9]+\] )?\[([0-9]{4})(-[0-9]{4})?,`, 2, 1, 5},
			{`^([^/\[\(]+) \| ([^/\[\(]+) (\[(ТВ|TV)-[0-9]+\] )?\[([0-9]{4})(-[0-9]{4})?,`, 2, 1, 5},
			{`^([^/\[\(]+) / [^/\[\(]+ / ([^/\[\(]+) (\[(ТВ|TV)-[0-9]+\] )?\[([0-9]{4})(-[0-9]{4})?,`, 2, 1, 5},
			{`^([^/\[\(]+) / ([^/\[\(]+) (\[(ТВ|TV)-[0-9]+\] )?\[([0-9]{4})(-[0-9]{4})?,`, 2, 1, 5},
		}
		for _, pt := range patterns {
			if try(pt.p, pt.ni, pt.oi, pt.yi) {
				return name, original, relased
			}
		}
	case "7":
		if !strings.Contains(strings.ToLower(title), "pdf") && (strings.Contains(strings.ToLower(row), "должительность") || strings.Contains(strings.ToLower(row), "мульт")) {
			if try(`^([^/\(\|]+) / [^/\(\|]+ / ([^/\(\|]+) \(([0-9]{4})(-[0-9]{4})?\)`, 1, 2, 3) ||
				try(`^([^/\(\|]+) / ([^/\(\|]+) \(([0-9]{4})(-[0-9]{4})?\)`, 1, 2, 3) {
				return name, original, relased
			}
			if m := regexp.MustCompile(`^([^/\(\|]+) \(([0-9]{4})(-[0-9]{4})?\)`).FindStringSubmatch(title); len(m) > 2 {
				return strings.TrimSpace(m[1]), "", parseYear(m[2])
			}
		}
	}
	return strings.TrimSpace(name), strings.TrimSpace(original), relased
}

func typesForCategory(cat string) []string {
	switch cat {
	case "10", "13", "6", "11":
		return []string{"movie"}
	case "4", "3":
		return []string{"serial"}
	case "22", "23":
		return []string{"docuserial", "documovie"}
	case "7":
		return []string{"multfilm", "multserial"}
	case "1":
		return []string{"anime"}
	default:
		return nil
	}
}

func requestHost(cfg app.TrackerSettings) string {
	if strings.TrimSpace(cfg.Alias) != "" {
		return cfg.Alias
	}
	return cfg.Host
}

func replaceBadNames(s string) string {
	r := strings.NewReplacer("ё", "е", "Ё", "Е", "й", "и", "Й", "И", "щ", "ш", "Щ", "Ш")
	return r.Replace(s)
}

func parseCreateTime(v, layout string) time.Time {
	repl := strings.NewReplacer(
		" января ", ".01.", " февраля ", ".02.", " марта ", ".03.", " апреля ", ".04.", " мая ", ".05.", " июня ", ".06.", " июля ", ".07.", " августа ", ".08.", " сентября ", ".09.", " октября ", ".10.", " ноября ", ".11.", " декабря ", ".12.",
	)
	line := strings.TrimSpace(strings.ToLower(v))
	line = repl.Replace(" " + line + " ")
	line = strings.TrimSpace(line)
	if tm, err := time.ParseInLocation(layout, line, time.Local); err == nil {
		return tm
	}
	return time.Time{}
}

func mergeTorrent(existing filedb.TorrentDetails, exists bool, incoming filedb.TorrentDetails) filedb.TorrentDetails {
	out := filedb.TorrentDetails{}
	if exists {
		for k, v := range existing {
			out[k] = v
		}
	}
	for k, v := range incoming {
		if v == nil {
			continue
		}
		if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
			continue
		}
		out[k] = v
	}
	if strings.TrimSpace(asString(out["name"])) == "" {
		out["name"] = fallbackName(asString(out["title"]))
	}
	if strings.TrimSpace(asString(out["originalname"])) == "" {
		out["originalname"] = out["name"]
	}
	out["_sn"] = core.SearchName(asString(out["name"]))
	out["_so"] = core.SearchName(firstNonEmpty(asString(out["originalname"]), asString(out["name"])))
	if fileTime(out).IsZero() {
		out["updateTime"] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	return out
}

func samePrimary(existing, incoming filedb.TorrentDetails) bool {
	return asString(existing["title"]) == asString(incoming["title"]) && asString(existing["magnet"]) == asString(incoming["magnet"]) && asInt(existing["sid"]) == asInt(incoming["sid"]) && asInt(existing["pir"]) == asInt(incoming["pir"])
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

func fallbackName(title string) string { return strings.TrimSpace(firstNamePart.Split(title, 2)[0]) }
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
