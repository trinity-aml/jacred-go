package rutor

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
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

const trackerName = "rutor"

var (
	rowSplitRe      = regexp.MustCompile(`<tr class="(?:gai|tum)">`)
	whitespaceRe    = regexp.MustCompile(`[\n\r\t]+`)
	cleanupSpaceRe  = regexp.MustCompile(`[\n\r\t\x{00A0} ]+`)
	firstNamePartRe = regexp.MustCompile(`(\[|/|\(|\|)`)

	// parsePageHTML field extractors
	createTimeRe = regexp.MustCompile(`(?i)<td>([^<]+)</td><td(?:[^>]+)?><a class="downgif"`)
	urlPathRe    = regexp.MustCompile(`(?i)<a href="/(torrent/[^"]+)">`)
	titleRe      = regexp.MustCompile(`(?i)<a href="/torrent/[^"]+">([^<]+)</a>`)
	sidRawRe     = regexp.MustCompile(`(?i)<span class="green"><img [^>]+>&nbsp;([0-9]+)</span>`)
	pirRawRe     = regexp.MustCompile(`(?i)<span class="red">&nbsp;([0-9]+)</span>`)
	sizeNameRe   = regexp.MustCompile(`(?i)<td align="right">([^<]+)</td>`)
	magnetRe     = regexp.MustCompile(`(?i)href="(magnet:\?xt=[^"]+)"`)

	// pagination: finds page numbers in browse links, e.g. href="/browse/12/1/0/0"
	browsePagesRe = regexp.MustCompile(`href="/browse/([0-9]+)/[0-9]+/0/0"`)

	// parseTitle patterns
	movieFullRe       = regexp.MustCompile(`^([^/]+) / ([^/]+) / ([^/\(]+) \(([0-9]{4})\)`)
	movieShortRe      = regexp.MustCompile(`^([^/\(]+) / ([^/\(]+) \(([0-9]{4})\)`)
	musicYearRe       = regexp.MustCompile(`^([^/\(]+) \(([0-9]{4})\)`)
	serialPattern1Re  = regexp.MustCompile(`^([^/]+) / [^/]+ / [^/]+ / ([^/\[]+) \[[^\]]+\] +\(([0-9]{4})(\)|-)`)
	serialPattern2Re  = regexp.MustCompile(`^([^/]+) / [^/]+ / ([^/\[]+) \[[^\]]+\] +\(([0-9]{4})(\)|-)`)
	serialPattern3Re  = regexp.MustCompile(`^([^/]+) / ([^/\[]+) \[[^\]]+\] +\(([0-9]{4})(\)|-)`)
	multSerialRe      = regexp.MustCompile(`^([^/]+) \[[^\]]+\] \(([0-9]{4})(\)|-)`)
	genBracketFullRe  = regexp.MustCompile(`^([^/]+) / ([^/]+) / ([^/\[]+) \[[^\]]+\] +\(([0-9]{4})(\)|-)`)
	genBracketShortRe = regexp.MustCompile(`^([^/]+) / ([^/\[]+) \[[^\]]+\] +\(([0-9]{4})(\)|-)`)
	genSlashFullRe    = regexp.MustCompile(`^([^/]+) / ([^/]+) / ([^/\(]+) \(([0-9]{4})\)`)
	genSlashShortRe   = regexp.MustCompile(`^([^/\(]+) / ([^/\(]+) \(([0-9]{4})\)`)
	genNoBracketRe    = regexp.MustCompile(`^([^/\[]+) \[[^\]]+\] +\(([0-9]{4})(\)|-)`)
	genPlainYearRe    = regexp.MustCompile(`^([^/\(]+) \(([0-9]{4})\)`)
	singleDigitDateRe = regexp.MustCompile(`^[0-9]\.`)
)

var categories = []string{"1", "5", "4", "16", "12", "6", "7", "10", "17", "13", "15"}

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
	mu      sync.Mutex
	working bool
	allWork bool
	latestMu sync.Mutex
	tasks   map[string][]Task
}

