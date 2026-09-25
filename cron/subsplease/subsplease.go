// Package subsplease parses subsplease.org, a public anime release group.
//
// Everything comes from one JSON API with no account and no Cloudflare, which
// makes this the simplest tracker in the tree — and the reason the interesting
// decisions here are about what counts as a failure rather than about parsing.
package subsplease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"jacred/app"
	"jacred/core"
	"jacred/filedb"
)

const trackerName = "subsplease"

// preferredRes is the only resolution stored. The API also publishes 480 and
// 720 for every release; keeping all three would put three records under one
// name for the same episode, which is noise in an index whose consumers filter
// by quality anyway.
const preferredRes = "1080"

const (
	defaultPages     = 2
	maxPages         = 50
	defaultShowLimit = 50
	maxShowLimit     = 200
)

// errLimitReached marks the API refusing to serve more — it answers 200 with a
// "limit_reached" payload rather than 429. It is not an empty catalogue, and
// the two must not be reported the same way: a quiet day and a throttled run
// look identical from the outside, which is how a dead tracker stays invisible.
var errLimitReached = errors.New("subsplease: api answered limit_reached")

var (
	// The show id lives on the release table of a /shows/<slug>/ page. Verified
	// against the live page: <table id="show-release-table" … sid="1224">.
	showSidRe = regexp.MustCompile(`(?is)<table[^>]*id=["']show-release-table["'][^>]*\bsid=["'](\d+)["']`)
	// Attribute order is not guaranteed, so accept sid before id as well.
	showSidLooseRe = regexp.MustCompile(`(?is)id=["']show-release-table["'][^>]*\bsid=["'](\d+)["']|\bsid=["'](\d+)["'][^>]*id=["']show-release-table["']`)
	showLinkRe     = regexp.MustCompile(`(?i)href=["']/shows/([^"'/]+)/?["']`)
	magnetXlRe     = regexp.MustCompile(`(?i)[?&]xl=(\d+)`)
	batchRangeRe   = regexp.MustCompile(`^\d+\s*~\s*\d+$`)
)

// release is one entry of the API's object-of-objects. f=latest keys them by
// "<show> - <episode>"; f=show nests the same shape under "batch"/"episode".
type release struct {
	Show        string     `json:"show"`
	Episode     string     `json:"episode"`
	Page        string     `json:"page"`
	ReleaseDate string     `json:"release_date"`
	Time        string     `json:"time"`
	ImageURL    string     `json:"image_url"`
	Xdcc        string     `json:"xdcc"`
	Downloads   []download `json:"downloads"`
}

type download struct {
	Res    string `json:"res"`
	Magnet string `json:"magnet"`
}

type Parser struct {
	Config  app.Config
	DB      *filedb.DB
	DataDir string
	Fetcher *core.Fetcher

	mu           sync.Mutex
	working      bool
	showsWorking bool
}

type ParseResult struct {
	Fetched, Added, Updated, Skipped, Failed int
	Status                                   string
}

func New(cfg app.Config, db *filedb.DB, dataDir string) *Parser {
	return &Parser{Config: cfg, DB: db, DataDir: dataDir, Fetcher: core.NewFetcher(cfg)}
}

// ---------------------------------------------------------------- latest --

