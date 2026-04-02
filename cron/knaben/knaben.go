package knaben

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

const (
	trackerName   = "knaben"
	minAPIDelayMs = 500
	maxSize       = 300
	maxPages      = 10
)

var defaultCategories = []int{2000000, 2001000, 2002000, 2003000, 2004000, 2005000, 2006000, 2007000, 2008000, 3000000, 3001000, 3002000, 3003000, 3004000, 3005000, 3006000, 3007000, 3008000}
var (
	yearRe            = regexp.MustCompile(`[\(\[](\d{4})[\)\]]`)
	yearStandaloneRe  = regexp.MustCompile(`\b(19|20)\d{2}\b`)
	cleanBracketsRe   = regexp.MustCompile(`\[[^\]]*\]`)
	seriesMatchRe     = regexp.MustCompile(`(?i)^(.+?)\s+S\d{1,2}E\d{1,2}\b`)
	yearBracketRe     = regexp.MustCompile(`[\(\[](\d{4})[\)\]]`)
	sxxexxRe          = regexp.MustCompile(`(?i)\b(S\d{1,2}E\d{1,2}|S\d{1,2}E?\d{0,2}|E\d{1,2}|\d{1,2}x\d{1,2})\b`)
	seasonWordRe      = regexp.MustCompile(`(?i)(?:^|\s)(Сезон|Season)\s*\d{1,2}\b.*$`)
	qualityWordRe     = regexp.MustCompile(`(?i)\b(2160p|1080p|720p|480p)\b`)
	hdrWordRe         = regexp.MustCompile(`(?i)\b(HDR10?|DV|HDR|SDR|10bit)\b`)
	sourceWordRe      = regexp.MustCompile(`(?i)\b(WEB[-\s]?DL|WEB[-\s]?Rip|WEB\b|BDRip|BDRemux|HDRip|BluRay|BRRip|DVDRip|HDTV)\b`)
	codecWordRe       = regexp.MustCompile(`(?i)\b(x264|x265|xvid|h\.?264|h\.?265|hevc|avc|aac|ac3|dts)\b`)
	serviceWordRe     = regexp.MustCompile(`(?i)\b(AMZN|NF|DS4K|DD\s*5\s*1|DD5\.?1|DDPA|DDP5\.?1|Atmos|DDP?\s*5\.?1|playWEB)\b`)
	subWordRe         = regexp.MustCompile(`(?i)\b(ESub|Sub)\b`)
	releaseGroupRe    = regexp.MustCompile(`(?i)[.\s]+-\s*[A-Za-z0-9][A-Za-z0-9.-]*$`)
	multiSpaceRe      = regexp.MustCompile(`\s{2,}`)
	bracketPipeRe     = regexp.MustCompile(`[\[\]|]`)
	res2160Re         = regexp.MustCompile(`(?i)\b2160p\b`)
	res1080Re         = regexp.MustCompile(`(?i)\b1080p\b`)
	res720Re          = regexp.MustCompile(`(?i)\b720p\b`)
	dotHdrRe          = regexp.MustCompile(`(?i)\.(HDR10?)\b`)
	dolbyVisionRe     = regexp.MustCompile(`(?i)(dolby\s*vision|10-?bit)`)
	hdrPresentRe      = regexp.MustCompile(`(?i)(\.|[\[,]| )hdr`)
	trackerSuffixRe   = regexp.MustCompile(`\s*\|\s+[^|]+$`)
)

type Parser struct {
	Config  app.Config
	DB      *filedb.DB
	Client  *http.Client
	mu      sync.Mutex
	working bool
}

type ParseResult struct {
	Fetched, Added, Updated, Skipped, Failed int
	Status                                   string
}

type apiRequest struct {
	Query                string `json:"query,omitempty"`
	SearchField          string `json:"search_field,omitempty"`
	SearchType           string `json:"search_type,omitempty"`
	Categories           []int  `json:"categories,omitempty"`
	OrderBy              string `json:"order_by,omitempty"`
	OrderDirection       string `json:"order_direction,omitempty"`
	From                 int    `json:"from"`
	Size                 int    `json:"size"`
	HideUnsafe           bool   `json:"hide_unsafe"`
	HideXxx              bool   `json:"hide_xxx"`
	SecondsSinceLastSeen *int   `json:"seconds_since_last_seen,omitempty"`
}

