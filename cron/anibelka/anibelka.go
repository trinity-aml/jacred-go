// Package anibelka parses anibelka.com — an anime-only phpBB tracker.
//
// Shape of the site (verified 2026-07-25):
//   - Listings live at viewforum.php?f=<id>&start=<n>, 15 topics per page.
//   - Every topic holds exactly one .torrent attachment plus a poster image;
//     the torrent anchor is the one carrying tooltip="Скачать торрент".
//   - There are no magnet links in the markup. The .torrent is downloadable
//     without an account and its info hash is identical to the one a logged-in
//     user gets, so the parser stays anonymous on purpose: a logged-in download
//     embeds the account's personal passkey in the announce URL, and that would
//     end up inside every magnet this database serves out over the search API
//     and /sync.
package anibelka

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

const trackerName = "anibelka"

// topicsPerPage is the site's fixed listing stride; pagination is ?start=N.
const topicsPerPage = 15

// section is one forum under the site's "Скачать аниме" menu. Everything here
// is anime, so the record type never varies — only the source forum does.
type section struct {
	id   string
	name string
}

var sections = []section{
	{id: "32", name: "Универсальные"},
	{id: "33", name: "С озвучкой"},
	{id: "34", name: "С субтитрами"},
	{id: "36", name: "Полнометражки"},
	{id: "37", name: "PSP"},
}

var (
	// Listing: <a href="./viewtopic.php?t=123&sid=..." class="topictitle">…</a>
	rowTopicRe = regexp.MustCompile(`(?is)href="\./viewtopic\.php\?t=(\d+)[^"]*"\s+class="topictitle">(.*?)</a>`)
	// Pagination links carry ?start=N; the largest one is the last page.
	pageStartRe = regexp.MustCompile(`start=(\d+)`)

	// Topic: the attachment anchor tagged as the torrent. A topic also carries
	// a poster image served by the same download/file.php endpoint, so the
	// tooltip is what separates them without fetching both.
	torrentLinkRe = regexp.MustCompile(`(?is)href="\./download/file\.php\?id=(\d+)[^"]*"[^>]*tooltip="Скачать торрент"`)

	// "Статистика раздачи" block.
	sizeRe  = regexp.MustCompile(`(?is)Размер:\s*<b>([0-9.,]+)&nbsp;(КБ|МБ|ГБ|ТБ)</b>`)
	addedRe = regexp.MustCompile(`(?is)Добавлен:\s*<b>\s*<span[^>]*>([^<]+)</span>`)
	seedRe  = regexp.MustCompile(`(?is)Сидеров:\s*<span class="seed">\s*<b>(\d+)</b>`)
	leechRe = regexp.MustCompile(`(?is)Личеров:\s*<span class="leech">\s*<b>(\d+)</b>`)

	tagRe    = regexp.MustCompile(`^\[(\w+)\]\s*`)
	yearRe   = regexp.MustCompile(`\[(\d{4})`)
	latinRe  = regexp.MustCompile(`[A-Za-z]`)
	cyrilRe  = regexp.MustCompile(`[А-Яа-яЁё]`)
	spacesRe = regexp.MustCompile(`\s+`)
)

// ruMonths maps the site's abbreviated Russian month names to numbers.
var ruMonths = map[string]time.Month{
	"янв": time.January, "фев": time.February, "мар": time.March,
	"апр": time.April, "май": time.May, "июн": time.June,
	"июл": time.July, "авг": time.August, "сен": time.September,
	"окт": time.October, "ноя": time.November, "дек": time.December,
}

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
		return false
	}
	y1, m1, d1 := tm.Date()
	y2, m2, d2 := time.Now().Date()
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
	loc     *time.Location

	mu       sync.Mutex
	working  bool
	allWork  bool
	latestMu sync.Mutex
	tasks    map[string][]Task
}

type ParseResult struct {
	Fetched, Added, Updated, Skipped, Failed int
	Status                                   string
	PerCategory                              map[string]int
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
	}
	_ = p.loadTasks()
	return p
}

func (p *Parser) UpdateConfig(cfg app.Config) { p.Config = cfg }