// Parse walks the f=latest pages, newest first.
func (p *Parser) Parse(ctx context.Context, pages int) (ParseResult, error) {
	p.mu.Lock()
	if p.working {
		p.mu.Unlock()
		return ParseResult{Status: "work"}, nil
	}
	p.working = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.working = false; p.mu.Unlock() }()

	host := strings.TrimRight(p.Config.SubsPlease.Host, "/")
	if host == "" {
		return ParseResult{Status: "config missing"}, nil
	}

	pageCap := clamp(pages, 1, maxPages)
	if pages <= 0 {
		pageCap = defaultPages
	}

	res := ParseResult{Status: "ok"}
	for i := 0; i < pageCap; i++ {
		if err := ctx.Err(); err != nil {
			res.Status = "canceled"
			return res, err
		}
		if i > 0 {
			if err := p.delay(ctx); err != nil {
				res.Status = "canceled"
				return res, err
			}
		}

		u := host + "/api/?f=latest&tz=UTC"
		if i > 0 {
			u = fmt.Sprintf("%s&p=%d", u, i)
		}

		body, err := p.fetchJSON(u)
		if err != nil {
			// The first page failing means the run collected nothing, and a
			// run that collects nothing must say so rather than report a
			// clean zero that reads as "no new releases today".
			if i == 0 {
				res.Status = statusFor(err)
				return res, fmt.Errorf("subsplease: latest page 0: %w", err)
			}
			log.Printf("subsplease: latest page %d failed, keeping %d record(s): %v", i, res.Added+res.Updated, err)
			res.Failed++
			break
		}

		items, err := parseLatestJSON(body, host)
		if err != nil {
			if i == 0 {
				return res, fmt.Errorf("subsplease: latest page 0: %w", err)
			}
			log.Printf("subsplease: latest page %d unparseable: %v", i, err)
			res.Failed++
			break
		}
		if len(items) == 0 {
			break
		}
		res.Fetched += len(items)

		added, updated, skipped, failed, err := p.saveTorrents(items)
		res.Added, res.Updated, res.Skipped, res.Failed = res.Added+added, res.Updated+updated, res.Skipped+skipped, res.Failed+failed
		if err != nil {
			return res, err
		}
		log.Printf("subsplease: latest page %d/%d fetched=%d added=%d updated=%d skipped=%d failed=%d",
			i+1, pageCap, len(items), added, updated, skipped, failed)
	}

	log.Printf("subsplease: done fetched=%d added=%d updated=%d skipped=%d failed=%d",
		res.Fetched, res.Added, res.Updated, res.Skipped, res.Failed)
	return res, nil
}

// ----------------------------------------------------------------- shows --

