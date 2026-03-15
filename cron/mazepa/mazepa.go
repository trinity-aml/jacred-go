package mazepa

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

const trackerName = "mazepa"

// Forum categories: id -> types
var categories = map[string][]string{
	// Українські фільми
	"37": {"movie"}, "7": {"movie"},
	// Фільми
	"175": {"movie"}, "147": {"movie"}, "12": {"movie"}, "13": {"movie"}, "174": {"movie"},
	// Українські серіали
	"38": {"serial"}, "8": {"serial"},
	// Серіали
	"152": {"serial"}, "44": {"serial"}, "14": {"serial"},
	// Українські мультфільми
	"35": {"multfilm"}, "5": {"multfilm"},
	// Мультфільми
	"155": {"multfilm"}, "41": {"multfilm"}, "10": {"multfilm"},
	// Українські мультсеріали
	"36": {"multserial"}, "6": {"multserial"},
	// Мультсеріали
	"43": {"multserial"}, "11": {"multserial"},
	// Аніме
	"16": {"anime"},
	// Документальні
	"39": {"documovie"}, "9": {"documovie"}, "157": {"documovie"}, "42": {"documovie"}, "15": {"documovie"},
}

var (
	rowRe       = regexp.MustCompile(`(?is)<tr id="tr-(\d+)".*?</tr>`)
	titleRe     = regexp.MustCompile(`(?i)class="torTopic[^"]*"><b>([^<]+)</b>`)
	magnetRe    = regexp.MustCompile(`(?i)href="(magnet:\?[^"]+)"`)
	seedRe      = regexp.MustCompile(`(?i)seedmed[^>]*><b>(\d+)</b>`)
	leechRe     = regexp.MustCompile(`(?i)leechmed[^>]*><b>(\d+)</b>`)
	sizeRe      = regexp.MustCompile(`(?i)>([0-9.,]+)\s*&nbsp;(GB|MB|TB)<`)
	sizeAltRe   = regexp.MustCompile(`(?i)([0-9.,]+)\s*(GB|MB|TB|ГБ|МБ|ТБ)\b`)
	lastPostRe  = regexp.MustCompile(`(?is)<ul class="last_post[^"]*">.*?<a[^>]*>([^<]+)</a>`)
	yearParenRe = regexp.MustCompile(`\((\d{4})\)`)
	namePartRe  = regexp.MustCompile(`(?i)\s*/\s*`)
	hasLatinRe  = regexp.MustCompile(`[A-Za-z]`)
	noLatinRe   = regexp.MustCompile(`^[^A-Za-z]+$`)
	btihRe      = regexp.MustCompile(`(?i)btih:([A-Fa-f0-9]{40}|[A-Z2-7]{32})`)
	cleanMetaRe = regexp.MustCompile(`(?i)\b(2160p|1080p|720p|480p|WEB-?DL|WEB-?Rip|BDRip|BDRemux|HDRip|BluRay|BRRip|DVDRip|HDTV|x264|x265|h\.?264|h\.?265|hevc|avc|aac|ac3|dts|ddp?\d\.\d|vc-?1)\b`)
	seasonRe    = regexp.MustCompile(`(?i)(?:^|\s)(Сезон|Season)\s*\d+.*$`)
	sxxexxRe    = regexp.MustCompile(`(?i)\b(S\d{1,2}|E\d{1,2}|S\d{1,2}E\d{1,2})\b`)
	yearStripRe = regexp.MustCompile(`(?i)\s*\((19|20)\d{2}(-\d{4})?\)`)
	hdrRe       = regexp.MustCompile(`(?i)\bhdr\b|hdr10`)
	qualityRe   = regexp.MustCompile(`(?i)(2160p|4k|uhd)`)
	quality1080 = regexp.MustCompile(`1080p`)
	quality720  = regexp.MustCompile(`720p`)
	monthMap    = map[string]int{
		"січ": 1, "сiч": 1, "лют": 2, "бер": 3, "кві": 4, "квi": 4, "тра": 5,
		"чер": 6, "лип": 7, "сер": 8, "вер": 9, "жов": 10, "лис": 11, "гру": 12,
	}
	mazDateRe = regexp.MustCompile(`(\d{1,2})\s+(\S+)\s+(\d{4}),\s*(\d{1,2}):(\d{2})`)
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
	if p.cookie != "" && time.Since(p.cookieT) < 2*time.Hour {
		return p.cookie
	}
	return ""
}

