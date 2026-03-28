package anifilm

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

const trackerName = "anifilm"

var catPages = []struct {
	cat      string
	fullMax  int
	quickMax int
}{
	{"serials", 70, 2},
	{"ova", 32, 2},
	{"ona", 2, 2},
	{"movies", 17, 2},
}

var (
	itemSplitRe  = regexp.MustCompile(`(?i)class="releases__item`)
	urlRe        = regexp.MustCompile(`(?i)<a[^>]+href="/(releases/[^"]+)"`)
	nameRuRe     = regexp.MustCompile(`(?i)class="releases__title-russian"[^>]*>([^<]+)</a>`)
	nameOrigRe   = regexp.MustCompile(`(?i)class="releases__title-original"[^>]*>([^<]+)</span>`)
	episodesRe   = regexp.MustCompile(`(?i)([0-9]+(-[0-9]+)?)\s*из\s*[0-9]+\s*эп`)
	yearRe       = regexp.MustCompile(`(?i)href="/releases/[^"]*">([0-9]{4})</a>`)
	yearAltRe    = regexp.MustCompile(`(?i)table-list__value[^>]*>[^<]*(\d{4})`)
	tidRe        = regexp.MustCompile(`(?i)href="/(releases/download-torrent/[0-9]+)"[^>]*>скачать</a>`)
	cleanSpaceRe = regexp.MustCompile(`[\n\r\t ]+`)
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
		log.Printf("anifilm: CFClient init error: %v", err)
	}
	return &Parser{Config: cfg, DB: db, DataDir: dataDir, CF: cf}
}

