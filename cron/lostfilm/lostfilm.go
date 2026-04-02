package lostfilm

import (
	"context"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"path/filepath"

	"jacred/app"
	"jacred/core"
	"jacred/filedb"
)

const trackerName = "lostfilm"

var (
	lostfilmMark  = "LostFilm.TV"
	pageCountRe   = regexp.MustCompile(`/new/page_(\d+)`)
	innerLinkRe   = regexp.MustCompile(`<div\s+class="inner-box--link\s+main"[^>]*><a\s+href="([^"]+)"[^>]*>([^<]+)</a></div>`)
	episodeLinkRe = regexp.MustCompile(`<a\s[^>]*href="[^"]*?(/series/([^/"]+)/season_(\d+)/episode_(\d+)/)[^"]*"[^>]*>([\s\S]*?)</a>`)
	seasonInfoRe  = regexp.MustCompile(`(\d+)\s*сезон\s*(\d+)\s*серия`)
	dateRe        = regexp.MustCompile(`(\d{2}\.\d{2}\.\d{4})`)
	newMovieRe    = regexp.MustCompile(`<a\s+class="new-movie"\s+href="(?:https?://[^"]+)?(/series/[^"]+)"[^>]*title="([^"]*)"[^>]*>([\s\S]*?)</a>`)
	vLinkRe       = regexp.MustCompile(`href="(/V/\?[^"]+)"`)

	inlineClsDateRe = regexp.MustCompile(`<div\s+class="date"[^>]*>(\d{2}\.\d{2}\.\d{4})</div>`)
	inlineClsTitleRe = regexp.MustCompile(`<div\s+class="title"[^>]*>\s*([^<]+)\s*</div>`)
	inlineRe30150bRe = regexp.MustCompile(`\bSD\b`)
	inlineRe8987e7Re = regexp.MustCompile(`PlayEpisode\s*\(\s*'(\d+)'\s*,\s*'(\d+)'\s*,\s*'(\d+)'\s*\)`)
	inlineReA2d1f6Re = regexp.MustCompile(`Play(?:Movie|Episode)\s*\(\s*'(\d+)'\s*,\s*'(\d+)'\s*,\s*'(\d+)'\s*\)`)
	inlineReA41f09Re = regexp.MustCompile(`[?&]s=(\d+)`)
	inlineReB39b2dRe = regexp.MustCompile(`^series/([^/]+)(?:/|$)`)
)

type Parser struct {
	Config app.Config
	DB     *filedb.DB
	Client *http.Client

	mu      sync.Mutex
	working bool
}

type ParseResult struct {
	Status      string `json:"status"`
	Fetched     int    `json:"fetched"`
	Added       int    `json:"added"`
	Updated     int    `json:"updated"`
	Skipped     int    `json:"skipped"`
	Failed      int    `json:"failed"`
	FromCache   int    `json:"fromCache"`
	WithoutMag  int    `json:"withoutMag"`
	TotalPages  int    `json:"totalPages"`
	ParsedPages int    `json:"parsedPages"`
}

type VerifyItem struct {
	Title   string `json:"title"`
	DateStr string `json:"dateStr"`
	Relased int    `json:"relased"`
	URL     string `json:"url"`
	Source  string `json:"source"`
}

type StatsResult struct {
	Total         int      `json:"total"`
	WithMagnet    int      `json:"withMagnet"`
	WithoutMagnet int      `json:"withoutMagnet"`
	KeysCount     int      `json:"keysCount"`
	Keys          []string `json:"keys"`
	KeysMore      int      `json:"keysMore"`
}

type magnetQuality struct {
	Magnet   string
	Quality  string
	SizeName string
}

type pageCounters struct {
	Fetched    int
	Added      int
	Updated    int
	Skipped    int
	Failed     int
	FromCache  int
	WithoutMag int
}

func New(cfg app.Config, db *filedb.DB) *Parser {
	return &Parser{Config: cfg, DB: db, Client: &http.Client{Timeout: 45 * time.Second}}
}

func (p *Parser) Parse(ctx context.Context) (ParseResult, error) {
	return p.parseRange(ctx, 1, 0)
}

func (p *Parser) ParsePages(ctx context.Context, pageFrom, pageTo int) (ParseResult, error) {
	if pageFrom < 1 {
		pageFrom = 1
	}
	if pageTo < pageFrom {
		pageTo = pageFrom
	}
	return p.parseRange(ctx, pageFrom, pageTo)
}