func (p *Parser) takeLogin(ctx context.Context) bool {
	host := strings.TrimRight(p.Config.Mazepa.Host, "/")
	if host == "" || p.Config.Mazepa.Login.U == "" {
		return false
	}
	form := url.Values{
		"login_username": {p.Config.Mazepa.Login.U},
		"login_password": {p.Config.Mazepa.Login.P},
		"autologin":      {"on"},
		"redirect":       {"/index.php"},
		"login":          {"Увійти"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/login.php", strings.NewReader(form.Encode()))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := p.Client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	var parts []string
	for _, line := range resp.Header.Values("Set-Cookie") {
		parts = append(parts, strings.SplitN(line, ";", 2)[0])
	}
	cookieStr := strings.Join(parts, "; ")
	if strings.Contains(cookieStr, "bb_") {
		p.mu.Lock()
		p.cookie = cookieStr
		p.cookieT = time.Now()
		p.mu.Unlock()
		return true
	}
	return false
}

func (p *Parser) Parse(ctx context.Context) (ParseResult, error) {
	p.mu.Lock()
	if p.working {
		p.mu.Unlock()
		return ParseResult{Status: "work"}, nil
	}
	p.working = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.working = false; p.mu.Unlock() }()

	host := strings.TrimRight(p.Config.Mazepa.Host, "/")
	if host == "" {
		return ParseResult{Status: "config missing"}, nil
	}
	if p.getCookie() == "" {
		if !p.takeLogin(ctx) {
			return ParseResult{Status: "login failed"}, nil
		}
	}

	res := ParseResult{Status: "ok", PerCategory: map[string]int{}}
	for catID, types := range categories {
		start := 0
		var lastSig string
		for {
			pageURL := fmt.Sprintf("%s/viewforum.php?f=%s&start=%d", host, catID, start)
			items, sig, err := p.parseForumPage(ctx, pageURL, types, host)
			if err != nil {
				return res, err
			}
			if len(items) == 0 || sig == lastSig {
				break
			}
			lastSig = sig
			res.Fetched += len(items)
			a, u, s, f, err := p.saveTorrents(items)
			if err != nil {
				return res, err
			}
			res.Added += a
			res.Updated += u
			res.Skipped += s
			res.Failed += f
			start += 50

			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-time.After(800 * time.Millisecond):
			}
		}
	}
	return res, nil
}

func (p *Parser) parseForumPage(ctx context.Context, pageURL string, types []string, host string) ([]filedb.TorrentDetails, string, error) {
	body, err := p.httpGet(ctx, pageURL)
	if err != nil {
		return nil, "", err
	}
	if body == "" {
		return nil, "", nil
	}

	rows := rowRe.FindAllString(body, -1)
	if len(rows) == 0 {
		return nil, "", nil
	}

	var out []filedb.TorrentDetails
	now := time.Now().UTC().Format(time.RFC3339Nano)

	for _, block := range rows {
		tidM := regexp.MustCompile(`tr-(\d+)`).FindStringSubmatch(block)
		if len(tidM) < 2 {
			continue
		}
		tid := tidM[1]

		titleM := titleRe.FindStringSubmatch(block)
		if len(titleM) < 2 || strings.TrimSpace(titleM[1]) == "" {
			continue
		}
		title := strings.TrimSpace(html.UnescapeString(titleM[1]))

		magnetM := magnetRe.FindStringSubmatch(block)
		if len(magnetM) < 2 || strings.TrimSpace(magnetM[1]) == "" {
			continue
		}
		magnet := normalizeMagnet(html.UnescapeString(magnetM[1]))
		if magnet == "" {
			continue
		}

		sid, pir := 0, 0
		if m := seedRe.FindStringSubmatch(block); len(m) > 1 {
			sid, _ = strconv.Atoi(m[1])
		}
		if m := leechRe.FindStringSubmatch(block); len(m) > 1 {
			pir, _ = strconv.Atoi(m[1])
		}

		sizeName := parseSizeName(block)

		// Date from last post
		createTime := time.Now().UTC()
		if m := lastPostRe.FindStringSubmatch(block); len(m) > 1 {
			if t := parseMazepaDate(m[1]); !t.IsZero() {
				createTime = t
			}
		}

		name, original, year := parseNamesAdvanced(title)
		quality := 480
		if qualityRe.MatchString(title) {
			quality = 2160
		} else if quality1080.MatchString(title) {
			quality = 1080
		} else if quality720.MatchString(title) {
			quality = 720
		}
		videotype := "sdr"
		if hdrRe.MatchString(title) {
			videotype = "hdr"
		}

		out = append(out, filedb.TorrentDetails{
			"trackerName":  trackerName,
			"types":        types,
			"url":          fmt.Sprintf("%s/viewtopic.php?t=%s", host, tid),
			"title":        title,
			"name":         name,
			"originalname": core.FirstNonEmpty(original, name),
			"magnet":       magnet,
			"sizeName":     sizeName,
			"quality":      quality,
			"videotype":    videotype,
			"sid":          sid,
			"pir":          pir,
			"createTime":   createTime.UTC().Format(time.RFC3339Nano),
			"updateTime":   now,
			"relased":      year,
			"_sn":          core.SearchName(name),
			"_so":          core.SearchName(core.FirstNonEmpty(original, name)),
		})
	}

	sig := ""
	for i, t := range out {
		if i >= 5 {
			break
		}
		sig += asString(t["url"]) + ","
	}
	return out, sig, nil
}

