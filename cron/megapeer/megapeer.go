package megapeer

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

const trackerName = "megapeer"
const browsePageValidMarker = `id="logo"`

var (
	rowSplitRe     = regexp.MustCompile(`class="table_fon"`)
	cleanupSpaceRe = regexp.MustCompile(`[\n\r\t\x{00A0} ]+`)
	firstNamePart  = regexp.MustCompile(`(\[|/|\(|\|)`)

	inlineYearRe = regexp.MustCompile(`^([^/]+) / ([^/]+) / ([^/\(]+) \(([0-9]{4})\)`)
	inlineYearRe2 = regexp.MustCompile(`^([^/\(]+) / ([^/\(]+) \(([0-9]{4})\)`)
	inlineYearRe3 = regexp.MustCompile(`^([^/\(]+) \(([0-9]{4})\)`)
	inlineYearRe4 = regexp.MustCompile(`^([^/]+) \[[^\]]+\] \(([0-9]{4})(\)|-)`)
	inlineYearRe5 = regexp.MustCompile(`^([^/]+) / ([^/]+) / ([^/\[]+) \[[^\]]+\] +\(([0-9]{4})(\)|-)`)
	inlineYearRe6 = regexp.MustCompile(`^([^/]+) / ([^/\[]+) \[[^\]]+\] +\(([0-9]{4})(\)|-)`)
	inlineYearRe7 = regexp.MustCompile(`^([^/\[]+) \[[^\]]+\] +\(([0-9]{4})(\)|-)`)
	mp10Re = regexp.MustCompile(`(?is)href="/?download/([0-9]+)"`)
	mp1Re = regexp.MustCompile(`(?is)<td>([0-9]+ [^ ]+ [0-9]+)</td><td[^>]*>`)
	mp2Re = regexp.MustCompile(`(?is)href="/(torrent/[0-9]+)`)
	mp3Re = regexp.MustCompile(`(?is)class="url"[^>]*>([^<]+)</a>`)
	mp4Re = regexp.MustCompile(`(?is)class="url">([^<]+)</a></td>`)
	mp5Re = regexp.MustCompile(`(?is)<td align="right">([^<\n\r]+)`)
	mp6Re = regexp.MustCompile(`(?is)alt="S">\s*<font [^>]+>([0-9]+)</font>`)
	mp7Re = regexp.MustCompile(`(?is)alt="S"[^>]*>\s*<font[^>]*>([0-9]+)`)
	mp8Re = regexp.MustCompile(`(?is)alt="L">\s*<font [^>]+>([0-9]+)</font>`)
	mp9Re = regexp.MustCompile(`(?is)alt="L"[^>]*>\s*<font[^>]*>([0-9]+)`)
)

var categories = []string{"80", "79", "6", "5", "55", "57", "76"}

type Parser struct {
	Config  app.Config
	DB      *filedb.DB
	Client  *http.Client
	CF      *core.CFClient
	mu      sync.Mutex
	working bool
	browse  sync.Mutex
}

type ParseResult struct {
	Fetched, Added, Updated, Skipped, Failed int
	Status                                   string
	PerCategory                              map[string]int
}

func New(cfg app.Config, db *filedb.DB) *Parser {
	cf, _ := core.NewCFClientWithConfig(cfg.CFClient.Profile, cfg.CFClient.UserAgent)
	return &Parser{Config: cfg, DB: db, Client: &http.Client{Timeout: 30 * time.Second}, CF: cf}
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

	if isDisabled(p.Config.DisableTrackers, trackerName) {
		return ParseResult{Status: "disabled"}, nil
	}
	if strings.TrimSpace(p.Config.Megapeer.Host) == "" {
		return ParseResult{Status: "config missing"}, nil
	}
	if maxpage <= 0 {
		maxpage = 1
	}
	res := ParseResult{Status: "ok", PerCategory: map[string]int{}}
	for _, cat := range categories {
		for page := 0; page < maxpage; page++ {
			items, err := p.fetchPage(ctx, cat, page)
			if err != nil {
				return res, err
			}
			res.Fetched += len(items)
			res.PerCategory[cat] += len(items)
			if len(items) == 0 {
				break // no more pages in this category
			}
			added, updated, skipped, failed, err := p.saveTorrents(ctx, items)
			if err != nil {
				return res, err
			}
			res.Added += added
			res.Updated += updated
			res.Skipped += skipped
			res.Failed += failed
			log.Printf("megapeer: cat=%s page %d/%d fetched=%d added=%d skipped=%d failed=%d", cat, page+1, maxpage, len(items), added, skipped, failed)

			if page < maxpage-1 && p.Config.Megapeer.ParseDelay > 0 {
				select {
				case <-ctx.Done():
					return res, ctx.Err()
				case <-time.After(time.Duration(p.Config.Megapeer.ParseDelay) * time.Millisecond):
				}
			}
		}
	}
	log.Printf("megapeer: done fetched=%d added=%d skipped=%d failed=%d", res.Fetched, res.Added, res.Skipped, res.Failed)
	return res, nil
}