func (p *Parser) parseRange(ctx context.Context, pageFrom, pageTo int) (ParseResult, error) {
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
	host := strings.TrimSpace(p.Config.Lostfilm.Host)
	if host == "" {
		return ParseResult{Status: "conf"}, nil
	}
	cookie := strings.TrimSpace(p.Config.Lostfilm.Cookie)

	firstHTML, err := p.fetchText(ctx, strings.TrimRight(host, "/")+"/new/", cookie, strings.TrimRight(host, "/")+"/")
	if err != nil {
		return ParseResult{}, err
	}
	if !strings.Contains(firstHTML, lostfilmMark) {
		return ParseResult{Status: "empty"}, nil
	}
	totalPages := 1
	for _, m := range pageCountRe.FindAllStringSubmatch(firstHTML, -1) {
		if len(m) > 1 {
			if n, _ := strconv.Atoi(m[1]); n > totalPages {
				totalPages = n
			}
		}
	}
	if totalPages > 100 {
		totalPages = 100
	}
	if pageTo <= 0 || pageTo > totalPages {
		pageTo = totalPages
	}

	res := ParseResult{Status: "ok", TotalPages: totalPages}
	for page := pageFrom; page <= pageTo; page++ {
		if page > pageFrom && p.Config.Lostfilm.ParseDelay > 0 {
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-time.After(time.Duration(p.Config.Lostfilm.ParseDelay) * time.Millisecond):
			}
		}
		preloaded := ""
		if page == 1 {
			preloaded = firstHTML
		}
		pageRes, err := p.parsePage(ctx, host, cookie, page, preloaded)
		if err != nil {
			return res, err
		}
		res.ParsedPages++
		res.Fetched += pageRes.Fetched
		res.Added += pageRes.Added
		res.Updated += pageRes.Updated
		res.Skipped += pageRes.Skipped
		res.Failed += pageRes.Failed
		res.FromCache += pageRes.FromCache
		res.WithoutMag += pageRes.WithoutMag
		log.Printf("lostfilm: page %d/%d fetched=%d added=%d skipped=%d failed=%d withoutMag=%d", page, pageTo, pageRes.Fetched, pageRes.Added, pageRes.Skipped, pageRes.Failed, pageRes.WithoutMag)
	}
	log.Printf("lostfilm: done fetched=%d added=%d skipped=%d failed=%d", res.Fetched, res.Added, res.Skipped, res.Failed)
	return res, nil
}

func (p *Parser) ParseSeasonPacks(ctx context.Context, series string) (string, error) {
	p.mu.Lock()
	if p.working {
		p.mu.Unlock()
		return "work", nil
	}
	p.working = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.working = false
		p.mu.Unlock()
	}()

	host := strings.TrimSpace(p.Config.Lostfilm.Host)
	if host == "" {
		return "conf", nil
	}
	series = strings.TrimSpace(series)
	if series == "" {
		return "series required", nil
	}
	cookie := strings.TrimSpace(p.Config.Lostfilm.Cookie)
	body, err := p.fetchText(ctx, strings.TrimRight(host, "/")+"/series/"+strings.Trim(series, "/")+"/seasons/", cookie, strings.TrimRight(host, "/")+"/")
	if err != nil {
		return "", err
	}
	if !strings.Contains(body, lostfilmMark) {
		return "empty", nil
	}
	relased, russianName := parseRelasedAndNameFromHTML(body)
	if relased <= 0 {
		return "no relased", nil
	}
	original := strings.ReplaceAll(series, "_", " ")
	name := firstNonEmpty(russianName, original)
	seen := map[string]struct{}{}
	var torrents []filedb.TorrentDetails
	for _, m := range vLinkRe.FindAllStringSubmatch(body, -1) {
		if len(m) < 2 {
			continue
		}
		vPath := m[1]
		if !strings.Contains(strings.ToLower(vPath), "e=999") {
			continue
		}
		seasonMatch := inlineReA41f09Re.FindStringSubmatch(vPath)
		if len(seasonMatch) < 2 {
			continue
		}
		seasonNum, _ := strconv.Atoi(seasonMatch[1])
		vURL := absURL(host, vPath)
		if _, ok := seen[vURL]; ok {
			continue
		}
		seen[vURL] = struct{}{}
		mags, err := p.getMagnetsFromVPage(ctx, host, cookie, vURL)
		if err != nil || len(mags) == 0 {
			continue
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		for _, mag := range mags {
			q := normalizeQuality(mag.Quality)
			torrents = append(torrents, filedb.TorrentRecord{
				TrackerName: trackerName,
				Types: []string{"serial"},
				URL: vURL + "#" + q,
				Title: fmt.Sprintf("%s / %s / %d сезон (полный сезон) [%d, %s]", name, original, seasonNum, relased, q),
				Sid: 1,
				CreateTime: now,
				UpdateTime: now,
				Name: name,
				OriginalName: original,
				Relased: relased,
				Magnet: mag.Magnet,
				SizeName: mag.SizeName,
			}.ToMap())
		}
	}
	if len(torrents) == 0 {
		return "ok", nil
	}
	_, _, _, _, _, _, err = p.saveTorrents(ctx, host, cookie, torrents)
	if err != nil {
		return "", err
	}
	return "ok", nil
}

func (p *Parser) VerifyPage(ctx context.Context, series string) ([]VerifyItem, string, error) {
	host := strings.TrimSpace(p.Config.Lostfilm.Host)
	if host == "" {
		return nil, "conf", nil
	}
	cookie := strings.TrimSpace(p.Config.Lostfilm.Cookie)
	body, err := p.fetchText(ctx, strings.TrimRight(host, "/")+"/new/", cookie, strings.TrimRight(host, "/")+"/")
	if err != nil {
		return nil, "", err
	}
	if !strings.Contains(body, lostfilmMark) {
		return nil, "empty", nil
	}
	items := parseNewPageDates(body, host)
	if strings.TrimSpace(series) != "" {
		filter := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(series), " ", "_"))
		filtered := make([]VerifyItem, 0, len(items))
		for _, item := range items {
			u := strings.ToLower(item.URL)
			t := strings.ToLower(item.Title)
			if strings.Contains(u, filter) || strings.Contains(t, filter) || strings.Contains(t, strings.ReplaceAll(filter, "_", " ")) {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}
	return items, "ok", nil
}

