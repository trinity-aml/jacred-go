package ultradox

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

const trackerName = "ultradox"

// section maps a URL path segment to torrent types stored in records. The
// site itself sometimes redirects /serial/ → /serial-hd/; we hit the final
// path directly.
type section struct {
	path  string
	types []string
}

var sections = []section{
	{path: "serial-hd", types: []string{"serial"}},
	{path: "hd", types: []string{"movie"}},
	{path: "rufilm", types: []string{"movie"}},
	{path: "camrip", types: []string{"movie"}},
	{path: "webrips", types: []string{"movie"}},
	{path: "anime", types: []string{"anime"}},
}

// Listing-row regexes. Each row in /<section>/[/page/N/] is a <tr> block
// with classes torrent-table-date / torrent-table-href / torrent-table-magnet
// etc. The listing-page magnet has an empty btih (likely a JS-templated
// redirect), so we ignore it and follow the title link to the detail page
// where real magnets with full info-hashes live.
var (
	rowSplitRe       = regexp.MustCompile(`<tr>\s*<td class="torrent-table-date">`)
	// Match either an absolute "DD-MM-YYYY, HH:MM" stamp or the relative
	// "Сегодня, HH:MM" / "Вчера, HH:MM" the site uses for recent rows.
	rowDateRe        = regexp.MustCompile(`^([^<]+)</td>`)
	rowTimeRe        = regexp.MustCompile(`([0-9]{2}):([0-9]{2})`)
	rowDetailLinkRe  = regexp.MustCompile(`(?is)<td class="torrent-table-href">\s*<a[^>]+href="([^"#]+)"[^>]*>([\s\S]*?)</a>`)
	rowImdbRe        = regexp.MustCompile(`(?is)<span\s+data-clipboard-text="https://www\.imdb\.com/title/(tt[0-9]+)/?"`)
	rowSpanQualityRe = regexp.MustCompile(`(?is)<span[^>]*style="font-weight:\s*bold;?"[^>]*>([\s\S]*?)</span>`)
	rowTagsRe        = regexp.MustCompile(`(?is)<[^>]+>`)

	// Detail-page magnet anchors. Each detail page lists 1–3 quality
	// variants — we extract the hash, byte length (xl=) and torrent
	// filename (dn=) from each one to build per-variant records.
	detailMagnetRe = regexp.MustCompile(`(?i)magnet:\?xt=urn:btih:([A-Fa-f0-9]+)&xl=([0-9]+)&dn=([^&"<\s]+)`)

	pageNumRe = regexp.MustCompile(`/page/([0-9]+)/`)

	titleYearRe = regexp.MustCompile(`\(([0-9]{4})\)`)
	// Strip trailing quality / studio markers so name/originalname are
	// clean for the search-name index. Anything in [...] or (XXX) blocks
	// past the (YYYY) year falls into this bucket.
	titleNameRe = regexp.MustCompile(`^([^(\[]+)`)
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

func (p *Parser) UpdateConfig(cfg app.Config) {
	p.Config = cfg
}

// listingItem is the result of parsing one row on a /<section>/ page. We
// follow detailURL afterwards to extract real magnets.
type listingItem struct {
	createTime time.Time
	detailURL  string
	title      string
	imdb       string
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

	if strings.TrimSpace(p.Config.Ultradox.Host) == "" {
		return ParseResult{Status: "config missing"}, nil
	}

	res := ParseResult{Status: "ok", PerCategory: map[string]int{}}
	for idx, sec := range sections {
		if idx > 0 {
			if err := p.delay(ctx); err != nil {
				return res, err
			}
		}
		items, err := p.fetchSectionPage(ctx, sec, page)
		if err != nil {
			log.Printf("ultradox: section %s page %d error: %v (continuing)", sec.path, page, err)
			continue
		}
		torrents, err := p.expandToTorrents(ctx, sec, items)
		if err != nil {
			return res, err
		}
		res.Fetched += len(torrents)
		res.PerCategory[sec.path] = len(torrents)
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
		log.Printf("ultradox: section=%s page=%d listing=%d torrents=%d added=%d skipped=%d failed=%d", sec.path, page, len(items), len(torrents), a, s, f)
	}
	log.Printf("ultradox: done fetched=%d added=%d skipped=%d failed=%d", res.Fetched, res.Added, res.Skipped, res.Failed)
	return res, nil
}

// expandToTorrents fetches each item's detail page and produces one
// TorrentDetails per magnet variant found.
func (p *Parser) expandToTorrents(ctx context.Context, sec section, items []listingItem) ([]filedb.TorrentDetails, error) {
	out := make([]filedb.TorrentDetails, 0, len(items)*2)
	host := strings.TrimRight(p.Config.Ultradox.Host, "/")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, it := range items {
		if err := p.delay(ctx); err != nil {
			return out, err
		}
		variants, err := p.fetchDetail(ctx, it.detailURL)
		if err != nil {
			log.Printf("ultradox: detail %s error: %v (skipping)", it.detailURL, err)
			continue
		}
		for _, v := range variants {
			t := buildTorrent(host, sec, it, v, now)
			if t != nil {
				out = append(out, t)
			}
		}
	}
	return out, nil
}

func (p *Parser) delay(ctx context.Context) error {
	d := time.Duration(p.Config.Ultradox.ParseDelay) * time.Millisecond
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

// fetchSectionPage downloads a listing page and parses out the rows.
func (p *Parser) fetchSectionPage(ctx context.Context, sec section, page int) ([]listingItem, error) {
	host := strings.TrimRight(p.Config.Ultradox.Host, "/")
	var u string
	if page <= 0 {
		u = fmt.Sprintf("%s/%s/", host, sec.path)
	} else {
		u = fmt.Sprintf("%s/%s/page/%d/", host, sec.path, page)
	}
	body, _, err := p.Fetcher.GetString(u, p.Config.Ultradox)
	if err != nil {
		return nil, err
	}
	return parseListingHTML(body, p.loc), nil
}

func parseListingHTML(body string, loc *time.Location) []listingItem {
	chunks := rowSplitRe.Split(body, -1)
	out := make([]listingItem, 0, len(chunks))
	for _, row := range chunks[1:] {
		row = strings.TrimSpace(row)
		if row == "" {
			continue
		}
		// rowSplitRe consumed the leading "<tr><td class=torrent-table-date>"
		// so the date is at the very start, ending at "</td>".
		dateMatch := rowDateRe.FindStringSubmatch(row)
		if len(dateMatch) < 2 {
			continue
		}
		createTime := parseRowDate(strings.TrimSpace(dateMatch[1]), loc)
		if createTime.IsZero() {
			continue
		}
		linkMatch := rowDetailLinkRe.FindStringSubmatch(row)
		if len(linkMatch) < 3 {
			continue
		}
		detailURL := strings.TrimSpace(html.UnescapeString(linkMatch[1]))
		if detailURL == "" {
			continue
		}
		title := flattenTitle(linkMatch[2])
		if title == "" {
			continue
		}
		imdb := ""
		if m := rowImdbRe.FindStringSubmatch(row); len(m) >= 2 {
			imdb = m[1]
		}
		out = append(out, listingItem{
			createTime: createTime.UTC(),
			detailURL:  detailURL,
			title:      title,
			imdb:       imdb,
		})
	}
	return out
}

// parseRowDate accepts the listing's three date shapes:
//
//	"02-04-2025, 14:32"     — absolute, the common case
//	"Сегодня, 13:31"        — relative "today" the site emits for fresh items
//	"Вчера, 22:05"          — yesterday
//
// Anything else returns the zero time so the caller skips the row.
func parseRowDate(s string, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	if t, err := time.ParseInLocation("02-01-2006, 15:04", s, loc); err == nil && !t.IsZero() {
		return t.UTC()
	}
	relative := -1
	if strings.HasPrefix(s, "Сегодня") {
		relative = 0
	} else if strings.HasPrefix(s, "Вчера") {
		relative = -1
	} else {
		return time.Time{}
	}
	hm := rowTimeRe.FindStringSubmatch(s)
	if len(hm) < 3 {
		return time.Time{}
	}
	hour, _ := strconv.Atoi(hm[1])
	minute, _ := strconv.Atoi(hm[2])
	now := time.Now().In(loc)
	day := now
	if relative != 0 {
		day = now.AddDate(0, 0, relative)
	}
	return time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, loc).UTC()
}

// flattenTitle merges the anchor's text and the bold quality span into a
// single space-collapsed string. The site's markup splits the quality info
// out into a <span>, but for our purposes — and the existing search index
// — we want them concatenated.
func flattenTitle(raw string) string {
	// Pull the bold span aside, strip remaining tags, then re-join with a
	// space. This keeps "(ПМ) [BDRip]" attached after the visible title.
	span := ""
	if m := rowSpanQualityRe.FindStringSubmatch(raw); len(m) >= 2 {
		span = m[1]
	}
	mainText := rowTagsRe.ReplaceAllString(raw, " ")
	mainText = html.UnescapeString(mainText)
	if span != "" {
		mainText = strings.ReplaceAll(mainText, html.UnescapeString(rowTagsRe.ReplaceAllString(span, "")), "")
		mainText = strings.TrimSpace(mainText) + " " + html.UnescapeString(rowTagsRe.ReplaceAllString(span, ""))
	}
	return core.StripTagsAndCollapseSpaces(mainText)
}

// magnetVariant describes one quality available on a detail page.
type magnetVariant struct {
	hash    string
	bytes   int64
	dn      string
	magnet  string
	quality string
}

// fetchDetail downloads a detail page (path is site-relative, e.g.
// "/nerufilm/123-foo.html") and extracts every magnet variant.
func (p *Parser) fetchDetail(ctx context.Context, path string) ([]magnetVariant, error) {
	host := strings.TrimRight(p.Config.Ultradox.Host, "/")
	if !strings.HasPrefix(path, "http") {
		path = host + "/" + strings.TrimLeft(path, "/")
	}
	body, _, err := p.Fetcher.GetString(path, p.Config.Ultradox)
	if err != nil {
		return nil, err
	}
	matches := detailMagnetRe.FindAllStringSubmatchIndex(body, -1)
	if len(matches) == 0 {
		return nil, nil
	}
	out := make([]magnetVariant, 0, len(matches))
	seen := map[string]struct{}{}
	for _, m := range matches {
		full := body[m[0]:m[1]]
		hash := strings.ToLower(body[m[2]:m[3]])
		if _, dup := seen[hash]; dup {
			continue
		}
		seen[hash] = struct{}{}
		bytesN, _ := strconv.ParseInt(body[m[4]:m[5]], 10, 64)
		dn := body[m[6]:m[7]]
		// Re-extract the full magnet up to the closing quote so trackers
		// (`tr=`) ride along with the URI even though our regex stopped
		// at the dn= terminator.
		magnet := extractFullMagnet(body, m[0])
		if magnet == "" {
			magnet = full
		}
		out = append(out, magnetVariant{
			hash:    hash,
			bytes:   bytesN,
			dn:      dn,
			magnet:  magnet,
			quality: extractQuality(dn),
		})
	}
	return out, nil
}

// extractFullMagnet locates the closing `"` of the href starting at start
// in body and returns the unescaped magnet URI between them.
func extractFullMagnet(body string, start int) string {
	end := strings.IndexAny(body[start:], `"<`)
	if end < 0 {
		return ""
	}
	return html.UnescapeString(body[start : start+end])
}

// extractQuality pulls a resolution token out of a torrent filename. The
// site occasionally obfuscates digits with capital O ("1O8Op" for "1080p");
// normalize before matching.
func extractQuality(dn string) string {
	clean := strings.ReplaceAll(dn, "O", "0")
	if m := regexp.MustCompile(`([0-9]{3,4})[pP]`).FindStringSubmatch(clean); len(m) >= 2 {
		return m[1] + "p"
	}
	for _, tag := range []string{"BDRip", "DVDRip", "HDRip", "WEBRip", "WEB-DL", "CAMRip", "CamRip", "TS"} {
		if strings.Contains(dn, tag) {
			return tag
		}
	}
	return ""
}

func buildTorrent(host string, sec section, item listingItem, v magnetVariant, nowRFC string) filedb.TorrentDetails {
	if v.hash == "" || v.magnet == "" {
		return nil
	}

	// Compose a per-variant title so existing UpdateFullDetails detects
	// quality (1080p / BDRip / etc.) for filtering.
	title := strings.TrimSpace(item.title)
	if v.quality != "" && !strings.Contains(strings.ToLower(title), strings.ToLower(v.quality)) {
		title = title + " [" + v.quality + "]"
	}

	name, originalName, year := parseTitle(title)
	if strings.TrimSpace(name) == "" {
		// Fallback: take everything up to the first paren/bracket.
		if m := titleNameRe.FindStringSubmatch(title); len(m) >= 2 {
			name = strings.TrimSpace(m[1])
		}
	}
	if strings.TrimSpace(name) == "" {
		return nil
	}

	// detailURL is path-only on the listing; canonicalize against host so
	// the saved record is portable. Append a hash anchor so per-variant
	// records get unique URLs (the bucket key is by name+orig, but
	// per-row dedup is by url).
	detailURL := item.detailURL
	if !strings.HasPrefix(detailURL, "http") {
		detailURL = host + "/" + strings.TrimLeft(detailURL, "/")
	}
	uniqueURL := detailURL + "#h=" + v.hash[:min(len(v.hash), 8)]

	sizeName := humanSize(v.bytes)

	rec := filedb.TorrentRecord{
		TrackerName:  trackerName,
		Types:        sec.types,
		URL:          uniqueURL,
		Title:        title,
		Sid:          1,
		Pir:          1,
		SizeName:     sizeName,
		Magnet:       v.magnet,
		CreateTime:   item.createTime.Format(time.RFC3339Nano),
		UpdateTime:   nowRFC,
		Name:         name,
		OriginalName: originalName,
		Relased:      year,
		SearchName:   core.SearchName(name),
		SearchOrig:   core.SearchName(firstNonEmpty(originalName, name)),
	}
	return rec.ToMap()
}

// parseTitle splits the listing title into (name, original, year). Most
// listings carry a "(YYYY)" block; if there's a slashed original name
// before it, capture that too.
func parseTitle(title string) (name, original string, year int) {
	if m := titleYearRe.FindStringSubmatch(title); len(m) >= 2 {
		year, _ = strconv.Atoi(m[1])
	}
	cut := title
	if y := titleYearRe.FindStringIndex(cut); y != nil {
		cut = strings.TrimSpace(cut[:y[0]])
	}
	if idx := strings.Index(cut, " / "); idx >= 0 {
		name = strings.TrimSpace(cut[:idx])
		original = strings.TrimSpace(cut[idx+3:])
		return
	}
	name = strings.TrimSpace(cut)
	return
}

func humanSize(bytes int64) string {
	if bytes <= 0 {
		return ""
	}
	const (
		kb = 1 << 10
		mb = 1 << 20
		gb = 1 << 30
		tb = 1 << 40
	)
	switch {
	case bytes >= tb:
		return fmt.Sprintf("%.2f TB", float64(bytes)/float64(tb))
	case bytes >= gb:
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(gb))
	case bytes >= mb:
		return fmt.Sprintf("%.2f MB", float64(bytes)/float64(mb))
	case bytes >= kb:
		return fmt.Sprintf("%.2f KB", float64(bytes)/float64(kb))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// saveTorrents merges incoming torrents into bucket cache and flushes
// touched buckets at the end. Mirrors the established parser pattern.
func (p *Parser) saveTorrents(torrents []filedb.TorrentDetails) (int, int, int, int, error) {
	added, updated, skipped, failed := 0, 0, 0, 0
	plog := core.NewParserLog(trackerName, filepath.Join(p.DB.DataDir, "log"), p.Config.LogParsers && p.Config.Ultradox.Log)
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

// UpdateTasksParse rebuilds the per-section page list from the current
// pagination link with the highest /page/N/. Storing tasks lets
// ParseAllTask iterate every page exactly once and ParseLatest resume from
// the most recent few pages.
func (p *Parser) UpdateTasksParse(ctx context.Context) (map[string][]Task, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tasks == nil {
		p.tasks = map[string][]Task{}
	}
	for _, sec := range sections {
		body, _, err := p.Fetcher.GetString(strings.TrimRight(p.Config.Ultradox.Host, "/")+"/"+sec.path+"/", p.Config.Ultradox)
		if err != nil {
			continue
		}
		maxPage := 1
		for _, m := range pageNumRe.FindAllStringSubmatch(body, -1) {
			if n, err := strconv.Atoi(m[1]); err == nil && n > maxPage {
				maxPage = n
			}
		}
		existing := p.tasks[sec.path]
		pages := map[int]Task{}
		for _, t := range existing {
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
		p.tasks[sec.path] = merged
	}
	if err := p.saveTasksLocked(); err != nil {
		return nil, err
	}
	return cloneTasks(p.tasks), nil
}

// ParseAllTask walks every page in every section, marking each as done so
// re-runs within the same day skip already-processed pages.
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
	for secPath, list := range snapshot {
		sec := sectionByPath(secPath)
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
			items, err := p.fetchSectionPage(ctx, *sec, task.Page)
			if err != nil {
				log.Printf("ultradox: parsealltask %s page=%d error: %v", sec.path, task.Page, err)
				errs++
				continue
			}
			processed++
			torrents, err := p.expandToTorrents(ctx, *sec, items)
			if err != nil {
				return "", err
			}
			if len(torrents) == 0 {
				p.markPageToday(sec.path, task.Page)
				continue
			}
			a, u, s, f, err := p.saveTorrents(torrents)
			if err != nil {
				log.Printf("ultradox: parsealltask %s page=%d save error: %v", sec.path, task.Page, err)
				errs++
				continue
			}
			fetched += len(torrents)
			added += a
			updated += u
			skipped += s
			failed += f
			log.Printf("ultradox: parsealltask %s page=%d torrents=%d added=%d skipped=%d failed=%d", sec.path, task.Page, len(torrents), a, s, f)
			p.markPageToday(sec.path, task.Page)
		}
	}
	log.Printf("ultradox: parsealltask done processed=%d fetched=%d added=%d updated=%d skipped=%d failed=%d errors=%d", processed, fetched, added, updated, skipped, failed, errs)
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
	processed, fetched, added, updated, skipped, failed, errs := 0, 0, 0, 0, 0, 0, 0
	var lines []string
	for secPath, list := range snapshot {
		sec := sectionByPath(secPath)
		if sec == nil {
			continue
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Page < list[j].Page })
		if len(list) > pages {
			list = list[:pages]
		}
		for _, task := range list {
			if err := p.delay(ctx); err != nil {
				return "", err
			}
			items, err := p.fetchSectionPage(ctx, *sec, task.Page)
			if err != nil {
				errs++
				continue
			}
			processed++
			torrents, err := p.expandToTorrents(ctx, *sec, items)
			if err != nil {
				return "", err
			}
			if len(torrents) == 0 {
				p.markPageToday(sec.path, task.Page)
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
			lines = append(lines, fmt.Sprintf("%s - %d", sec.path, task.Page))
			p.markPageToday(sec.path, task.Page)
		}
	}
	log.Printf("ultradox: parselatest done processed=%d fetched=%d added=%d updated=%d skipped=%d failed=%d errors=%d", processed, fetched, added, updated, skipped, failed, errs)
	if len(lines) == 0 {
		return "ok", nil
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func (p *Parser) markPageToday(secPath string, page int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if list, ok := p.tasks[secPath]; ok {
		for i := range list {
			if list[i].Page == page {
				list[i].MarkToday()
			}
		}
		p.tasks[secPath] = list
	}
	_ = p.saveTasksLocked()
}

func sectionByPath(path string) *section {
	for i := range sections {
		if sections[i].path == path {
			return &sections[i]
		}
	}
	return nil
}

// ---- task persistence ----

func (p *Parser) loadTasks() error {
	path := filepath.Join(p.DataDir, "temp", "ultradox_taskParse.json")
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
	path := filepath.Join(p.DataDir, "temp", "ultradox_taskParse.json")
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

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
