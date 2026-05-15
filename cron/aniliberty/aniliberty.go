package aniliberty

import (
	"context"
	"encoding/json"
	"fmt"
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

const trackerName = "aniliberty"

var (
	qualityInfoRe = regexp.MustCompile(`(\[[^\]]+\](?:\s*\[[^\]]+\])*)\s*$`)
	qualityNumRe  = regexp.MustCompile(`(\d{3,4})p?`)
)

type apiResponse struct {
	Data []apiTorrent `json:"data"`
	Meta *apiMeta     `json:"meta"`
}

type apiMeta struct {
	LastPage int `json:"last_page"`
}

type apiTorrent struct {
	ID        int         `json:"id"`
	Hash      string      `json:"hash"`
	Magnet    string      `json:"magnet"`
	Label     string      `json:"label"`
	Size      int64       `json:"size"`
	Seeders   int         `json:"seeders"`
	Leechers  int         `json:"leechers"`
	CreatedAt string      `json:"created_at"`
	UpdatedAt string      `json:"updated_at"`
	Quality   *apiValue   `json:"quality"`
	Type      *apiValue   `json:"type"`
	Release   *apiRelease `json:"release"`
}

type apiValue struct {
	Value string `json:"value"`
}

type apiRelease struct {
	Alias string    `json:"alias"`
	Year  *int      `json:"year"`
	Type  *apiValue `json:"type"`
	Name  *apiName  `json:"name"`
}

type apiName struct {
	Main    string `json:"main"`
	English string `json:"english"`
}

type ParseResult struct {
	Status   string `json:"status"`
	Parsed   int    `json:"parsed"`
	Added    int    `json:"added"`
	Updated  int    `json:"updated"`
	Skipped  int    `json:"skipped"`
	Failed   int    `json:"failed"`
	LastPage int    `json:"lastPage"`
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
	defer func() { p.mu.Lock(); p.working = false; p.mu.Unlock() }()
	if isDisabled(p.Config.DisableTrackers, trackerName) {
		return ParseResult{Status: "disabled"}, nil
	}
	host := strings.TrimRight(strings.TrimSpace(p.Config.Aniliberty.Host), "/")
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
	lastPage := int(^uint(0) >> 1)
	for page := startPage; page <= endPage && page <= lastPage; page++ {
		if page > startPage && p.Config.Aniliberty.ParseDelay > 0 {
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-time.After(time.Duration(p.Config.Aniliberty.ParseDelay) * time.Millisecond):
			}
		}
		parsed, added, updated, skipped, failed, lp, err := p.parsePage(ctx, page)
		if err != nil {
			return res, err
		}
		res.Parsed += parsed
		res.Added += added
		res.Updated += updated
		res.Skipped += skipped
		res.Failed += failed
		if lp > 0 {
			lastPage = lp
			res.LastPage = lp
		}
		if page >= lastPage {
			break
		}
	}
	log.Printf("aniliberty: done parsed=%d added=%d skipped=%d failed=%d", res.Parsed, res.Added, res.Skipped, res.Failed)
	return res, nil
}

func (p *Parser) parsePage(ctx context.Context, page int) (int, int, int, int, int, int, error) {
	host := strings.TrimRight(strings.TrimSpace(p.Config.Aniliberty.Host), "/")
	apiURL := fmt.Sprintf("%s/api/v1/anime/torrents?page=%d&limit=50", host, page)
	body, err := p.fetch(ctx, apiURL)
	if err != nil {
		return 0, 0, 0, 0, 0, 0, err
	}
	var resp apiResponse
	if err := json.Unmarshal(body, &resp); err != nil || len(resp.Data) == 0 {
		return 0, 0, 0, 0, 0, 0, nil
	}
	items := make([]filedb.TorrentDetails, 0, len(resp.Data))
	failed := 0
	for _, it := range resp.Data {
		td, ok := convertTorrent(host, it)
		if !ok {
			failed++
			continue
		}
		items = append(items, td)
	}
	added, updated, skipped, saveFailed, err := p.saveTorrents(items)
	failed += saveFailed
	lp := 0
	if resp.Meta != nil {
		lp = resp.Meta.LastPage
	}
	return len(items), added, updated, skipped, failed, lp, err
}