func (p *Parser) Stats() StatsResult {
	_ = p.DB.RebuildIndexes()
	keySet := map[string]struct{}{}
	for _, keys := range p.DB.FastDB() {
		for _, key := range keys {
			keySet[key] = struct{}{}
		}
	}
	res := StatsResult{}
	var keysWithLost []string
	for key := range keySet {
		bucket, err := p.DB.OpenRead(key)
		if err != nil {
			continue
		}
		hasLost := false
		for _, t := range bucket {
			if !strings.EqualFold(asString(t["trackerName"]), trackerName) {
				continue
			}
			hasLost = true
			res.Total++
			if strings.TrimSpace(asString(t["magnet"])) != "" {
				res.WithMagnet++
			}
		}
		if hasLost {
			keysWithLost = append(keysWithLost, key)
		}
	}
	sort.Strings(keysWithLost)
	res.WithoutMagnet = res.Total - res.WithMagnet
	res.KeysCount = len(keysWithLost)
	if len(keysWithLost) > 50 {
		res.Keys = append([]string(nil), keysWithLost[:50]...)
		res.KeysMore = len(keysWithLost) - 50
	} else {
		res.Keys = keysWithLost
	}
	return res
}

func (p *Parser) parsePage(ctx context.Context, host, cookie string, page int, preloaded string) (pageCounters, error) {
	out := pageCounters{}
	rawURL := strings.TrimRight(host, "/") + "/new/"
	if page > 1 {
		rawURL = fmt.Sprintf("%s/new/page_%d", strings.TrimRight(host, "/"), page)
	}
	body := preloaded
	var err error
	if strings.TrimSpace(body) == "" {
		body, err = p.fetchText(ctx, rawURL, cookie, strings.TrimRight(host, "/")+"/")
		if err != nil {
			return out, err
		}
	}
	if !strings.Contains(body, lostfilmMark) {
		return out, nil
	}
	normalized := strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(body)
	nameMap := buildHorBreakerNameMap(normalized)
	list := p.collectFromEpisodeLinks(normalized, host, nameMap)
	if len(list) == 0 {
		list = p.collectFromNewMovie(normalized, host, page)
	}
	if len(list) == 0 {
		list = p.collectFromHorBreaker(normalized, host, page)
	}
	movies, err := p.collectFromMovies(ctx, normalized, host, cookie)
	if err != nil {
		return out, err
	}
	list = append(list, movies...)
	list = dedupeListByURL(list)
	out.Fetched = len(list)
	out.Added, out.Updated, out.Skipped, out.Failed, out.FromCache, out.WithoutMag, err = p.saveTorrents(ctx, host, cookie, list)
	return out, err
}

func (p *Parser) collectFromEpisodeLinks(htmlBody, host string, nameMap map[string][2]string) []filedb.TorrentDetails {
	seen := map[string]struct{}{}
	list := make([]filedb.TorrentDetails, 0)
	for _, m := range episodeLinkRe.FindAllStringSubmatch(htmlBody, -1) {
		if len(m) < 6 {
			continue
		}
		urlPath := strings.TrimPrefix(strings.TrimSpace(m[1]), "/")
		serieName := strings.TrimSpace(m[2])
		block := m[5]
		if serieName == "" {
			continue
		}
		if _, ok := seen[urlPath]; ok {
			continue
		}
		sm := seasonInfoRe.FindStringSubmatch(block)
		dms := dateRe.FindAllStringSubmatch(block, -1)
		if len(sm) < 3 || len(dms) == 0 {
			continue
		}
		ct := parseDate(dms[len(dms)-1][1])
		if ct.IsZero() {
			ct = time.Now().UTC()
		}
		relased := ct.Year()
		sinfo := strings.TrimSpace(sm[1] + " сезон " + sm[2] + " серия")
		original := strings.ReplaceAll(serieName, "_", " ")
		name := original
		if pair, ok := nameMap[strings.TrimRight(urlPath, "/")]; ok {
			name, original = pair[0], pair[1]
		} else if pair, ok := nameMap["series/"+serieName]; ok {
			name, original = pair[0], pair[1]
		}
		seen[urlPath] = struct{}{}
		now := ct.UTC().Format(time.RFC3339Nano)
		list = append(list, filedb.TorrentRecord{
			TrackerName: trackerName,
			Types: []string{"serial"},
			URL: absURL(host, "/"+urlPath),
			Title: fmt.Sprintf("%s / %s / %s [%d]", name, original, sinfo, relased),
			Sid: 1,
			CreateTime: now,
			UpdateTime: now,
			Name: name,
			OriginalName: original,
			Relased: relased,
		}.ToMap())
	}
	return list
}