// ParseShows sweeps the whole catalogue a batch at a time, so the f=show
// endpoint (the only one carrying batch releases and back-catalogue episodes)
// is covered without one enormous run. The cursor is persisted, so consecutive
// runs continue rather than restart.
func (p *Parser) ParseShows(ctx context.Context, limit int, reset bool) (ParseResult, error) {
	p.mu.Lock()
	if p.showsWorking {
		p.mu.Unlock()
		return ParseResult{Status: "work"}, nil
	}
	p.showsWorking = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.showsWorking = false; p.mu.Unlock() }()

	host := strings.TrimRight(p.Config.SubsPlease.Host, "/")
	if host == "" {
		return ParseResult{Status: "config missing"}, nil
	}

	take := clamp(limit, 1, maxShowLimit)
	if limit <= 0 {
		take = defaultShowLimit
	}

	state := p.loadCheckpoint(reset)
	res := ParseResult{Status: "ok"}

	// Airing shows first: they are the ones gaining episodes, and a run that is
	// cut short should have spent its budget on them.
	var schedule []string
	if body, err := p.fetchJSON(host + "/api/?f=schedule&tz=UTC"); err == nil {
		schedule = parseSchedulePageSlugs(body)
	} else {
		log.Printf("subsplease: schedule unavailable, falling back to catalogue order: %v", err)
	}
	state.SchedulePriority = schedule

	if err := p.delay(ctx); err != nil {
		res.Status = "canceled"
		return res, err
	}

	indexHTML, err := p.fetchText(host + "/shows/")
	if err != nil {
		res.Status = statusFor(err)
		return res, fmt.Errorf("subsplease: show index: %w", err)
	}
	catalog := parseShowSlugsFromIndexHTML(indexHTML)

	workOrder := mergeUnique(schedule, catalog)
	if len(workOrder) == 0 {
		// Both the schedule and the catalogue came back without a single slug.
		// That is the markup or the API changing under us, never a real state
		// of the site, so it has to be an error and not an empty success.
		return res, errors.New("subsplease: neither the schedule nor the catalogue yielded a show")
	}
	for _, slug := range workOrder {
		state.ensure(slug)
	}

	start := clamp(state.Cursor, 0, len(workOrder))
	if state.Cursor < 0 || state.Cursor > len(workOrder) {
		start = 0
	}
	processed := 0

	for i := 0; i < take && start+i < len(workOrder); i++ {
		if err := ctx.Err(); err != nil {
			p.persistCheckpoint(state)
			res.Status = "canceled"
			return res, err
		}
		slug := workOrder[start+i]
		if processed > 0 {
			if err := p.delay(ctx); err != nil {
				p.persistCheckpoint(state)
				res.Status = "canceled"
				return res, err
			}
		}

		entry := state.ensure(slug)
		sid := entry.Sid
		if strings.TrimSpace(sid) == "" {
			html, err := p.fetchText(host + "/shows/" + slug + "/")
			if err != nil {
				log.Printf("subsplease: show %s page failed: %v", slug, err)
				res.Failed++
				processed++
				continue
			}
			sid = extractShowSid(html)
			if sid == "" {
				log.Printf("subsplease: show %s has no sid on its page", slug)
				res.Failed++
				processed++
				continue
			}
			entry.Sid = sid
			p.persistCheckpoint(state)
			if err := p.delay(ctx); err != nil {
				p.persistCheckpoint(state)
				res.Status = "canceled"
				return res, err
			}
		}

		body, err := p.fetchJSON(host + "/api/?f=show&tz=UTC&sid=" + url.QueryEscape(sid))
		if err != nil {
			log.Printf("subsplease: show %s (sid=%s) failed: %v", slug, sid, err)
			res.Failed++
			processed++
			// A throttled API will refuse every remaining show too, so stop
			// and keep the cursor where it is instead of burning the batch.
			if errors.Is(err, errLimitReached) {
				res.Status = "rate_limited"
				break
			}
			continue
		}

		items, err := parseShowJSON(body, host, slug)
		if err != nil {
			log.Printf("subsplease: show %s (sid=%s) unparseable: %v", slug, sid, err)
			res.Failed++
			processed++
			continue
		}
		res.Fetched += len(items)

		added, updated, skipped, failed, err := p.saveTorrents(items)
		res.Added, res.Updated, res.Skipped, res.Failed = res.Added+added, res.Updated+updated, res.Skipped+skipped, res.Failed+failed
		if err != nil {
			p.persistCheckpoint(state)
			return res, err
		}

		entry.Title = firstName(items, entry.Title)
		entry.LastFetched = time.Now().UTC().Format(time.RFC3339)
		entry.BatchCount, entry.EpisodeCount = countSections(items)

		processed++
		state.Cursor = start + processed
		state.UpdatedAt = entry.LastFetched
		p.persistCheckpoint(state)
	}

	if state.Cursor >= len(workOrder) {
		state.Cursor = 0 // catalogue swept; the next run starts over
	}
	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	p.persistCheckpoint(state)

	log.Printf("subsplease: shows done processed=%d cursor=%d catalog=%d fetched=%d added=%d updated=%d skipped=%d failed=%d",
		processed, state.Cursor, len(workOrder), res.Fetched, res.Added, res.Updated, res.Skipped, res.Failed)
	return res, nil
}

// ------------------------------------------------------------- parsing ----

// parseLatestJSON reads the f=latest (and f=search) shape: a flat object whose
// keys are "<show> - <episode>" and whose values are releases.
func parseLatestJSON(body, host string) ([]filedb.TorrentDetails, error) {
	if isLimitReached(body) {
		return nil, errLimitReached
	}
	if strings.TrimSpace(body) == "" || strings.TrimSpace(body) == "[]" {
		return nil, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		return nil, fmt.Errorf("decode latest: %w", err)
	}

	out := make([]filedb.TorrentDetails, 0, len(root))
	for _, key := range sortedKeys(root) {
		var rel release
		if err := json.Unmarshal(root[key], &rel); err != nil {
			continue
		}
		if rec := buildTorrent(rel, host, ""); rec != nil {
			out = append(out, rec)
		}
	}
	return out, nil
}