func (p *Parser) fetchPage(ctx context.Context, cat string, page int) ([]filedb.TorrentDetails, error) {
	browseURL := strings.TrimRight(requestHost(p.Config.Megapeer), "/") + "/browse.php?cat=" + cat + "&page=" + strconv.Itoa(page)
	body, err := p.getBrowsePage(ctx, browseURL, cat)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(body, browsePageValidMarker) {
		return nil, nil
	}
	return parsePageHTML(strings.TrimRight(p.Config.Megapeer.Host, "/"), cat, body), nil
}

func (p *Parser) getBrowsePage(ctx context.Context, rawURL, cat string) (string, error) {
	p.browse.Lock()
	defer p.browse.Unlock()
	cookie := strings.TrimSpace(p.Config.Megapeer.Cookie)

	// Try CFClient first, then standard http.Client
	for attempt := 1; attempt <= 2; attempt++ {
		var data []byte
		var status int
		var err error

		if attempt == 1 && p.CF != nil {
			data, status, err = p.CF.Download(rawURL, cookie, "")
		} else {
			req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
			if reqErr != nil {
				return "", reqErr
			}
			req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Ubuntu; Linux x86_64; rv:148.0) Gecko/20100101 Firefox/148.0")
			if cookie != "" {
				req.Header.Set("Cookie", cookie)
			}
			resp, doErr := p.Client.Do(req)
			if doErr != nil {
				err = doErr
			} else {
				data, _ = io.ReadAll(io.LimitReader(resp.Body, 5<<20))
				resp.Body.Close()
				status = resp.StatusCode
			}
		}
		if err != nil {
			continue
		}
		body := core.DecodeCP1251(data)
		if status >= 200 && status < 300 && strings.Contains(body, browsePageValidMarker) {
			return body, nil
		}
	}
	return "", nil
}

func parsePageHTML(host, cat, body string) []filedb.TorrentDetails {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	parts := rowSplitRe.Split(body, -1)
	out := make([]filedb.TorrentDetails, 0, len(parts))
	for _, row := range parts[1:] {
		reFind := func(re *regexp.Regexp, group int) string {
			m := re.FindStringSubmatch(row)
			if len(m) <= group {
				return ""
			}
			res := html.UnescapeString(strings.TrimSpace(m[group]))
			res = cleanupSpaceRe.ReplaceAllString(strings.ReplaceAll(res, "\u0000", " "), " ")
			return strings.TrimSpace(strings.ReplaceAll(res, "\u00a0", " "))
		}
		createTime := parseCreateTime(reFind(mp1Re, 1), "02.01.06")
		if createTime.IsZero() {
			continue
		}
		urlPath := reFind(mp2Re, 1)
		title := reFind(mp3Re, 1)
		if title == "" {
			title = reFind(mp4Re, 1)
		}
		sizeName := reFind(mp5Re, 1)
		if title == "" || urlPath == "" {
			continue
		}
		sidRaw := reFind(mp6Re, 1)
		if sidRaw == "" {
			sidRaw = reFind(mp7Re, 1)
		}
		pirRaw := reFind(mp8Re, 1)
		if pirRaw == "" {
			pirRaw = reFind(mp9Re, 1)
		}
		downloadID := reFind(mp10Re, 1)
		if downloadID == "" {
			continue
		}
		name, original, relased := parseTitle(cat, title)
		if strings.TrimSpace(name) == "" {
			name = fallbackName(title)
		}
		types := typesForCategory(cat)
		if name == "" || len(types) == 0 {
			continue
		}
		sid, _ := strconv.Atoi(sidRaw)
		pir, _ := strconv.Atoi(pirRaw)
		out = append(out, filedb.TorrentRecord{
			TrackerName: trackerName,
			Types: types,
			URL: strings.TrimRight(host, "/") + "/" + strings.TrimLeft(urlPath, "/"),
			Title: title,
			Sid: sid,
			Pir: pir,
			SizeName: sizeName,
			CreateTime: createTime.UTC().Format(time.RFC3339Nano),
			UpdateTime: now,
			Name: name,
			OriginalName: original,
			Relased: relased,
			DownloadID: downloadID,
			SearchName: core.SearchName(name),
			SearchOrig: core.SearchName(firstNonEmpty(original, name)),
		}.ToMap())
	}
	return out
}

