package bitru

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
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

const trackerName = "bitru"

var (
	rowSplitRe       = regexp.MustCompile(`(?i)<div class="b-title"`)
	cleanSpaceRe     = regexp.MustCompile(`[\n\r\t\x{00A0} ]+`)
	maxPagesRe       = regexp.MustCompile(`(?i)<a href="browse\.php\?tmp=[^"]+&page=[^"]+">([0-9]+)</a></div>`)
	detailsURLRe     = regexp.MustCompile(`(?i)href="(details\.php\?id=[0-9]+)"`)
	titleRe          = regexp.MustCompile(`(?i)<div class="it-title">([^<]+)</div>`)
	sidRe            = regexp.MustCompile(`(?i)<span class="b-seeders">([0-9]+)</span>`)
	pirRe            = regexp.MustCompile(`(?i)<span class="b-leechers">([0-9]+)</span>`)
	sizeNameRe       = regexp.MustCompile(`(?i)title="Размер">([^<]+)</td>`)
	dateRe           = regexp.MustCompile(`(?i)<div class="ellips"><span>([0-9]{2}) ([^ ]+) ([0-9]{4}) в ([0-9]{2}:[0-9]{2}) от <a`)
	yearMain4Re      = regexp.MustCompile(`^([^/\(]+) / [^/]+ / [^/]+ / ([^/\(]+) \(([0-9]{4})\)`)
	yearMain3Re      = regexp.MustCompile(`^([^/\(]+) / [^/]+ / ([^/\(]+) \(([0-9]{4})\)`)
	yearMain2Re      = regexp.MustCompile(`^([^/\(]+) / ([^/\(]+) \(([0-9]{4})\)`)
	yearOnlyRe       = regexp.MustCompile(`^([^/\(]+) \(([0-9]{4})\)`)
	serialSeason3Re  = regexp.MustCompile(`^([^/\(]+) [0-9\-]+ сезон [^/]+ / [^/]+ / ([^/\(]+) \(([0-9]{4})(\)|-)`)
	serialSeason2Re  = regexp.MustCompile(`^([^/\(]+) / [^/]+ / ([^/\(]+) \(([0-9]{4})(\)|-)`)
	serialSeason1Re  = regexp.MustCompile(`^([^/\(]+) [0-9\-]+ сезон [^/]+ / ([^/\(]+) \(([0-9]{4})(\)|-)`)
	serialSeasonRuRe = regexp.MustCompile(`^([^/\(]+) [0-9\-]+ сезон \([^\)]+\) +\(([0-9]{4})(\)|-)`)
	serialPlainRe    = regexp.MustCompile(`^([^/\(]+) \([^\)]+\) +\(([0-9]{4})(\)|-)`)
	downloadIDRe     = regexp.MustCompile(`id=([0-9]+)`)
)

type Task struct {
	UpdateTime string `json:"updateTime"`
	Page       int    `json:"page"`
}

func (t Task) UpdatedToday() bool {
	if strings.TrimSpace(t.UpdateTime) == "" {
		return false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if tm, err := time.Parse(layout, t.UpdateTime); err == nil {
			now := time.Now().UTC()
			return tm.UTC().Year() == now.Year() && tm.UTC().YearDay() == now.YearDay()
		}
	}
	return false
}

func (t *Task) MarkToday() { t.UpdateTime = time.Now().UTC().Format(time.RFC3339Nano) }

type ParseResult struct {
	Status      string         `json:"status"`
	Fetched     int            `json:"fetched"`
	Added       int            `json:"added"`
	Updated     int            `json:"updated"`
	Skipped     int            `json:"skipped"`
	Failed      int            `json:"failed"`
	PerCategory map[string]int `json:"by_category"`
}

type Parser struct {
	Config  app.Config
	DB      *filedb.DB
	DataDir string
	Client  *http.Client

	mu       sync.Mutex
	working  bool
	allWork  bool
	latestMu sync.Mutex
	tasks    map[string][]Task
}

func New(cfg app.Config, db *filedb.DB, dataDir string) *Parser {
	p := &Parser{Config: cfg, DB: db, DataDir: dataDir, Client: &http.Client{Timeout: 35 * time.Second}, tasks: map[string][]Task{}}
	_ = p.loadTasks()
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
	if page <= 0 {
		page = 1
	}
	res := ParseResult{Status: "ok", PerCategory: map[string]int{}}
	for _, cat := range []string{"movie", "serial"} {
		items, err := p.parsePage(ctx, cat, page)
		if err != nil {
			return res, err
		}
		res.Fetched += len(items)
		res.PerCategory[cat] = len(items)
		if len(items) == 0 {
			continue
		}
		a, u, s, f, err := p.saveTorrents(ctx, items)
		if err != nil {
			return res, err
		}
		res.Added += a
		res.Updated += u
		res.Skipped += s
		res.Failed += f
	}
	return res, nil
}

