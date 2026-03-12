
package selezen

import (
	"context"
	"fmt"
	"html"
	"io"
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

const trackerName = "selezen"
const selezenUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

var (
	rowSplitRe           = regexp.MustCompile(`card overflow-hidden`)
	cardURLTitleRe       = regexp.MustCompile(`<a href="(https?://[^"]+)"><h4 class="card-title">([^<]+)</h4>`)
	createTimeRe         = regexp.MustCompile(`class="bx bx-calendar"></span>\s*([0-9]{2}\.[0-9]{2}\.[0-9]{4} [0-9]{2}:[0-9]{2})</a>`)
	sidRe                = regexp.MustCompile(`<i class="bx bx-chevrons-up"></i>([0-9 ]+)`)
	pirRe                = regexp.MustCompile(`<i class="bx bx-chevrons-down"></i>([0-9 ]+)`)
	sizeNameRe           = regexp.MustCompile(`<span class="bx bx-download"></span>([^<]+)</a>`)
	magnetRe             = regexp.MustCompile(`href="(magnet:\?xt=urn:btih:[^"]+)"`)
	itemIDRe             = regexp.MustCompile(`/relizy-ot-selezen/(\d+)-`)
	movieMainRe          = regexp.MustCompile(`^([^/\(]+) / [^/]+ / ([^/\(]+) \(([0-9]{4})\)`)
	movieShortRe         = regexp.MustCompile(`^([^/\(]+) / ([^/\(]+) \(([0-9]{4})\)`)
	serialSeasonRe       = regexp.MustCompile(`\[S\d+\]`)
	serialEpisodeRe      = regexp.MustCompile(`\[\d+[xх]\d+`)
	cleanSpaceRe         = regexp.MustCompile(`[\t\r\n\x{00A0} ]+`)
	badAnimeMarkerRe     = regexp.MustCompile(`>Аниме</a>`)
	multMarkerRe         = regexp.MustCompile(`>Мульт|>мульт`)
	loginSessionCookieRe = regexp.MustCompile(`PHPSESSID=([^;]+)(;|$)`)
)

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

type ParseResult struct {
	Status  string `json:"status"`
	Parsed  int    `json:"parsed"`
	Added   int    `json:"added"`
	Updated int    `json:"updated"`
	Skipped int    `json:"skipped"`
	Failed  int    `json:"failed"`
}

func New(cfg app.Config, db *filedb.DB) *Parser {
	return &Parser{Config: cfg, DB: db, Client: &http.Client{Timeout: 25 * time.Second}}
}

func (p *Parser) Parse(ctx context.Context, parseFrom, parseTo int) (ParseResult, error) {
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
	host := strings.TrimRight(strings.TrimSpace(p.Config.Selezen.Host), "/")
	if host == "" {
		return ParseResult{Status: "conf"}, nil
	}

	startPage, endPage := 1, 1
	if parseFrom > 0 {
		startPage = parseFrom
		endPage = parseFrom
	}
	if parseTo > 0 {
		endPage = parseTo
	}
	if startPage > endPage {
		startPage, endPage = endPage, startPage
	}

	res := ParseResult{Status: "ok"}
	for page := startPage; page <= endPage; page++ {
		if page > startPage && p.Config.Selezen.ParseDelay > 0 {
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-time.After(time.Duration(p.Config.Selezen.ParseDelay) * time.Millisecond):
			}
		}
		parsed, added, updated, skipped, failed, err := p.parsePage(ctx, page)
		if err != nil {
			return res, err
		}
		res.Parsed += parsed
		res.Added += added
		res.Updated += updated
		res.Skipped += skipped
		res.Failed += failed
	}
	return res, nil
}