func (p *Parser) saveTorrents(ctx context.Context, torrents []filedb.TorrentDetails) (int, int, int, int, error) {
	added, updated, skipped, failed := 0, 0, 0, 0
	plog := core.NewParserLog(trackerName, filepath.Join(p.DB.DataDir, "log"), p.Config.Megapeer.Log)
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
		urlv := strings.TrimSpace(asString(incoming["url"]))
		if urlv == "" {
			skipped++
			continue
		}
		existing, exists := bucket[urlv]
		if !exists {
			if oldURL, found := filedb.FindByTrackerID(bucket, trackerName, urlv); found {
				existing = bucket[oldURL]
				delete(bucket, oldURL)
				exists = true
			}
		}
		if exists && strings.TrimSpace(asString(existing["title"])) == strings.TrimSpace(asString(incoming["title"])) {
			skipped++
			continue
		}
		magnet, err := p.downloadMagnet(ctx, asString(incoming["downloadId"]))
		if err != nil || strings.TrimSpace(magnet) == "" {
			plog.WriteFailed(urlv, asString(incoming["title"]))
			failed++
			continue
		}
		incoming["magnet"] = magnet
		delete(incoming, "downloadId")
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
			return added, updated, skipped, failed, err
		}
	}
	return added, updated, skipped, failed, nil
}

func (p *Parser) downloadMagnet(ctx context.Context, downloadID string) (string, error) {
	if strings.TrimSpace(downloadID) == "" {
		return "", nil
	}
	rawURL := strings.TrimRight(p.Config.Megapeer.Host, "/") + "/download/" + strings.TrimSpace(downloadID)
	cookie := strings.TrimSpace(p.Config.Megapeer.Cookie)

	// Try CFClient first, then standard
	for attempt := 1; attempt <= 2; attempt++ {
		var data []byte
		var status int
		var err error

		if attempt == 1 && p.CF != nil {
			data, status, err = p.CF.Download(rawURL, cookie, "")
		} else {
			req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
			if reqErr != nil {
				return "", reqErr
			}
			req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Ubuntu; Linux x86_64; rv:148.0) Gecko/20100101 Firefox/148.0")
			if cookie != "" {
				req.Header.Set("Cookie", cookie)
			}
			resp, doErr := p.Client.Do(req)
			if doErr != nil {
				err = doErr
			} else {
				data, _ = io.ReadAll(io.LimitReader(resp.Body, 5<<20))
				resp.Body.Close()
				status = resp.StatusCode
			}
		}
		if err != nil {
			continue
		}
		if status >= 200 && status < 300 {
			return core.TorrentBytesToMagnet(data), nil
		}
	}
	return "", fmt.Errorf("download failed for %s", downloadID)
}