func (p *Parser) UpdateTasksParse(ctx context.Context) (map[string][]Task, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tasks == nil {
		p.tasks = map[string][]Task{}
	}
	for _, cat := range []string{"movie", "serial"} {
		htmlBody, err := p.fetchBrowse(ctx, cat, 1)
		if err != nil || htmlBody == "" {
			continue
		}
		maxPages := 1
		if m := maxPagesRe.FindStringSubmatch(htmlBody); len(m) > 1 {
			if n, _ := strconv.Atoi(strings.TrimSpace(m[1])); n > 0 {
				maxPages = n
			}
		}
		pagesMap := map[int]Task{}
		for _, t := range p.tasks[cat] {
			pagesMap[t.Page] = t
		}
		for page := 1; page <= maxPages; page++ {
			if _, ok := pagesMap[page]; !ok {
				pagesMap[page] = Task{Page: page}
			}
		}
		merged := make([]Task, 0, len(pagesMap))
		for _, t := range pagesMap {
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
	p.mu.Lock()
	if p.allWork {
		p.mu.Unlock()
		return "work", nil
	}
	p.allWork = true
	snapshot := cloneTasks(p.tasks)
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.allWork = false; p.mu.Unlock() }()
	for cat, tasks := range snapshot {
		for _, task := range tasks {
			if task.UpdatedToday() {
				continue
			}
			if p.Config.Bitru.ParseDelay > 0 {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(time.Duration(p.Config.Bitru.ParseDelay) * time.Millisecond):
				}
			}
			items, err := p.parsePage(ctx, cat, task.Page)
			if err != nil {
				return "", err
			}
			if len(items) == 0 {
				continue
			}
			if _, _, _, _, err := p.saveTorrents(ctx, items); err != nil {
				return "", err
			}
			p.mu.Lock()
			for i := range p.tasks[cat] {
				if p.tasks[cat][i].Page == task.Page {
					p.tasks[cat][i].MarkToday()
				}
			}
			if err := p.saveTasksLocked(); err != nil {
				p.mu.Unlock()
				return "", err
			}
			p.mu.Unlock()
		}
	}
	return "ok", nil
}

func (p *Parser) ParseLatest(ctx context.Context, pages int) (string, error) {
	p.latestMu.Lock()
	defer p.latestMu.Unlock()
	if pages <= 0 {
		pages = 5
	}
	p.mu.Lock()
	snapshot := cloneTasks(p.tasks)
	p.mu.Unlock()
	logLines := make([]string, 0)
	for cat, tasks := range snapshot {
		sort.Slice(tasks, func(i, j int) bool { return tasks[i].Page < tasks[j].Page })
		if len(tasks) > pages {
			tasks = tasks[:pages]
		}
		for _, task := range tasks {
			if p.Config.Bitru.ParseDelay > 0 {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(time.Duration(p.Config.Bitru.ParseDelay) * time.Millisecond):
				}
			}
			items, err := p.parsePage(ctx, cat, task.Page)
			if err != nil {
				return "", err
			}
			if len(items) == 0 {
				continue
			}
			if _, _, _, _, err := p.saveTorrents(ctx, items); err != nil {
				return "", err
			}
			logLines = append(logLines, fmt.Sprintf("%s - %d", cat, task.Page))
			p.mu.Lock()
			for i := range p.tasks[cat] {
				if p.tasks[cat][i].Page == task.Page {
					p.tasks[cat][i].MarkToday()
				}
			}
			_ = p.saveTasksLocked()
			p.mu.Unlock()
		}
	}
	if len(logLines) == 0 {
		return "ok", nil
	}
	return strings.Join(logLines, "\n"), nil
}

func (p *Parser) parsePage(ctx context.Context, cat string, page int) ([]filedb.TorrentDetails, error) {
	htmlBody, err := p.fetchBrowse(ctx, cat, page)
	if err != nil || htmlBody == "" || !strings.Contains(htmlBody, `id="logo"`) {
		return nil, err
	}
	return parsePageHTML(strings.TrimRight(p.Config.Bitru.Host, "/"), cat, htmlBody, time.Now().UTC()), nil
}