func (p *Parser) host() string {
	return strings.TrimRight(strings.TrimSpace(p.Config.Anibelka.Host), "/")
}

// listingItem is one row of a forum page.
type listingItem struct {
	topicID string
	title   string
}

// topicInfo is what a topic page contributes on top of its listing row.
type topicInfo struct {
	torrentID  string
	sizeName   string
	sid, pir   int
	createTime time.Time
}

func (p *Parser) delay(ctx context.Context) error {
	d := time.Duration(p.Config.Anibelka.ParseDelay) * time.Millisecond
	if d <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func (p *Parser) fetchPage(rawURL string) (string, error) {
	body, status, err := p.Fetcher.GetString(rawURL, p.Config.Anibelka)
	if err != nil {
		return "", err
	}
	if status >= 400 {
		return "", fmt.Errorf("http %d", status)
	}
	return body, nil
}

// Parse walks one listing page of every section. page is zero-based.
func (p *Parser) Parse(ctx context.Context, page int) (ParseResult, error) {
	p.mu.Lock()
	if p.working {
		p.mu.Unlock()
		return ParseResult{Status: "work"}, nil
	}
	p.working = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.working = false; p.mu.Unlock() }()

	if p.host() == "" {
		return ParseResult{Status: "config missing"}, nil
	}
	if isDisabled(p.Config.DisableTrackers, trackerName) {
		return ParseResult{Status: "disabled"}, nil
	}

	res := ParseResult{Status: "ok", PerCategory: map[string]int{}}
	for idx, sec := range sections {
		if idx > 0 {
			if err := p.delay(ctx); err != nil {
				return res, err
			}
		}
		torrents, items, err := p.parseSectionPage(ctx, sec, page)
		if err != nil {
			log.Printf("anibelka: section %s page %d error: %v (continuing)", sec.id, page, err)
			continue
		}
		res.Fetched += len(torrents)
		res.PerCategory[sec.id] = len(torrents)
		if len(torrents) == 0 {
			continue
		}
		a, u, s, f, err := p.saveTorrents(torrents)
		if err != nil {
			return res, err
		}
		res.Added += a
		res.Updated += u
		res.Skipped += s
		res.Failed += f
		log.Printf("anibelka: f=%s page=%d topics=%d torrents=%d added=%d skipped=%d failed=%d",
			sec.id, page, len(items), len(torrents), a, s, f)
	}
	log.Printf("anibelka: done fetched=%d added=%d skipped=%d failed=%d", res.Fetched, res.Added, res.Skipped, res.Failed)
	return res, nil
}

// parseSectionPage fetches one listing page and expands every topic on it.
func (p *Parser) parseSectionPage(ctx context.Context, sec section, page int) ([]filedb.TorrentDetails, []listingItem, error) {
	body, err := p.fetchPage(p.forumURL(sec, page))
	if err != nil {
		return nil, nil, err
	}
	items := parseListingHTML(body)
	out := make([]filedb.TorrentDetails, 0, len(items))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, it := range items {
		if err := p.delay(ctx); err != nil {
			return out, items, err
		}
		rec, err := p.buildFromTopic(ctx, sec, it, now)
		if err != nil {
			log.Printf("anibelka: topic t=%s error: %v (skipping)", it.topicID, err)
			continue
		}
		if rec != nil {
			out = append(out, rec)
		}
	}
	return out, items, nil
}

func (p *Parser) forumURL(sec section, page int) string {
	if page <= 0 {
		return fmt.Sprintf("%s/viewforum.php?f=%s", p.host(), sec.id)
	}
	return fmt.Sprintf("%s/viewforum.php?f=%s&start=%d", p.host(), sec.id, page*topicsPerPage)
}

