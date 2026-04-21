package anidub

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

	"path/filepath"

	"jacred/app"
	"jacred/core"
	"jacred/filedb"
)

const trackerName = "anidub"

var (
	cleanSpaceRe = regexp.MustCompile(`[\n\r\t\x{00A0} ]+`)
	dateLineRe   = regexp.MustCompile(`(?is)<li><b>Дата:</b>\s*([^<]+)</li>`)
	linkRe       = regexp.MustCompile(`(?is)<a href="([^"]+)"[^>]*>([^<]+)</a>`)
	namePairRe   = regexp.MustCompile(`^([^/]+)\s*/\s*([^\[]+?)(?:\s*\[|$)`)
	simpleNameRe = regexp.MustCompile(`^([^\[]+?)(?:\s*\[|$)`)
	yearLinkRe   = regexp.MustCompile(`(?is)<b>Год:\s*</b>\s*<span>\s*<a[^>]*>([0-9]{4})</a>\s*</span>`)
	yearSpanRe   = regexp.MustCompile(`(?is)<b>Год:\s*</b>\s*<span>([0-9]{4})</span>`)
	magnetRe     = regexp.MustCompile(`(?is)href="(magnet:\?[^\"]+)"`)
	downloadRe   = regexp.MustCompile(`(?is)href="([^\"]*engine/download\.php\?id=[0-9]+)"`)
	sizeSpanRe   = regexp.MustCompile(`(?is)Размер[^:]*:\s*<span[^>]*>([^<]+)</span>`)
	sizeInlineRe = regexp.MustCompile(`(?is)Размер[^:]*:\s*([^<]+)`)

	inlineHrefClsRe = regexp.MustCompile(`<div class="story|<div class="rand|<li><a href="`)
)

type pendingTorrent struct {
	Torrent     filedb.TorrentDetails
	DownloadURI string
}

type ParseResult struct {
	Status  string `json:"status"`
	Parsed  int    `json:"parsed"`
	Added   int    `json:"added"`
	Updated int    `json:"updated"`
	Skipped int    `json:"skipped"`
	Failed  int    `json:"failed"`
}

type Parser struct {
	Config  app.Config
	DB      *filedb.DB
	Fetcher *core.Fetcher

	mu      sync.Mutex
	working bool
}

