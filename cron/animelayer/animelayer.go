package animelayer

import (
	"context"
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

	"jacred/app"
	"jacred/core"
	"jacred/filedb"
)

const trackerName = "animelayer"
const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"

var (
	rowSplitRe       = regexp.MustCompile(`class="torrent-item torrent-item-medium panel"`)
	titleURLRe       = regexp.MustCompile(`<a href="/(torrent/[a-z0-9]+)/?">([^<]+)</a>`)
	sidRe            = regexp.MustCompile(`class="icon s-icons-upload"></i>([0-9]+)`)
	pirRe            = regexp.MustCompile(`class="icon s-icons-download"></i>([0-9]+)`)
	resolution1080Re = regexp.MustCompile(`Разрешение: ?</strong>1920x1080`)
	resolution720Re  = regexp.MustCompile(`Разрешение: ?</strong>1280x720`)
	yearRe           = regexp.MustCompile(`Год выхода: ?</strong>([0-9]{4})`)
	nameYearSlashRe  = regexp.MustCompile(`([^/\[\(]+)\([0-9]{4}\)[^/]+/([^/\[\(]+)`)
	nameSlashRe      = regexp.MustCompile(`^([^/\[\(]+)/([^/\[\(]+)`)
	createFullRe     = regexp.MustCompile(`>(?:Добавл|Обновл)[^<]+</span>([0-9]+ [^ ]+ [0-9]{4})`)
	createShortRe    = regexp.MustCompile(`(?:Добавл|Обновл)[^<]+</span>([^\n<]+) в`)
	layerHashRe      = regexp.MustCompile(`layer_hash=([^;]+)(;|$)`)
	layerIDRe        = regexp.MustCompile(`layer_id=([^;]+)(;|$)`)
	phpSessRe        = regexp.MustCompile(`PHPSESSID=([^;]+)(;|$)`)
	wsRe             = regexp.MustCompile(`[\n\r\t\x{00A0} ]+`)
)

type ParseResult struct {
	Status  string `json:"status"`
	Parsed  int    `json:"parsed"`
	Added   int    `json:"added"`
	Updated int    `json:"updated"`
	Skipped int    `json:"skipped"`
	Failed  int    `json:"failed"`
}

type Parser struct {
	Config app.Config
	DB     *filedb.DB
	Client *http.Client

	mu               sync.Mutex
	working          bool
	cookieMu         sync.Mutex
	cookie           string
	lastLoginAttempt time.Time
}

func New(cfg app.Config, db *filedb.DB) *Parser {
	return &Parser{Config: cfg, DB: db, Client: &http.Client{Timeout: 30 * time.Second}}
}

func (p *Parser) Parse(ctx context.Context, maxpage int) (ParseResult, error) {
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
	if maxpage <= 0 {
		maxpage = 1
	}
	res := ParseResult{Status: "ok"}
	for page := 1; page <= maxpage; page++ {
		parsed, added, updated, skipped, failed, err := p.parsePage(ctx, page)
		res.Parsed += parsed
		res.Added += added
		res.Updated += updated
		res.Skipped += skipped
		res.Failed += failed
		if err != nil {
			log.Printf("animelayer: page %d/%d error: %v", page, maxpage, err)
			res.Status = "error"
			return res, err
		}
		log.Printf("animelayer: page %d/%d parsed=%d added=%d skipped=%d failed=%d", page, maxpage, parsed, added, skipped, failed)
		if parsed == 0 {
			break // no more results
		}
		if page < maxpage && p.Config.Animelayer.ParseDelay > 0 {
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-time.After(time.Duration(p.Config.Animelayer.ParseDelay) * time.Millisecond):
			}
		}
	}
	return res, nil
}