type apiResponse struct {
	Hits []hit `json:"hits"`
}
type hit struct {
	Title      string `json:"title"`
	Bytes      int64  `json:"bytes"`
	Seeders    int    `json:"seeders"`
	Peers      int    `json:"peers"`
	MagnetURL  string `json:"magnetUrl"`
	Link       string `json:"link"`
	Details    string `json:"details"`
	Category   string `json:"category"`
	CategoryID []int  `json:"categoryId"`
	Date       string `json:"date"`
	LastSeen   string `json:"lastSeen"`
	Tracker    string `json:"tracker"`
	TrackerID  string `json:"trackerId"`
	ID         string `json:"id"`
	Hash       string `json:"hash"`
}

func New(cfg app.Config, db *filedb.DB) *Parser {
	return &Parser{Config: cfg, DB: db, Client: &http.Client{Timeout: 20 * time.Second}}
}

func (p *Parser) Parse(ctx context.Context, from, size, pages int, query string, hours int, orderBy, categoriesRaw string) (ParseResult, error) {
	p.mu.Lock()
	if p.working {
		p.mu.Unlock()
		return ParseResult{Status: "work"}, nil
	}
	p.working = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.working = false; p.mu.Unlock() }()
	if strings.TrimSpace(p.Config.Knaben.Host) == "" {
		return ParseResult{Status: "config missing"}, nil
	}
	size = min(max(size, 1), maxSize)
	pages = min(max(pages, 1), maxPages)
	categories := parseCategories(categoriesRaw)
	query = strings.TrimSpace(query)
	if strings.TrimSpace(orderBy) == "" {
		orderBy = "date"
	}
	secondsSince := 0
	if hours > 0 {
		secondsSince = hours * 3600
	}
	res := ParseResult{Status: "ok"}
	var all []filedb.TorrentDetails
	for page := 0; page < pages; page++ {
		batch, err := p.fetchPage(ctx, from+page*size, size, secondsSince, query, orderBy, categories)
		if err != nil {
			return res, err
		}
		if len(batch) == 0 {
			break
		}
		all = append(all, batch...)
		res.Fetched += len(batch)
		if len(batch) < size {
			break
		}
	}
	if len(all) == 0 {
		return res, nil
	}
	res.Added, res.Updated, res.Skipped, res.Failed, _ = p.saveTorrents(ctx, all)
	log.Printf("knaben: done fetched=%d added=%d skipped=%d failed=%d", res.Fetched, res.Added, res.Skipped, res.Failed)
	return res, nil
}

func (p *Parser) fetchPage(ctx context.Context, from, size, secondsSince int, query, orderBy string, categories []int) ([]filedb.TorrentDetails, error) {
	reqBody := apiRequest{Categories: categories, OrderBy: orderByAllowed(orderBy), OrderDirection: "desc", From: from, Size: size, HideUnsafe: true, HideXxx: true}
	if query != "" {
		reqBody.Query = query
		reqBody.SearchField = "title"
	}
	if secondsSince > 0 {
		reqBody.SecondsSinceLastSeen = &secondsSince
	}
	payload, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.Config.Knaben.Host, "/")+"/v1", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(p.apiDelay()):
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("knaben api status %d", resp.StatusCode)
	}
	var parsed apiResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	out := make([]filedb.TorrentDetails, 0, len(parsed.Hits))
	for _, h := range parsed.Hits {
		if t := mapHit(h); t != nil {
			out = append(out, t)
		}
	}
	return out, nil
}

func (p *Parser) saveTorrents(ctx context.Context, torrents []filedb.TorrentDetails) (int, int, int, int, error) {
	added, updated, skipped, failed := 0, 0, 0, 0
	plog := core.NewParserLog(trackerName, filepath.Join(p.DB.DataDir, "log"))
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
		if exists && samePrimary(existing, incoming) {
			skipped++
			continue
		}
		magnet := strings.TrimSpace(asString(incoming["magnet"]))
		if magnet == "" {
			downloadURL := strings.TrimSpace(asString(incoming["_sn"]))
			if strings.HasPrefix(strings.ToLower(downloadURL), "http") {
				select {
				case <-ctx.Done():
					return added, updated, skipped, failed, ctx.Err()
				case <-time.After(p.apiDelay()):
				}
				b, err := p.download(ctx, downloadURL, asString(incoming["url"]))
				if err == nil {
					magnet = core.TorrentBytesToMagnet(b)
				}
			}
			if magnet == "" {
				failed++
				continue
			}
			incoming["magnet"] = magnet
			delete(incoming, "_sn")
		}
		bucket[urlv] = mergeTorrent(existing, exists, incoming)
		changed[key] = fileTime(bucket[urlv])
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
	if len(changed) > 0 {
		if err := p.DB.SaveChangesToFile(); err != nil {
			return added, updated, skipped, failed, err
		}
	}
	return added, updated, skipped, failed, nil
}