func parsePageHTML(host, cat, htmlBody string, now time.Time) []filedb.TorrentDetails {
	items := make([]filedb.TorrentDetails, 0)
	for _, row := range rowSplitRe.Split(htmlBody, -1)[1:] {
		if strings.TrimSpace(row) == "" || strings.Contains(row, ">Аниме</a>") || strings.Contains(row, ">Мульт") {
			continue
		}
		match := func(re *regexp.Regexp, group int) string {
			m := re.FindStringSubmatch(row)
			if len(m) <= group {
				return ""
			}
			res := html.UnescapeString(strings.TrimSpace(m[group]))
			res = cleanSpaceRe.ReplaceAllString(res, " ")
			return strings.TrimSpace(res)
		}
		createTime := time.Time{}
		if strings.Contains(row, "<span>Сегодня") {
			createTime = now.UTC()
		} else if strings.Contains(row, "<span>Вчера") {
			createTime = now.UTC().AddDate(0, 0, -1)
		} else if m := dateRe.FindStringSubmatch(row); len(m) == 5 {
			createTime = parseBitruDate(m[1], m[2], m[3], m[4])
		}
		if createTime.IsZero() {
			continue
		}
		urlPath := match(detailsURLRe, 1)
		title := match(titleRe, 1)
		sidRaw := match(sidRe, 1)
		pirRaw := match(pirRe, 1)
		sizeName := match(sizeNameRe, 1)
		if urlPath == "" || title == "" || sidRaw == "" || pirRaw == "" || sizeName == "" {
			continue
		}
		name, original, relased := parseTitle(cat, title, row)
		if strings.TrimSpace(name) == "" {
			name = fallbackName(title)
		}
		if strings.TrimSpace(name) == "" {
			continue
		}
		sid, _ := strconv.Atoi(sidRaw)
		pir, _ := strconv.Atoi(pirRaw)
		types := []string{cat}
		items = append(items, filedb.TorrentDetails{
			"trackerName":  trackerName,
			"types":        types,
			"url":          strings.TrimRight(host, "/") + "/" + strings.TrimLeft(urlPath, "/"),
			"title":        title,
			"sid":          sid,
			"pir":          pir,
			"sizeName":     sizeName,
			"size":         parseSizeBytes(sizeName),
			"createTime":   createTime.Format(time.RFC3339Nano),
			"updateTime":   now.UTC().Format(time.RFC3339Nano),
			"name":         name,
			"originalname": original,
			"relased":      relased,
			"_sn":          core.SearchName(name),
			"_so":          core.SearchName(firstNonEmpty(original, name)),
		})
	}
	return items
}

func parseTitle(cat, title, row string) (string, string, int) {
	title = strings.TrimSpace(title)
	if cat == "movie" {
		if m := yearMain4Re.FindStringSubmatch(title); len(m) > 3 {
			y, _ := strconv.Atoi(m[3])
			return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), y
		}
		if m := yearMain3Re.FindStringSubmatch(title); len(m) > 3 {
			y, _ := strconv.Atoi(m[3])
			return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), y
		}
		if m := yearMain2Re.FindStringSubmatch(title); len(m) > 3 {
			y, _ := strconv.Atoi(m[3])
			return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), y
		}
		if m := yearOnlyRe.FindStringSubmatch(title); len(m) > 2 {
			y, _ := strconv.Atoi(m[2])
			return strings.TrimSpace(m[1]), "", y
		}
	}
	if cat == "serial" {
		if strings.Contains(strings.ToLower(row), "сезон") {
			if m := serialSeason3Re.FindStringSubmatch(title); len(m) > 3 {
				y, _ := strconv.Atoi(m[3])
				return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), y
			}
			if m := serialSeason2Re.FindStringSubmatch(title); len(m) > 3 {
				y, _ := strconv.Atoi(m[3])
				return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), y
			}
			if m := serialSeason1Re.FindStringSubmatch(title); len(m) > 3 {
				y, _ := strconv.Atoi(m[3])
				return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), y
			}
			if m := serialSeasonRuRe.FindStringSubmatch(title); len(m) > 2 {
				y, _ := strconv.Atoi(m[2])
				return strings.TrimSpace(m[1]), "", y
			}
		} else if m := serialPlainRe.FindStringSubmatch(title); len(m) > 2 {
			y, _ := strconv.Atoi(m[2])
			return strings.TrimSpace(m[1]), "", y
		}
	}
	return "", "", 0
}

func fallbackName(title string) string {
	parts := regexp.MustCompile(`(\[|\/|\(|\|)`).Split(title, 2)
	if len(parts) == 0 {
		return ""
	}
	return strings.TrimSpace(parts[0])
}