// parseShowJSON reads the f=show shape: "batch" and "episode" bags of the same
// release objects. Entries there carry no "page", so the slug we asked for is
// substituted — without it buildTorrent cannot form a URL and drops the row.
func parseShowJSON(body, host, pageSlug string) ([]filedb.TorrentDetails, error) {
	if isLimitReached(body) {
		return nil, errLimitReached
	}
	if strings.TrimSpace(body) == "" || strings.TrimSpace(body) == "[]" {
		return nil, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		return nil, fmt.Errorf("decode show: %w", err)
	}

	var out []filedb.TorrentDetails
	for _, section := range []string{"batch", "episode"} {
		raw, ok := root[section]
		if !ok {
			continue
		}
		// The API sends an empty *array* for a section with nothing in it,
		// while a populated one is an object — decoding must tolerate both.
		var bag map[string]json.RawMessage
		if err := json.Unmarshal(raw, &bag); err != nil {
			continue
		}
		for _, key := range sortedKeys(bag) {
			var rel release
			if err := json.Unmarshal(bag[key], &rel); err != nil {
				continue
			}
			if strings.TrimSpace(rel.Page) == "" {
				rel.Page = pageSlug
			}
			if rec := buildTorrent(rel, host, section); rec != nil {
				out = append(out, rec)
			}
		}
	}
	return out, nil
}

// buildTorrent turns one release into a record, or nil when it carries no
// 1080p download.
func buildTorrent(rel release, host, section string) filedb.TorrentDetails {
	show := strings.TrimSpace(rel.Show)
	episode := strings.TrimSpace(rel.Episode)
	page := strings.TrimSpace(rel.Page)
	if show == "" || episode == "" || page == "" {
		return nil
	}

	// The API also publishes a .torrent URL per download. It is deliberately
	// ignored: the magnet carries the info hash, the trackers and the exact
	// byte count, so fetching the file would add a request per release and
	// tell us nothing new.
	var magnet string
	for _, d := range rel.Downloads {
		if strings.EqualFold(strings.TrimSpace(d.Res), preferredRes) {
			magnet = strings.TrimSpace(d.Magnet)
			break
		}
	}
	if magnet == "" {
		return nil
	}

	isBatch := strings.EqualFold(section, "batch") || isBatchEpisode(episode)
	created := parseReleaseDate(rel.ReleaseDate)
	if created.IsZero() {
		created = time.Now().UTC()
	}

	rec := filedb.TorrentRecord{
		TrackerName: trackerName,
		Types:       []string{"anime"},
		URL:         buildURL(host, page, episode),
		Title:       buildTitle(show, episode, isBatch),
		Name:        show,
		// SubsPlease publishes romaji titles only, so the Latin name is both
		// the display name and the original one. Storing it twice is what
		// makes the bucket key stable for a show with no Russian title.
		OriginalName: show,
		Sid:          1,
		Pir:          0,
		// Quality is deliberately NOT set here even though it is known to be
		// 1080. filedb.UpdateFullDetails skips a record whose quality and _sn
		// are both already populated, and mergeNew populates _sn — so a parser
		// that fills in quality itself makes every NEW record look
		// "already processed" and silently loses seasons, videotype, voices
		// and languages. (An update recovers them: mergeExisting deletes the
		// computed fields first.) The title carries "1080p", which is what
		// UpdateFullDetails reads, so the value comes out the same.
		SizeName:   formatSize(magnetSizeBytes(magnet)),
		Magnet:     magnet,
		CreateTime: created.Format(time.RFC3339),
		UpdateTime: time.Now().UTC().Format(time.RFC3339),
	}
	return rec.ToMap()
}

// buildURL is the record's identity. Episode and resolution are in it because
// one show has many episodes and dedup is by URL — without the episode every
// release of a show would overwrite the previous one.
func buildURL(host, page, episode string) string {
	return fmt.Sprintf("%s/shows/%s/?ep=%s&res=%s",
		strings.TrimRight(host, "/"), strings.Trim(page, "/"), url.QueryEscape(episode), preferredRes)
}

func buildTitle(show, episode string, isBatch bool) string {
	ep := strings.TrimSpace(episode)
	if ep == "" {
		ep = "?"
	}
	title := fmt.Sprintf("[SubsPlease] %s - %s (%sp)", strings.TrimSpace(show), ep, preferredRes)
	if isBatch {
		title += " [Batch]"
	}
	return title
}

// isBatchEpisode recognises the season-pack forms the API puts in the episode
// field ("01-12", "1 ~ 12") rather than a single number.
func isBatchEpisode(episode string) bool {
	ep := strings.TrimSpace(episode)
	if ep == "" {
		return false
	}
	if strings.Contains(ep, "-") {
		return true
	}
	return batchRangeRe.MatchString(ep)
}