func convertTorrent(host string, it apiTorrent) (filedb.TorrentDetails, bool) {
	if strings.TrimSpace(it.Magnet) == "" || it.Release == nil {
		return nil, false
	}
	name := strings.TrimSpace(safeName(it.Release.Name, true))
	original := strings.TrimSpace(safeName(it.Release.Name, false))
	if name == "" && original == "" {
		return nil, false
	}
	title := buildBaseTitle(name, original)
	if it.Release.Year != nil && *it.Release.Year > 0 {
		title += fmt.Sprintf(" / %d", *it.Release.Year)
	}
	if qi := extractQualityInfo(it.Label); qi != "" {
		title += " / " + qi
	}
	createTime := parseAPITime(it.CreatedAt)
	updateTime := parseAPITime(it.UpdatedAt)
	if updateTime.IsZero() {
		updateTime = createTime
	}
	baseURL := host + "/api/v1/anime/torrents/" + strings.TrimSpace(it.Hash)
	if strings.TrimSpace(it.Release.Alias) != "" {
		baseURL = host + "/anime/releases/release/" + strings.TrimLeft(strings.TrimSpace(it.Release.Alias), "/")
	}
	urlv := baseURL
	if strings.TrimSpace(it.Hash) != "" {
		urlv += "?hash=" + strings.TrimSpace(it.Hash)
	}
	if name == "" {
		name = original
	}
	if original == "" {
		original = name
	}
	year := 0
	if it.Release.Year != nil {
		year = *it.Release.Year
	}
	videotype := ""
	if it.Type != nil {
		videotype = strings.ToLower(strings.TrimSpace(it.Type.Value))
	}
	return filedb.TorrentRecord{
		TrackerName: trackerName,
		Types: determineTypes(releaseType(it.Release)),
		URL: urlv,
		Title: title,
		Sid: it.Seeders,
		Pir: it.Leechers,
		CreateTime: createTime.UTC().Format(time.RFC3339Nano),
		UpdateTime: updateTime.UTC().Format(time.RFC3339Nano),
		Name: name,
		OriginalName: original,
		Relased: year,
		Magnet: strings.TrimSpace(it.Magnet),
		SizeName: formatSize(it.Size),
		Quality: parseQuality(valueOrEmpty(it.Quality)),
		VideoType: videotype,
		SearchName: core.SearchName(name),
		SearchOrig: core.SearchName(firstNonEmpty(original, name)),
	}.ToMap(), true
}

func (p *Parser) saveTorrents(items []filedb.TorrentDetails) (int, int, int, int, error) {
	added, updated, skipped, failed := 0, 0, 0, 0
	plog := core.NewParserLog(trackerName, filepath.Join(p.DB.DataDir, "log"), p.Config.LogParsers && p.Config.Aniliberty.Log)
	bucketCache := make(map[string]map[string]filedb.TorrentDetails, len(items))
	changed := make(map[string]time.Time, len(items))
	for _, incoming := range items {
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

func (p *Parser) fetch(ctx context.Context, urlv string) ([]byte, error) {
	data, status, err := p.Fetcher.Download(urlv, p.Config.Aniliberty)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("http status %d", status)
	}
	return data, nil
}

func determineTypes(typeValue string) []string {
	switch strings.ToUpper(strings.TrimSpace(typeValue)) {
	case "MOVIE":
		return []string{"anime", "movie"}
	case "OVA", "OAD":
		return []string{"anime", "ova"}
	case "SPECIAL":
		return []string{"anime", "special"}
	case "ONA", "WEB":
		return []string{"anime", "ona"}
	case "DORAMA":
		return []string{"dorama"}
	default:
		return []string{"anime", "serial"}
	}
}

func formatSize(bytes int64) string {
	if bytes < 1073741824 {
		return fmt.Sprintf("%.2f Mb", float64(bytes)/1048576.0)
	}
	if bytes < 1099511627776 {
		return fmt.Sprintf("%.2f GB", float64(bytes)/1073741824.0)
	}
	return fmt.Sprintf("%.2f TB", float64(bytes)/1099511627776.0)
}

func parseQuality(v string) int {
	q := strings.ToLower(strings.TrimSpace(v))
	if q == "" {
		return 480
	}
	if strings.Contains(q, "4k") || strings.Contains(q, "2160p") || strings.Contains(q, "uhd") {
		return 2160
	}
	if m := qualityNumRe.FindStringSubmatch(q); len(m) > 1 {
		n, _ := strconv.Atoi(m[1])
		switch {
		case n >= 2160:
			return 2160
		case n >= 1080:
			return 1080
		case n >= 720:
			return 720
		case n >= 480:
			return 480
		default:
			return n
		}
	}
	return 480
}
func extractQualityInfo(label string) string {
	m := qualityInfoRe.FindStringSubmatch(strings.TrimSpace(label))
	if len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}
func safeName(n *apiName, main bool) string {
	if n == nil {
		return ""
	}
	if main {
		return n.Main
	}
	return n.English
}
func releaseType(r *apiRelease) string {
	if r != nil && r.Type != nil {
		return r.Type.Value
	}
	return ""
}
func valueOrEmpty(v *apiValue) string {
	if v == nil {
		return ""
	}
	return v.Value
}
func buildBaseTitle(name, original string) string {
	name = strings.TrimSpace(name)
	original = strings.TrimSpace(original)
	if name != "" && original != "" && !strings.EqualFold(name, original) {
		return name + " / " + original
	}
	if name != "" {
		return name
	}
	if original != "" {
		return original
	}
	return "Unknown"
}
func parseAPITime(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Now().UTC()
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05.000000Z", "2006-01-02T15:04:05.000Z"} {
		if tm, err := time.Parse(layout, v); err == nil {
			return tm.UTC()
		}
	}
	return time.Now().UTC()
}
func fileTime(t filedb.TorrentDetails) time.Time {
	for _, key := range []string{"updateTime", "createTime"} {
		s := strings.TrimSpace(asString(t[key]))
		if s == "" {
			continue
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
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
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		return fmt.Sprintf("%v", v)
	}
}
func isDisabled(list []string, tracker string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(tracker)) {
			return true
		}
	}
	return false
}