func (p *Parser) download(ctx context.Context, rawURL, referer string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(referer) != "" {
		req.Header.Set("Referer", referer)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("download status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 5<<20))
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
		out["name"] = asString(out["title"])
	}
	if strings.TrimSpace(asString(out["originalname"])) == "" {
		out["originalname"] = asString(out["name"])
	}
	out["_sn"] = core.SearchName(asString(out["name"]))
	out["_so"] = core.SearchName(asString(out["originalname"]))
	if fileTime(out).IsZero() {
		out["updateTime"] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	return out
}

func samePrimary(existing, incoming filedb.TorrentDetails) bool {
	return strings.TrimSpace(asString(existing["title"])) == strings.TrimSpace(asString(incoming["title"])) && strings.EqualFold(strings.TrimSpace(asString(existing["magnet"])), strings.TrimSpace(asString(incoming["magnet"])))
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

func mapHit(h hit) filedb.TorrentDetails {
	if strings.TrimSpace(h.Title) == "" {
		return nil
	}
	types := typesFromCategoryID(h.CategoryID)
	if len(types) == 0 {
		return nil
	}
	detailURL := strings.TrimSpace(h.Details)
	if detailURL == "" {
		detailURL = strings.TrimSpace(h.Link)
	}
	if detailURL == "" && strings.TrimSpace(h.ID) != "" {
		detailURL = "https://knaben.xyz/?id=" + url.QueryEscape(strings.TrimSpace(h.ID))
	}
	if detailURL == "" {
		return nil
	}
	title := strings.TrimSpace(htmlDecode(h.Title))
	createTime := parseDate(h.Date)
	if createTime.IsZero() {
		createTime = parseDate(h.LastSeen)
	}
	if createTime.IsZero() {
		createTime = time.Now().UTC()
	}
	updateTime := parseDate(h.LastSeen)
	if updateTime.IsZero() {
		updateTime = createTime
	}
	name, relased := parseNameAndYear(title)
	title = buildTitleForFileDB(title)
	if strings.TrimSpace(h.Tracker) != "" && !strings.Contains(title, h.Tracker) {
		title = title + " | " + strings.TrimSpace(h.Tracker)
	}
	res := filedb.TorrentRecord{TrackerName: trackerName, Types: types, URL: detailURL, Title: title, Sid: h.Seeders, Pir: h.Peers, Size: float64(h.Bytes), SizeName: formatSize(h.Bytes), CreateTime: createTime.Format(time.RFC3339Nano), UpdateTime: updateTime.Format(time.RFC3339Nano), Magnet: strings.TrimSpace(h.MagnetURL), Name: name, OriginalName: name, Relased: relased, Quality: qualityFromCategoryID(h.CategoryID)}.ToMap()
	if strings.TrimSpace(h.MagnetURL) == "" && strings.TrimSpace(h.Link) != "" {
		res["_sn"] = strings.TrimSpace(h.Link)
	}
	res["_so"] = core.SearchName(name)
	return res
}

func parseCategories(s string) []int {
	if strings.TrimSpace(s) == "" {
		return append([]int(nil), defaultCategories...)
	}
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' })
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return append([]int(nil), defaultCategories...)
	}
	return out
}
func orderByAllowed(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "seeders", "peers":
		return v
	default:
		return "date"
	}
}
func typesFromCategoryID(ids []int) []string {
	if len(ids) == 0 {
		return []string{"movie", "serial"}
	}
	for _, id := range ids {
		if id >= 2000000 && id < 3000000 {
			return []string{"serial"}
		}
		if id >= 3000000 && id < 4000000 {
			return []string{"movie"}
		}
	}
	return []string{"movie", "serial"}
}
func qualityFromCategoryID(ids []int) int {
	for _, id := range ids {
		switch id {
		case 2003000, 3003000:
			return 2160
		case 2001000, 3001000:
			return 1080
		case 2002000, 3002000:
			return 720
		}
	}
	return 480
}
// ParseNameAndYear is the exported version of parseNameAndYear for use by server/admin.go.
func ParseNameAndYear(title string) (string, int) { return parseNameAndYear(title) }

// BuildTitleForFileDB is the exported version of buildTitleForFileDB.
func BuildTitleForFileDB(title string) string { return buildTitleForFileDB(title) }

func parseNameAndYear(title string) (string, int) {
	title = strings.TrimSpace(title)
	if title == "" {
		return "", 0
	}
	name := trackerSuffixRe.ReplaceAllString(title, "")
	name = strings.TrimSpace(name)
	if name == "" {
		return title, 0
	}
	relased := 0
	if m := yearBracketRe.FindStringSubmatchIndex(name); len(m) >= 4 {
		if y, err := strconv.Atoi(name[m[2]:m[3]]); err == nil {
			relased = y
			if m[0] > 0 {
				name = strings.TrimSpace(strings.TrimRight(name[:m[0]], " /-|"))
			}
		}
	} else if m := yearStandaloneRe.FindString(name); m != "" {
		if y, err := strconv.Atoi(m); err == nil {
			relased = y
			name = strings.TrimSpace(yearStandaloneRe.ReplaceAllString(name, ""))
		}
	}
	name = cleanTitleForSearch(name)
	if name == "" {
		return title, relased
	}
	return name, relased
}

func cleanTitleForSearch(title string) string {
	if strings.TrimSpace(title) == "" {
		return title
	}
	t := strings.TrimSpace(title)
	t = cleanBracketsRe.ReplaceAllString(t, " ")
	if m := seriesMatchRe.FindStringSubmatch(t); len(m) > 1 && len(m[1]) > 0 {
		t = strings.TrimSpace(m[1])
	} else {
		if idx := yearBracketRe.FindStringIndex(t); idx != nil && idx[0] > 0 {
			t = t[:idx[0]]
		}
		t = sxxexxRe.ReplaceAllString(t, "")
		t = seasonWordRe.ReplaceAllString(t, "")
	}
	t = qualityWordRe.ReplaceAllString(t, "")
	t = hdrWordRe.ReplaceAllString(t, "")
	t = sourceWordRe.ReplaceAllString(t, "")
	t = codecWordRe.ReplaceAllString(t, "")
	t = serviceWordRe.ReplaceAllString(t, "")
	t = subWordRe.ReplaceAllString(t, "")
	t = strings.ReplaceAll(t, ".", " ")
	t = bracketPipeRe.ReplaceAllString(t, " ")
	t = multiSpaceRe.ReplaceAllString(t, " ")
	t = strings.TrimRight(strings.TrimSpace(t), " /-|")
	t = releaseGroupRe.ReplaceAllString(t, "")
	t = strings.TrimRight(strings.TrimSpace(t), " -")
	if strings.TrimSpace(t) == "" {
		return title
	}
	return t
}

func buildTitleForFileDB(title string) string {
	if strings.TrimSpace(title) == "" {
		return title
	}
	t := strings.TrimSpace(title)
	t = res2160Re.ReplaceAllString(t, "2160p")
	t = res1080Re.ReplaceAllString(t, "1080p")
	t = res720Re.ReplaceAllString(t, "720p")
	t = dotHdrRe.ReplaceAllString(t, " $1")
	if dolbyVisionRe.MatchString(t) && !hdrPresentRe.MatchString(t) {
		t += " HDR"
	}
	return t
}

func parseDate(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if tm, err := time.Parse(layout, s); err == nil {
			return tm.UTC()
		}
	}
	return time.Time{}
}
func formatSize(bytes int64) string {
	const (
		mb = 1024 * 1024
		gb = 1024 * mb
		tb = 1024 * gb
	)
	switch {
	case bytes < gb:
		return fmt.Sprintf("%.2f Mb", float64(bytes)/float64(mb))
	case bytes < tb:
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(gb))
	default:
		return fmt.Sprintf("%.2f TB", float64(bytes)/float64(tb))
	}
}
func htmlDecode(s string) string {
	return strings.NewReplacer("&amp;", "&", "&quot;", "\"", "&#39;", "'", "&lt;", "<", "&gt;", ">").Replace(s)
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
func (p *Parser) apiDelay() time.Duration {
	ms := p.Config.Knaben.ParseDelay
	if ms < minAPIDelayMs {
		ms = minAPIDelayMs
	}
	return time.Duration(ms) * time.Millisecond
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