func (p *Parser) saveTorrents(torrents []filedb.TorrentDetails) (int, int, int, int, error) {
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
	return string(b), nil
}

func normalizeMagnet(raw string) string {
	raw = html.UnescapeString(raw)
	m := btihRe.FindStringSubmatch(raw)
	if len(m) < 2 {
		return ""
	}
	return "magnet:?xt=urn:btih:" + m[1]
}

func parseSizeName(block string) string {
	if m := sizeRe.FindStringSubmatch(block); len(m) > 2 {
		return strings.TrimSpace(m[1]) + " " + strings.TrimSpace(m[2])
	}
	if m := sizeAltRe.FindStringSubmatch(block); len(m) > 2 {
		return strings.TrimSpace(m[1]) + " " + strings.TrimSpace(m[2])
	}
	return ""
}

func parseMazepaDate(text string) time.Time {
	text = html.UnescapeString(strings.TrimSpace(text))
	m := mazDateRe.FindStringSubmatch(text)
	if len(m) < 6 {
		return time.Time{}
	}
	day, _ := strconv.Atoi(m[1])
	monthRaw := strings.ToLower(strings.TrimSpace(m[2]))
	year, _ := strconv.Atoi(m[3])
	hour, _ := strconv.Atoi(m[4])
	minute, _ := strconv.Atoi(m[5])

	month, ok := monthMap[monthRaw]
	if !ok || day == 0 || year == 0 {
		return time.Time{}
	}
	return time.Date(year, time.Month(month), day, hour, minute, 0, 0, time.UTC)
}

func parseNamesAdvanced(title string) (string, string, int) {
	if strings.TrimSpace(title) == "" {
		return "", "", 0
	}
	yearM := yearParenRe.FindStringSubmatch(title)
	year := 0
	if len(yearM) > 1 {
		year, _ = strconv.Atoi(yearM[1])
	}

	beforeYear := title
	if idx := yearParenRe.FindStringIndex(title); idx != nil {
		beforeYear = strings.TrimSpace(title[:idx[0]])
	}

	parts := namePartRe.Split(beforeYear, -1)
	var cleaned []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			cleaned = append(cleaned, p)
		}
	}
	if len(cleaned) == 0 {
		return title, title, year
	}

	var original, name string
	for _, p := range cleaned {
		if hasLatinRe.MatchString(p) {
			original = p
		}
	}
	for _, p := range cleaned {
		if noLatinRe.MatchString(p) {
			name = p
			break
		}
	}
	if name == "" {
		name = cleaned[0]
	}
	if original == "" {
		original = name
	}
	name = cleanTitle(name)
	original = cleanTitle(original)
	return name, original, year
}

func cleanTitle(title string) string {
	if title == "" {
		return ""
	}
	t := yearStripRe.ReplaceAllString(title, "")
	t = seasonRe.ReplaceAllString(t, "")
	t = sxxexxRe.ReplaceAllString(t, "")
	t = cleanMetaRe.ReplaceAllString(t, "")
	t = regexp.MustCompile(`[\[\]|]`).ReplaceAllString(t, " ")
	t = regexp.MustCompile(`\s{2,}`).ReplaceAllString(t, " ")
	return strings.TrimSpace(t)
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