func New(cfg app.Config, db *filedb.DB) *Parser {
	return &Parser{Config: cfg, DB: db, Fetcher: core.NewFetcher(cfg)}
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
	host := strings.TrimRight(strings.TrimSpace(p.Config.Anidub.Host), "/")
	if host == "" {
		return ParseResult{Status: "conf"}, nil
	}
	startPage := 1
	if parseFrom > 0 {
		startPage = parseFrom
	}
	endPage := startPage
	if parseTo > 0 {
		endPage = parseTo
	}
	if startPage > endPage {
		startPage, endPage = endPage, startPage
	}
	res := ParseResult{Status: "ok"}
	for page := startPage; page <= endPage; page++ {
		if page > startPage && p.Config.Anidub.ParseDelay > 0 {
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-time.After(time.Duration(p.Config.Anidub.ParseDelay) * time.Millisecond):
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
	log.Printf("anidub: done parsed=%d added=%d skipped=%d failed=%d", res.Parsed, res.Added, res.Skipped, res.Failed)
	return res, nil
}

func (p *Parser) parsePage(ctx context.Context, page int) (int, int, int, int, int, error) {
	host := strings.TrimRight(strings.TrimSpace(p.Config.Anidub.Host), "/")
	urlv := host
	if page > 1 {
		urlv = fmt.Sprintf("%s/page/%d/", host, page)
	}
	htmlBody, err := p.fetchText(ctx, urlv)
	if err != nil || htmlBody == "" || !strings.Contains(htmlBody, "dle-content") {
		return 0, 0, 0, 0, 0, err
	}
	items := parsePageHTML(host, htmlBody, page, time.Now().UTC())
	if len(items) == 0 {
		return 0, 0, 0, 0, 0, nil
	}
	added, updated, skipped, failed, err := p.saveTorrents(ctx, items)
	return len(items), added, updated, skipped, failed, err
}

func parsePageHTML(host, htmlBody string, page int, now time.Time) []pendingTorrent {
	decoded := replaceBadNames(html.UnescapeString(htmlBody))
	var rows []string
	if strings.Contains(decoded, "<article") {
		rows = strings.Split(decoded, "<article")
	} else {
		rows = inlineHrefClsRe.Split(decoded, -1)
	}
	out := make([]pendingTorrent, 0, len(rows))
	seen := map[string]struct{}{}
	for _, row := range rows[1:] {
		if strings.TrimSpace(row) == "" || !strings.Contains(row, `href="`) || !strings.Contains(row, ".html") {
			continue
		}
		createTime := parseCreateTime(matchOne(dateLineRe, row), page, now)
		if createTime.IsZero() {
			continue
		}
		m := linkRe.FindStringSubmatch(row)
		if len(m) < 3 {
			continue
		}
		urlPath := strings.TrimSpace(m[1])
		title := cleanText(m[2])
		if urlPath == "" || title == "" {
			continue
		}
		if strings.Contains(urlPath, "/user/") || strings.Contains(urlPath, "/xfsearch/") || strings.Contains(urlPath, "/forum/") || strings.Contains(urlPath, "javascript:") || strings.HasPrefix(urlPath, "#") || !strings.Contains(urlPath, ".html") {
			continue
		}
		fullURL := urlPath
		if !strings.HasPrefix(strings.ToLower(fullURL), "http") {
			fullURL = host + "/" + strings.TrimLeft(fullURL, "/")
		}
		if _, ok := seen[fullURL]; ok {
			continue
		}
		seen[fullURL] = struct{}{}
		name, original := parseNames(title)
		if strings.TrimSpace(name) == "" {
			name = fallbackName(title)
		}
		if strings.TrimSpace(name) == "" {
			continue
		}
		if strings.TrimSpace(original) == "" {
			original = name
		}
		out = append(out, pendingTorrent{Torrent: filedb.TorrentRecord{
			TrackerName: trackerName,
			Types: determineTypes(urlPath),
			URL: fullURL,
			Title: title,
			Sid: 1,
			CreateTime: createTime.UTC().Format(time.RFC3339Nano),
			UpdateTime: now.UTC().Format(time.RFC3339Nano),
			Name: name,
			OriginalName: original,
			SearchName: core.SearchName(name),
			SearchOrig: core.SearchName(firstNonEmpty(original, name)),
		}.ToMap(), DownloadURI: fullURL})
	}
	return out
}

func (p *Parser) saveTorrents(ctx context.Context, items []pendingTorrent) (int, int, int, int, error) {
	added, updated, skipped, failed := 0, 0, 0, 0
	plog := core.NewParserLog(trackerName, filepath.Join(p.DB.DataDir, "log"), p.Config.LogParsers && p.Config.Anidub.Log)
	bucketCache := map[string]map[string]filedb.TorrentDetails{}
	changed := map[string]time.Time{}
	for _, item := range items {
		incoming := item.Torrent
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
		// Fetch detail page for magnet/relased/sizeName
		detailHTML, err := p.fetchText(ctx, urlv)
		if err != nil {
			failed++
			continue
		}
		if detailHTML != "" {
			if released := extractReleased(detailHTML); released > 0 {
				incoming["relased"] = released
			}
			if magnet := matchOne(magnetRe, detailHTML); magnet != "" {
				incoming["magnet"] = magnet
			}
			if sz := extractSizeName(detailHTML); sz != "" {
				incoming["sizeName"] = sz
				incoming["size"] = parseSizeBytes(sz)
			}
		}
		// Fallback: download .torrent file
		if strings.TrimSpace(asString(incoming["magnet"])) == "" && detailHTML != "" {
			if downloadURL := buildDownloadURL(strings.TrimRight(strings.TrimSpace(p.Config.Anidub.Host), "/"), matchOne(downloadRe, detailHTML)); downloadURL != "" {
				if b, err := p.download(ctx, downloadURL, urlv); err == nil && len(b) > 0 {
					if magnet := core.TorrentBytesToMagnet(b); magnet != "" {
						incoming["magnet"] = magnet
					}
				}
			}
		}
		if strings.TrimSpace(asString(incoming["magnet"])) == "" && !exists {
			plog.WriteFailed(urlv, asString(incoming["title"]))
			failed++
			continue
		}
		var ex filedb.TorrentDetails
		if exists {
			ex = existing
		}
		result := filedb.MergeTorrent(ex, incoming)
		if !result.Changed {
			skipped++
			continue
		}
		bucket[urlv] = result.Torrent
		changed[key] = fileTime(result.Torrent)
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

func (p *Parser) fetchText(ctx context.Context, urlv string) (string, error) {
	body, status, err := p.Fetcher.GetString(urlv, p.Config.Anidub)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", nil
	}
	return body, nil
}

func (p *Parser) download(ctx context.Context, urlv, referer string) ([]byte, error) {
	data, status, err := p.Fetcher.Download(urlv, p.Config.Anidub)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, nil
	}
	return data, nil
}

func extractReleased(htmlBody string) int {
	for _, re := range []*regexp.Regexp{yearLinkRe, yearSpanRe} {
		if m := re.FindStringSubmatch(htmlBody); len(m) > 1 {
			y, _ := strconv.Atoi(strings.TrimSpace(m[1]))
			if y > 1900 && y <= time.Now().UTC().Year()+1 {
				return y
			}
		}
	}
	return 0
}

func extractSizeName(htmlBody string) string {
	if s := cleanText(matchOne(sizeSpanRe, htmlBody)); s != "" {
		return s
	}
	return cleanText(matchOne(sizeInlineRe, htmlBody))
}

func parseCreateTime(raw string, page int, now time.Time) time.Time {
	raw = cleanText(raw)
	if raw == "" {
		if page == 1 {
			return now.UTC()
		}
		return time.Time{}
	}
	if strings.Contains(raw, "Сегодня") {
		return now.UTC()
	}
	if strings.Contains(raw, "Вчера") {
		return now.UTC().AddDate(0, 0, -1)
	}
	m := regexp.MustCompile(`([0-9]{1,2})-([0-9]{2})-([0-9]{4})`).FindStringSubmatch(raw)
	if len(m) != 4 {
		if page == 1 {
			return now.UTC()
		}
		return time.Time{}
	}
	tm, err := time.Parse("02.01.2006", fmt.Sprintf("%02s.%s.%s", pad2(m[1]), m[2], m[3]))
	if err != nil {
		return time.Time{}
	}
	return tm.UTC()
}

func pad2(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		return s
	}
	return "0" + s
}

func determineTypes(urlPath string) []string {
	lower := strings.ToLower(urlPath)
	switch {
	case strings.Contains(lower, "/dorama/"):
		return []string{"dorama"}
	case strings.Contains(lower, "/anime_movie/") || strings.Contains(lower, "/anime-movie/"):
		return []string{"anime", "movie"}
	case strings.Contains(lower, "/anime_ova/") || strings.Contains(lower, "/anime-ova/"):
		return []string{"anime", "ova"}
	case strings.Contains(lower, "/anime_tv/") || strings.Contains(lower, "/anime-tv/"):
		return []string{"anime", "serial"}
	default:
		return []string{"anime"}
	}
}

func parseNames(title string) (string, string) {
	if m := namePairRe.FindStringSubmatch(title); len(m) > 2 {
		return cleanText(m[1]), cleanText(m[2])
	}
	if m := simpleNameRe.FindStringSubmatch(title); len(m) > 1 {
		return cleanText(m[1]), ""
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
	s = cleanSpaceRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func replaceBadNames(s string) string {
	s = strings.ReplaceAll(s, "\u00a0", " ")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	return s
}

func matchOne(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return cleanText(m[1])
}

func buildDownloadURL(host, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(raw), "http") {
		return raw
	}
	return strings.TrimRight(host, "/") + "/" + strings.TrimLeft(raw, "/")
}

func parseSizeBytes(v string) float64 {
	m := regexp.MustCompile(`(?i)([0-9]+(?:[\.,][0-9]+)?)\s*(TB|GB|MB|KB|ТБ|ГБ|МБ|КБ)`).FindStringSubmatch(strings.TrimSpace(v))
	if len(m) < 3 {
		return 0
	}
	n, _ := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", "."), 64)
	switch strings.ToUpper(m[2]) {
	case "TB", "ТБ":
		return n * 1024 * 1024 * 1024 * 1024
	case "GB", "ГБ":
		return n * 1024 * 1024 * 1024
	case "MB", "МБ":
		return n * 1024 * 1024
	case "KB", "КБ":
		return n * 1024
	}
	return 0
}


func fileTime(t filedb.TorrentDetails) time.Time {
	for _, key := range []string{"updateTime", "createTime"} {
		s := strings.TrimSpace(asString(t[key]))
		if s == "" {
			continue
		}
		if tm, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return tm.UTC()
		}
		if tm, err := time.Parse(time.RFC3339, s); err == nil {
			return tm.UTC()
		}
	}
	return time.Time{}
}

func asString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		return fmt.Sprintf("%v", v)
	}
}

func asInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(t))
		return n
	default:
		n, _ := strconv.Atoi(strings.TrimSpace(asString(v)))
		return n
	}
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