func (p *Parser) parsePage(ctx context.Context, page int) (int, int, int, int, int, error) {
	cookie, err := p.ensureCookie(ctx)
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}
	baseHost := ensureHTTPS(firstNonEmpty(strings.TrimSpace(p.Config.Animelayer.Alias), strings.TrimSpace(p.Config.Animelayer.Host)))
	if baseHost == "" {
		return 0, 0, 0, 0, 0, nil
	}
	rawURL := baseHost + "/torrents/anime/"
	if page > 1 {
		rawURL = fmt.Sprintf("%s/torrents/anime/?page=%d", baseHost, page)
	}
	body, err := p.fetchHTML(ctx, rawURL, cookie)
	if err != nil {
		log.Printf("animelayer: fetchHTML error page=%d url=%s err=%v", page, rawURL, err)
		return 0, 0, 0, 0, 0, err
	}
	if body == "" || !strings.Contains(body, `id="wrapper"`) {
		log.Printf("animelayer: page %d empty or no wrapper, url=%s bodyLen=%d", page, rawURL, len(body))
		return 0, 0, 0, 0, 0, nil
	}

	rows := rowSplitRe.Split(html.UnescapeString(strings.ReplaceAll(body, "&nbsp;", "")), -1)
	torrents := make([]filedb.TorrentDetails, 0, len(rows))
	for _, row := range rows[1:] {
		row = replaceBadNames(row)
		if strings.TrimSpace(row) == "" {
			continue
		}
		createTime := parseCreateTime(row, page)
		if createTime.IsZero() {
			continue
		}
		m := titleURLRe.FindStringSubmatch(row)
		if len(m) < 3 {
			continue
		}
		urlPath := strings.TrimSpace(m[1])
		title := cleanText(m[2])
		if urlPath == "" || title == "" {
			continue
		}
		if resolution1080Re.MatchString(row) {
			title += " [1080p]"
		} else if resolution720Re.MatchString(row) {
			title += " [720p]"
		}
		relased, _ := strconv.Atoi(matchFirst(yearRe, row))
		if relased == 0 {
			continue
		}
		name, original := parseNames(title)
		if strings.TrimSpace(name) == "" {
			name = fallbackName(title)
		}
		if strings.TrimSpace(name) == "" {
			continue
		}
		sid, _ := strconv.Atoi(matchFirst(sidRe, row))
		pir, _ := strconv.Atoi(matchFirst(pirRe, row))
		fullURL := strings.TrimRight(baseHost, "/") + "/" + strings.Trim(urlPath, "/") + "/"
		torrents = append(torrents, filedb.TorrentDetails{
			"trackerName":  trackerName,
			"types":        []string{"anime"},
			"url":          fullURL,
			"title":        title,
			"sid":          sid,
			"pir":          pir,
			"createTime":   createTime.UTC().Format(time.RFC3339Nano),
			"updateTime":   time.Now().UTC().Format(time.RFC3339Nano),
			"name":         name,
			"originalname": original,
			"relased":      relased,
			"_sn":          core.SearchName(name),
			"_so":          core.SearchName(firstNonEmpty(original, name)),
		})
	}
	return p.saveTorrents(ctx, cookie, torrents)
}

func (p *Parser) saveTorrents(ctx context.Context, cookie string, torrents []filedb.TorrentDetails) (int, int, int, int, int, error) {
	parsedCount := len(torrents)
	addedCount, updatedCount, skippedCount, failedCount := 0, 0, 0, 0
	bucketCache := map[string]map[string]filedb.TorrentDetails{}
	changed := map[string]time.Time{}

	for _, t := range torrents {
		key := p.DB.KeyDb(asString(t["name"]), asString(t["originalname"]))
		if strings.TrimSpace(key) == "" || key == ":" {
			skippedCount++
			continue
		}
		bucket, ok := bucketCache[key]
		if !ok {
			loaded, err := p.DB.OpenReadOrEmpty(key)
			if err != nil {
				return parsedCount, addedCount, updatedCount, skippedCount, failedCount, err
			}
			bucket = loaded
			bucketCache[key] = bucket
		}
		urlv := asString(t["url"])
		existing, exists := bucket[urlv]
		if exists && asString(existing["title"]) == asString(t["title"]) {
			skippedCount++
			continue
		}

		torrentBytes, err := p.downloadTorrent(ctx, urlv+"download/", cookie)
		if err != nil || len(torrentBytes) == 0 {
			failedCount++
			continue
		}
		magnet := core.TorrentBytesToMagnet(torrentBytes)
		sizeName := torrentBytesToSizeName(torrentBytes)
		if strings.TrimSpace(magnet) == "" || strings.TrimSpace(sizeName) == "" {
			failedCount++
			continue
		}
		t["magnet"] = magnet
		t["sizeName"] = sizeName
		merged := mergeTorrent(existing, exists, t)
		bucket[urlv] = merged
		changed[key] = fileTime(merged)
		if exists {
			updatedCount++
		} else {
			addedCount++
		}
	}
	for key, tm := range changed {
		if err := p.DB.SaveBucket(key, bucketCache[key], tm); err != nil {
			return parsedCount, addedCount, updatedCount, skippedCount, failedCount, err
		}
	}
	if len(changed) > 0 {
		if err := p.DB.SaveChangesToFile(); err != nil {
			return parsedCount, addedCount, updatedCount, skippedCount, failedCount, err
		}
	}
	return parsedCount, addedCount, updatedCount, skippedCount, failedCount, nil
}