// parseListingHTML pulls the topic rows out of a forum page. Pinned service
// topics ("Информация по разделу", "Большие аниме …") carry no [tag] prefix
// and hold no torrent, so they are dropped here rather than costing a request.
func parseListingHTML(body string) []listingItem {
	matches := rowTopicRe.FindAllStringSubmatch(body, -1)
	out := make([]listingItem, 0, len(matches))
	seen := map[string]struct{}{}
	for _, m := range matches {
		title := core.StripTagsAndCollapseSpaces(html.UnescapeString(m[2]))
		if title == "" || !strings.HasPrefix(title, "[") {
			continue
		}
		if _, dup := seen[m[1]]; dup {
			continue
		}
		seen[m[1]] = struct{}{}
		out = append(out, listingItem{topicID: m[1], title: title})
	}
	return out
}

// buildFromTopic fetches a topic, downloads its .torrent and turns the pair
// into a record. Returns nil when the topic carries no torrent.
func (p *Parser) buildFromTopic(ctx context.Context, sec section, it listingItem, nowRFC string) (filedb.TorrentDetails, error) {
	topicURL := fmt.Sprintf("%s/viewtopic.php?t=%s", p.host(), it.topicID)
	body, err := p.fetchPage(topicURL)
	if err != nil {
		return nil, err
	}
	info, ok := parseTopicHTML(body, p.loc)
	if !ok {
		return nil, nil
	}
	if err := p.delay(ctx); err != nil {
		return nil, err
	}
	data, status, err := p.Fetcher.Download(fmt.Sprintf("%s/download/file.php?id=%s", p.host(), info.torrentID), p.Config.Anibelka)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("torrent http %d", status)
	}
	magnet, err := core.TorrentBytesToMagnetErr(data)
	if err != nil {
		return nil, fmt.Errorf("bencode: %w", err)
	}
	return buildTorrent(p.host(), sec, it, info, magnet, nowRFC), nil
}

// parseTopicHTML reads the "Статистика раздачи" block and the torrent anchor.
func parseTopicHTML(body string, loc *time.Location) (topicInfo, bool) {
	var info topicInfo
	m := torrentLinkRe.FindStringSubmatch(body)
	if len(m) < 2 {
		return info, false
	}
	info.torrentID = m[1]
	if s := sizeRe.FindStringSubmatch(body); len(s) >= 3 {
		info.sizeName = strings.TrimSpace(s[1]) + " " + s[2]
	}
	if s := seedRe.FindStringSubmatch(body); len(s) >= 2 {
		info.sid, _ = strconv.Atoi(s[1])
	}
	if s := leechRe.FindStringSubmatch(body); len(s) >= 2 {
		info.pir, _ = strconv.Atoi(s[1])
	}
	if s := addedRe.FindStringSubmatch(body); len(s) >= 2 {
		info.createTime = parseRuDate(html.UnescapeString(s[1]), loc)
	}
	if info.createTime.IsZero() {
		info.createTime = time.Now().UTC()
	}
	return info, true
}

// parseRuDate reads the site's "23 июл 2026, 08:56" stamps.
func parseRuDate(s string, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	s = strings.TrimSpace(spacesRe.ReplaceAllString(strings.ReplaceAll(s, " ", " "), " "))
	m := regexp.MustCompile(`^(\d{1,2})\s+([А-Яа-яЁё]+)\s+(\d{4})(?:,\s*(\d{2}):(\d{2}))?`).FindStringSubmatch(s)
	if len(m) < 4 {
		return time.Time{}
	}
	day, _ := strconv.Atoi(m[1])
	year, _ := strconv.Atoi(m[3])
	key := strings.ToLower(m[2])
	if len([]rune(key)) > 3 {
		key = string([]rune(key)[:3])
	}
	mon, ok := ruMonths[key]
	if !ok {
		return time.Time{}
	}
	hour, minute := 0, 0
	if len(m) >= 6 && m[4] != "" {
		hour, _ = strconv.Atoi(m[4])
		minute, _ = strconv.Atoi(m[5])
	}
	return time.Date(year, mon, day, hour, minute, 0, 0, loc).UTC()
}