func parseTitle(cat, title string) (string, string, int) {
	switch cat {
	case "80":
		if m := inlineYearRe.FindStringSubmatch(title); len(m) == 5 {
			return strings.TrimSpace(m[1]), strings.TrimSpace(m[3]), atoi(m[4])
		}
		if m := inlineYearRe2.FindStringSubmatch(title); len(m) == 4 {
			return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), atoi(m[3])
		}
	case "79":
		if m := inlineYearRe3.FindStringSubmatch(title); len(m) == 3 {
			return strings.TrimSpace(m[1]), "", atoi(m[2])
		}
	case "6":
		patterns := []string{
			`^([^/]+) / [^/]+ / [^/]+ / ([^/\[]+) \[[^\]]+\] +\(([0-9]{4})(\)|-)`,
			`^([^/]+) / [^/]+ / ([^/\[]+) \[[^\]]+\] +\(([0-9]{4})(\)|-)`,
			`^([^/]+) / ([^/\[]+) \[[^\]]+\] +\(([0-9]{4})(\)|-)`,
		}
		for _, pat := range patterns {
			if m := regexp.MustCompile(pat).FindStringSubmatch(title); len(m) >= 4 {
				return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), atoi(m[3])
			}
		}
	case "5":
		if m := inlineYearRe4.FindStringSubmatch(title); len(m) >= 3 {
			return strings.TrimSpace(m[1]), "", atoi(m[2])
		}
	case "55", "57", "76":
		if strings.Contains(title, " / ") {
			if strings.Contains(title, "[") && strings.Contains(title, "]") {
				if m := inlineYearRe5.FindStringSubmatch(title); len(m) >= 5 {
					return strings.TrimSpace(m[1]), strings.TrimSpace(m[3]), atoi(m[4])
				}
				if m := inlineYearRe6.FindStringSubmatch(title); len(m) >= 4 {
					return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), atoi(m[3])
				}
			} else {
				if m := inlineYearRe.FindStringSubmatch(title); len(m) == 5 {
					return strings.TrimSpace(m[1]), strings.TrimSpace(m[3]), atoi(m[4])
				}
				if m := inlineYearRe2.FindStringSubmatch(title); len(m) == 4 {
					return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), atoi(m[3])
				}
			}
		} else {
			if strings.Contains(title, "[") && strings.Contains(title, "]") {
				if m := inlineYearRe7.FindStringSubmatch(title); len(m) >= 3 {
					return strings.TrimSpace(m[1]), "", atoi(m[2])
				}
			} else if m := inlineYearRe3.FindStringSubmatch(title); len(m) == 3 {
				return strings.TrimSpace(m[1]), "", atoi(m[2])
			}
		}
	}
	return "", "", 0
}

func fallbackName(title string) string {
	parts := firstNamePart.Split(title, 2)
	if len(parts) == 0 {
		return strings.TrimSpace(title)
	}
	return strings.TrimSpace(parts[0])
}

func typesForCategory(cat string) []string {
	switch cat {
	case "80", "79":
		return []string{"movie"}
	case "6", "5":
		return []string{"serial"}
	case "55":
		return []string{"docuserial", "documovie"}
	case "57":
		return []string{"tvshow"}
	case "76":
		return []string{"multfilm", "multserial"}
	default:
		return nil
	}
}

func requestHost(t app.TrackerSettings) string {
	if strings.TrimSpace(t.Alias) != "" {
		return strings.TrimSpace(t.Alias)
	}
	return strings.TrimSpace(t.Host)
}

func parseCreateTime(line, layout string) time.Time {
	repl := strings.NewReplacer(
		" янв ", ".01.", " фев ", ".02.", " мар ", ".03.", " апр ", ".04.", " май ", ".05.", " июн ", ".06.", " июл ", ".07.", " авг ", ".08.", " сен ", ".09.", " сент ", ".09.", " окт ", ".10.", " ноя ", ".11.", " дек ", ".12.",
		"янв", ".01.", "фев", ".02.", "мар", ".03.", "апр", ".04.", "май", ".05.", "июн", ".06.", "июл", ".07.", "авг", ".08.", "сен", ".09.", "сент", ".09.", "окт", ".10.", "ноя", ".11.", "дек", ".12.",
	)
	line = strings.ToLower(strings.TrimSpace(line))
	line = repl.Replace(" " + line + " ")
	line = strings.TrimSpace(strings.ReplaceAll(line, " ", ""))
	if matched, _ := regexp.MatchString(`^[0-9]\.[0-9]{2}\.[0-9]{2}$`, line); matched {
		line = "0" + line
	}
	tm, _ := time.ParseInLocation(layout, line, time.Local)
	return tm
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
	if strings.TrimSpace(asString(out["name"])) == "" {
		out["name"] = fallbackName(asString(out["title"]))
	}
	if strings.TrimSpace(asString(out["originalname"])) == "" {
		out["originalname"] = out["name"]
	}
	out["_sn"] = core.SearchName(asString(out["name"]))
	out["_so"] = core.SearchName(firstNonEmpty(asString(out["originalname"]), asString(out["name"])))
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

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func isDisabled(values []string, tracker string) bool {
	for _, v := range values {
		if strings.EqualFold(strings.TrimSpace(v), tracker) {
			return true
		}
	}
	return false
}