func isLimitReached(body string) bool {
	return strings.Contains(strings.ToLower(body), "limit_reached")
}

func parseSchedulePageSlugs(body string) []string {
	var root struct {
		Schedule map[string][]struct {
			Page string `json:"page"`
		} `json:"schedule"`
	}
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		return nil
	}
	var out []string
	seen := map[string]struct{}{}
	for _, day := range sortedScheduleDays(root.Schedule) {
		for _, item := range root.Schedule[day] {
			slug := strings.TrimSpace(item.Page)
			if slug == "" {
				continue
			}
			if _, dup := seen[strings.ToLower(slug)]; dup {
				continue
			}
			seen[strings.ToLower(slug)] = struct{}{}
			out = append(out, slug)
		}
	}
	return out
}

func parseShowSlugsFromIndexHTML(html string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, m := range showLinkRe.FindAllStringSubmatch(html, -1) {
		slug := strings.TrimSpace(m[1])
		if slug == "" || strings.EqualFold(slug, "shows") {
			continue
		}
		if _, dup := seen[strings.ToLower(slug)]; dup {
			continue
		}
		seen[strings.ToLower(slug)] = struct{}{}
		out = append(out, slug)
	}
	return out
}

func extractShowSid(html string) string {
	if m := showSidRe.FindStringSubmatch(html); m != nil {
		return m[1]
	}
	if m := showSidLooseRe.FindStringSubmatch(html); m != nil {
		if m[1] != "" {
			return m[1]
		}
		return m[2]
	}
	return ""
}