// parseTitle splits a listing title into (name, original, year).
//
// Titles are uniform: a category tag, then one to four slash-separated names,
// then bracketed metadata:
//
//	"[rus] Фермерская жизнь в ином мире / Isekai Nonbiri Nouka [2TV][2023-2026, …]"
//	"[rus] Туалетный мальчик Ханако / Ханако после школы / Jibaku Shounen Hanako-kun / … [TV][2020, …]"
//
// The Russian title can therefore be followed by a second Russian variant
// before the romaji one, so the original is taken as the first Latin-script
// part rather than simply the second.
func parseTitle(title string) (name, original string, year int) {
	if m := yearRe.FindStringSubmatch(title); len(m) >= 2 {
		year, _ = strconv.Atoi(m[1])
	}
	body := tagRe.ReplaceAllString(title, "")
	if i := strings.Index(body, "["); i >= 0 {
		body = body[:i]
	}
	parts := strings.Split(body, " / ")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	if len(parts) == 0 {
		return "", "", year
	}
	name = parts[0]
	for _, part := range parts[1:] {
		if latinRe.MatchString(part) && !cyrilRe.MatchString(part) {
			original = part
			break
		}
	}
	return strings.TrimSpace(name), strings.TrimSpace(original), year
}

// categoryTag returns the "[uni]"-style marker, used only for logging and the
// stored title; every section on this site is anime.
func categoryTag(title string) string {
	if m := tagRe.FindStringSubmatch(title); len(m) >= 2 {
		return m[1]
	}
	return ""
}

func buildTorrent(host string, sec section, it listingItem, info topicInfo, magnet, nowRFC string) filedb.TorrentDetails {
	name, original, year := parseTitle(it.title)
	if strings.TrimSpace(name) == "" || strings.TrimSpace(magnet) == "" {
		return nil
	}
	rec := filedb.TorrentRecord{
		TrackerName:  trackerName,
		Types:        []string{"anime"},
		URL:          fmt.Sprintf("%s/viewtopic.php?t=%s", host, it.topicID),
		Title:        it.title,
		Sid:          info.sid,
		Pir:          info.pir,
		SizeName:     info.sizeName,
		Magnet:       magnet,
		CreateTime:   info.createTime.Format(time.RFC3339Nano),
		UpdateTime:   nowRFC,
		Name:         name,
		OriginalName: original,
		Relased:      year,
		SearchName:   core.SearchName(name),
		SearchOrig:   core.SearchName(firstNonEmpty(original, name)),
	}
	return rec.ToMap()
}

// saveTorrents merges into the bucket cache and flushes touched buckets,
// mirroring the pattern used by the other parsers.
func (p *Parser) saveTorrents(torrents []filedb.TorrentDetails) (int, int, int, int, error) {
	added, updated, skipped, failed := 0, 0, 0, 0
	plog := core.NewParserLog(trackerName, filepath.Join(p.DB.DataDir, "log"), p.Config.LogParsers && p.Config.Anibelka.Log)
	bucketCache := make(map[string]map[string]filedb.TorrentDetails, len(torrents))
	changed := make(map[string]time.Time, len(torrents))
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

// UpdateTasksParse rebuilds the per-section page list from the highest
// ?start= link on the first listing page.
func (p *Parser) UpdateTasksParse(ctx context.Context) (map[string][]Task, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tasks == nil {
		p.tasks = map[string][]Task{}
	}
	for _, sec := range sections {
		body, err := p.fetchPage(p.forumURL(sec, 0))
		if err != nil {
			log.Printf("anibelka: updatetasks f=%s error: %v", sec.id, err)
			continue
		}
		maxPage := lastPageFromHTML(body)
		existing := p.tasks[sec.id]
		pages := map[int]Task{}
		for _, t := range existing {
			pages[t.Page] = t
		}
		for pg := 0; pg <= maxPage; pg++ {
			if _, ok := pages[pg]; !ok {
				pages[pg] = Task{Page: pg, UpdateTime: ""}
			}
		}
		merged := make([]Task, 0, len(pages))
		for _, t := range pages {
			merged = append(merged, t)
		}
		sort.Slice(merged, func(i, j int) bool { return merged[i].Page < merged[j].Page })
		p.tasks[sec.id] = merged
	}
	if err := p.saveTasksLocked(); err != nil {
		return nil, err
	}
	return cloneTasks(p.tasks), nil
}

// lastPageFromHTML converts the largest ?start= offset into a zero-based page
// index.
func lastPageFromHTML(body string) int {
	maxStart := 0
	for _, m := range pageStartRe.FindAllStringSubmatch(body, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > maxStart {
			maxStart = n
		}
	}
	return maxStart / topicsPerPage
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
		if _, err := p.UpdateTasksParse(ctx); err != nil {
			return "", err
		}
		p.mu.Lock()
		snapshot = cloneTasks(p.tasks)
		p.mu.Unlock()
	}

	processed, fetched, added, updated, skipped, failed, errs := 0, 0, 0, 0, 0, 0, 0
	for secID, list := range snapshot {
		sec := sectionByID(secID)
		if sec == nil {
			continue
		}
		for _, task := range list {
			if !force && task.UpdatedToday() {
				skipped++
				continue
			}
			if err := p.delay(ctx); err != nil {
				return "", err
			}
			torrents, _, err := p.parseSectionPage(ctx, *sec, task.Page)
			if err != nil {
				log.Printf("anibelka: parsealltask f=%s page=%d error: %v", sec.id, task.Page, err)
				errs++
				continue
			}
			processed++
			if len(torrents) == 0 {
				p.markPageToday(sec.id, task.Page)
				continue
			}
			a, u, s, f, err := p.saveTorrents(torrents)
			if err != nil {
				errs++
				continue
			}
			fetched += len(torrents)
			added += a
			updated += u
			skipped += s
			failed += f
			log.Printf("anibelka: parsealltask f=%s page=%d torrents=%d added=%d skipped=%d failed=%d",
				sec.id, task.Page, len(torrents), a, s, f)
			p.markPageToday(sec.id, task.Page)
		}
	}
	log.Printf("anibelka: parsealltask done processed=%d fetched=%d added=%d updated=%d skipped=%d failed=%d errors=%d",
		processed, fetched, added, updated, skipped, failed, errs)
	return "ok", nil
}

