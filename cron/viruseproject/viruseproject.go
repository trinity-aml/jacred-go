package viruseproject

import (
	"context"
	"fmt"
	"html"
	"log"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"jacred/app"
	"jacred/core"
	"jacred/filedb"
)

const trackerName = "viruseproject"

var catTypes = map[string][]string{
	"serials":      {"serial"},
	"movies":       {"movie"},
	"documentary":  {"docuserial", "documovie"},
	"cartoons":     {"multfilm", "multserial"},
	"reality-show": {"tvshow"},
}

// catPageStep — шаг ?start для каждой категории (подсмотрен в pagination-end на самом сайте).
var catPageStep = map[string]int{
	"serials":      10,
	"movies":       10,
	"documentary":  6,
	"cartoons":     6,
	"reality-show": 6,
}

var (
	itemHrefRe      = regexp.MustCompile(`(?is)<h3\s+class="catItemTitle">\s*<a\s+href="([^"]+)"`)
	paginationEndRe = regexp.MustCompile(`(?is)<li\s+class="pagination-end">\s*<a[^>]+href="[^"]*?[?&]start=(\d+)"`)
	itemTitleRe     = regexp.MustCompile(`(?is)<h2\s+class="itemTitle">\s*(.+?)\s*</h2>`)
	itemDateRe      = regexp.MustCompile(`(?is)<span\s+class="itemDateCreated">\s*(.+?)\s*</span>`)
	extraFieldRe    = regexp.MustCompile(`(?is)<span\s+class="itemExtraFieldsLabel">\s*([^<]+?)\s*</span>\s*<span\s+class="itemExtraFieldsValue">\s*([^<]+?)\s*</span>`)
	attachmentRe    = regexp.MustCompile(`(?is)<a\s+title="([^"]+?\.torrent)"\s+href="([^"]+/download/(\d+)_[a-f0-9]+)"\s*>\s*([^<]+?)\s*</a>`)
	yearInTextRe    = regexp.MustCompile(`\b(19|20)\d{2}\b`)
	resolutionRe    = regexp.MustCompile(`(?i)\b(2160|1440|1080|720|480|400)p\b`)
	sizeRe          = regexp.MustCompile(`(?i)размер\s+([0-9]+(?:[.,][0-9]+)?)\s*(Гб|Мб|Тб|Кб|GB|MB|TB|KB)`)
	parenEnRe       = regexp.MustCompile(`^([^()]+?)\s*\(([^()]+)\)\s*$`)
	seasonInfoRe    = regexp.MustCompile(`(?i)^сезон\s+\d+`)
	episodeInfoRe   = regexp.MustCompile(`(?i)^\d+(\s*[-,]\s*\d+)*\s+из\s+\d+$`)
	whitespaceRe    = regexp.MustCompile(`[\s\x{00A0}]+`)
	stripTagsRe     = regexp.MustCompile(`<[^>]+>`)
)

var russianMonths = []struct {
	prefix string
	m      time.Month
}{
	{"янв", time.January}, {"фев", time.February}, {"мар", time.March},
	{"апр", time.April}, {"май", time.May}, {"мая", time.May},
	{"июн", time.June}, {"июл", time.July}, {"авг", time.August},
	{"сен", time.September}, {"окт", time.October}, {"ноя", time.November},
	{"дек", time.December},
}

type Parser struct {
	Config  app.Config
	DB      *filedb.DB
	DataDir string
	Fetcher *core.Fetcher
	mu      sync.Mutex
	working bool
}

type ParseResult struct {
	Fetched, Added, Updated, Skipped, Failed int
	Status                                   string
}

func New(cfg app.Config, db *filedb.DB, dataDir string) *Parser {
	return &Parser{Config: cfg, DB: db, DataDir: dataDir, Fetcher: core.NewFetcher(cfg)}
}

