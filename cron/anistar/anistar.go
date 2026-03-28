package anistar

import (
	"context"
	"fmt"
	"html"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"jacred/app"
	"jacred/core"
	"jacred/filedb"
)

const trackerName = "anistar"

var catTypes = []struct {
	path  string
	types []string
}{
	{"anime", []string{"anime"}},
	{"hentai", []string{"anime"}},
	{"dorams", []string{"serial"}},
}

var (
	postURLAbsRe   = regexp.MustCompile(`https?://[^"'>]+/\d{2,}-[^"'>]+?\.html`)
	postURLRelRe   = regexp.MustCompile(`/\d{2,}-[^"'>]+?\.html`)
	pageNumRe      = regexp.MustCompile(`/page/([0-9]+)/`)
	h1Re           = regexp.MustCompile(`(?is)<h1[^>]*>\s*(.*?)\s*</h1>`)
	torrentBlockRe = regexp.MustCompile(`(?i)<div id="torrent_(\d+)_info"\s+class="torrent"`)
	infoD1Re       = regexp.MustCompile(`(?is)<div class="info_d1">\s*([^<]+?)\s*</div>`)
	dateRe         = regexp.MustCompile(`\b(\d{2})-(\d{2})-(\d{4})\b`)
	sidRe          = regexp.MustCompile(`(?i)<div class="li_distribute">\s*([0-9]+)\s*</div>`)
	pirRe          = regexp.MustCompile(`(?i)<div class="li_swing">\s*([0-9]+)\s*</div>`)
	rangeRe        = regexp.MustCompile(`\b(\d{1,4})\s*-\s*(\d{1,4})\b`)
	singleNumRe    = regexp.MustCompile(`\b(\d{1,4})\b`)
	cleanSpaceRe   = regexp.MustCompile(`\s+`)
)

type Parser struct {
	Config  app.Config
	DB      *filedb.DB
	DataDir string
	CF      *core.CFClient
	mu      sync.Mutex
	working bool
}

type ParseResult struct {
	Fetched, Added, Updated, Skipped, Failed int
	Status                                   string
}

func New(cfg app.Config, db *filedb.DB, dataDir string) *Parser {
	cf, err := core.NewCFClientWithConfig(cfg.CFClient.Profile, cfg.CFClient.UserAgent)
	if err != nil {
		log.Printf("anistar: CFClient init error: %v, falling back to nil", err)
	}
	return &Parser{Config: cfg, DB: db, DataDir: dataDir, CF: cf}
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

	host := strings.TrimRight(p.Config.Anistar.Host, "/")
	if host == "" {
		return ParseResult{Status: "config missing"}, nil
	}

	res := ParseResult{Status: "ok"}
	for _, cat := range catTypes {
		lastPage := limitPage
		if lastPage <= 0 {
			lastPage = p.detectLastPage(ctx, host, cat.path)
		}
		for page := 1; page <= lastPage; page++ {
			listURL := fmt.Sprintf("%s/%s/", host, cat.path)
			if page > 1 {
				listURL = fmt.Sprintf("%s/%s/page/%d/", host, cat.path, page)
			}
			listHTML, err := p.httpGet(ctx, listURL, "")
			if err != nil || listHTML == "" {
				continue
			}
			postURLs := p.extractPostURLs(listHTML, host)
			res.Fetched += len(postURLs)

			for _, postURL := range postURLs {
				items, err := p.parseDetailPage(ctx, postURL, listURL, host, cat.types)
				if err != nil {
					continue
				}
				if len(items) == 0 {
					continue
				}
				a, u, s, f, err := p.saveTorrents(ctx, items, host)
				if err != nil {
					res.Failed += len(items)
					continue
				}
				res.Added += a
				res.Updated += u
				res.Skipped += s
				res.Failed += f

				if p.Config.Anistar.ParseDelay > 0 {
					select {
					case <-ctx.Done():
						return res, ctx.Err()
					case <-time.After(time.Duration(p.Config.Anistar.ParseDelay) * time.Millisecond):
					}
				}
			}
		}
	}
	log.Printf("anistar: done fetched=%d added=%d skipped=%d failed=%d", res.Fetched, res.Added, res.Skipped, res.Failed)
	return res, nil
}

func (p *Parser) detectLastPage(ctx context.Context, host, cat string) int {
	listHTML, err := p.httpGet(ctx, fmt.Sprintf("%s/%s/", host, cat), "")
	if err != nil || listHTML == "" {
		return 1
	}
	maxPage := 1
	for _, m := range pageNumRe.FindAllStringSubmatch(listHTML, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > maxPage {
			maxPage = n
		}
	}
	return maxPage
}

func (p *Parser) extractPostURLs(listHTML, host string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, m := range postURLAbsRe.FindAllString(listHTML, -1) {
		if _, ok := seen[m]; !ok {
			seen[m] = struct{}{}
			out = append(out, m)
		}
	}
	for _, m := range postURLRelRe.FindAllString(listHTML, -1) {
		abs := host + m
		if _, ok := seen[abs]; !ok {
			seen[abs] = struct{}{}
			out = append(out, abs)
		}
	}
	return out
}