func (p *Parser) ensureCookie(ctx context.Context) (string, error) {
	if cfg := strings.TrimSpace(p.Config.Animelayer.Cookie); cfg != "" {
		return cfg, nil
	}
	p.cookieMu.Lock()
	if strings.TrimSpace(p.cookie) != "" {
		c := p.cookie
		p.cookieMu.Unlock()
		return c, nil
	}
	if time.Since(p.lastLoginAttempt) < time.Minute {
		p.cookieMu.Unlock()
		return "", nil
	}
	p.lastLoginAttempt = time.Now()
	p.cookieMu.Unlock()
	cookie, err := p.takeLogin(ctx)
	if err != nil {
		return "", err
	}
	p.cookieMu.Lock()
	p.cookie = cookie
	p.cookieMu.Unlock()
	return cookie, nil
}

func (p *Parser) takeLogin(ctx context.Context) (string, error) {
	host := ensureHTTPS(strings.TrimSpace(p.Config.Animelayer.Host))
	if host == "" || strings.TrimSpace(p.Config.Animelayer.Login.U) == "" || strings.TrimSpace(p.Config.Animelayer.Login.P) == "" {
		return "", nil
	}
	vals := url.Values{}
	vals.Set("login", p.Config.Animelayer.Login.U)
	vals.Set("password", p.Config.Animelayer.Login.P)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(host, "/")+"/auth/login/", strings.NewReader(vals.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var layerHash, layerID, phpsess string
	for _, line := range resp.Header.Values("Set-Cookie") {
		for _, part := range strings.Split(line, ", ") {
			if layerHash == "" {
				layerHash = matchFirst(layerHashRe, part)
			}
			if layerID == "" {
				layerID = matchFirst(layerIDRe, part)
			}
			if phpsess == "" {
				phpsess = matchFirst(phpSessRe, part)
			}
		}
	}
	if layerHash == "" || layerID == "" {
		return "", nil
	}
	cookie := fmt.Sprintf("layer_hash=%s;layer_id=%s", layerHash, layerID)
	if phpsess != "" {
		cookie += ";PHPSESSID=" + phpsess
	}
	return cookie, nil
}

func (p *Parser) fetchHTML(ctx context.Context, rawURL, cookie string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")
	if strings.TrimSpace(cookie) != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (p *Parser) downloadTorrent(ctx context.Context, rawURL, cookie string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")
	if strings.TrimSpace(cookie) != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil
	}
	return io.ReadAll(resp.Body)
}

func ensureHTTPS(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(host), "http://") {
		return "https://" + host[7:]
	}
	if !strings.HasPrefix(strings.ToLower(host), "https://") {
		return "https://" + host
	}
	return strings.TrimRight(host, "/")
}

func parseCreateTime(row string, page int) time.Time {
	if v := matchFirst(createFullRe, row); v != "" {
		if tm := parseRussianDate(v); !tm.IsZero() {
			return tm
		}
	}
	if v := matchFirst(createShortRe, row); v != "" {
		if tm := parseRussianDate(v + " " + strconv.Itoa(time.Now().Year())); !tm.IsZero() {
			return tm
		}
	}
	if page == 1 {
		return time.Now().UTC()
	}
	return time.Time{}
}

