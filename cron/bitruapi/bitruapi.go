package bitruapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"jacred/app"
	"jacred/core"
	"jacred/filedb"
)

const (
	apiGetTorrents = "torrents"
	apiDelayMs     = 250
)

var detailsIDRe = regexp.MustCompile(`\?id=(\d+)`)

type Parser struct {
	Config  app.Config
	DB      *filedb.DB
	Client  *http.Client
	DataDir string
	mu      sync.Mutex
	working bool
}

type ParseResult struct {
	Fetched, Added, Updated, Skipped, Failed int
	Status                                   string
}

type apiResponse struct {
	Error   bool          `json:"error"`
	Message string        `json:"message"`
	Result  *resultObject `json:"result"`
}

type resultObject struct {
	Items      []itemWrap `json:"items"`
	BeforeDate any        `json:"before_date"`
}

type itemWrap struct {
	Item *apiItem `json:"item"`
}

type apiItem struct {
	Torrent  *torrentInfo  `json:"torrent"`
	Info     *infoBlock    `json:"info"`
	Template *templateInfo `json:"template"`
}

type torrentInfo struct {
	ID       flexInt64 `json:"id"`
	Size     flexInt64 `json:"size"`
	Added    flexInt64 `json:"added"`
	Seeders  flexInt   `json:"seeders"`
	Leechers flexInt   `json:"leechers"`
	File     string    `json:"file"`
}

// flexInt64 unmarshals from both JSON number and string
type flexInt64 int64

func (f *flexInt64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		*f = 0
		return nil
	}
	*f = flexInt64(n)
	return nil
}

// flexInt unmarshals from both JSON number and string
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		*f = 0
		return nil
	}
	*f = flexInt(n)
	return nil
}

type infoBlock struct {
	Name string `json:"name"`
	Year any    `json:"year"`
}

type templateInfo struct {
	Category string      `json:"category"`
	OrigName string      `json:"orig_name"`
	Video    *videoBlock `json:"video"`
	Other    string      `json:"other"`
}

type videoBlock struct {
	Quality string `json:"quality"`
}

func New(cfg app.Config, db *filedb.DB, dataDir string) *Parser {
	if strings.TrimSpace(dataDir) == "" {
		dataDir = "Data"
	}
	return &Parser{Config: cfg, DB: db, DataDir: dataDir, Client: &http.Client{Timeout: 20 * time.Second}}
}

func (p *Parser) Parse(ctx context.Context, limit int) (ParseResult, error) {
	return p.parseInternal(ctx, "", limit)
}

func (p *Parser) ParseFromDate(ctx context.Context, lastnewtor string, limit int) (ParseResult, error) {
	lastnewtor = strings.TrimSpace(lastnewtor)
	if lastnewtor == "" {
		return ParseResult{Status: "bad lastnewtor (use dd.MM.yyyy)"}, nil
	}
	if _, err := time.Parse("02.01.2006", lastnewtor); err != nil {
		return ParseResult{Status: "bad date format (use dd.MM.yyyy)"}, nil
	}
	return p.parseInternal(ctx, lastnewtor, limit)
}