func (p *Parser) parseDetailPage(ctx context.Context, postURL, referer, host string, types []string) ([]filedb.TorrentDetails, error) {
	postHTML, err := p.httpGet(ctx, postURL, referer)
	if err != nil || postHTML == "" {
		return nil, err
	}

	// Extract h1 for name/originalname
	h1 := ""
	if m := h1Re.FindStringSubmatch(postHTML); len(m) > 1 {
		h1 = strings.TrimSpace(html.UnescapeString(cleanSpaceRe.ReplaceAllString(m[1], " ")))
	}
	name, original := h1, ""
	if strings.Contains(h1, " / ") {
		parts := strings.SplitN(h1, " / ", 2)
		name = strings.TrimSpace(parts[0])
		original = strings.TrimSpace(parts[1])
	}
	if name == "" {
		return nil, nil
	}

	var out []filedb.TorrentDetails
	now := time.Now().UTC().Format(time.RFC3339Nano)

	// Find torrent blocks
	for _, bm := range torrentBlockRe.FindAllStringSubmatchIndex(postHTML, -1) {
		tid := postHTML[bm[2]:bm[3]]
		startIdx := bm[0]
		endIdx := startIdx + 4000
		if endIdx > len(postHTML) {
			endIdx = len(postHTML)
		}
		around := postHTML[startIdx:endIdx]

		// Episode info
		epLabel := "Серия 1"
		epNum := "1"
		if m := infoD1Re.FindStringSubmatch(around); len(m) > 1 {
			info := strings.TrimSpace(html.UnescapeString(m[1]))
			if rm := rangeRe.FindStringSubmatch(info); len(rm) > 2 {
				epLabel = fmt.Sprintf("Серии %s-%s", rm[1], rm[2])
				epNum = rm[1]
			} else if sm := singleNumRe.FindStringSubmatch(info); len(sm) > 1 {
				epNum = sm[1]
				epLabel = "Серия " + epNum
			}
		}

		// Date
		createTime := time.Now().UTC()
		relased := createTime.Year()
		if dm := dateRe.FindStringSubmatch(around); len(dm) > 3 {
			day, _ := strconv.Atoi(dm[1])
			month, _ := strconv.Atoi(dm[2])
			year, _ := strconv.Atoi(dm[3])
			if day > 0 && month > 0 && year > 0 {
				createTime = time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
				relased = year
			}
		}

		// Seeds/peers
		sid, pir := 0, 0
		if m := sidRe.FindStringSubmatch(around); len(m) > 1 {
			sid, _ = strconv.Atoi(m[1])
		}
		if m := pirRe.FindStringSubmatch(around); len(m) > 1 {
			pir, _ = strconv.Atoi(m[1])
		}

		uniqueURL := fmt.Sprintf("%s?e=%s&id=%s", postURL, epNum, tid)
		titleBase := name
		if original != "" {
			titleBase = name + " / " + original
		}
		title := titleBase + " — " + epLabel

		out = append(out, filedb.TorrentRecord{
			TrackerName: trackerName,
			Types: types,
			URL: uniqueURL,
			Title: title,
			Sid: sid,
			Pir: pir,
			CreateTime: createTime.UTC().Format(time.RFC3339Nano),
			UpdateTime: now,
			Name: name,
			OriginalName: core.FirstNonEmpty(original, name),
			Relased: relased,
			SearchName: core.SearchName(name),
			SearchOrig: core.SearchName(core.FirstNonEmpty(original, name)),
			TID: tid,
		}.ToMap())
	}
	return out, nil
}

func (p *Parser) saveTorrents(ctx context.Context, torrents []filedb.TorrentDetails, host string) (int, int, int, int, error) {
	added, updated, skipped, failed := 0, 0, 0, 0
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
		if exists && asString(existing["title"]) == asString(incoming["title"]) && strings.TrimSpace(asString(existing["magnet"])) != "" {
			skipped++
			continue
		}

		// Download torrent file
		tid := asString(incoming["_tid"])
		delete(incoming, "_tid")
		if tid != "" {
			downURL := fmt.Sprintf("%s/engine/gettorrent.php?id=%s", host, tid)
			torrentBytes, err := p.httpDownload(ctx, downURL, host)
			if err == nil && len(torrentBytes) > 0 {
				magnet := core.TorrentBytesToMagnet(torrentBytes)
				if magnet != "" {
					incoming["magnet"] = magnet
				}
			}
		}
		if strings.TrimSpace(asString(incoming["magnet"])) == "" {
			failed++
			continue
		}

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
			out[k] = v
		}
		out["_sn"] = core.SearchName(asString(out["name"]))
		out["_so"] = core.SearchName(core.FirstNonEmpty(asString(out["originalname"]), asString(out["name"])))

		bucket[urlv] = out
		changed[key] = time.Now().UTC()
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
		_ = p.DB.SaveChangesToFile()
	}
	return added, updated, skipped, failed, nil
}

func (p *Parser) httpGet(_ context.Context, rawURL, referer string) (string, error) {
	if p.CF == nil {
		return "", fmt.Errorf("anistar: CFClient not initialized")
	}
	cookie := strings.TrimSpace(p.Config.Anistar.Cookie)
	data, status, err := p.CF.Download(rawURL, cookie, referer)
	if err != nil {
		return "", err
	}
	if status == 403 {
		return "", fmt.Errorf("anistar: 403 Forbidden (cookie expired?)")
	}
	// anistar.org serves all pages as charset=windows-1251
	return core.DecodeCP1251(data), nil
}

func (p *Parser) httpDownload(_ context.Context, rawURL, referer string) ([]byte, error) {
	if p.CF == nil {
		return nil, fmt.Errorf("anistar: CFClient not initialized")
	}
	cookie := strings.TrimSpace(p.Config.Anistar.Cookie)
	data, status, err := p.CF.Download(rawURL, cookie, referer)
	if err != nil {
		return nil, err
	}
	if status == 403 {
		return nil, fmt.Errorf("anistar: 403 Forbidden (cookie expired?)")
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