func (p *Parser) Parse(ctx context.Context, limitPage int) (ParseResult, error) {
	p.mu.Lock()
	if p.working {
		p.mu.Unlock()
		return ParseResult{Status: "work"}, nil
	}
	p.working = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.working = false; p.mu.Unlock() }()

	host := strings.TrimRight(p.Config.Viruseproject.Host, "/")
	if host == "" {
		return ParseResult{Status: "config missing"}, nil
	}

	res := ParseResult{Status: "ok"}
	for cat, types := range catTypes {
		step := catPageStep[cat]
		if step <= 0 {
			step = 10
		}
		// Fetch page 1 first so we can both extract items and detect the
		// last page from the same response — saves a duplicate fetch.
		firstBody, err := p.httpGet(ctx, fmt.Sprintf("%s/releases/%s?start=0", host, cat))
		if err != nil || firstBody == "" {
			continue
		}
		lastPage := detectLastPageFromBody(firstBody, step)
		totalPages := limitPage
		if totalPages <= 0 || totalPages > lastPage {
			totalPages = lastPage
		}
		for page := 1; page <= totalPages; page++ {
			var body string
			if page == 1 {
				body = firstBody
			} else {
				if p.Config.Viruseproject.ParseDelay > 0 {
					select {
					case <-ctx.Done():
						return res, ctx.Err()
					case <-time.After(time.Duration(p.Config.Viruseproject.ParseDelay) * time.Millisecond):
					}
				}
				pageURL := fmt.Sprintf("%s/releases/%s?start=%d", host, cat, (page-1)*step)
				body, err = p.httpGet(ctx, pageURL)
				if err != nil || body == "" {
					continue
				}
			}
			items := p.parseListingBody(ctx, body, host, cat, types)
			res.Fetched += len(items)
			a, u, s, f, err := p.saveTorrents(ctx, items)
			if err != nil {
				res.Failed += len(items)
				continue
			}
			res.Added += a
			res.Updated += u
			res.Skipped += s
			res.Failed += f
			log.Printf("viruseproject: cat=%s page %d/%d fetched=%d added=%d skipped=%d failed=%d", cat, page, totalPages, len(items), a, s, f)
		}
	}
	log.Printf("viruseproject: done fetched=%d added=%d skipped=%d failed=%d", res.Fetched, res.Added, res.Skipped, res.Failed)
	return res, nil
}

// detectLastPageFromBody returns the highest page number visible in the
// pagination block (1 when there is no pagination, i.e. only one page).
func detectLastPageFromBody(body string, step int) int {
	if step <= 0 {
		return 1
	}
	if m := paginationEndRe.FindStringSubmatch(body); len(m) > 1 {
		if last, _ := strconv.Atoi(m[1]); last > 0 {
			return last/step + 1
		}
	}
	return 1
}

func (p *Parser) parseListingBody(ctx context.Context, body, host, cat string, types []string) []filedb.TorrentDetails {
	postURLs := extractItemURLs(body, host)
	if len(postURLs) == 0 {
		return nil
	}

	var out []filedb.TorrentDetails
	for _, postURL := range postURLs {
		dhtml, err := p.httpGet(ctx, postURL)
		if err != nil || dhtml == "" {
			continue
		}
		records := p.buildRecords(ctx, postURL, dhtml, host, cat, types)
		out = append(out, records...)
	}
	return out
}