func (p *Parser) parseInternal(ctx context.Context, lastnewtor string, limit int) (ParseResult, error) {
	p.mu.Lock()
	if p.working {
		p.mu.Unlock()
		return ParseResult{Status: "work"}, nil
	}
	p.working = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.working = false; p.mu.Unlock() }()

	if limit <= 0 || limit > 100 {
		limit = 100
	}
	var afterDate *int64
	if lastnewtor != "" {
		fromDate, _ := time.Parse("02.01.2006", lastnewtor)
		utcNow := time.Now().UTC()
		if utcNow.Year() == fromDate.Year() && utcNow.Month() == fromDate.Month() && utcNow.Day() == fromDate.Day() {
			zero := int64(0)
			afterDate = &zero
		} else {
			v := time.Date(fromDate.Year(), fromDate.Month(), fromDate.Day(), 0, 0, 0, 0, time.UTC).Unix()
			afterDate = &v
		}
	}
	torrents, err := p.fetchTorrentsFromAPI(ctx, limit, afterDate)
	if err != nil {
		return ParseResult{}, err
	}
	res := ParseResult{Status: "ok", Fetched: len(torrents)}
	if len(torrents) == 0 {
		return res, nil
	}
	res.Added, res.Updated, res.Skipped, res.Failed, err = p.saveTorrentsAndMagnets(ctx, torrents)
	if err != nil {
		return res, err
	}
	_ = p.writeLastNewTor(torrents)
	log.Printf("bitruapi: done fetched=%d added=%d skipped=%d failed=%d", res.Fetched, res.Added, res.Skipped, res.Failed)
	return res, nil
}

func (p *Parser) fetchTorrentsFromAPI(ctx context.Context, limit int, afterDate *int64) ([]filedb.TorrentDetails, error) {
	all := make([]filedb.TorrentDetails, 0, limit)
	currentParams := map[string]any{"limit": limit, "category": []string{"movie", "serial"}}
	if afterDate != nil {
		currentParams["after_date"] = strconv.FormatInt(*afterDate, 10)
	}
	for page := 0; page < 50; page++ {
		resp, err := p.apiRequestAsync(ctx, currentParams)
		if err != nil {
			return all, err
		}
		if resp == nil || resp.Error || resp.Result == nil || resp.Result.Items == nil {
			break
		}
		for _, wrap := range resp.Result.Items {
			if wrap.Item == nil {
				continue
			}
			if t := p.mapToTorrentDetails(wrap.Item); t != nil {
				all = append(all, t)
			}
		}
		if len(resp.Result.Items) == 0 {
			break
		}
		beforeUnix := parseAnyInt64(resp.Result.BeforeDate)
		if beforeUnix == 0 {
			break
		}
		currentParams = map[string]any{"limit": limit, "category": []string{"movie", "serial"}, "before_date": strconv.FormatInt(beforeUnix, 10)}
	}
	return all, nil
}

func (p *Parser) apiRequestAsync(ctx context.Context, jsonParams map[string]any) (*apiResponse, error) {
	b, _ := json.Marshal(jsonParams)
	form := url.Values{}
	form.Set("get", apiGetTorrents)
	form.Set("json", string(b))
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(apiDelay()):
	}
	apiURL := strings.TrimRight(p.Config.Bitru.Host, "/") + "/api.php"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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
		return nil, fmt.Errorf("bitru api status %d", resp.StatusCode)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, nil
	}
	var out apiResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (p *Parser) mapToTorrentDetails(item *apiItem) filedb.TorrentDetails {
	if item == nil || item.Torrent == nil || item.Info == nil || item.Template == nil {
		return nil
	}
	category := strings.ToLower(strings.TrimSpace(item.Template.Category))
	var types []string
	switch category {
	case "movie":
		types = []string{"movie"}
	case "serial":
		types = []string{"serial"}
	default:
		return nil
	}
	name := strings.TrimSpace(item.Info.Name)
	originalname := strings.TrimSpace(item.Template.OrigName)
	yearDisplay := bitruYearToDisplayString(item.Info.Year)
	relased := bitruYearToReleased(item.Info.Year)
	titlePart := name
	if originalname != "" {
		titlePart += " / " + originalname
	}
	if yearDisplay != "" {
		titlePart += " (" + yearDisplay + ")"
	}
	if item.Template.Video != nil && strings.TrimSpace(item.Template.Video.Quality) != "" {
		titlePart += " " + strings.TrimSpace(item.Template.Video.Quality)
	}
	if strings.TrimSpace(item.Template.Other) != "" {
		titlePart += " | " + strings.TrimSpace(item.Template.Other)
	}
	detailURL := strings.TrimRight(p.Config.Bitru.Host, "/") + "/details.php?id=" + strconv.FormatInt(int64(item.Torrent.ID), 10)
	createTime := time.Unix(int64(item.Torrent.Added), 0).UTC()
	res := filedb.TorrentRecord{
		TrackerName: "bitru",
		Types: types,
		URL: detailURL,
		Title: htmlDecode(strings.TrimSpace(titlePart)),
		Sid: int(item.Torrent.Seeders),
		Pir: int(item.Torrent.Leechers),
		Size: float64(item.Torrent.Size),
		SizeName: formatSize(int64(item.Torrent.Size)),
		CreateTime: createTime.Format(time.RFC3339Nano),
		Name: name,
		Relased: relased,
		SearchName: strings.TrimSpace(item.Torrent.File),
	}.ToMap()
	if originalname != "" {
		res["originalname"] = originalname
	}
	res["_so"] = core.SearchName(originalname)
	if res["_so"] == "" {
		res["_so"] = core.SearchName(name)
	}
	return res
}