func (p *Parser) Parse(ctx context.Context, fullparse bool) (ParseResult, error) {
	p.mu.Lock()
	if p.working {
		p.mu.Unlock()
		return ParseResult{Status: "work"}, nil
	}
	p.working = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.working = false; p.mu.Unlock() }()

	host := strings.TrimRight(p.Config.Anifilm.Host, "/")
	if host == "" {
		return ParseResult{Status: "config missing"}, nil
	}

	res := ParseResult{Status: "ok"}

	for _, cp := range catPages {
		maxPage := cp.quickMax
		if fullparse {
			maxPage = cp.fullMax
		}
		for page := 1; page <= maxPage; page++ {
			if page > 1 && p.Config.Anifilm.ParseDelay > 0 {
				select {
				case <-ctx.Done():
					return res, ctx.Err()
				case <-time.After(time.Duration(p.Config.Anifilm.ParseDelay) * time.Millisecond):
				}
			}

			createTime := time.Now().UTC()
			if fullparse {
				createTime = time.Now().UTC().AddDate(0, 0, -(2 * page))
			}

			items, err := p.fetchPage(ctx, host, cp.cat, page, createTime)
			if err != nil {
				continue
			}
			res.Fetched += len(items)
			if len(items) == 0 {
				continue
			}

			a, u, s, f, err := p.saveTorrents(ctx, host, items)
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
	log.Printf("anifilm: done fetched=%d added=%d skipped=%d failed=%d", res.Fetched, res.Added, res.Skipped, res.Failed)
	return res, nil
}

func (p *Parser) fetchPage(ctx context.Context, host, cat string, page int, createTime time.Time) ([]filedb.TorrentDetails, error) {
	pageURL := fmt.Sprintf("%s/releases/page/%d?category=%s", host, page, cat)
	body, err := p.httpGet(ctx, pageURL, "")
	if err != nil {
		return nil, err
	}
	if !strings.Contains(body, `AniFilm`) {
		return nil, nil
	}

	chunks := itemSplitRe.Split(body, -1)
	if len(chunks) < 2 {
		return nil, nil
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	var out []filedb.TorrentDetails

	for _, row := range chunks[1:] {
		if strings.TrimSpace(row) == "" {
			continue
		}

		extract := func(re *regexp.Regexp) string {
			m := re.FindStringSubmatch(row)
			if len(m) < 2 {
				return ""
			}
			s := html.UnescapeString(strings.TrimSpace(m[1]))
			s = cleanSpaceRe.ReplaceAllString(s, " ")
			return strings.TrimSpace(s)
		}

		urlPath := extract(urlRe)
		name := extract(nameRuRe)
		originalname := extract(nameOrigRe)
		episodes := extract(episodesRe)

		if urlPath == "" || name == "" {
			continue
		}
		if originalname == "" {
			originalname = name
		}

		fullURL := host + "/" + strings.TrimLeft(urlPath, "/")
		title := name
		if originalname != name {
			title = name + " / " + originalname
		}
		if episodes != "" {
			title += " (" + episodes + ")"
		}

		// Strip parenthetical from name
		if idx := strings.Index(name, "("); idx > 0 {
			name = strings.TrimSpace(name[:idx])
		}

		// Year - try main pattern, then alternate
		yearStr := extract(yearRe)
		if yearStr == "" {
			yearStr = extract(yearAltRe)
		}
		relased, _ := strconv.Atoi(yearStr)

		out = append(out, filedb.TorrentRecord{
			TrackerName: trackerName,
			Types: []string{"anime"},
			URL: fullURL,
			Title: title,
			Sid: 1,
			Pir: 0,
			CreateTime: createTime.UTC().Format(time.RFC3339Nano),
			UpdateTime: now,
			Name: name,
			OriginalName: originalname,
			Relased: relased,
			SearchName: core.SearchName(name),
			SearchOrig: core.SearchName(core.FirstNonEmpty(originalname, name)),
		}.ToMap())
	}
	return out, nil
}

func (p *Parser) saveTorrents(ctx context.Context, host string, torrents []filedb.TorrentDetails) (int, int, int, int, error) {
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
		// Skip if title matches (ignoring [1080p] suffix which we may add)
		if exists {
			existTitle := strings.ReplaceAll(asString(existing["title"]), " [1080p]", "")
			incomTitle := asString(incoming["title"])
			if existTitle == incomTitle && strings.TrimSpace(asString(existing["magnet"])) != "" {
				skipped++
				continue
			}
		}

		// Fetch detail page to find torrent download link
		detailHTML, err := p.httpGet(ctx, urlv, "")
		if err != nil || detailHTML == "" {
			if err != nil {
				log.Printf("anifilm: detail page %s error: %v", urlv, err)
			} else {
				log.Printf("anifilm: detail page %s empty response", urlv)
			}
			failed++
			continue
		}

		// Prefer 1080p torrent
		title := asString(incoming["title"])
		var tid string
		torrentBlocks := strings.Split(detailHTML, `<li class="release__torrents-item">`)
		for _, block := range torrentBlocks {
			if strings.Contains(block, "1080p") && strings.Contains(block, `href="/releases/download-torrent/`) {
				if m := tidRe.FindStringSubmatch(block); len(m) > 1 {
					tid = m[1]
					if !strings.Contains(title, " [1080p]") {
						title += " [1080p]"
					}
					break
				}
			}
		}
		// Fallback: any torrent link
		if tid == "" {
			if m := tidRe.FindStringSubmatch(detailHTML); len(m) > 1 {
				tid = m[1]
			}
		}
		if tid == "" {
			// Log first few failures
			hasTorrentBlock := strings.Contains(detailHTML, "release__torrents")
			hasDownloadLink := strings.Contains(detailHTML, "download-torrent")
			hasCloudflare := strings.Contains(detailHTML, "cf-browser-verification") || strings.Contains(detailHTML, "challenge-platform")
			log.Printf("anifilm: tid not found url=%s torrentBlock=%v downloadLink=%v cloudflare=%v htmlLen=%d",
				urlv, hasTorrentBlock, hasDownloadLink, hasCloudflare, len(detailHTML))
			failed++
			continue
		}

		// Download .torrent → magnet
		torrentBytes, err := p.httpDownload(ctx, host+"/"+tid, urlv)
		if err != nil || len(torrentBytes) == 0 {
			log.Printf("anifilm: torrent download failed tid=%s err=%v size=%d", tid, err, len(torrentBytes))
			failed++
			continue
		}
		magnet, magnetErr := core.TorrentBytesToMagnetErr(torrentBytes)
		if magnet == "" {
			prefix := ""
			if len(torrentBytes) > 0 {
				end := 80
				if end > len(torrentBytes) {
					end = len(torrentBytes)
				}
				prefix = string(torrentBytes[:end])
			}
			log.Printf("anifilm: magnet extraction failed tid=%s torrentSize=%d err=%v prefix=%q", tid, len(torrentBytes), magnetErr, prefix)
			failed++
			continue
		}

		incoming["title"] = title
		incoming["magnet"] = magnet

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
		out["_sn"] = core.SearchName(asString(out["name"]))
		out["_so"] = core.SearchName(core.FirstNonEmpty(asString(out["originalname"]), asString(out["name"])))

		bucket[urlv] = out
		changed[key] = time.Now().UTC()
		if exists {
			updated++
		} else {
			added++
		}

		// Delay between detail page fetches
		if p.Config.Anifilm.ParseDelay > 0 {
			select {
			case <-ctx.Done():
				return added, updated, skipped, failed, ctx.Err()
			case <-time.After(time.Duration(p.Config.Anifilm.ParseDelay) * time.Millisecond):
			}
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
		return "", fmt.Errorf("anifilm: CFClient not initialized")
	}
	cookie := strings.TrimSpace(p.Config.Anifilm.Cookie)
	body, status, err := p.CF.Get(rawURL, cookie, referer)
	if err != nil {
		return "", err
	}
	if status == 403 {
		return "", fmt.Errorf("anifilm: 403 Forbidden (cookie expired?)")
	}
	return body, nil
}

func (p *Parser) httpDownload(_ context.Context, rawURL, referer string) ([]byte, error) {
	if p.CF == nil {
		return nil, fmt.Errorf("anifilm: CFClient not initialized")
	}
	cookie := strings.TrimSpace(p.Config.Anifilm.Cookie)
	data, status, err := p.CF.Download(rawURL, cookie, referer)
	if err != nil {
		return nil, err
	}
	if status == 403 {
		return nil, fmt.Errorf("anifilm: 403 Forbidden (cookie expired?)")
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
