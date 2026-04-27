package torrentby

import (
	"context"
	"crypto/tls"
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

const trackerName = "torrentby"

var (
	rowSplitRe     = regexp.MustCompile(`<tr class="ttable_col`)
	cleanupSpaceRe = regexp.MustCompile(`[\n\r\t\x{00A0} ]+`)
	firstNamePart  = regexp.MustCompile(`(\[|/|\(|\|)`)
	pageAllRe      = regexp.MustCompile(`href="\?page=([0-9]+)"`)
	pageDotRe      = regexp.MustCompile(`href="\?page=([0-9]+)"><span[^>]*>\s*\.\.\.\s*</span>`)

	inlineYearRe   = regexp.MustCompile(`^([^/\(]+) / [^/]+ / ([^/\(]+) \(([0-9]{4})\)`)
	inlineYearRe10 = regexp.MustCompile(`^([^/\(]+) / [^/]+ / ([^/\(]+) \(([0-9]{4})(\)|-)`)
	inlineYearRe11 = regexp.MustCompile(`^([^/\(]+) / ([^/\(]+) \(([0-9]{4})(\)|-)`)
	inlineYearRe12 = regexp.MustCompile(`^([^/\[]+) \[[^\]]+\] +\(([0-9]{4})(\)|-)`)
	inlineYearRe13 = regexp.MustCompile(`^([^/\(]+) \(([0-9]{4})(\)|-)`)
	inlineYearRe2  = regexp.MustCompile(`^([^/\(]+) / ([^/\(]+) \(([0-9]{4})\)`)
	inlineYearRe3  = regexp.MustCompile(`^([^/\(]+) (/ [^/\(]+)?\(([0-9]{4})\)`)
	inlineYearRe4  = regexp.MustCompile(`^([^/\(\[]+) / [^/]+ / [^/]+ / ([^/\(\[]+) \[[^\]]+\] +\(([0-9]{4})(\)|-)`)
	inlineYearRe5  = regexp.MustCompile(`^([^/\(\[]+) / [^/]+ / ([^/\(\[]+) \[[^\]]+\] +\(([0-9]{4})(\)|-)`)
	inlineYearRe6  = regexp.MustCompile(`^([^/\(\[]+) / ([^/\[]+) \[[^\]]+\] +\(([0-9]{4})(\)|-)`)
	inlineYearRe7  = regexp.MustCompile(`^([^/\(\[]+) \[[^\]]+\] +\(([0-9]{4})(\)|-)`)
	inlineYearRe8  = regexp.MustCompile(`^([^/]+) / [^/]+ / ([^/\[]+) \[[^\]]+\] +\(([0-9]{4})(\)|-)`)
	inlineYearRe9  = regexp.MustCompile(`^([^/]+) / ([^/\[]+) \[[^\]]+\] +\(([0-9]{4})(\)|-)`)
	mp1Re          = regexp.MustCompile(`(?is)>([0-9]{4}-[0-9]{2}-[0-9]{2})</td>`)
	mp2Re          = regexp.MustCompile(`(?is)<a name="search_select" [^>]+ href="/([0-9]+/[^"]+)"`)
	mp3Re          = regexp.MustCompile(`(?is)<a name="search_select" [^>]*>([^<]+)</a>`)
	mp4Re          = regexp.MustCompile(`(?is)<font color="green">(?:&uarr;|↑)\s*([0-9]+)</font>`)
	mp5Re          = regexp.MustCompile(`(?is)<font color="red">(?:&darr;|↓)\s*([0-9]+)</font>`)
	mp6Re          = regexp.MustCompile(`(?is)<td style="white-space:nowrap;?">([0-9][^<]+)</td>`)
	mp7Re          = regexp.MustCompile(`(?is)href="(magnet:\?xt=[^"]+)"`)
)

var categories = []string{"films", "movies", "serials", "series", "tv", "humor", "cartoons", "anime", "sport"}

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
	Fetcher *core.Fetcher
	loc     *time.Location

	mu               sync.Mutex
	working          bool
	allWork          bool
	latestMu         sync.Mutex
	tasks            map[string][]Task
	cookieMu         sync.Mutex
	dynCookie        string
	lastLoginAttempt time.Time
	cookieStore      *core.CookieStore
}

