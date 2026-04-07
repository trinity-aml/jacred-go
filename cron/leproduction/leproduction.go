package leproduction

import (
	"context"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"path/filepath"

	"jacred/app"
	"jacred/core"
	"jacred/filedb"
)

const trackerName = "leproduction"

var catTypes = map[string][]string{
	"anime":      {"anime"},
	"dorama":     {"serial"},
	"film":       {"movie"},
	"serial":     {"serial"},
	"fulcartoon": {"multfilm"},
	"cartoon":    {"multserial"},
}

var (
	shortImgRe    = regexp.MustCompile(`(?i)<a\s+class="short-img"\s+href="(?P<url>(?:https?://[^"]+)?/[^"]+?\.html)"`)
	h3LinkRe      = regexp.MustCompile(`(?i)<h3>\s*<a\s+href="(?P<url>(?:https?://[^"]+)?/[^"]+?\.html)"`)
	pageNumRe     = regexp.MustCompile(`(?i)/page/([0-9]+)/`)
	nameRuRe      = regexp.MustCompile(`(?is)Русское\s+название:\s*</div>\s*<div[^>]*class="info-desc"[^>]*>\s*([^<]+)\s*</div>`)
	nameEnRe      = regexp.MustCompile(`(?is)Оригинальное\s+название:\s*</div>\s*<div[^>]*class="info-desc"[^>]*>\s*([^<]+)\s*</div>`)
	h1Re          = regexp.MustCompile(`(?i)<h1>([^<]+)</h1>`)
	yearRe        = regexp.MustCompile(`(?i)info-label">Год выпуска:</div>\s*<div[^>]*class="info-desc"[^>]*>\s*<a[^>]*>(\d{4})</a>`)
	downloadIDRe  = regexp.MustCompile(`(?i)index\.php\?do=download&(?:amp;)?id=(\d+)`)
	torrentInfoRe = regexp.MustCompile(`(?i)id\s*=\s*"torrent_(\d+)_info"`)
	magnetHrefRe  = regexp.MustCompile(`(?i)href\s*=\s*"(magnet:[^"]+)"`)
	magnetRawRe   = regexp.MustCompile(`(?i)(magnet:[^\s"'<]+)`)
	fileNameRe    = regexp.MustCompile(`(?is)class="info_d1-le"[^>]*>\s*([^<]+)\s*</div>`)
	sidLeRe       = regexp.MustCompile(`(?i)Раздают:\s*</b>\s*<span[^>]*class="li_distribute_m-le"[^>]*>\s*([0-9]+)\s*</span>`)
	pirLeRe       = regexp.MustCompile(`(?i)Качают:\s*</b>\s*<span[^>]*class="li_swing_m-le"[^>]*>\s*([0-9]+)\s*</span>`)
	sizeLeRe      = regexp.MustCompile(`(?i)Размер:\s*<span[^>]*>\s*([0-9]+(?:[.,][0-9]+)?)\s*G[bB]\s*</span>`)
	qualityRe     = regexp.MustCompile(`(?i)\b([0-9]{3,4}p)\b`)
	cleanSpaceRe  = regexp.MustCompile(`\s+`)

	inlineRe89769cRe = regexp.MustCompile(`\s*/\s*.*$`)
)

type Parser struct {
	Config  app.Config
	DB      *filedb.DB
	DataDir string
	Client  *http.Client
	mu      sync.Mutex
	working bool
}

type ParseResult struct {
	Fetched, Added, Updated, Skipped, Failed int
	Status                                   string
}

func New(cfg app.Config, db *filedb.DB, dataDir string) *Parser {
	return &Parser{Config: cfg, DB: db, DataDir: dataDir, Client: &http.Client{Timeout: 30 * time.Second}}
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

	host := strings.TrimRight(p.Config.Leproduction.Host, "/")
	if host == "" {
		return ParseResult{Status: "config missing"}, nil
	}

	res := ParseResult{Status: "ok"}
	for cat, types := range catTypes {
		totalPages := limitPage
		if totalPages <= 0 {
			totalPages = p.detectLastPage(ctx, host, cat)
		}
		for page := 1; page <= totalPages; page++ {
			pageURL := fmt.Sprintf("%s/%s/", host, cat)
			if page > 1 {
				pageURL = fmt.Sprintf("%s/%s/page/%d/", host, cat, page)
			}
			items, err := p.parsePage(ctx, pageURL, host, cat, types)
			if err != nil {
				continue
			}
			res.Fetched += len(items)
			a, u, s, f, err := p.saveTorrents(ctx, items, host)
			if err != nil {
				res.Failed += len(items)
				continue
			}
			res.Added += a
			res.Updated += u
			res.Skipped += s
			res.Failed += f
		}
	}
	log.Printf("leproduction: done fetched=%d added=%d skipped=%d failed=%d", res.Fetched, res.Added, res.Skipped, res.Failed)
	return res, nil
}