func parseBitruDate(day, monWord, year, hm string) time.Time {
	months := map[string]string{"января": "01", "февраля": "02", "марта": "03", "апреля": "04", "мая": "05", "июня": "06", "июля": "07", "августа": "08", "сентября": "09", "октября": "10", "ноября": "11", "декабря": "12"}
	mon, ok := months[strings.ToLower(strings.TrimSpace(monWord))]
	if !ok {
		return time.Time{}
	}
	tm, err := time.Parse(time.RFC3339, fmt.Sprintf("%s-%s-%sT%s:00Z", strings.TrimSpace(year), mon, strings.TrimSpace(day), strings.TrimSpace(hm)))
	if err != nil {
		return time.Time{}
	}
	return tm.UTC()
}

func parseSizeBytes(v string) float64 {
	m := regexp.MustCompile(`(?i)([0-9]+(?:[\.,][0-9]+)?)\s*(TB|GB|MB|KB|ТБ|ГБ|МБ|КБ)`).FindStringSubmatch(strings.TrimSpace(v))
	if len(m) < 3 {
		return 0
	}
	n, _ := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", "."), 64)
	switch strings.ToUpper(m[2]) {
	case "TB", "ТБ":
		return n * 1024 * 1024 * 1024 * 1024
	case "GB", "ГБ":
		return n * 1024 * 1024 * 1024
	case "MB", "МБ":
		return n * 1024 * 1024
	case "KB", "КБ":
		return n * 1024
	}
	return 0
}

func (p *Parser) saveTorrents(ctx context.Context, torrents []filedb.TorrentDetails) (int, int, int, int, error) {
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
		if exists && strings.TrimSpace(asString(existing["title"])) == strings.TrimSpace(asString(incoming["title"])) {
			skipped++
			continue
		}
		downloadURL := ""
		if m := downloadIDRe.FindStringSubmatch(urlv); len(m) == 2 {
			downloadURL = strings.TrimRight(p.Config.Bitru.Host, "/") + "/download.php?id=" + m[1]
		}
		if downloadURL == "" {
			failed++
			continue
		}
		b, err := p.download(ctx, downloadURL, urlv)
		magnet := ""
		if err == nil {
			magnet = core.TorrentBytesToMagnet(b)
		}
		if strings.TrimSpace(magnet) == "" {
			failed++
			continue
		}
		incoming["magnet"] = magnet
		merged := mergeTorrent(existing, exists, incoming)
		bucket[urlv] = merged
		changed[key] = fileTime(merged)
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

func (p *Parser) fetchBrowse(ctx context.Context, cat string, page int) (string, error) {
	if page <= 0 {
		page = 1
	}
	urlv := strings.TrimRight(p.Config.Bitru.Host, "/") + "/browse.php?tmp=" + cat + "&page=" + strconv.Itoa(page)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlv, nil)
	if err != nil {
		return "", err
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("bitru status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (p *Parser) download(ctx context.Context, rawURL, referer string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(referer) != "" {
		req.Header.Set("Referer", referer)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("download status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func (p *Parser) tasksPath() string { return filepath.Join(p.DataDir, "temp", "bitru_taskParse.json") }

func (p *Parser) loadTasks() error {
	path := p.tasksPath()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var tasks map[string][]Task
	if err := json.Unmarshal(b, &tasks); err != nil {
		return err
	}
	if tasks == nil {
		tasks = map[string][]Task{}
	}
	p.tasks = tasks
	return nil
}

func (p *Parser) saveTasksLocked() error {
	if p.tasks == nil {
		p.tasks = map[string][]Task{}
	}
	if err := os.MkdirAll(filepath.Dir(p.tasksPath()), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p.tasks, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p.tasksPath(), b, 0o644)
}

func cloneTasks(src map[string][]Task) map[string][]Task {
	out := make(map[string][]Task, len(src))
	for k, list := range src {
		dup := make([]Task, len(list))
		copy(dup, list)
		out[k] = dup
	}
	return out
}

func mergeTorrent(existing filedb.TorrentDetails, exists bool, incoming filedb.TorrentDetails) filedb.TorrentDetails {
	out := filedb.TorrentDetails{}
	if exists {
		for k, v := range existing {
			out[k] = v
		}
	}
	for k, v := range incoming {
		out[k] = v
	}
	out["_sn"] = core.SearchName(asString(out["name"]))
	out["_so"] = core.SearchName(firstNonEmpty(asString(out["originalname"]), asString(out["name"])))
	if fileTime(out).IsZero() {
		out["updateTime"] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	return out
}

func fileTime(t filedb.TorrentDetails) time.Time {
	for _, key := range []string{"updateTime", "createTime"} {
		if tm, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(asString(t[key]))); err == nil {
			return tm.UTC()
		}
	}
	return time.Time{}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}
func isDisabled(list []string, tracker string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), tracker) {
			return true
		}
	}
	return false
}