func (p *Parser) saveTorrentsAndMagnets(ctx context.Context, torrents []filedb.TorrentDetails) (int, int, int, int, error) {
	added, updated, skipped, failed := 0, 0, 0, 0
	plog := core.NewParserLog("bitruapi", filepath.Join(p.DB.DataDir, "log"))
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
		if exists && strings.TrimSpace(asString(existing["title"])) == strings.TrimSpace(asString(incoming["title"])) {
			skipped++
			continue
		}
		downloadURL := strings.TrimSpace(asString(incoming["_sn"]))
		if !strings.HasPrefix(strings.ToLower(downloadURL), "http") {
			if m := detailsIDRe.FindStringSubmatch(urlv); len(m) == 2 {
				downloadURL = strings.TrimRight(p.Config.Bitru.Host, "/") + "/download.php?id=" + m[1]
			}
		}
		if strings.TrimSpace(downloadURL) == "" {
			failed++
			continue
		}
		select {
		case <-ctx.Done():
			return added, updated, skipped, failed, ctx.Err()
		case <-time.After(apiDelay()):
		}
		b, err := p.download(ctx, downloadURL, strings.TrimRight(p.Config.Bitru.Host, "/")+"/")
		magnet := ""
		if err == nil {
			magnet = core.TorrentBytesToMagnet(b)
		}
		if strings.TrimSpace(magnet) == "" {
			failed++
			continue
		}
		incoming["magnet"] = magnet
		incoming["_sn"] = nil
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

func (p *Parser) writeLastNewTor(torrents []filedb.TorrentDetails) error {
	if len(torrents) == 0 {
		return nil
	}
	var maxTime time.Time
	for _, t := range torrents {
		tm := fileTime(t)
		if tm.After(maxTime) {
			maxTime = tm
		}
	}
	if maxTime.IsZero() {
		return nil
	}
	path := filepath.Join(p.DataDir, "temp", "bitruapi_lastnewtor.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(maxTime.UTC().Format("02.01.2006")), 0o644)
}

func apiDelay() time.Duration { return apiDelayMs * time.Millisecond }

func bitruYearToDisplayString(year any) string {
	if year == nil {
		return ""
	}
	switch v := year.(type) {
	case float64:
		return strconv.FormatInt(int64(v), 10)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case json.Number:
		return v.String()
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func bitruYearToReleased(year any) int {
	s := bitruYearToDisplayString(year)
	if s == "" {
		return 0
	}
	if dash := strings.IndexByte(s, '-'); dash > 0 {
		s = strings.TrimSpace(s[:dash])
	}
	n, _ := strconv.Atoi(s)
	return n
}

func formatSize(bytes int64) string {
	if bytes < 1000*1024 {
		return fmt.Sprintf("%.2f КБ", float64(bytes)/1024.0)
	}
	if bytes < 1000*1048576 {
		return fmt.Sprintf("%.2f МБ", float64(bytes)/1048576.0)
	}
	if bytes < 1000*1073741824 {
		return fmt.Sprintf("%.2f ГБ", float64(bytes)/1073741824.0)
	}
	return fmt.Sprintf("%.2f ТБ", float64(bytes)/1099511627776.0)
}

func parseAnyInt64(v any) int64 {
	switch x := v.(type) {
	case nil:
		return 0
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n
	default:
		n, _ := strconv.ParseInt(strings.TrimSpace(fmt.Sprint(x)), 10, 64)
		return n
	}
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
		out["originalname"] = out["name"]
	}
	out["_sn"] = core.SearchName(asString(out["name"]))
	out["_so"] = core.SearchName(asString(out["originalname"]))
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

// CleanTitleForSearch strips quality tags, season/episode markers, and release group suffixes.
// Port of C# BitruApiController.CleanTitleForSearch.
func CleanTitleForSearch(title string) string {
	if strings.TrimSpace(title) == "" {
		return title
	}
	t := strings.TrimSpace(title)
	if m := bitruYearBracketRe.FindStringIndex(t); m != nil && m[0] > 0 {
		t = t[:m[0]]
	}
	t = bitruSxxExxRe.ReplaceAllString(t, "")
	t = bitruSeasonWordRe.ReplaceAllString(t, "")
	t = bitruSeasonWordRe2.ReplaceAllString(t, "")
	t = bitruQualityRe.ReplaceAllString(t, "")
	t = bitruSourceRe.ReplaceAllString(t, "")
	t = bitruCodecRe.ReplaceAllString(t, "")
	t = bitruBracketsRe.ReplaceAllString(t, " ")
	t = bitruMultiSpaceRe.ReplaceAllString(t, " ")
	t = strings.TrimRight(strings.TrimSpace(t), " /|-")
	t = bitruReleaseGroupRe.ReplaceAllString(t, "")
	t = strings.TrimRight(strings.TrimSpace(t), " -")
	if strings.TrimSpace(t) == "" {
		return title
	}
	return t
}

var (
	bitruYearBracketRe  = regexp.MustCompile(`[(\[]\d{4}[)\]]`)
	bitruSxxExxRe       = regexp.MustCompile(`(?i)\b(S\d{1,2}E\d{1,2}|S\d{1,2}E?\d{0,2}|E\d{1,2}|\d{1,2}x\d{1,2})\b`)
	bitruSeasonWordRe   = regexp.MustCompile(`(?i)\s*\d{1,2}(-\d{1,2})?\s*сезон\s*.*$`)
	bitruSeasonWordRe2  = regexp.MustCompile(`(?i)\b(Сезон|Season)\s*\d{1,2}\b.*$`)
	bitruQualityRe      = regexp.MustCompile(`(?i)\b(2160p|1080p|720p|480p)\b`)
	bitruSourceRe       = regexp.MustCompile(`(?i)\b(WEB[-\s]?DL|WEB[-\s]?Rip|BDRip|BDRemux|HDRip|BluRay|BRRip|DVDRip|HDTV)\b`)
	bitruCodecRe        = regexp.MustCompile(`(?i)\b(x264|x265|h\.?264|h\.?265|hevc|avc|aac|ac3|dts)\b`)
	bitruBracketsRe     = regexp.MustCompile(`[\[\]|]`)
	bitruMultiSpaceRe   = regexp.MustCompile(`\s{2,}`)
	bitruReleaseGroupRe = regexp.MustCompile(`(?i)[.\s]+-\s*[A-Za-z0-9][A-Za-z0-9.-]*$`)
)

func htmlDecode(s string) string {
	return strings.NewReplacer("&amp;", "&", "&quot;", "\"", "&#39;", "'", "&lt;", "<", "&gt;", ">", "&nbsp;", " ").Replace(s)
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