func (p *Parser) detectLastPage(ctx context.Context, host, cat string) int {
	body, err := p.httpGet(ctx, fmt.Sprintf("%s/%s/", host, cat), "")
	if err != nil || body == "" {
		return 1
	}
	maxPage := 1
	for _, m := range pageNumRe.FindAllStringSubmatch(body, -1) {
		if n, _ := strconv.Atoi(m[1]); n > maxPage {
			maxPage = n
		}
	}
	return maxPage
}

func (p *Parser) parsePage(ctx context.Context, pageURL, host, cat string, types []string) ([]filedb.TorrentDetails, error) {
	body, err := p.httpGet(ctx, pageURL, "")
	if err != nil {
		return nil, err
	}
	// Extract post URLs from cards
	postURLs := extractPostURLs(body, host)
	if len(postURLs) == 0 {
		return nil, nil
	}

	var out []filedb.TorrentDetails
	now := time.Now().UTC().Format(time.RFC3339Nano)

	for _, postURL := range postURLs {
		dhtml, err := p.httpGet(ctx, postURL, pageURL)
		if err != nil || dhtml == "" {
			continue
		}

		// Names from info box / h1
		nameRu := extractMatch(nameRuRe, dhtml)
		nameEn := extractMatch(nameEnRe, dhtml)
		if nameRu == "" {
			if h1 := extractMatch(h1Re, dhtml); h1 != "" {
				nameRu = strings.TrimSpace(inlineRe89769cRe.ReplaceAllString(h1, ""))
			}
		}
		if nameRu == "" {
			continue
		}

		// Year
		relased := 0
		if ym := yearRe.FindStringSubmatch(dhtml); len(ym) > 1 {
			relased, _ = strconv.Atoi(ym[1])
		}

		// Find torrent IDs
		dhtmlDecoded := html.UnescapeString(dhtml)
		ids := uniqueMatches(downloadIDRe, dhtmlDecoded)
		if len(ids) == 0 {
			ids = uniqueMatches(downloadIDRe, dhtml)
		}
		if len(ids) == 0 {
			ids = uniqueMatches(torrentInfoRe, dhtml)
		}
		if len(ids) == 0 {
			continue
		}

		// Collect page magnets
		pageMagnets := collectMagnets(dhtml)

		for i, tid := range ids {
			around := takeAround(dhtmlDecoded, "torrent_"+tid+"_info", 20000)
			if around == "" {
				around = takeAround(dhtml, "torrent_"+tid+"_info", 20000)
			}

			sid, pir := 0, 0
			if m := sidLeRe.FindStringSubmatch(around); len(m) > 1 {
				sid, _ = strconv.Atoi(m[1])
			}
			if m := pirLeRe.FindStringSubmatch(around); len(m) > 1 {
				pir, _ = strconv.Atoi(m[1])
			}

			// Quality from filename
			q := ""
			if fn := extractMatch(fileNameRe, around); fn != "" {
				if qm := qualityRe.FindStringSubmatch(fn); len(qm) > 1 {
					q = qm[1]
				}
			}
			qDigits := "0"
			if q != "" {
				qDigits = strings.ToLower(strings.ReplaceAll(q, "p", ""))
			}

			// Magnet from block or page
			magnet := ""
			if mm := magnetHrefRe.FindStringSubmatch(around); len(mm) > 1 {
				magnet = html.UnescapeString(mm[1])
			} else if mm := magnetRawRe.FindStringSubmatch(around); len(mm) > 1 {
				magnet = html.UnescapeString(mm[1])
			}
			if magnet == "" && i < len(pageMagnets) && len(pageMagnets) == len(ids) {
				magnet = pageMagnets[i]
			}

			title := nameRu
			if nameEn != "" {
				title = nameRu + " / " + nameEn
			}
			if relased > 0 {
				title += fmt.Sprintf(" %d", relased)
			}
			if q != "" {
				title += " [" + q + "]"
			}

			url := fmt.Sprintf("%s?q=%s&id=%s", postURL, qDigits, tid)

			out = append(out, filedb.TorrentRecord{
				TrackerName: trackerName,
				Types: types,
				URL: url,
				Title: title,
				Sid: sid,
				Pir: pir,
				CreateTime: now,
				UpdateTime: now,
				Name: nameRu,
				OriginalName: core.FirstNonEmpty(nameEn, nameRu),
				Relased: relased,
				Magnet: magnet,
				SearchName: core.SearchName(nameRu),
				SearchOrig: core.SearchName(core.FirstNonEmpty(nameEn, nameRu)),
				TID: tid,
			}.ToMap())
		}

		if p.Config.Leproduction.ParseDelay > 0 {
			select {
			case <-ctx.Done():
				return out, ctx.Err()
			case <-time.After(time.Duration(p.Config.Leproduction.ParseDelay) * time.Millisecond):
			}
		}
	}
	return out, nil
}