type ParseResult struct {
	Fetched, Added, Updated, Skipped, Failed int
	Status                                   string
	PerCategory                              map[string]int
}

func New(cfg app.Config, db *filedb.DB, dataDir string) *Parser {
	p := &Parser{Config: cfg, DB: db, DataDir: dataDir, Fetcher: core.NewFetcher(cfg), tasks: map[string][]Task{}}
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
	defer func() {
		p.mu.Lock()
		p.working = false
		p.mu.Unlock()
	}()

	res := ParseResult{Status: "ok", PerCategory: map[string]int{}}
	if strings.TrimSpace(p.Config.Rutor.Host) == "" {
		return ParseResult{Status: "config missing"}, nil
	}

	for idx, cat := range categories {
		if idx > 0 && p.Config.Rutor.ParseDelay > 0 {
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-time.After(time.Duration(p.Config.Rutor.ParseDelay) * time.Millisecond):
			}
		}
		items, err := p.fetchPage(ctx, cat, page)
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
		log.Printf("rutor: cat=%s fetched=%d added=%d skipped=%d failed=%d", cat, len(items), added, skipped, failed)
	}
	log.Printf("rutor: done fetched=%d added=%d skipped=%d failed=%d", res.Fetched, res.Added, res.Skipped, res.Failed)
	return res, nil
}

func (p *Parser) fetchPage(ctx context.Context, cat string, page int) ([]filedb.TorrentDetails, error) {
	host := strings.TrimRight(p.Config.Rutor.Host, "/")
	htmlBody, err := p.fetchPageRaw(ctx, cat, page)
	if err != nil {
		return nil, err
	}
	return parsePageHTML(host, cat, htmlBody), nil
}

func parsePageHTML(host, cat, htmlBody string) []filedb.TorrentDetails {
	cleaned := whitespaceRe.ReplaceAllString(htmlBody, "")
	chunks := rowSplitRe.Split(cleaned, -1)
	out := make([]filedb.TorrentDetails, 0, len(chunks))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, row := range chunks[1:] {
		if strings.TrimSpace(row) == "" || !strings.Contains(row, "magnet:?xt=urn") {
			continue
		}
		extract := func(re *regexp.Regexp, idx int) string {
			g := re.FindStringSubmatch(row)
			if len(g) <= idx {
				return ""
			}
			s := strings.TrimSpace(html.UnescapeString(g[idx]))
			s = cleanupSpaceRe.ReplaceAllString(strings.ReplaceAll(s, "\u0000", " "), " ")
			return strings.TrimSpace(s)
		}

		createTime := parseCreateTime(extract(createTimeRe, 1), "02.01.06")
		if createTime.IsZero() {
			continue
		}
		urlPath := extract(urlPathRe, 1)
		title := extract(titleRe, 1)
		sidRaw := extract(sidRawRe, 1)
		pirRaw := extract(pirRawRe, 1)
		sizeName := extract(sizeNameRe, 1)
		magnet := extract(magnetRe, 1)
		if urlPath == "" || title == "" || strings.Contains(strings.ToLower(title), "трейлер") || sidRaw == "" || pirRaw == "" || sizeName == "" || magnet == "" {
			continue
		}
		if cat == "17" && !strings.Contains(title, " UKR") {
			continue
		}
		if strings.Contains(title, " КПК") {
			continue
		}

		name, original, relased := parseTitle(cat, title)
		if strings.TrimSpace(name) == "" {
			name = fallbackName(title)
		}
		if strings.TrimSpace(name) == "" {
			continue
		}
		types := typesForCategory(cat)
		if len(types) == 0 {
			continue
		}
		sid, _ := strconv.Atoi(sidRaw)
		pir, _ := strconv.Atoi(pirRaw)
		td := filedb.TorrentRecord{
			TrackerName: trackerName,
			Types: types,
			URL: strings.TrimRight(host, "/") + "/" + strings.TrimLeft(urlPath, "/"),
			Title: title,
			Sid: sid,
			Pir: pir,
			SizeName: sizeName,
			Magnet: magnet,
			CreateTime: createTime.UTC().Format(time.RFC3339Nano),
			UpdateTime: now,
			Name: name,
			OriginalName: original,
			Relased: relased,
			SearchName: core.SearchName(name),
			SearchOrig: core.SearchName(firstNonEmpty(original, name)),
		}.ToMap()
		out = append(out, td)
	}
	return out
}