func (p *Parser) collectFromNewMovie(htmlBody, host string, page int) []filedb.TorrentDetails {
	list := make([]filedb.TorrentDetails, 0)
	for _, m := range newMovieRe.FindAllStringSubmatch(htmlBody, -1) {
		if len(m) < 4 {
			continue
		}
		urlPath := strings.TrimPrefix(strings.TrimSpace(m[1]), "/")
		block := m[3]
		if urlPath == "" || !strings.HasPrefix(urlPath, "series/") {
			continue
		}
		titleMatch := inlineClsTitleRe.FindStringSubmatch(block)
		if len(titleMatch) < 2 {
			continue
		}
		dm := inlineClsDateRe.FindAllStringSubmatch(block, -1)
		if len(dm) == 0 && page != 1 {
			continue
		}
		dateStr := ""
		if len(dm) > 0 {
			dateStr = dm[len(dm)-1][1]
		}
		ct := parseDate(dateStr)
		if ct.IsZero() {
			ct = time.Now().UTC()
		}
		relased := ct.Year()
		serieName := firstSubmatch(urlPath, `series/([^/]+)(?:/|$)`)
		if serieName == "" {
			continue
		}
		original := strings.ReplaceAll(serieName, "_", " ")
		name := firstNonEmpty(shortenSeriesName(html.UnescapeString(strings.TrimSpace(m[2]))), original)
		now := ct.UTC().Format(time.RFC3339Nano)
		list = append(list, filedb.TorrentRecord{
			TrackerName: trackerName,
			Types: []string{"serial"},
			URL: absURL(host, "/"+urlPath),
			Title: fmt.Sprintf("%s / %s / %s [%d]", name, original, html.UnescapeString(strings.TrimSpace(titleMatch[1])), relased),
			Sid: 1,
			CreateTime: now,
			UpdateTime: now,
			Name: name,
			OriginalName: original,
			Relased: relased,
		}.ToMap())
	}
	return list
}

func (p *Parser) collectFromHorBreaker(htmlBody, host string, page int) []filedb.TorrentDetails {
	list := make([]filedb.TorrentDetails, 0)
	for _, row := range strings.Split(htmlBody, `class="hor-breaker dashed"`)[1:] {
		urlPath := firstSubmatch(row, `href="/([^"]+)"`)
		sinfo := strings.TrimSpace(firstSubmatch(row, `<div class="left-part">([^<]+)</div>`))
		name := strings.TrimSpace(firstSubmatch(row, `<div class="name-ru">([^<]+)</div>`))
		original := strings.TrimSpace(firstSubmatch(row, `<div class="name-en">([^<]+)</div>`))
		dateStr := strings.TrimSpace(firstSubmatch(row, `<div class="right-part">(\d{2}\.\d{2}\.\d{4})</div>`))
		if urlPath == "" || !strings.HasPrefix(urlPath, "series/") || name == "" || original == "" || sinfo == "" {
			continue
		}
		ct := parseDate(dateStr)
		if ct.IsZero() && page != 1 {
			continue
		}
		if ct.IsZero() {
			ct = time.Now().UTC()
		}
		relased := ct.Year()
		now := ct.UTC().Format(time.RFC3339Nano)
		list = append(list, filedb.TorrentRecord{
			TrackerName: trackerName,
			Types: []string{"serial"},
			URL: absURL(host, "/"+urlPath),
			Title: fmt.Sprintf("%s / %s / %s [%d]", html.UnescapeString(name), html.UnescapeString(original), sinfo, relased),
			Sid: 1,
			CreateTime: now,
			UpdateTime: now,
			Name: html.UnescapeString(name),
			OriginalName: html.UnescapeString(original),
			Relased: relased,
		}.ToMap())
	}
	return list
}

func (p *Parser) collectFromMovies(ctx context.Context, htmlBody, host, cookie string) ([]filedb.TorrentDetails, error) {
	seen := map[string]struct{}{}
	list := make([]filedb.TorrentDetails, 0)
	for _, row := range strings.Split(htmlBody, `class="hor-breaker dashed"`)[1:] {
		urlPath := firstSubmatch(row, `href="/([^"]+)"`)
		leftPart := strings.TrimSpace(firstSubmatch(row, `<div class="left-part">([^<]+)</div>`))
		if urlPath == "" || !strings.HasPrefix(urlPath, "movies/") || !strings.Contains(strings.ToLower(leftPart), strings.ToLower("Фильм")) {
			continue
		}
		name := strings.TrimSpace(firstSubmatch(row, `<div class="name-ru">([^<]+)</div>`))
		original := strings.TrimSpace(firstSubmatch(row, `<div class="name-en">([^<]+)</div>`))
		dateStr := strings.TrimSpace(firstSubmatch(row, `<div class="right-part">(\d{2}\.\d{2}\.\d{4})</div>`))
		if name == "" || original == "" || dateStr == "" {
			continue
		}
		movieURL := absURL(host, "/"+urlPath)
		if _, ok := seen[movieURL]; ok {
			continue
		}
		seen[movieURL] = struct{}{}
		ct := parseDate(dateStr)
		if ct.IsZero() {
			ct = time.Now().UTC()
		}
		relased := ct.Year()
		vURL, err := p.getVURLFromMoviePage(ctx, host, cookie, movieURL)
		if err != nil || vURL == "" {
			continue
		}
		mags, err := p.getMagnetsFromVPage(ctx, host, cookie, vURL)
		if err != nil || len(mags) == 0 {
			continue
		}
		now := ct.UTC().Format(time.RFC3339Nano)
		name = html.UnescapeString(name)
		original = html.UnescapeString(original)
		for _, mag := range mags {
			q := normalizeQuality(mag.Quality)
			list = append(list, filedb.TorrentRecord{
				TrackerName: trackerName,
				Types: []string{"movie"},
				URL: movieURL + "#" + q,
				Title: fmt.Sprintf("%s / %s [Фильм, %d, %s]", name, original, relased, q),
				Sid: 1,
				CreateTime: now,
				UpdateTime: now,
				Name: name,
				OriginalName: original,
				Relased: relased,
				Magnet: mag.Magnet,
				SizeName: mag.SizeName,
			}.ToMap())
		}
	}
	return list, nil
}