type ParseResult struct {
	Fetched, Added, Updated, Skipped, Failed int
	Status                                   string
	PerCategory                              map[string]int
}

func New(cfg app.Config, db *filedb.DB, dataDir string) *Parser {
	loc, err := time.LoadLocation("Asia/Jerusalem")
	if err != nil {
		loc = time.FixedZone("+0200", 2*3600)
	}
	p := &Parser{Config: cfg, DB: db, DataDir: dataDir, Client: &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}, Fetcher: core.NewFetcher(cfg), loc: loc, tasks: map[string][]Task{}, cookieStore: core.NewCookieStore(dataDir)}
	_ = p.loadTasks()
	if saved := p.cookieStore.Load(trackerName); saved != "" {
		p.dynCookie = saved
		log.Printf("torrentby: loaded saved cookie from disk")
	}
	return p
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
	if strings.TrimSpace(p.Config.TorrentBy.Host) == "" {
		return ParseResult{Status: "config missing"}, nil
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
		log.Printf("torrentby: cat=%s fetched=%d added=%d skipped=%d failed=%d", cat, len(items), added, skipped, failed)
	}
	log.Printf("torrentby: done fetched=%d added=%d skipped=%d failed=%d", res.Fetched, res.Added, res.Skipped, res.Failed)
	return res, nil
}