func (p *Parser) saveTorrents(ctx context.Context, torrents []filedb.TorrentDetails, host string) (int, int, int, int, error) {
	added, updated, skipped, failed := 0, 0, 0, 0
	plog := core.NewParserLog(trackerName, filepath.Join(p.DB.DataDir, "log"), p.Config.LogParsers && p.Config.Leproduction.Log)
	bucketCache := map[string]map[string]filedb.TorrentDetails{}
	changed := map[string]time.Time{}

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
		needMagnet := !exists || asString(existing["title"]) != asString(incoming["title"]) || strings.TrimSpace(asString(existing["magnet"])) == ""
		if needMagnet && strings.TrimSpace(asString(incoming["magnet"])) == "" {
			tid := asString(incoming["_tid"])
			if tid != "" {
				downURL := fmt.Sprintf("%s/index.php?do=download&id=%s", host, tid)
				magHTML, err := p.httpGet(ctx, downURL, host)
				if err == nil && magHTML != "" {
					if mm := magnetHrefRe.FindStringSubmatch(magHTML); len(mm) > 1 {
						incoming["magnet"] = html.UnescapeString(mm[1])
					} else if mm := magnetRawRe.FindStringSubmatch(magHTML); len(mm) > 1 {
						incoming["magnet"] = html.UnescapeString(mm[1])
					}
				}
			}
		}
		delete(incoming, "_tid")
		if needMagnet && strings.TrimSpace(asString(incoming["magnet"])) == "" {
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
		changed[key] = time.Now().UTC()
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

func extractPostURLs(body, host string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, re := range []*regexp.Regexp{shortImgRe, h3LinkRe} {
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			if len(m) < 2 {
				continue
			}
			u := m[1]
			if strings.HasPrefix(u, "/") {
				u = host + u
			}
			if _, ok := seen[u]; !ok {
				seen[u] = struct{}{}
				out = append(out, u)
			}
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

func uniqueMatches(re *regexp.Regexp, body string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		if len(m) > 1 {
			v := m[1]
			if _, ok := seen[v]; !ok {
				seen[v] = struct{}{}
				out = append(out, v)
			}
		}
	}
	return out
}

func collectMagnets(body string) []string {
	var out []string
	for _, m := range magnetHrefRe.FindAllStringSubmatch(body, -1) {
		if len(m) > 1 {
			out = append(out, html.UnescapeString(m[1]))
		}
	}
	return out
}

func takeAround(text, needle string, radius int) string {
	idx := strings.Index(strings.ToLower(text), strings.ToLower(needle))
	if idx < 0 {
		return ""
	}
	s := idx - radius
	if s < 0 {
		s = 0
	}
	e := idx + len(needle) + radius
	if e > len(text) {
		e = len(text)
	}
	return text[s:e]
}

func (p *Parser) httpGet(ctx context.Context, rawURL, referer string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	return string(b), err
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