func (p *Parser) saveTorrents(ctx context.Context, host, cookie string, torrents []filedb.TorrentDetails) (int, int, int, int, int, int, error) {
	added, updated, skipped, failed, fromCache, noMagnet := 0, 0, 0, 0, 0, 0
	plog := core.NewParserLog(trackerName, filepath.Join(p.DB.DataDir, "log"))
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
				return added, updated, skipped, failed, fromCache, noMagnet, err
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
		if strings.TrimSpace(asString(incoming["magnet"])) == "" {
			if cached, ok := bucket[urlv]; ok && strings.TrimSpace(asString(cached["magnet"])) != "" {
				incoming["magnet"] = cached["magnet"]
				incoming["title"] = cached["title"]
				if strings.TrimSpace(asString(cached["sizeName"])) != "" {
					incoming["sizeName"] = cached["sizeName"]
				}
				if strings.TrimSpace(asString(cached["name"])) != "" {
					incoming["name"] = cached["name"]
				}
				if strings.TrimSpace(asString(cached["originalname"])) != "" {
					incoming["originalname"] = cached["originalname"]
				}
				fromCache++
			} else {
				var mag magnetQuality
				var err error
				if containsString(toStrings(incoming["types"]), "movie") {
					mag, err = p.getMagnetForMovie(ctx, host, cookie, urlv)
				} else {
					mag, err = p.getMagnet(ctx, host, cookie, urlv)
				}
				if err != nil {
					return added, updated, skipped, failed, fromCache, noMagnet, err
				}
				if strings.TrimSpace(mag.Magnet) == "" {
					failed++
					noMagnet++
					continue
				}
				incoming["magnet"] = mag.Magnet
				incoming["sizeName"] = mag.SizeName
				if strings.TrimSpace(mag.Quality) != "" {
					q := normalizeQuality(mag.Quality)
					title := strings.TrimSpace(asString(incoming["title"]))
					if strings.HasSuffix(title, "]") {
						title = strings.TrimSuffix(title, "]") + ", " + q + "]"
					} else {
						title = title + " [" + q + "]"
					}
					incoming["title"] = title
				}
			}
		}
		if exists && samePrimary(existing, incoming) {
			skipped++
			continue
		}
		merged := mergeTorrent(existing, exists, incoming)
		bucket[urlv] = merged
		changed[key] = fileTime(merged)
		if exists {
			plog.WriteUpdated(urlv, asString(incoming["title"]))
			updated++
		} else {
			plog.WriteAdded(urlv, asString(incoming["title"]))
			added++
		}
	}
	for key, when := range changed {
		if err := p.DB.SaveBucket(key, bucketCache[key], when); err != nil {
			return added, updated, skipped, failed, fromCache, noMagnet, err
		}
	}
	if len(changed) > 0 {
		if err := p.DB.SaveChangesToFile(); err != nil {
			return added, updated, skipped, failed, fromCache, noMagnet, err
		}
	}
	return added, updated, skipped, failed, fromCache, noMagnet, nil
}

func (p *Parser) getMagnet(ctx context.Context, host, cookie, episodeURL string) (magnetQuality, error) {
	body, err := p.fetchText(ctx, episodeURL, cookie, strings.TrimRight(host, "/")+"/")
	if err != nil || body == "" {
		return magnetQuality{}, err
	}
	// Prefer full combined ID like PlayEpisode('780002005') over PlayEpisode('780','2','5')
	epID := firstSubmatch(body, `PlayEpisode\s*\(\s*['"](\d{6,})['"]`)
	if epID == "" {
		// Fallback: build from 3-arg form PlayEpisode('780','2','5') → 780002005
		m := inlineRe8987e7Re.FindStringSubmatch(body)
		if len(m) == 4 {
			s, _ := strconv.Atoi(m[2])
			e, _ := strconv.Atoi(m[3])
			epID = fmt.Sprintf("%s%03d%03d", m[1], s, e)
		}
	}
	if epID == "" {
		return magnetQuality{}, nil
	}
	searchHTML, err := p.fetchVPageHTML(ctx, host, cookie, "", epID)
	if err != nil {
		return magnetQuality{}, err
	}
	if !strings.Contains(searchHTML, "inner-box--link") {
		return magnetQuality{}, nil
	}
	list, err := p.parseVPageQualityLinks(ctx, host, cookie, searchHTML)
	if err != nil || len(list) == 0 {
		return magnetQuality{}, err
	}
	return list[0], nil
}

func (p *Parser) getMagnetsFromVPage(ctx context.Context, host, cookie, vPageURL string) ([]magnetQuality, error) {
	searchHTML, err := p.fetchVPageHTML(ctx, host, cookie, vPageURL, "")
	if err != nil || !strings.Contains(searchHTML, "inner-box--link") {
		return nil, err
	}
	return p.parseVPageQualityLinks(ctx, host, cookie, searchHTML)
}