// ParseLatest walks the first N pages of every section — new topics land on
// page 0, so this is the cheap daily pass.
func (p *Parser) ParseLatest(ctx context.Context, pages int) (string, error) {
	if !p.latestMu.TryLock() {
		return "work", nil
	}
	defer p.latestMu.Unlock()
	if pages <= 0 {
		pages = 1
	}
	processed, fetched, added, updated, skipped, failed, errs := 0, 0, 0, 0, 0, 0, 0
	for _, sec := range sections {
		for page := 0; page < pages; page++ {
			if err := p.delay(ctx); err != nil {
				return "", err
			}
			torrents, _, err := p.parseSectionPage(ctx, sec, page)
			if err != nil {
				errs++
				continue
			}
			processed++
			if len(torrents) == 0 {
				continue
			}
			a, u, s, f, err := p.saveTorrents(torrents)
			if err != nil {
				errs++
				continue
			}
			fetched += len(torrents)
			added += a
			updated += u
			skipped += s
			failed += f
		}
	}
	log.Printf("anibelka: parselatest done processed=%d fetched=%d added=%d updated=%d skipped=%d failed=%d errors=%d",
		processed, fetched, added, updated, skipped, failed, errs)
	return "ok", nil
}

func (p *Parser) markPageToday(secID string, page int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if list, ok := p.tasks[secID]; ok {
		for i := range list {
			if list[i].Page == page {
				list[i].MarkToday()
			}
		}
		p.tasks[secID] = list
	}
	_ = p.saveTasksLocked()
}

func sectionByID(id string) *section {
	for i := range sections {
		if sections[i].id == id {
			return &sections[i]
		}
	}
	return nil
}

// ---- task persistence ----

func (p *Parser) tasksPath() string {
	return filepath.Join(p.DataDir, "temp", "anibelka_taskParse.json")
}

func (p *Parser) loadTasks() error {
	b, err := os.ReadFile(p.tasksPath())
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
	path := p.tasksPath()
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

// ---- shared utilities ----

func fileTime(t filedb.TorrentDetails) time.Time {
	if tm, ok := t["updateTime"].(time.Time); ok {
		return tm
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

func isDisabled(list []string, name string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), name) {
			return true
		}
	}
	return false
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
