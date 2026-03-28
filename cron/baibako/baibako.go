package baibako

import (
	"context"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"jacred/app"
	"jacred/core"
	"jacred/filedb"
)

const trackerName = "baibako"

var (
	rowSplitRe    = regexp.MustCompile(`(?i)<tr`)
	cleanSpaceRe  = regexp.MustCompile(`[\n\r\t\x{00A0} ]+`)
	detailsURLRe  = regexp.MustCompile(`(?i)<a href="/?(details\.php\?id=[0-9]+)[^"]+">([^<]+)</a>`)
	downloadRe    = regexp.MustCompile(`(?i)href="/?(download\.php\?id=([0-9]+))"`)
	createTimeRe  = regexp.MustCompile(`(?i)<small>Загружена: ([0-9]+ [^ ]+ [0-9]{4}) в [^<]+</small>`)
	nameOrigRe    = regexp.MustCompile(`([^/\(]+)[^/]+/([^/\(]+)`)
	firstPartRe   = regexp.MustCompile(`(\[|/|\(|\|)`)
	sessidRe      = regexp.MustCompile(`PHPSESSID=([^;]+)`)
	passRe        = regexp.MustCompile(`pass=([^;]+)`)
	uidRe         = regexp.MustCompile(`uid=([^;]+)`)
)

type Parser struct {
	Config  app.Config
	DB      *filedb.DB
	DataDir string
	Client  *http.Client
	mu      sync.Mutex
	working bool
	cookie  string
	cookieT time.Time
}

type ParseResult struct {
	Fetched, Added, Updated, Skipped, Failed int
	Status                                   string
	PerCategory                              map[string]int
}

func New(cfg app.Config, db *filedb.DB, dataDir string) *Parser {
	return &Parser{Config: cfg, DB: db, DataDir: dataDir, Client: &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func (p *Parser) getCookie() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cookie != "" && time.Since(p.cookieT) < 24*time.Hour {
		return p.cookie
	}
	return ""
}

func (p *Parser) setCookie(c string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cookie = c
	p.cookieT = time.Now()
}

func (p *Parser) takeLogin(ctx context.Context) bool {
	host := strings.TrimRight(p.Config.Baibako.Host, "/")
	if host == "" || p.Config.Baibako.Login.U == "" {
		return false
	}
	form := url.Values{
		"username": {p.Config.Baibako.Login.U},
		"password": {p.Config.Baibako.Login.P},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/takelogin.php", strings.NewReader(form.Encode()))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := p.Client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	var sessid, pass, uid string
	for _, line := range resp.Header.Values("Set-Cookie") {
		if m := sessidRe.FindStringSubmatch(line); len(m) > 1 {
			sessid = m[1]
		}
		if m := passRe.FindStringSubmatch(line); len(m) > 1 {
			pass = m[1]
		}
		if m := uidRe.FindStringSubmatch(line); len(m) > 1 {
			uid = m[1]
		}
	}
	if sessid != "" && uid != "" && pass != "" {
		p.setCookie(fmt.Sprintf("PHPSESSID=%s; uid=%s; pass=%s", sessid, uid, pass))
		return true
	}
	return false
}

func (p *Parser) Parse(ctx context.Context, maxpage int) (ParseResult, error) {
	p.mu.Lock()
	if p.working {
		p.mu.Unlock()
		return ParseResult{Status: "work"}, nil
	}
	p.working = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.working = false; p.mu.Unlock() }()

	host := strings.TrimRight(p.Config.Baibako.Host, "/")
	if host == "" {
		return ParseResult{Status: "config missing"}, nil
	}

	if p.getCookie() == "" {
		if !p.takeLogin(ctx) {
			return ParseResult{Status: "login failed"}, nil
		}
	}
	if maxpage <= 0 {
		maxpage = 10
	}

	res := ParseResult{Status: "ok", PerCategory: map[string]int{}}
	for page := 0; page <= maxpage; page++ {
		if page > 0 && p.Config.Baibako.ParseDelay > 0 {
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-time.After(time.Duration(p.Config.Baibako.ParseDelay) * time.Millisecond):
			}
		}
		items, err := p.fetchPage(ctx, host, page)
		if err != nil {
			return res, err
		}
		res.Fetched += len(items)
		if len(items) == 0 {
			break // no more pages
		}
		added, updated, skipped, failed, err := p.saveTorrents(ctx, host, items)
		if err != nil {
			return res, err
		}
		res.Added += added
		res.Updated += updated
		res.Skipped += skipped
		res.Failed += failed
		log.Printf("baibako: page %d/%d fetched=%d added=%d skipped=%d failed=%d", page+1, maxpage, len(items), added, skipped, failed)
	}
	log.Printf("baibako: done fetched=%d added=%d skipped=%d failed=%d", res.Fetched, res.Added, res.Skipped, res.Failed)
	return res, nil
}