func (p *Parser) getMagnetForMovie(ctx context.Context, host, cookie, movieURL string) (magnetQuality, error) {
	vURL, err := p.getVURLFromMoviePage(ctx, host, cookie, movieURL)
	if err != nil || vURL == "" {
		return magnetQuality{}, err
	}
	list, err := p.getMagnetsFromVPage(ctx, host, cookie, vURL)
	if err != nil || len(list) == 0 {
		return magnetQuality{}, err
	}
	return list[0], nil
}

func (p *Parser) getVURLFromMoviePage(ctx context.Context, host, cookie, movieURL string) (string, error) {
	body, err := p.fetchText(ctx, movieURL, cookie, strings.TrimRight(host, "/")+"/")
	if err != nil || body == "" {
		return "", err
	}
	if v := firstSubmatch(body, `href="(/V/\?[^"]+)"`); v != "" {
		return absURL(host, v), nil
	}
	// Prefer full combined ID (6+ digits)
	id := firstSubmatch(body, `Play(?:Movie|Episode)\s*\(\s*['"](\d{6,})['"]`)
	if id == "" {
		// Fallback: 3-arg form PlayEpisode('780','2','5') → 780002005
		m := inlineReA2d1f6Re.FindStringSubmatch(body)
		if len(m) == 4 {
			s, _ := strconv.Atoi(m[2])
			e, _ := strconv.Atoi(m[3])
			id = fmt.Sprintf("%s%03d%03d", m[1], s, e)
		}
	}
	if id == "" {
		// Last resort: single short arg
		id = firstSubmatch(body, `Play(?:Movie|Episode)\s*\(\s*['"]?(\d+)['"]?`)
	}
	if id != "" {
		searchHTML, err := p.fetchText(ctx, strings.TrimRight(host, "/")+"/v_search.php?a="+id, cookie, strings.TrimRight(host, "/")+"/")
		if err != nil || searchHTML == "" {
			return "", err
		}
		if v := strings.TrimSpace(firstSubmatch(searchHTML, `(?:content="[^"]*url\s*=\s*|location\.replace\s*\(\s*["'])([^"']+)`)); v != "" {
			return absURL(host, v), nil
		}
		if v := firstSubmatch(searchHTML, `href="(/V/\?[^"]+)"`); v != "" {
			return absURL(host, v), nil
		}
	}
	return "", nil
}

func (p *Parser) fetchVPageHTML(ctx context.Context, host, cookie, vPageURL, episodeID string) (string, error) {
	searchHTML := ""
	var err error
	if episodeID != "" {
		searchHTML, err = p.fetchText(ctx, strings.TrimRight(host, "/")+"/v_search.php?a="+episodeID, cookie, strings.TrimRight(host, "/")+"/")
	} else if vPageURL != "" {
		searchHTML, err = p.fetchText(ctx, absURL(host, vPageURL), cookie, strings.TrimRight(host, "/")+"/")
	}
	if err != nil || searchHTML == "" {
		return searchHTML, err
	}
	if strings.Contains(searchHTML, "inner-box--link") {
		return searchHTML, nil
	}
	vPage := strings.TrimSpace(firstSubmatch(searchHTML, `(?:content="[^"]*url\s*=\s*|location\.replace\s*\(\s*["'])([^"']+)`))
	if vPage == "" {
		vPage = strings.TrimSpace(firstSubmatch(searchHTML, `href="(/V/\?[^"]+)"`))
	}
	if vPage == "" {
		return searchHTML, nil
	}
	finalURL := absURL(host, vPage)
	finalHTML, err := p.fetchText(ctx, finalURL, cookie, strings.TrimRight(host, "/")+"/")
	if err != nil {
		return "", err
	}
	return finalHTML, nil
}

func (p *Parser) parseVPageQualityLinks(ctx context.Context, host, cookie, searchHTML string) ([]magnetQuality, error) {
	if !strings.Contains(searchHTML, "inner-box--link") {
		return nil, nil
	}
	flat := strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(searchHTML)
	out := make([]magnetQuality, 0)
	for _, m := range innerLinkRe.FindAllStringSubmatch(flat, -1) {
		if len(m) < 3 {
			continue
		}
		linkText := strings.TrimSpace(m[2])
		quality := firstSubmatch(linkText, `(2160p|2060p|1440p|1080p|720p)`)
		if quality == "" {
			quality = firstSubmatch(linkText, `\b(1080|720)\b`)
		}
		if quality == "" && strings.Contains(strings.ToLower(linkText), "mp4") {
			quality = "720p"
		}
		if quality == "" && inlineRe30150bRe.MatchString(linkText) {
			quality = "SD"
		}
		if quality == "" {
			continue
		}
		data, err := p.fetchBytes(ctx, m[1], cookie, strings.TrimRight(host, "/")+"/")
		if err != nil || len(data) == 0 {
			continue
		}
		magnet := core.TorrentBytesToMagnet(data)
		if magnet == "" {
			continue
		}
		out = append(out, magnetQuality{Magnet: magnet, Quality: normalizeQuality(quality), SizeName: torrentBytesToSizeName(data)})
	}
	return out, nil
}

func (p *Parser) fetchText(ctx context.Context, rawURL, cookie, referer string) (string, error) {
	b, err := p.fetchBytes(ctx, rawURL, cookie, referer)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (p *Parser) fetchBytes(ctx context.Context, rawURL, cookie, referer string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cookie) != "" {
		req.Header.Set("Cookie", cookie)
	}
	if strings.TrimSpace(referer) != "" {
		req.Header.Set("Referer", referer)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Ubuntu; Linux x86_64; rv:148.0) Gecko/20100101 Firefox/148.0")
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 5<<20))
}