func parseRussianDate(s string) time.Time {
	s = strings.TrimSpace(strings.ToLower(s))
	repl := map[string]string{
		"января": "01", "февраля": "02", "марта": "03", "апреля": "04", "мая": "05", "июня": "06",
		"июля": "07", "августа": "08", "сентября": "09", "октября": "10", "ноября": "11", "декабря": "12",
	}
	for k, v := range repl {
		s = strings.ReplaceAll(s, k, v)
	}
	s = wsRe.ReplaceAllString(s, " ")
	for _, layout := range []string{"2 01 2006", "02 01 2006", "2.01.2006", "02.01.2006"} {
		if tm, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return tm.UTC()
		}
	}
	return time.Time{}
}

func parseNames(title string) (string, string) {
	if m := nameYearSlashRe.FindStringSubmatch(title); len(m) > 2 {
		return cleanText(m[2]), cleanText(m[1])
	}
	if m := nameSlashRe.FindStringSubmatch(title); len(m) > 2 {
		return cleanText(m[2]), cleanText(m[1])
	}
	return "", ""
}

func fallbackName(title string) string {
	parts := regexp.MustCompile(`(\[|\/|\(|\|)`).Split(title, 2)
	if len(parts) == 0 {
		return ""
	}
	return cleanText(parts[0])
}

func cleanText(s string) string {
	s = html.UnescapeString(strings.TrimSpace(s))
	s = wsRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func replaceBadNames(s string) string {
	s = strings.ReplaceAll(s, "\u00a0", " ")
	return wsRe.ReplaceAllString(s, " ")
}

func matchFirst(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return cleanText(m[1])
}

func mergeTorrent(existing filedb.TorrentDetails, exists bool, incoming filedb.TorrentDetails) filedb.TorrentDetails {
	out := filedb.TorrentDetails{}
	if exists {
		for k, v := range existing {
			out[k] = v
		}
	}
	for k, v := range incoming {
		if k == "" || v == nil {
			continue
		}
		out[k] = v
	}
	out["_sn"] = core.SearchName(asString(out["name"]))
	out["_so"] = core.SearchName(firstNonEmpty(asString(out["originalname"]), asString(out["name"])))
	if strings.TrimSpace(asString(out["updateTime"])) == "" {
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
		if tm, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return tm
		}
	}
	return time.Now().UTC()
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
			_, err := parseBytes(p)
			return err
		}
	}
	var parseInfo func(*parser) (int64, error)
	parseInfo = func(p *parser) (int64, error) {
		if p.i >= len(p.b) || p.b[p.i] != 'd' {
			return 0, fmt.Errorf("bad dict")
		}
		p.i++
		var total int64
		for p.i < len(p.b) && p.b[p.i] != 'e' {
			key, err := parseBytes(p)
			if err != nil {
				return 0, err
			}
			switch string(key) {
			case "length":
				n, err := parseInt(p)
				if err != nil {
					return 0, err
				}
				total += n
			default:
				if string(key) == "files" && p.i < len(p.b) && p.b[p.i] == 'l' {
					p.i++
					for p.i < len(p.b) && p.b[p.i] != 'e' {
						n, err := parseInfo(p)
						if err != nil {
							return 0, err
						}
						total += n
					}
					if p.i < len(p.b) {
						p.i++
					}
				} else {
					if err := skip(p); err != nil {
						return 0, err
					}
				}
			}
		}
		if p.i < len(p.b) {
			p.i++
		}
		return total, nil
	}
	p := &parser{b: data}
	if p.i >= len(p.b) || p.b[p.i] != 'd' {
		return 0
	}
	p.i++
	for p.i < len(p.b) && p.b[p.i] != 'e' {
		key, err := parseBytes(p)
		if err != nil {
			return 0
		}
		if string(key) == "info" {
			n, err := parseInfo(p)
			if err != nil {
				return 0
			}
			return n
		}
		if err := skip(p); err != nil {
			return 0
		}
	}
	return 0
}

func isDisabled(list []string, tracker string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), tracker) {
			return true
		}
	}
	return false
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
	return fmt.Sprint(v)
}