func (p *Parser) UpdateTasksParse(ctx context.Context) (map[string][]Task, error) {
	p.ensureLogin(ctx)

	// Discover max page per category by following "..." pagination links.
	// Done WITHOUT holding p.mu to avoid deadlock (fetches call cookie()).
	type catMax struct {
		cat     string
		maxPage int
	}
	var results []catMax
	for _, cat := range categories {
		maxPage := 0
		// Follow "..." links until we reach the last pagination group.
		fetchNext := -1 // -1 = fetch root; >=0 = fetch ?page=N
		for {
			var (
				htmlBody string
				err      error
			)
			if fetchNext < 0 {
				htmlBody, err = p.fetchCategoryRoot(ctx, cat)
			} else {
				htmlBody, err = p.fetchCategoryPage(ctx, cat, fetchNext)
			}
			if err != nil {
				break
			}
			for _, m := range pageAllRe.FindAllStringSubmatch(htmlBody, -1) {
				if n, err2 := strconv.Atoi(m[1]); err2 == nil && n > maxPage {
					maxPage = n
				}
			}
			// If there is a "..." link, follow it to discover more pages.
			if m := pageDotRe.FindStringSubmatch(htmlBody); len(m) > 1 {
				next, err2 := strconv.Atoi(m[1])
				if err2 != nil || next <= fetchNext {
					break
				}
				fetchNext = next
			} else {
				break
			}
		}
		results = append(results, catMax{cat, maxPage})
	}

	// Merge results into tasks under lock.
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tasks == nil {
		p.tasks = map[string][]Task{}
	}
	for _, cm := range results {
		existing := p.tasks[cm.cat]
		pages := map[int]Task{}
		for _, t := range existing {
			pages[t.Page] = t
		}
		for page := 0; page <= cm.maxPage; page++ {
			if _, ok := pages[page]; !ok {
				pages[page] = Task{Page: page, UpdateTime: "0001-01-01T00:00:00"}
			}
		}
		merged := make([]Task, 0, len(pages))
		for _, t := range pages {
			merged = append(merged, t)
		}
		sort.Slice(merged, func(i, j int) bool { return merged[i].Page < merged[j].Page })
		p.tasks[cm.cat] = merged
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
	if p.tasks == nil {
		p.tasks = map[string][]Task{}
	}
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.allWork = false; p.mu.Unlock() }()

	p.mu.Lock()
	snapshot := cloneTasks(p.tasks)
	p.mu.Unlock()

	if len(snapshot) == 0 {
		log.Printf("torrentby: parsealltask — tasks empty, running updatetasksparse first")
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
			if task.UpdatedToday(p.loc) {
				continue
			}
			if p.Config.TorrentBy.ParseDelay > 0 {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(time.Duration(p.Config.TorrentBy.ParseDelay) * time.Millisecond):
				}
			}
			items, err := p.parsePage(ctx, cat, task.Page)
			if err != nil {
				log.Printf("torrentby: parsealltask cat=%s page=%d error: %v", cat, task.Page, err)
				errs++
				continue
			}
			processed++
			if len(items) == 0 {
				log.Printf("torrentby: parsealltask cat=%s page=%d empty (marking today)", cat, task.Page)
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
			a, u, s, f, err := p.saveTorrents(items)
			if err != nil {
				log.Printf("torrentby: parsealltask cat=%s page=%d save error: %v", cat, task.Page, err)
				errs++
				continue
			}
			fetched += len(items)
			added += a
			updated += u
			skipped += s
			failed += f
			log.Printf("torrentby: parsealltask cat=%s page=%d fetched=%d added=%d skipped=%d failed=%d", cat, task.Page, len(items), a, s, f)
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
	log.Printf("torrentby: parsealltask done processed=%d/%d fetched=%d added=%d updated=%d skipped=%d failed=%d errors=%d", processed, totalPages, fetched, added, updated, skipped, failed, errs)
	return "ok", nil
}

func (p *Parser) ParseLatest(ctx context.Context, pages int) (string, error) {
	if !p.latestMu.TryLock() {
		return "work", nil
	}
	defer p.latestMu.Unlock()
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
	processed, fetched, added, updated, skipped, failed, errs := 0, 0, 0, 0, 0, 0, 0
	for cat, list := range snapshot {
		sort.Slice(list, func(i, j int) bool { return list[i].Page < list[j].Page })
		if len(list) > pages {
			list = list[:pages]
		}
		for _, task := range list {
			if p.Config.TorrentBy.ParseDelay > 0 {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(time.Duration(p.Config.TorrentBy.ParseDelay) * time.Millisecond):
				}
			}
			items, err := p.parsePage(ctx, cat, task.Page)
			if err != nil {
				log.Printf("torrentby: parselatest cat=%s page=%d error: %v", cat, task.Page, err)
				errs++
				continue
			}
			processed++
			if len(items) == 0 {
				log.Printf("torrentby: parselatest cat=%s page=%d empty (marking today)", cat, task.Page)
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
			a, u, s, f, err := p.saveTorrents(items)
			if err != nil {
				log.Printf("torrentby: parselatest cat=%s page=%d save error: %v", cat, task.Page, err)
				errs++
				continue
			}
			fetched += len(items)
			added += a
			updated += u
			skipped += s
			failed += f
			log.Printf("torrentby: parselatest cat=%s page=%d fetched=%d added=%d skipped=%d failed=%d", cat, task.Page, len(items), a, s, f)
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
	log.Printf("torrentby: parselatest done processed=%d fetched=%d added=%d updated=%d skipped=%d failed=%d errors=%d", processed, fetched, added, updated, skipped, failed, errs)
	if len(lines) == 0 {
		return "ok", nil
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func (p *Parser) parsePage(ctx context.Context, cat string, page int) ([]filedb.TorrentDetails, error) {
	baseURL := strings.TrimRight(requestHost(p.Config.TorrentBy), "/")
	rawURL := fmt.Sprintf("%s/%s/?page=%d", baseURL, cat, page)
	cookie := p.cookie()
	body, err := p.httpGet(ctx, rawURL, cookie)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, nil
	}
	normalized := replaceBadNames(html.UnescapeString(string(body)))
	return parsePageHTML(strings.TrimRight(p.Config.TorrentBy.Host, "/"), cat, normalized, time.Now().UTC()), nil
}

func (p *Parser) fetchCategoryRoot(ctx context.Context, cat string) (string, error) {
	baseURL := strings.TrimRight(requestHost(p.Config.TorrentBy), "/")
	rawURL := baseURL + "/" + cat + "/"
	cookie := p.cookie()
	body, err := p.httpGet(ctx, rawURL, cookie)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (p *Parser) fetchCategoryPage(ctx context.Context, cat string, page int) (string, error) {
	baseURL := strings.TrimRight(requestHost(p.Config.TorrentBy), "/")
	rawURL := fmt.Sprintf("%s/%s/?page=%d", baseURL, cat, page)
	cookie := p.cookie()
	body, err := p.httpGet(ctx, rawURL, cookie)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// httpGet uses Fetcher with dynamic cookie override.
func (p *Parser) httpGet(ctx context.Context, rawURL, cookie string) ([]byte, error) {
	if p.Fetcher == nil {
		return nil, fmt.Errorf("torrentby: Fetcher not initialized")
	}
	// Override cookie in tracker settings with dynamic cookie
	ts := p.Config.TorrentBy
	if cookie != "" {
		ts.Cookie = cookie
	}
	data, status, err := p.Fetcher.Download(rawURL, ts)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("torrentby status %d", status)
	}
	return data, nil
}

func parsePageHTML(host, cat, htmlBody string, now time.Time) []filedb.TorrentDetails {
	out := make([]filedb.TorrentDetails, 0)
	for _, row := range rowSplitRe.Split(htmlBody, -1)[1:] {
		if strings.TrimSpace(row) == "" || !strings.Contains(row, "magnet:?xt=urn") {
			continue
		}
		reFind := func(re *regexp.Regexp, group ...int) string {
			idx := 1
			if len(group) > 0 {
				idx = group[0]
			}
			m := re.FindStringSubmatch(row)
			if len(m) <= idx {
				return ""
			}
			res := cleanupSpaceRe.ReplaceAllString(html.UnescapeString(strings.TrimSpace(m[idx])), " ")
			return strings.TrimSpace(strings.ReplaceAll(res, "\u0000", " "))
		}

		createTime := parseCreateTime(reFind(mp1Re))
		if createTime.IsZero() {
			if strings.Contains(row, ">Сегодня</td>") {
				createTime = now.UTC()
			} else if strings.Contains(row, ">Вчера</td>") {
				createTime = now.UTC().AddDate(0, 0, -1)
			}
		}
		if createTime.IsZero() {
			continue
		}
		urlPath := reFind(mp2Re)
		title := reFind(mp3Re)
		sidRaw := reFind(mp4Re)
		pirRaw := reFind(mp5Re)
		sizeName := reFind(mp6Re)
		magnet := reFind(mp7Re)
		if urlPath == "" || title == "" || sidRaw == "" || pirRaw == "" || sizeName == "" || magnet == "" {
			continue
		}

		name, original, relased := parseTitle(cat, title)
		if strings.TrimSpace(name) == "" {
			name = fallbackName(title)
		}
		types := typesForCategory(cat)
		if strings.TrimSpace(name) == "" || len(types) == 0 {
			continue
		}
		sid, _ := strconv.Atoi(sidRaw)
		pir, _ := strconv.Atoi(pirRaw)
		out = append(out, filedb.TorrentRecord{
			TrackerName:  trackerName,
			Types:        types,
			URL:          strings.TrimRight(host, "/") + "/" + strings.TrimLeft(urlPath, "/"),
			Title:        title,
			Sid:          sid,
			Pir:          pir,
			SizeName:     sizeName,
			Magnet:       magnet,
			CreateTime:   createTime.UTC().Format(time.RFC3339Nano),
			UpdateTime:   now.UTC().Format(time.RFC3339Nano),
			Name:         name,
			OriginalName: original,
			Relased:      relased,
			SearchName:   core.SearchName(name),
			SearchOrig:   core.SearchName(firstNonEmpty(original, name)),
		}.ToMap())
	}
	return out
}

func (p *Parser) saveTorrents(torrents []filedb.TorrentDetails) (int, int, int, int, error) {
	added, updated, skipped, failed := 0, 0, 0, 0
	plog := core.NewParserLog(trackerName, filepath.Join(p.DataDir, "log"), p.Config.LogParsers && p.Config.TorrentBy.Log)
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
		if !exists {
			if oldURL, found := filedb.FindByTrackerID(bucket, trackerName, urlv); found {
				existing = bucket[oldURL]
				delete(bucket, oldURL)
				exists = true
			}
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

func (p *Parser) loadTasks() error {
	path := filepath.Join(p.DataDir, "temp", "torrentby_taskParse.json")
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			p.tasks = map[string][]Task{}
			return nil
		}
		return err
	}
	tasks := map[string][]Task{}
	if err := json.Unmarshal(b, &tasks); err != nil {
		return err
	}
	p.tasks = tasks
	return nil
}

func (p *Parser) saveTasksLocked() error {
	path := filepath.Join(p.DataDir, "temp", "torrentby_taskParse.json")
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

func (p *Parser) cookie() string {
	p.cookieMu.Lock()
	if strings.TrimSpace(p.dynCookie) != "" {
		c := p.dynCookie
		p.cookieMu.Unlock()
		return c
	}
	p.cookieMu.Unlock()
	if strings.TrimSpace(p.Config.TorrentBy.Cookie) != "" {
		return strings.TrimSpace(p.Config.TorrentBy.Cookie)
	}
	b, err := os.ReadFile(filepath.Join(p.DataDir, "temp", "torrentby.cookie"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (p *Parser) takeLogin(ctx context.Context) error {
	p.cookieMu.Lock()
	if time.Since(p.lastLoginAttempt) < 2*time.Minute {
		p.cookieMu.Unlock()
		return nil
	}
	p.lastLoginAttempt = time.Now()
	p.cookieMu.Unlock()

	host := strings.TrimRight(p.Config.TorrentBy.Host, "/")
	if host == "" || strings.TrimSpace(p.Config.TorrentBy.Login.U) == "" {
		return fmt.Errorf("torrentby: no host or login configured")
	}
	log.Printf("torrentby: attempting login to %s as %s", host, p.Config.TorrentBy.Login.U)

	form := url.Values{}
	form.Set("username", p.Config.TorrentBy.Login.U)
	form.Set("password", p.Config.TorrentBy.Login.P)
	form.Set("login", "Вход")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/login/", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	loginClient := &http.Client{
		Timeout:   20 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := loginClient.Do(req)
	if err != nil {
		log.Printf("torrentby: login error: %v", err)
		return err
	}
	defer resp.Body.Close()
	log.Printf("torrentby: login response status=%d", resp.StatusCode)

	var parts []string
	for _, line := range resp.Header.Values("Set-Cookie") {
		part := strings.SplitN(line, ";", 2)[0]
		parts = append(parts, part)
	}
	cookieStr := strings.Join(parts, "; ")
	if strings.Contains(cookieStr, "uid=") && strings.Contains(cookieStr, "pass=") {
		p.cookieMu.Lock()
		p.dynCookie = cookieStr
		p.cookieMu.Unlock()
		if p.cookieStore != nil {
			_ = p.cookieStore.Save(trackerName, cookieStr)
		}
		log.Printf("torrentby: login OK")
		return nil
	}
	log.Printf("torrentby: login FAILED — cookies: %s", cookieStr)
	return fmt.Errorf("torrentby: login failed")
}

func (p *Parser) ensureLogin(ctx context.Context) {
	if p.cookie() != "" {
		return
	}
	_ = p.takeLogin(ctx)
}

func parseTaskTime(s string, loc *time.Location) time.Time {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "0001-01-01") {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04:05Z07:00"} {
		if tm, err := time.ParseInLocation(layout, s, loc); err == nil {
			return tm.In(loc)
		}
	}
	return time.Time{}
}

func parseTitle(cat, title string) (string, string, int) {
	switch cat {
	case "films":
		if m := inlineYearRe.FindStringSubmatch(title); len(m) == 4 {
			return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), atoi(m[3])
		}
		if m := inlineYearRe2.FindStringSubmatch(title); len(m) == 4 {
			return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), atoi(m[3])
		}
	case "movies":
		if m := inlineYearRe3.FindStringSubmatch(title); len(m) == 4 {
			return strings.TrimSpace(m[1]), "", atoi(m[3])
		}
	case "serials", "series":
		if m := inlineYearRe4.FindStringSubmatch(title); len(m) >= 4 {
			return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), atoi(m[3])
		}
		if m := inlineYearRe5.FindStringSubmatch(title); len(m) >= 4 {
			return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), atoi(m[3])
		}
		if m := inlineYearRe6.FindStringSubmatch(title); len(m) >= 4 {
			return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), atoi(m[3])
		}
		if m := inlineYearRe7.FindStringSubmatch(title); len(m) >= 3 {
			return strings.TrimSpace(m[1]), "", atoi(m[2])
		}
	case "cartoons", "anime", "tv", "humor", "sport":
		if strings.Contains(title, " / ") {
			if strings.Contains(title, "[") && strings.Contains(title, "]") {
				if m := inlineYearRe8.FindStringSubmatch(title); len(m) >= 4 {
					return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), atoi(m[3])
				}
				if m := inlineYearRe9.FindStringSubmatch(title); len(m) >= 4 {
					return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), atoi(m[3])
				}
			} else {
				if m := inlineYearRe10.FindStringSubmatch(title); len(m) >= 4 {
					return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), atoi(m[3])
				}
				if m := inlineYearRe11.FindStringSubmatch(title); len(m) >= 4 {
					return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), atoi(m[3])
				}
			}
		} else {
			if strings.Contains(title, "[") && strings.Contains(title, "]") {
				if m := inlineYearRe12.FindStringSubmatch(title); len(m) >= 3 {
					return strings.TrimSpace(m[1]), "", atoi(m[2])
				}
			} else if m := inlineYearRe13.FindStringSubmatch(title); len(m) >= 3 {
				return strings.TrimSpace(m[1]), "", atoi(m[2])
			}
		}
	}
	return "", "", 0
}