func buildHorBreakerNameMap(htmlBody string) map[string][2]string {
	out := map[string][2]string{}
	for _, row := range strings.Split(htmlBody, `class="hor-breaker dashed"`)[1:] {
		urlPath := strings.TrimSpace(firstSubmatch(row, `href="/([^"]+)"`))
		name := html.UnescapeString(strings.TrimSpace(firstSubmatch(row, `<div class="name-ru">([^<]+)</div>`)))
		original := html.UnescapeString(strings.TrimSpace(firstSubmatch(row, `<div class="name-en">([^<]+)</div>`)))
		if urlPath == "" || !strings.HasPrefix(urlPath, "series/") || name == "" || original == "" {
			continue
		}
		key := strings.TrimRight(urlPath, "/")
		if _, ok := out[key]; !ok {
			out[key] = [2]string{name, original}
		}
		if m := inlineReB39b2dRe.FindStringSubmatch(urlPath); len(m) > 1 {
			seriesKey := "series/" + strings.TrimRight(m[1], "/")
			if _, ok := out[seriesKey]; !ok {
				out[seriesKey] = [2]string{name, original}
			}
		}
	}
	return out
}

func dedupeListByURL(list []filedb.TorrentDetails) []filedb.TorrentDetails {
	byURL := map[string]filedb.TorrentDetails{}
	for _, t := range list {
		urlv := strings.TrimSpace(asString(t["url"]))
		if urlv == "" {
			continue
		}
		existing, ok := byURL[urlv]
		if !ok {
			byURL[urlv] = t
			continue
		}
		curHasRu := hasRuName(t)
		prevHasRu := hasRuName(existing)
		if curHasRu && !prevHasRu {
			byURL[urlv] = t
		}
	}
	out := make([]filedb.TorrentDetails, 0, len(byURL))
	for _, t := range byURL {
		out = append(out, t)
	}
	return out
}

func hasRuName(t filedb.TorrentDetails) bool {
	name := strings.TrimSpace(asString(t["name"]))
	original := strings.TrimSpace(asString(t["originalname"]))
	return name != "" && original != "" && !strings.EqualFold(name, original)
}

func parseRelasedAndNameFromHTML(htmlBody string) (int, string) {
	m := regexp.MustCompile(`itemprop="dateCreated"\s+content="(\d{4})-\d{2}-\d{2}"`).FindStringSubmatch(htmlBody)
	if len(m) < 2 {
		return 0, ""
	}
	year, _ := strconv.Atoi(m[1])
	russian := strings.TrimSpace(firstSubmatch(htmlBody, `<meta\s+property="og:title"\s+content="([^"]+)"`))
	if russian == "" {
		russian = strings.TrimSpace(firstSubmatch(htmlBody, `<title>([^<]+?)\.?\s*[–-]\s*LostFilm`))
	}
	return year, shortenSeriesName(html.UnescapeString(russian))
}

func parseNewPageDates(htmlBody, host string) []VerifyItem {
	parser := &Parser{}
	nameMap := buildHorBreakerNameMap(htmlBody)
	list := parser.collectFromEpisodeLinks(htmlBody, host, nameMap)
	list = append(list, parser.collectFromNewMovie(htmlBody, host, 1)...)
	list = append(list, parser.collectFromHorBreaker(htmlBody, host, 1)...)
	list = dedupeListByURL(list)
	out := make([]VerifyItem, 0, len(list))
	for _, t := range list {
		dt := fileTime(t)
		out = append(out, VerifyItem{Title: asString(t["title"]), DateStr: dt.Format("02.01.2006"), Relased: asInt(t["relased"]), URL: asString(t["url"]), Source: sourceOfVerifyItem(asString(t["url"]))})
	}
	return out
}

func sourceOfVerifyItem(urlv string) string {
	if strings.Contains(urlv, "/season_") && strings.Contains(urlv, "/episode_") {
		return "episode_links"
	}
	return "hor-breaker"
}

func normalizeQuality(quality string) string {
	q := strings.TrimSpace(strings.ToLower(quality))
	switch q {
	case "1080":
		return "1080p"
	case "720":
		return "720p"
	case "sd":
		return "SD"
	case "mp4":
		return "720p"
	}
	if regexp.MustCompile(`^\d{3,4}p$`).MatchString(q) {
		return q
	}
	return quality
}

func shortenSeriesName(title string) string {
	s := strings.TrimSpace(title)
	if s == "" {
		return ""
	}
	if idx := strings.Index(strings.ToLower(s), ". сериал"); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
		if p := strings.Index(s, " ("); p >= 0 {
			s = strings.TrimSpace(s[:p])
		}
		if s != "" {
			return s
		}
	}
	if m := regexp.MustCompile(`^(.+?)\s*/\s*[^/]+?\s*/\s*\d+\s*сезон\s*\d+\s*серия\s*\[\d{4}(?:,[^\]]*)?\]\s*$`).FindStringSubmatch(s); len(m) > 1 {
		s = strings.TrimSpace(m[1])
		if s != "" {
			return s
		}
	}
	if idx := strings.Index(s, " ("); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	if len(s) > 200 {
		s = strings.TrimSpace(s[:200])
	}
	return s
}