func (p *Parser) buildRecords(ctx context.Context, postURL, dhtml, host string, cat string, types []string) []filedb.TorrentDetails {
	rawTitle := cleanText(extractMatch(itemTitleRe, dhtml))
	if rawTitle == "" {
		return nil
	}

	fields := extractExtraFields(dhtml)
	yearStr := strings.TrimSpace(fields["Год выпуска"])
	year := 0
	if yearStr != "" {
		if n, err := strconv.Atoi(yearStr); err == nil {
			year = n
		}
	}
	videoQuality := strings.TrimSpace(fields["Качество видео"])

	createTime := parseRussianDate(cleanText(extractMatch(itemDateRe, dhtml)))
	createTimeStr := createTime.UTC().Format(time.RFC3339Nano)

	nameRu, nameEn := parseNames(rawTitle)
	titleHasYear := yearInTextRe.MatchString(rawTitle)

	baseTitle := rawTitle
	if !titleHasYear && year > 0 {
		baseTitle = fmt.Sprintf("%s (%d)", baseTitle, year)
	}
	if videoQuality != "" {
		baseTitle = fmt.Sprintf("%s [%s]", baseTitle, videoQuality)
	}

	attachments := attachmentRe.FindAllStringSubmatch(dhtml, -1)
	if len(attachments) == 0 {
		return nil
	}

	var out []filedb.TorrentDetails
	for _, att := range attachments {
		fileTitle := strings.TrimSpace(att[1])
		downloadURL := html.UnescapeString(strings.TrimSpace(att[2]))
		if strings.HasPrefix(downloadURL, "/") {
			downloadURL = host + downloadURL
		}
		downloadID := strings.TrimSpace(att[3])
		linkText := cleanText(att[4])

		resolution := "400p"
		resInt := 400
		if m := resolutionRe.FindStringSubmatch(fileTitle); len(m) > 1 {
			resInt, _ = strconv.Atoi(m[1])
			resolution = m[1] + "p"
		}

		sizeName := ""
		var sizeBytes int64
		if m := sizeRe.FindStringSubmatch(linkText); len(m) > 2 {
			sizeName = strings.ReplaceAll(m[1], ",", ".") + " " + m[2]
			num, _ := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", "."), 64)
			switch strings.ToLower(m[2]) {
			case "тб", "tb":
				num *= 1024 * 1024
			case "гб", "gb":
				num *= 1024
			case "кб", "kb":
				num /= 1024
			}
			sizeBytes = int64(num * 1048576)
		}

		title := fmt.Sprintf("%s [%s]", baseTitle, resolution)

		recordURL := fmt.Sprintf("%s#q=%d&id=%s", postURL, resInt, downloadID)

		out = append(out, filedb.TorrentRecord{
			TrackerName:  trackerName,
			Types:        types,
			URL:          recordURL,
			Title:        title,
			Sid:          1, // site doesn't expose peer counts
			SizeName:     sizeName,
			Size:         sizeBytes,
			CreateTime:   createTimeStr,
			UpdateTime:   createTimeStr,
			Name:         nameRu,
			OriginalName: core.FirstNonEmpty(nameEn, nameRu),
			Relased:      year,
			Quality:      resInt,
			VideoType:    videoQuality,
			SearchName:   core.SearchName(nameRu),
			SearchOrig:   core.SearchName(core.FirstNonEmpty(nameEn, nameRu)),
			TID:          downloadID,
			DownloadURI:  downloadURL,
		}.ToMap())
	}
	return out
}