func (p *Parser) parsePage(ctx context.Context, page int) (int, int, int, int, int, error) {
	cookie, err := p.ensureCookie(ctx)
	if err != nil || strings.TrimSpace(cookie) == "" {
		return 0, 0, 0, 0, 0, err
	}
	host := strings.TrimRight(strings.TrimSpace(p.Config.Selezen.Host), "/")
	listURL := host + "/relizy-ot-selezen/"
	if page > 1 {
		listURL = fmt.Sprintf("%s/relizy-ot-selezen/page/%d/", host, page)
	}
	body, err := p.fetchText(ctx, listURL, cookie, host+"/")
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}
	if body == "" || !strings.Contains(body, "dle_root") {
		return 0, 0, 0, 0, 0, nil
	}
	if loginUser := strings.TrimSpace(p.Config.Selezen.Login.U); loginUser != "" && !strings.Contains(body, ">"+loginUser+"<") {
		return 0, 0, 0, 0, 0, nil
	}
	torrents := parsePageHTML(body)
	if len(torrents) == 0 {
		return 0, 0, 0, 0, 0, nil
	}
	added, updated, skipped, failed, err := p.saveTorrents(ctx, cookie, torrents)
	return len(torrents), added, updated, skipped, failed, err
}

func parsePageHTML(htmlBody string) []filedb.TorrentDetails {
	rows := rowSplitRe.Split(replaceBadNames(htmlBody), -1)
	out := make([]filedb.TorrentDetails, 0, len(rows))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, row := range rows[1:] {
		if strings.TrimSpace(row) == "" || badAnimeMarkerRe.MatchString(row) {
			continue
		}
		createRaw := matchDecode(createTimeRe, row)
		createTime, err := time.ParseInLocation("02.01.2006 15:04", createRaw, time.UTC)
		if err != nil || createTime.IsZero() {
			continue
		}
		m := cardURLTitleRe.FindStringSubmatch(row)
		if len(m) < 3 {
			continue
		}
		urlv := strings.TrimSpace(m[1])
		title := strings.TrimSpace(html.UnescapeString(m[2]))
		if urlv == "" || !strings.Contains(strings.ToLower(urlv), ".html") || title == "" {
			continue
		}
		sidRaw := strings.ReplaceAll(matchDecode(sidRe, row), " ", "")
		pirRaw := strings.ReplaceAll(matchDecode(pirRe, row), " ", "")
		sizeName := strings.TrimSpace(strings.ReplaceAll(matchDecode(sizeNameRe, row), "&nbsp;", " "))
		if sidRaw == "" || pirRaw == "" || sizeName == "" {
			continue
		}
		name, original, relased := parseNames(title)
		if strings.TrimSpace(name) == "" {
			name = fallbackName(title)
		}
		if strings.TrimSpace(name) == "" {
			continue
		}
		sid, _ := strconv.Atoi(sidRaw)
		pir, _ := strconv.Atoi(pirRaw)
		out = append(out, filedb.TorrentDetails{
			"trackerName":  trackerName,
			"types":        typesForRow(row, title, urlv),
			"url":          urlv,
			"title":        title,
			"sid":          sid,
			"pir":          pir,
			"sizeName":     sizeName,
			"createTime":   createTime.UTC().Format(time.RFC3339Nano),
			"updateTime":   now,
			"name":         strings.TrimSpace(name),
			"originalname": strings.TrimSpace(original),
			"relased":      relased,
			"_sn":          core.SearchName(name),
			"_so":          core.SearchName(firstNonEmpty(original, name)),
		})
	}
	return out
}

func (p *Parser) saveTorrents(ctx context.Context, cookie string, torrents []filedb.TorrentDetails) (int, int, int, int, error) {
	added, updated, skipped, failed := 0, 0, 0, 0
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
			if id := itemID(urlv); id != "" {
				for oldURL, oldItem := range bucket {
					if itemID(oldURL) == id {
						existing, exists = oldItem, true
						if strings.TrimSpace(asString(existing["magnet"])) != "" {
							incoming["magnet"] = existing["magnet"]
						}
						break
					}
				}
			}
		}
		if strings.TrimSpace(asString(incoming["magnet"])) == "" {
			magnet, err := p.fetchMagnet(ctx, cookie, urlv)
			if err != nil {
				failed++
				continue
			}
			if strings.TrimSpace(magnet) == "" {
				failed++
				continue
			}
			incoming["magnet"] = magnet
		}
		if exists && samePrimary(existing, incoming) {
			skipped++
			continue
		}
		merged := mergeTorrent(existing, exists, incoming)
		bucket[urlv] = merged
		changed[key] = fileTime(merged)
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
		if err := p.DB.SaveChangesToFile(); err != nil {
			return added, updated, skipped, failed, err
		}
	}
	return added, updated, skipped, failed, nil
}