func torrentBytesToSizeName(data []byte) string {
	size := torrentTotalSize(data)
	if size <= 0 {
		return ""
	}
	suffix := []string{"B", "KB", "MB", "GB", "TB"}
	val := float64(size)
	i := 0
	for i < len(suffix)-1 && val >= 1024 {
		val /= 1024
		i++
	}
	return fmt.Sprintf("%.2f %s", val, suffix[i])
}

func torrentTotalSize(data []byte) int64 {
	type parser struct {
		b []byte
		i int
	}
	var parseBytes func(*parser) ([]byte, error)
	var parseInt func(*parser) (int64, error)
	var skip func(*parser) error
	parseBytes = func(p *parser) ([]byte, error) {
		start := p.i
		for p.i < len(p.b) && p.b[p.i] >= '0' && p.b[p.i] <= '9' {
			p.i++
		}
		if p.i >= len(p.b) || p.b[p.i] != ':' {
			return nil, fmt.Errorf("bad bytes")
		}
		n, _ := strconv.Atoi(string(p.b[start:p.i]))
		p.i++
		if p.i+n > len(p.b) {
			return nil, io.ErrUnexpectedEOF
		}
		out := p.b[p.i : p.i+n]
		p.i += n
		return out, nil
	}
	parseInt = func(p *parser) (int64, error) {
		if p.i >= len(p.b) || p.b[p.i] != 'i' {
			return 0, fmt.Errorf("bad int")
		}
		p.i++
		start := p.i
		for p.i < len(p.b) && p.b[p.i] != 'e' {
			p.i++
		}
		if p.i >= len(p.b) {
			return 0, io.ErrUnexpectedEOF
		}
		n, _ := strconv.ParseInt(string(p.b[start:p.i]), 10, 64)
		p.i++
		return n, nil
	}
	skip = func(p *parser) error {
		if p.i >= len(p.b) {
			return io.ErrUnexpectedEOF
		}
		switch p.b[p.i] {
		case 'i':
			_, err := parseInt(p)
			return err
		case 'l':
			p.i++
			for p.i < len(p.b) && p.b[p.i] != 'e' {
				if err := skip(p); err != nil {
					return err
				}
			}
			if p.i < len(p.b) {
				p.i++
			}
			return nil
		case 'd':
			p.i++
			for p.i < len(p.b) && p.b[p.i] != 'e' {
				if _, err := parseBytes(p); err != nil {
					return err
				}
				if err := skip(p); err != nil {
					return err
				}
			}
			if p.i < len(p.b) {
				p.i++
			}
			return nil
		default:
			if p.b[p.i] >= '0' && p.b[p.i] <= '9' {
				_, err := parseBytes(p)
				return err
			}
			return fmt.Errorf("unknown token")
		}
	}
	var walkDict func(*parser) (int64, error)
	walkDict = func(p *parser) (int64, error) {
		if p.i >= len(p.b) || p.b[p.i] != 'd' {
			return 0, fmt.Errorf("dict expected")
		}
		p.i++
		var total int64
		for p.i < len(p.b) && p.b[p.i] != 'e' {
			k, err := parseBytes(p)
			if err != nil {
				return 0, err
			}
			switch string(k) {
			case "length":
				n, err := parseInt(p)
				if err != nil {
					return 0, err
				}
				total += n
			case "files":
				if p.i >= len(p.b) || p.b[p.i] != 'l' {
					return 0, fmt.Errorf("files list expected")
				}
				p.i++
				for p.i < len(p.b) && p.b[p.i] != 'e' {
					n, err := walkDict(p)
					if err != nil {
						return 0, err
					}
					total += n
				}
				if p.i < len(p.b) {
					p.i++
				}
			default:
				if err := skip(p); err != nil {
					return 0, err
				}
			}
		}
		if p.i < len(p.b) {
			p.i++
		}
		return total, nil
	}
	p := &parser{b: data}
	if len(p.b) == 0 || p.b[p.i] != 'd' {
		return 0
	}
	p.i++
	for p.i < len(p.b) && p.b[p.i] != 'e' {
		k, err := parseBytes(p)
		if err != nil {
			return 0
		}
		if string(k) == "info" {
			n, _ := walkDict(p)
			return n
		}
		if err := skip(p); err != nil {
			return 0
		}
	}
	return 0
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
	if strings.TrimSpace(asString(out["trackerName"])) == "" {
		out["trackerName"] = trackerName
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

func parseDate(v string) time.Time {
	tm, _ := time.Parse("02.01.2006", strings.TrimSpace(v))
	return tm.UTC()
}

func absURL(host, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	base, _ := url.Parse(strings.TrimRight(host, "/") + "/")
	rel, _ := url.Parse(ref)
	return base.ResolveReference(rel).String()
}

func firstSubmatch(s, pattern string) string {
	m := regexp.MustCompile(pattern).FindStringSubmatch(s)
	if len(m) > 1 {
		return m[1]
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func containsString(list []string, v string) bool {
	for _, item := range list {
		if strings.EqualFold(strings.TrimSpace(item), strings.TrimSpace(v)) {
			return true
		}
	}
	return false
}

func toStrings(v any) []string {
	switch x := v.(type) {
	case []string:
		return append([]string(nil), x...)
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			out = append(out, asString(item))
		}
		return out
	default:
		if s := asString(v); s != "" {
			return []string{s}
		}
		return nil
	}
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