func (p *Parser) saveTorrents(torrents []filedb.TorrentDetails) (int, int, int, int, error) {
	added, updated, skipped, failed := 0, 0, 0, 0
	plog := core.NewParserLog(trackerName, filepath.Join(p.DB.DataDir, "log"), p.Config.LogParsers && p.Config.Rutor.Log)
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
		result := filedb.MergeTorrent(ex, incoming)
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

func (p *Parser) UpdateTasksParse(ctx context.Context) (map[string][]Task, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tasks == nil {
		p.tasks = map[string][]Task{}
	}
	for _, cat := range categories {
		htmlBody, err := p.fetchPageRaw(ctx, cat, 0)
		if err != nil {
			continue
		}
		maxPage := 0
		for _, m := range browsePagesRe.FindAllStringSubmatch(htmlBody, -1) {
			if n, err := strconv.Atoi(m[1]); err == nil && n > maxPage {
				maxPage = n
			}
		}
		existing := p.tasks[cat]
		pages := map[int]Task{}
		for _, t := range existing {
			pages[t.Page] = t
		}
		for pg := 0; pg <= maxPage; pg++ {
			if _, ok := pages[pg]; !ok {
				pages[pg] = Task{Page: pg, UpdateTime: "0001-01-01T00:00:00"}
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
			if task.UpdatedToday() {
				continue
			}
			if p.Config.Rutor.ParseDelay > 0 {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(time.Duration(p.Config.Rutor.ParseDelay) * time.Millisecond):
				}
			}
			items, err := p.fetchPage(ctx, cat, task.Page)
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
							list2[i].MarkToday()
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
	var lines []string
	for cat, list := range snapshot {
		sort.Slice(list, func(i, j int) bool { return list[i].Page < list[j].Page })
		if len(list) > pages {
			list = list[:pages]
		}
		for _, task := range list {
			if p.Config.Rutor.ParseDelay > 0 {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(time.Duration(p.Config.Rutor.ParseDelay) * time.Millisecond):
				}
			}
			items, err := p.fetchPage(ctx, cat, task.Page)
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
						list2[i].MarkToday()
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

func (p *Parser) fetchPageRaw(ctx context.Context, cat string, page int) (string, error) {
	host := strings.TrimRight(p.Config.Rutor.Host, "/")
	urlv := fmt.Sprintf("%s/browse/%d/%s/0/0", host, page, cat)
	body, _, err := p.Fetcher.GetString(urlv, p.Config.Rutor)
	return body, err
}

func (p *Parser) loadTasks() error {
	path := filepath.Join(p.DataDir, "temp", "rutor_taskParse.json")
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
	path := filepath.Join(p.DataDir, "temp", "rutor_taskParse.json")
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

func parseTitle(cat, title string) (string, string, int) {
	switch cat {
	case "1", "17":
		if m := movieFullRe.FindStringSubmatch(title); len(m) == 5 {
			return strings.TrimSpace(m[1]), strings.TrimSpace(m[3]), atoi(m[4])
		}
		if m := movieShortRe.FindStringSubmatch(title); len(m) == 4 {
			return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), atoi(m[3])
		}
	case "5":
		if m := musicYearRe.FindStringSubmatch(title); len(m) == 3 {
			return strings.TrimSpace(m[1]), "", atoi(m[2])
		}
	case "4":
		for _, re := range []*regexp.Regexp{serialPattern1Re, serialPattern2Re, serialPattern3Re} {
			if m := re.FindStringSubmatch(title); len(m) >= 4 {
				return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), atoi(m[3])
			}
		}
	case "16":
		if m := multSerialRe.FindStringSubmatch(title); len(m) >= 3 {
			return strings.TrimSpace(m[1]), "", atoi(m[2])
		}
	case "12", "6", "7", "10", "15", "13":
		if strings.Contains(title, " / ") {
			if strings.Contains(title, "[") && strings.Contains(title, "]") {
				if m := genBracketFullRe.FindStringSubmatch(title); len(m) >= 5 {
					return strings.TrimSpace(m[1]), strings.TrimSpace(m[3]), atoi(m[4])
				}
				if m := genBracketShortRe.FindStringSubmatch(title); len(m) >= 4 {
					return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), atoi(m[3])
				}
			} else {
				if m := genSlashFullRe.FindStringSubmatch(title); len(m) == 5 {
					return strings.TrimSpace(m[1]), strings.TrimSpace(m[3]), atoi(m[4])
				}
				if m := genSlashShortRe.FindStringSubmatch(title); len(m) == 4 {
					return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), atoi(m[3])
				}
			}
		} else {
			if strings.Contains(title, "[") && strings.Contains(title, "]") {
				if m := genNoBracketRe.FindStringSubmatch(title); len(m) >= 3 {
					return strings.TrimSpace(m[1]), "", atoi(m[2])
				}
			} else if m := genPlainYearRe.FindStringSubmatch(title); len(m) == 3 {
				return strings.TrimSpace(m[1]), "", atoi(m[2])
			}
		}
	}
	return "", "", 0
}

func fallbackName(title string) string {
	parts := firstNamePartRe.Split(title, 2)
	if len(parts) == 0 {
		return strings.TrimSpace(title)
	}
	return strings.TrimSpace(parts[0])
}

func typesForCategory(cat string) []string {
	switch cat {
	case "1", "5", "17":
		return []string{"movie"}
	case "4", "16":
		return []string{"serial"}
	case "12":
		return []string{"docuserial", "documovie"}
	case "6", "15":
		return []string{"tvshow"}
	case "7":
		return []string{"multfilm", "multserial"}
	case "10":
		return []string{"anime"}
	case "13":
		return []string{"sport"}
	default:
		return nil
	}
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

func parseCreateTime(line, layout string) time.Time {
	repl := strings.NewReplacer(
		// abbreviated with period (original C# format)
		" янв. ", ".01.", " февр. ", ".02.", " март ", ".03.", " апр. ", ".04.", " май ", ".05.", " июнь ", ".06.", " июль ", ".07.", " авг. ", ".08.", " сент. ", ".09.", " окт. ", ".10.", " нояб. ", ".11.", " дек. ", ".12.",
		// abbreviated WITHOUT period (rutor actual format: "14 Мар 26")
		" янв ", ".01.", " фев ", ".02.", " мар ", ".03.", " апр ", ".04.", " май ", ".05.", " июн ", ".06.", " июл ", ".07.", " авг ", ".08.", " сен ", ".09.", " окт ", ".10.", " ноя ", ".11.", " дек ", ".12.",
		// English full
		" january ", ".01.", " february ", ".02.", " march ", ".03.", " april ", ".04.", " may ", ".05.", " june ", ".06.", " july ", ".07.", " august ", ".08.", " september ", ".09.", " october ", ".10.", " november ", ".11.", " december ", ".12.",
		// English abbreviated
		" jan ", ".01.", " feb ", ".02.", " mar ", ".03.", " apr ", ".04.", " jun ", ".06.", " jul ", ".07.", " aug ", ".08.", " sep ", ".09.", " oct ", ".10.", " nov ", ".11.", " dec ", ".12.",
	)
	line = repl.Replace(" " + strings.ToLower(strings.TrimSpace(line)) + " ")
	line = strings.TrimSpace(line)
	if singleDigitDateRe.MatchString(line) {
		line = "0" + line
	}
	tm, _ := time.ParseInLocation(layout, line, time.Local)
	return tm
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
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
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	default:
		n, _ := strconv.Atoi(strings.TrimSpace(fmt.Sprint(v)))
		return n
	}
}