func (p *Parser) ensureCookie(ctx context.Context) (string, error) {
	if cfg := strings.TrimSpace(p.Config.Selezen.Cookie); cfg != "" {
		return cfg, nil
	}
	p.cookieMu.Lock()
	if strings.TrimSpace(p.cookie) != "" {
		cookie := p.cookie
		p.cookieMu.Unlock()
		return cookie, nil
	}
	if time.Since(p.lastLoginAttempt) < 2*time.Minute {
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
	host := strings.TrimRight(strings.TrimSpace(p.Config.Selezen.Host), "/")
	if host == "" || strings.TrimSpace(p.Config.Selezen.Login.U) == "" || strings.TrimSpace(p.Config.Selezen.Login.P) == "" {
		return "", nil
	}
	vals := url.Values{}
	vals.Set("login_name", p.Config.Selezen.Login.U)
	vals.Set("login_password", p.Config.Selezen.Login.P)
	vals.Set("login_not_save", "1")
	vals.Set("login", "submit")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host, strings.NewReader(vals.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", selezenUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Referer", host+"/")
	req.Header.Set("Origin", host)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	for _, setCookie := range resp.Header.Values("Set-Cookie") {
		if m := loginSessionCookieRe.FindStringSubmatch(setCookie); len(m) > 1 && strings.TrimSpace(m[1]) != "" {
			return fmt.Sprintf("PHPSESSID=%s; _ym_isad=2;", strings.TrimSpace(m[1])), nil
		}
	}
	return "", nil
}

func (p *Parser) fetchText(ctx context.Context, urlv, cookie, referer string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlv, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", selezenUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	if strings.TrimSpace(referer) != "" {
		req.Header.Set("Referer", referer)
	}
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

func (p *Parser) fetchMagnet(ctx context.Context, cookie, urlv string) (string, error) {
	host := strings.TrimRight(strings.TrimSpace(p.Config.Selezen.Host), "/")
	body, err := p.fetchText(ctx, urlv, cookie, host+"/")
	if err != nil || body == "" {
		return "", err
	}
	return html.UnescapeString(matchDecode(magnetRe, body)), nil
}

func parseNames(title string) (string, string, int) {
	if m := movieMainRe.FindStringSubmatch(title); len(m) > 3 {
		year, _ := strconv.Atoi(strings.TrimSpace(m[3]))
		return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), year
	}
	if m := movieShortRe.FindStringSubmatch(title); len(m) > 3 {
		year, _ := strconv.Atoi(strings.TrimSpace(m[3]))
		return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), year
	}
	return "", "", 0
}

func fallbackName(title string) string {
	parts := regexp.MustCompile(`(\[|/|\(|\|)`).Split(title, 2)
	if len(parts) == 0 {
		return ""
	}
	return strings.TrimSpace(parts[0])
}

func typesForRow(row, title, urlv string) []string {
	if multMarkerRe.MatchString(row) {
		return []string{"multfilm"}
	}
	if strings.Contains(strings.ToLower(title), "tvshows") || strings.Contains(strings.ToLower(urlv), "tvshows") || serialSeasonRe.MatchString(title) || serialEpisodeRe.MatchString(title) {
		return []string{"serial"}
	}
	return []string{"movie"}
}

func itemID(urlv string) string {
	m := itemIDRe.FindStringSubmatch(urlv)
	if len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func replaceBadNames(s string) string {
	s = strings.ReplaceAll(s, "\u00a0", " ")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	return s
}

func matchDecode(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(cleanSpaceRe.ReplaceAllString(html.UnescapeString(m[1]), " "))
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

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
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