func (p *Parser) saveTorrents(ctx context.Context, torrents []filedb.TorrentDetails) (int, int, int, int, error) {
	added, updated, skipped, failed := 0, 0, 0, 0
	plog := core.NewParserLog(trackerName, filepath.Join(p.DB.DataDir, "log"), p.Config.LogParsers && p.Config.Viruseproject.Log)
	bucketCache := make(map[string]map[string]filedb.TorrentDetails, len(torrents))
	changed := make(map[string]time.Time, len(torrents))

	for _, incoming := range torrents {
		key := p.DB.KeyDb(asString(incoming["name"]), asString(incoming["originalname"]))
		if key == ":" || strings.TrimSpace(key) == "" {
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
		if urlv == "" {
			skipped++
			continue
		}
		existing, exists := bucket[urlv]
		needMagnet := !exists || strings.TrimSpace(asString(existing["magnet"])) == "" || asString(existing["title"]) != asString(incoming["title"])
		if needMagnet {
			downloadURL := asString(incoming["_downloadURI"])
			if downloadURL != "" {
				data, err := p.download(ctx, downloadURL, urlv)
				if err == nil && len(data) > 0 {
					if magnet := core.TorrentBytesToMagnet(data); magnet != "" {
						incoming["magnet"] = magnet
					}
				}
			}
		} else if mg := asString(existing["magnet"]); mg != "" {
			incoming["magnet"] = mg
		}
		delete(incoming, "_downloadURI")

		if strings.TrimSpace(asString(incoming["magnet"])) == "" && !exists {
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
			return added, updated, skipped, failed, err
		}
	}
	return added, updated, skipped, failed, nil
}

// --- Helpers ---

func extractItemURLs(body, host string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, m := range itemHrefRe.FindAllStringSubmatch(body, -1) {
		if len(m) < 2 {
			continue
		}
		u := html.UnescapeString(strings.TrimSpace(m[1]))
		if strings.HasPrefix(u, "/") {
			u = host + u
		}
		if _, ok := seen[u]; !ok {
			seen[u] = struct{}{}
			out = append(out, u)
		}
	}
	return out
}

func extractMatch(re *regexp.Regexp, body string) string {
	m := re.FindStringSubmatch(body)
	if len(m) > 1 {
		return strings.TrimSpace(html.UnescapeString(m[1]))
	}
	return ""
}

func extractExtraFields(body string) map[string]string {
	out := map[string]string{}
	for _, m := range extraFieldRe.FindAllStringSubmatch(body, -1) {
		if len(m) < 3 {
			continue
		}
		label := cleanText(m[1])
		label = strings.TrimSuffix(label, ":")
		label = strings.TrimSpace(label)
		value := cleanText(m[2])
		out[label] = value
	}
	return out
}

func cleanText(s string) string {
	s = stripTagsRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = whitespaceRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func parseRussianDate(s string) time.Time {
	if s == "" {
		return time.Now().UTC()
	}
	// Format: "Четверг, 13 Февраль 2025 00:00"
	if idx := strings.Index(s, ","); idx >= 0 {
		s = s[idx+1:]
	}
	s = strings.TrimSpace(s)
	parts := strings.Fields(s)
	if len(parts) < 3 {
		return time.Now().UTC()
	}
	day, err := strconv.Atoi(parts[0])
	if err != nil {
		return time.Now().UTC()
	}
	month := monthFromRussian(parts[1])
	if month == 0 {
		return time.Now().UTC()
	}
	year, err := strconv.Atoi(parts[2])
	if err != nil {
		return time.Now().UTC()
	}
	hour, minute := 0, 0
	if len(parts) >= 4 {
		hm := strings.SplitN(parts[3], ":", 2)
		if len(hm) == 2 {
			hour, _ = strconv.Atoi(hm[0])
			minute, _ = strconv.Atoi(hm[1])
		}
	}
	return time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
}

func monthFromRussian(s string) time.Month {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, rm := range russianMonths {
		if strings.HasPrefix(s, rm.prefix) {
			return rm.m
		}
	}
	return 0
}

func parseNames(rawTitle string) (string, string) {
	rawTitle = strings.TrimSpace(rawTitle)
	if rawTitle == "" {
		return "", ""
	}
	// Try parens form: "Russian (English)"
	if m := parenEnRe.FindStringSubmatch(rawTitle); len(m) == 3 {
		ru := strings.TrimSpace(m[1])
		en := strings.TrimSpace(m[2])
		if hasCyrillic(ru) && hasLatin(en) && !strings.Contains(en, "/") {
			return ru, en
		}
	}
	// Slash-separated form
	if !strings.Contains(rawTitle, "/") {
		return rawTitle, rawTitle
	}
	parts := strings.Split(rawTitle, "/")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	clean := make([]string, 0, len(parts))
	for _, pt := range parts {
		if pt == "" || isYearOnly(pt) || seasonInfoRe.MatchString(pt) || episodeInfoRe.MatchString(pt) {
			continue
		}
		clean = append(clean, pt)
	}
	if len(clean) == 0 {
		return rawTitle, rawTitle
	}
	ru := clean[0]
	en := ru
	for _, pt := range clean[1:] {
		if hasLatin(pt) && !hasCyrillic(pt) {
			en = pt
			break
		}
	}
	return ru, en
}

func isYearOnly(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != 4 {
		return false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return false
	}
	return n >= 1900 && n <= 2100
}

func hasCyrillic(s string) bool {
	for _, r := range s {
		if (r >= 'А' && r <= 'я') || r == 'Ё' || r == 'ё' {
			return true
		}
	}
	return false
}

func hasLatin(s string) bool {
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			return true
		}
	}
	return false
}

func (p *Parser) httpGet(ctx context.Context, rawURL string) (string, error) {
	body, _, err := p.Fetcher.GetString(rawURL, p.Config.Viruseproject)
	return body, err
}

func (p *Parser) download(ctx context.Context, urlv, referer string) ([]byte, error) {
	data, status, err := p.Fetcher.Download(urlv, p.Config.Viruseproject)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, nil
	}
	return data, nil
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

func fileTime(t filedb.TorrentDetails) time.Time {
	for _, key := range []string{"updateTime", "createTime"} {
		s := strings.TrimSpace(asString(t[key]))
		if s == "" {
			continue
		}
		if tm, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return tm.UTC()
		}
		if tm, err := time.Parse(time.RFC3339, s); err == nil {
			return tm.UTC()
		}
	}
	return time.Now().UTC()
}
