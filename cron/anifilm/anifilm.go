package anifilm

import (
	"context"
	"crypto/tls"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
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

const trackerName = "anifilm"

var catPages = []struct {
	cat      string
	types    []string
	fullMax  int
	quickMax int
}{
	{"serials", []string{"anime"}, 70, 2},
	{"ova", []string{"anime"}, 32, 2},
	{"ona", []string{"anime"}, 2, 2},
	{"movies", []string{"anime"}, 17, 2},
	{"dorams", []string{"serial"}, 10, 2},
	{"special", []string{"anime"}, 5, 2},
	{"hentai", []string{"anime"}, 5, 2},
	{"short-serials", []string{"anime"}, 5, 2},
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
	csrfInputRe = regexp.MustCompile(`(?i)<input[^>]+name="([^"]*CSRF[^"]*)"[^>]+value="([^"]+)"`)
	csrfInputRe2 = regexp.MustCompile(`(?i)<input[^>]+value="([^"]+)"[^>]+name="([^"]*CSRF[^"]*)"`)
)

type Parser struct {
	Config  app.Config
	DB      *filedb.DB
	DataDir string
	Fetcher *core.Fetcher
	Client  *http.Client
	mu      sync.Mutex
	working bool

	cookieMu         sync.Mutex
	dynCookie        string
	lastLoginAttempt time.Time
}

type ParseResult struct {
	Fetched, Added, Updated, Skipped, Failed int
	Status                                   string
}

func New(cfg app.Config, db *filedb.DB, dataDir string) *Parser {
	return &Parser{
		Config:  cfg,
		DB:      db,
		DataDir: dataDir,
		Fetcher: core.NewFetcher(cfg),
		Client: &http.Client{
			Timeout:   20 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
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

	p.ensureLogin(ctx)

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

			items, err := p.fetchPage(ctx, host, cp.cat, cp.types, page, createTime)
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
			log.Printf("anifilm: cat=%s page %d/%d fetched=%d added=%d skipped=%d failed=%d", cp.cat, page, maxPage, len(items), a, s, f)
		}
	}
	log.Printf("anifilm: done fetched=%d added=%d skipped=%d failed=%d", res.Fetched, res.Added, res.Skipped, res.Failed)
	return res, nil
}

func (p *Parser) fetchPage(ctx context.Context, host, cat string, types []string, page int, createTime time.Time) ([]filedb.TorrentDetails, error) {
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
			Types: types,
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
	plog := core.NewParserLog(trackerName, filepath.Join(p.DB.DataDir, "log"), p.Config.LogParsers && p.Config.Anifilm.Log)
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
		// Only fetch detail page if title changed or no magnet
		needMagnet := !exists || strings.TrimSpace(asString(existing["magnet"])) == ""
		if !needMagnet {
			existTitle := strings.ReplaceAll(asString(existing["title"]), " [1080p]", "")
			if existTitle != asString(incoming["title"]) {
				needMagnet = true
			}
		}
		if needMagnet {
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
			if tid == "" {
				if m := tidRe.FindStringSubmatch(detailHTML); len(m) > 1 {
					tid = m[1]
				}
			}
			if tid == "" {
				hasTorrentBlock := strings.Contains(detailHTML, "release__torrents")
				hasDownloadLink := strings.Contains(detailHTML, "download-torrent")
				hasCloudflare := strings.Contains(detailHTML, "cf-browser-verification") || strings.Contains(detailHTML, "challenge-platform")
				log.Printf("anifilm: tid not found url=%s torrentBlock=%v downloadLink=%v cloudflare=%v htmlLen=%d",
					urlv, hasTorrentBlock, hasDownloadLink, hasCloudflare, len(detailHTML))
				failed++
				continue
			}
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

func (p *Parser) cookie() string {
	p.cookieMu.Lock()
	defer p.cookieMu.Unlock()
	if p.dynCookie != "" {
		return p.dynCookie
	}
	return strings.TrimSpace(p.Config.Anifilm.Cookie)
}

func (p *Parser) takeLogin(ctx context.Context) error {
	p.cookieMu.Lock()
	if time.Since(p.lastLoginAttempt) < 2*time.Minute {
		p.cookieMu.Unlock()
		return nil
	}
	p.lastLoginAttempt = time.Now()
	p.cookieMu.Unlock()

	host := strings.TrimRight(p.Config.Anifilm.Host, "/")
	if host == "" || strings.TrimSpace(p.Config.Anifilm.Login.U) == "" {
		return fmt.Errorf("anifilm: no host or login configured")
	}
	log.Printf("anifilm: attempting login to %s as %s", host, p.Config.Anifilm.Login.U)

	// /account/login itself is not behind CF's managed challenge, so we hit it
	// directly. Triggering a flare solve on the site origin from here has two
	// problems on some hosts: (1) the root path gets a stricter interactive
	// challenge than /releases/*, which Camoufox/geckodriver can't auto-click
	// today; (2) if that solve fails, the domain-wide cooldown blocks every
	// subsequent /releases/* fetch for 3 minutes, breaking the whole parser.
	flareCookie := ""
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"

	// Step 1: GET login page to obtain CSRF token cookie
	loginPageURL := host + "/account/login"
	getReq, err := http.NewRequestWithContext(ctx, http.MethodGet, loginPageURL, nil)
	if err != nil {
		return err
	}
	getReq.Header.Set("User-Agent", ua)
	if flareCookie != "" {
		getReq.Header.Set("Cookie", flareCookie)
	}
	getResp, err := p.Client.Do(getReq)
	if err != nil {
		log.Printf("anifilm: login page error: %v", err)
		return err
	}
	pageBody, _ := io.ReadAll(io.LimitReader(getResp.Body, 512*1024))
	getResp.Body.Close()

	// Collect all cookies: flare + login page Set-Cookie
	allCookies := flareCookie
	for _, line := range getResp.Header.Values("Set-Cookie") {
		part := strings.TrimSpace(strings.SplitN(line, ";", 2)[0])
		if part != "" {
			allCookies = core.MergeCookieStrings(allCookies, part)
		}
	}

	// Extract CSRF token from HTML: <input name="*CSRF*" value="...">
	csrfName, csrfToken := "", ""
	pageHTML := string(pageBody)
	if m := csrfInputRe.FindStringSubmatch(pageHTML); len(m) > 2 {
		csrfName = m[1]
		csrfToken = html.UnescapeString(m[2])
	} else if m := csrfInputRe2.FindStringSubmatch(pageHTML); len(m) > 2 {
		csrfName = m[2]
		csrfToken = html.UnescapeString(m[1])
	}
	if csrfToken != "" {
		log.Printf("anifilm: found CSRF field %s", csrfName)
	} else {
		log.Printf("anifilm: CSRF token not found in login page")
		return fmt.Errorf("anifilm: CSRF token not found")
	}
	log.Printf("anifilm: login page status=%d", getResp.StatusCode)

	// Step 2: POST login as form (Yii expects form POST, not JSON)
	form := url.Values{}
	form.Set(csrfName, csrfToken)
	form.Set("LoginForm[username]", p.Config.Anifilm.Login.U)
	form.Set("LoginForm[password]", p.Config.Anifilm.Login.P)
	form.Set("LoginForm[pass]", "") // honeypot field, must be empty

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/account/login", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Referer", loginPageURL)
	if allCookies != "" {
		req.Header.Set("Cookie", allCookies)
	}

	resp, err := p.Client.Do(req)
	if err != nil {
		log.Printf("anifilm: login error: %v", err)
		return err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	log.Printf("anifilm: login response status=%d body=%s", resp.StatusCode, string(respBody))

	if resp.StatusCode != 302 && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		return fmt.Errorf("anifilm: login failed status=%d", resp.StatusCode)
	}

	// Collect login cookies and merge with all prior cookies
	finalCookies := allCookies
	var loginCount int
	for _, line := range resp.Header.Values("Set-Cookie") {
		part := strings.TrimSpace(strings.SplitN(line, ";", 2)[0])
		if part != "" {
			finalCookies = core.MergeCookieStrings(finalCookies, part)
			loginCount++
		}
	}
	if loginCount > 0 || finalCookies != "" {
		p.cookieMu.Lock()
		p.dynCookie = finalCookies
		p.cookieMu.Unlock()
		log.Printf("anifilm: login OK, new cookies=%d", loginCount)
		return nil
	}
	log.Printf("anifilm: login FAILED — no cookies in response")
	return fmt.Errorf("anifilm: login failed")
}

func (p *Parser) ensureLogin(ctx context.Context) {
	if p.cookie() != "" {
		return
	}
	if strings.TrimSpace(p.Config.Anifilm.Login.U) == "" {
		return
	}
	_ = p.takeLogin(ctx)
}

func (p *Parser) trackerSettings() app.TrackerSettings {
	ts := p.Config.Anifilm
	if c := p.cookie(); c != "" {
		ts.Cookie = c
	}
	return ts
}

func (p *Parser) httpGet(_ context.Context, rawURL, referer string) (string, error) {
	if p.Fetcher == nil {
		return "", fmt.Errorf("anifilm: Fetcher not initialized")
	}
	body, status, err := p.Fetcher.GetString(rawURL, p.trackerSettings())
	if err != nil {
		return "", err
	}
	if status == 403 {
		return "", fmt.Errorf("anifilm: 403 Forbidden (cookie expired?)")
	}
	return body, nil
}

func (p *Parser) httpDownload(_ context.Context, rawURL, referer string) ([]byte, error) {
	if p.Fetcher == nil {
		return nil, fmt.Errorf("anifilm: Fetcher not initialized")
	}
	data, status, err := p.Fetcher.Download(rawURL, p.trackerSettings())
	if err != nil {
		return nil, err
	}
	if status == 403 {
		return nil, fmt.Errorf("anifilm: 403 Forbidden (cookie expired?)")
	}
	return data, nil
}

func extractCSRFCookie(cookieStr string) (name, value string) {
	for _, part := range strings.Split(cookieStr, ";") {
		part = strings.TrimSpace(part)
		if eq := strings.IndexByte(part, '='); eq > 0 {
			k := strings.TrimSpace(part[:eq])
			if strings.Contains(strings.ToUpper(k), "CSRF") {
				return k, strings.TrimSpace(part[eq+1:])
			}
		}
	}
	return "", ""
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