// magnetSizeBytes reads the exact byte count SubsPlease puts in the magnet's
// xl= parameter, so a size is available without fetching the .torrent.
func magnetSizeBytes(magnet string) int64 {
	m := magnetXlRe.FindStringSubmatch(magnet)
	if m == nil {
		return 0
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// formatSize spells the size the way filedb.computeSize reads it back — "Mb"
// below a gigabyte, then "GB"/"TB". Verified against computeSize: "1.23 GB"
// round-trips to 1320702443 bytes, "123.45 Mb" to 129446707.
func formatSize(bytes int64) string {
	if bytes <= 0 {
		return ""
	}
	const (
		gb = int64(1) << 30
		tb = int64(1) << 40
	)
	switch {
	case bytes >= tb:
		return fmt.Sprintf("%.2f TB", float64(bytes)/float64(tb))
	case bytes >= gb:
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(gb))
	default:
		return fmt.Sprintf("%.2f Mb", float64(bytes)/float64(int64(1)<<20))
	}
}

func parseReleaseDate(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC1123Z, // "Thu, 10 Sep 2026 17:32:07 +0000" — what the API sends
		time.RFC1123,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// ------------------------------------------------------------ checkpoint --

type showEntry struct {
	Sid          string `json:"sid,omitempty"`
	Title        string `json:"title,omitempty"`
	LastFetched  string `json:"lastFetched,omitempty"`
	BatchCount   int    `json:"batchCount,omitempty"`
	EpisodeCount int    `json:"episodeCount,omitempty"`
}

type checkpoint struct {
	Cursor           int                   `json:"cursor"`
	UpdatedAt        string                `json:"updatedAt,omitempty"`
	Shows            map[string]*showEntry `json:"shows,omitempty"`
	SchedulePriority []string              `json:"schedulePrioritySlugs,omitempty"`
}

func (c *checkpoint) ensure(slug string) *showEntry {
	if c.Shows == nil {
		c.Shows = map[string]*showEntry{}
	}
	e, ok := c.Shows[slug]
	if !ok {
		e = &showEntry{}
		c.Shows[slug] = e
	}
	return e
}

func (p *Parser) checkpointPath() string {
	return filepath.Join(p.DataDir, "temp", "subsplease_shows.json")
}

// loadCheckpoint never fails the run: the cursor is an optimisation, and
// starting the sweep from the top is always correct, just slower.
func (p *Parser) loadCheckpoint(reset bool) *checkpoint {
	state := &checkpoint{Shows: map[string]*showEntry{}}
	if reset {
		return state
	}
	data, err := os.ReadFile(p.checkpointPath())
	if err != nil {
		return state
	}
	if err := json.Unmarshal(data, state); err != nil {
		log.Printf("subsplease: checkpoint unreadable, starting from the top: %v", err)
		return &checkpoint{Shows: map[string]*showEntry{}}
	}
	if state.Shows == nil {
		state.Shows = map[string]*showEntry{}
	}
	return state
}

func (p *Parser) persistCheckpoint(state *checkpoint) {
	path := p.checkpointPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("subsplease: cannot create %s: %v", filepath.Dir(path), err)
		return
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	// Write through a temp file: a run interrupted mid-write would otherwise
	// leave truncated JSON and lose every cached sid.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Printf("subsplease: cannot write checkpoint: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("subsplease: cannot replace checkpoint: %v", err)
	}
}

// ----------------------------------------------------------------- save ---

func (p *Parser) saveTorrents(torrents []filedb.TorrentDetails) (int, int, int, int, error) {
	added, updated, skipped, failed := 0, 0, 0, 0
	plog := core.NewParserLog(trackerName, filepath.Join(p.DB.DataDir, "log"), p.Config.LogParsers && p.Config.SubsPlease.Log)
	bucketCache := make(map[string]map[string]filedb.TorrentDetails, len(torrents))
	changed := make(map[string]time.Time, len(torrents))

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
		if strings.TrimSpace(asString(incoming["magnet"])) == "" {
			plog.WriteFailed(urlv, asString(incoming["title"]))
			failed++
			continue
		}

		var ex filedb.TorrentDetails
		if existing, exists := bucket[urlv]; exists {
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

// ----------------------------------------------------------------- http ---

func (p *Parser) fetchJSON(rawURL string) (string, error) {
	body, err := p.fetchWithAccept(rawURL, "application/json")
	if err != nil {
		return "", err
	}
	if isLimitReached(body) {
		return "", errLimitReached
	}
	return body, nil
}

func (p *Parser) fetchText(rawURL string) (string, error) {
	return p.fetchWithAccept(rawURL, "text/html,application/xhtml+xml,*/*;q=0.8")
}

func (p *Parser) fetchWithAccept(rawURL, accept string) (string, error) {
	res, err := p.Fetcher.Do(rawURL, p.Config.SubsPlease, core.FetchOptions{
		ExtraHeaders: map[string]string{"Accept": accept},
	})
	if err != nil {
		return "", err
	}
	if res.StatusCode >= 400 {
		return "", fmt.Errorf("http %d", res.StatusCode)
	}
	return string(res.Body), nil
}

func (p *Parser) delay(ctx context.Context) error {
	d := p.Config.SubsPlease.ParseDelay
	if d <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Duration(d) * time.Millisecond):
		return nil
	}
}

// -------------------------------------------------------------- helpers ---

// statusFor keeps a throttled run distinguishable from a broken one on the
// /trackers page; anything else keeps whatever the caller had.
func statusFor(err error) string {
	if errors.Is(err, errLimitReached) {
		return "rate_limited"
	}
	return "error"
}

func mergeUnique(first, second []string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, list := range [][]string{first, second} {
		for _, s := range list {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if _, dup := seen[strings.ToLower(s)]; dup {
				continue
			}
			seen[strings.ToLower(s)] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

// sortedKeys makes a run reproducible: Go randomises map iteration, and without
// this the order records are saved in changes between identical runs.
func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedScheduleDays[T any](m map[string][]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func countSections(items []filedb.TorrentDetails) (batch, episode int) {
	for _, it := range items {
		if strings.Contains(asString(it["title"]), "[Batch]") {
			batch++
		} else {
			episode++
		}
	}
	return
}

func firstName(items []filedb.TorrentDetails, fallback string) string {
	for _, it := range items {
		if n := asString(it["name"]); n != "" {
			return n
		}
	}
	return fallback
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func asString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

func fileTime(t filedb.TorrentDetails) time.Time {
	if v := asString(t["updateTime"]); v != "" {
		if ts, err := time.Parse(time.RFC3339, v); err == nil {
			return ts
		}
	}
	return time.Now().UTC()
}