func (p *Parser) fetchPage(ctx context.Context, host string, page int) ([]filedb.TorrentDetails, error) {
	pageURL := fmt.Sprintf("%s/browse.php?page=%d", host, page)
	body, err := p.httpGet(ctx, pageURL)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(body, `id="navtop"`) {
		return nil, nil
	}
	decoded := html.UnescapeString(strings.ReplaceAll(body, "&nbsp;", ""))
	rows := rowSplitRe.Split(decoded, -1)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var out []filedb.TorrentDetails

	for _, row := range rows {
		if strings.TrimSpace(row) == "" {
			continue
		}
		// Дата
		createTimeStr := ""
		if m := createTimeRe.FindStringSubmatch(row); len(m) > 1 {
			createTimeStr = m[1]
		}
		ct := parseCreateTime(createTimeStr, "02.01.2006")
		if ct.IsZero() {
			ct = time.Now().UTC()
		}

		// URL + title
		gurl := detailsURLRe.FindStringSubmatch(row)
		if len(gurl) < 3 {
			continue
		}
		urlPath := gurl[1]
		title := strings.TrimSpace(gurl[2])
		title = strings.ReplaceAll(title, "(Обновляемая)", "")
		title = strings.ReplaceAll(title, "(Золото)", "")
		title = strings.ReplaceAll(title, "(Оновлюється)", "")
		title = strings.TrimRight(strings.TrimSpace(title), "/ ")

		if urlPath == "" || title == "" {
			continue
		}
		fullURL := host + "/" + strings.TrimLeft(urlPath, "/")

		// name / originalname
		var name, original string
		if m := nameOrigRe.FindStringSubmatch(title); len(m) >= 3 && strings.TrimSpace(m[1]) != "" && strings.TrimSpace(m[2]) != "" {
			name = strings.TrimSpace(m[1])
			original = strings.TrimSpace(m[2])
		}
		if name == "" {
			parts := firstPartRe.Split(title, 2)
			if len(parts) > 0 {
				name = strings.TrimSpace(parts[0])
			}
		}
		if name == "" {
			continue
		}

		// download link
		dm := downloadRe.FindStringSubmatch(row)
		if len(dm) < 2 {
			continue
		}
		downloadURI := host + "/" + strings.TrimLeft(dm[1], "/")

		out = append(out, filedb.TorrentRecord{
			TrackerName: trackerName,
			Types: []string{"serial"},
			URL: fullURL,
			Title: title,
			Sid: 1,
			Pir: 0,
			CreateTime: ct.UTC().Format(time.RFC3339Nano),
			UpdateTime: now,
			Name: name,
			OriginalName: core.FirstNonEmpty(original, name),
			SearchName: core.SearchName(name),
			SearchOrig: core.SearchName(core.FirstNonEmpty(original, name)),
			DownloadURI: downloadURI,
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
		if exists && asString(existing["title"]) == asString(incoming["title"]) && strings.TrimSpace(asString(existing["magnet"])) != "" {
			skipped++
			continue
		}

		// Download .torrent file to extract magnet
		downloadURI := asString(incoming["_downloadURI"])
		delete(incoming, "_downloadURI")
		if downloadURI != "" {
			torrentBytes, err := p.httpDownload(ctx, downloadURI, host+"/browse.php")
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

		bucket[urlv] = mergeTorrent(existing, exists, incoming)
		changed[key] = fileTime(bucket[urlv])
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

func (p *Parser) httpGet(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	if c := p.getCookie(); c != "" {
		req.Header.Set("Cookie", c)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	// Baibako uses CP1251
	return core.DecodeCP1251(b), nil
}

func (p *Parser) httpDownload(ctx context.Context, rawURL, referer string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	if c := p.getCookie(); c != "" {
		req.Header.Set("Cookie", c)
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func parseCreateTime(raw, layout string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	repl := strings.NewReplacer(
		"Янв", "01", "Фев", "02", "Мар", "03", "Апр", "04", "Май", "05", "Июн", "06",
		"Июл", "07", "Авг", "08", "Сен", "09", "Окт", "10", "Ноя", "11", "Дек", "12",
		"янв", "01", "фев", "02", "мар", "03", "апр", "04", "май", "05", "июн", "06",
		"июл", "07", "авг", "08", "сен", "09", "окт", "10", "ноя", "11", "дек", "12",
	)
	raw = repl.Replace(raw)
	// Try parse "DD MM YYYY"
	t, err := time.Parse("02 01 2006", raw)
	if err != nil {
		t, err = time.Parse(layout, raw)
		if err != nil {
			return time.Time{}
		}
	}
	return t
}

func mergeTorrent(existing filedb.TorrentDetails, exists bool, incoming filedb.TorrentDetails) filedb.TorrentDetails {
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
	if fileTime(out).IsZero() {
		out["updateTime"] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	return out
}

func fileTime(t filedb.TorrentDetails) time.Time {
	for _, key := range []string{"updateTime", "createTime"} {
		s := strings.TrimSpace(asString(t[key]))
		if s == "" {
			continue
		}
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05Z07:00", time.RFC3339} {
			if tm, err := time.Parse(layout, s); err == nil {
				return tm.UTC()
			}
		}
	}
	return time.Now().UTC()
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