func typesForCategory(cat string) []string {
	switch cat {
	case "films", "movies":
		return []string{"movie"}
	case "serials", "series":
		return []string{"serial"}
	case "tv", "humor":
		return []string{"tvshow"}
	case "cartoons":
		return []string{"multfilm", "multserial"}
	case "anime":
		return []string{"anime"}
	case "sport":
		return []string{"sport"}
	default:
		return nil
	}
}

func requestHost(t app.TrackerSettings) string {
	if strings.TrimSpace(t.Alias) != "" {
		return strings.TrimSpace(t.Alias)
	}
	return strings.TrimSpace(t.Host)
}

func parseCreateTime(v string) time.Time {
	v = strings.TrimSpace(strings.ReplaceAll(v, "-", " "))
	if v == "" {
		return time.Time{}
	}
	tm, _ := time.ParseInLocation("2006 01 02", v, time.Local)
	return tm
}

func replaceBadNames(s string) string {
	s = strings.ReplaceAll(s, "Ванда/Вижн ", "ВандаВижн ")
	s = strings.ReplaceAll(s, "Ё", "Е")
	s = strings.ReplaceAll(s, "ё", "е")
	return s
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

func fallbackName(title string) string {
	return strings.TrimSpace(firstNamePart.Split(title, 2)[0])
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

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
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
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	default:
		return 0
	}
}
