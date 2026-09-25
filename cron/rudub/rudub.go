// Package rudub parses rudub.world (ex-BaibaKoTV), a Russian dubbing tracker.
//
// Two things shape this parser and neither is obvious from the markup:
//
//   - The listing is public but every magnet has to be built from a downloaded
//     .torrent, so a tracker that starts gating downloads would produce one
//     failure per row and name nothing. authorize() turns that into a single
//     named failure instead.
//   - Every .torrent carries a passkey in its announce — including the ones
//     served to a guest — so the magnet must be built without announces.
package rudub

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
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

const trackerName = "rudub"

const (
	endpointBrowse   = "/browse.php"
	endpointDownload = "/download2.php"
	endpointLogin    = "/takelogin.php"

	// validationMarker is the card wrapper. A body without it is not a
	// listing — a login wall, an error page or a changed layout all arrive as
	// HTTP 200, and parsing one yields zero rows that read as a quiet day.
	validationMarker = "card__torlist__browse_2"
)

// videoFormats are the site's `videoformat` query values worth collecting:
// 4 = HD 1080, 5 = HD 2160. 720p and SD are deliberately not fetched at all
// rather than parsed and discarded.
var videoFormats = []int{4, 5}

var (
	cardSplitRe = regexp.MustCompile(`(?i)<div\s+class="card__torlist__browse_2"`)
	detailsRe   = regexp.MustCompile(`(?is)href=["']/?(details\.php\?id=([0-9]+))["'][^>]*>\s*<b>([\s\S]*?)</b>`)
	downloadRe  = regexp.MustCompile(`(?i)href=["']/?(?:download2\.php\?id=|download\.php\?id=)([0-9]+)["']`)
	dateRe      = regexp.MustCompile(`(?is)li\s+title=["']Дата["'][^>]*>[\s\S]*?</i>\s*([0-9]{4}-[0-9]{2}-[0-9]{2}\s+[0-9]{2}:[0-9]{2}:[0-9]{2})`)
	sizeRe      = regexp.MustCompile(`(?is)li\s+title=["']Размер["'][^>]*>[\s\S]*?</i>\s*([^<]+)`)
	activityRe  = regexp.MustCompile(`(?is)li\s+title=["']Активность["'][^>]*>[\s\S]*?</i>\s*(\d+)\s*<[\s\S]*?</i>\s*(\d+)`)

	// Quality gate. The listing is already filtered by videoformat, but a card
	// can still carry a 720p variant, so the title is checked too.
	goodQualityRe = regexp.MustCompile(`(?i)(?:\b|[^0-9])(?:HD|BD|HDR)?(?:1080p|2160p)\b`)
	badQualityRe  = regexp.MustCompile(`(?i)(?:\bWEBRip\s*XviD\b|\bWEBRip\s*x264\b|\bHD720p\b|(?:[^0-9]|^)720p\b)`)

	yearOnlyRe = regexp.MustCompile(`^(?:19|20)\d{2}(?:\s*[-–]\s*(?:19|20)\d{2})?$`)

	serialSeasonRe  = regexp.MustCompile(`[CcСс]езон`)
	serialEpisodeRe = regexp.MustCompile(`[CcСс]ери`)
	serialSxxExxRe  = regexp.MustCompile(`(?i)/\s*s\d+e\d+`)

	whitespaceRe = regexp.MustCompile(`[\n\r\t ]+`)
	brRe         = regexp.MustCompile(`(?i)<br\s*/?>`)
)

// errNotATorrent marks download2.php answering with something other than a
// bencoded file. It is the signal that the tracker has started gating
// downloads, which is the one change that would silently break this parser.
var errNotATorrent = errors.New("rudub: download did not return a torrent")

type Parser struct {
	Config  app.Config
	DB      *filedb.DB
	DataDir string
	Fetcher *core.Fetcher
	Client  *http.Client

	mu      sync.Mutex
	working bool
	cookie  string
	cookieT time.Time
	domain  string
}

type ParseResult struct {
	Fetched, Added, Updated, Skipped, Failed int
	Status                                   string
}

func New(cfg app.Config, db *filedb.DB, dataDir string) *Parser {
	p := &Parser{
		Config:  cfg,
		DB:      db,
		DataDir: dataDir,
		Fetcher: core.NewFetcher(cfg),
		Client:  &http.Client{Timeout: 30 * time.Second},
		domain:  core.DomainFromHost(cfg.Rudub.Host),
	}
	if saved, _ := core.DefaultSessionStore().LoadAuth(p.domain); saved != "" {
		p.cookie = saved
		p.cookieT = time.Now()
	}
	return p
}

// card is one parsed listing entry before its magnet exists.
type card struct {
	url        string
	title      string
	name       string
	original   string
	relased    int
	sid, pir   int
	sizeName   string
	types      []string
	createTime time.Time
	downloadID string
}

func (p *Parser) Parse(ctx context.Context, limitPage int) (ParseResult, error) {
	p.mu.Lock()
	if p.working {
		p.mu.Unlock()
		return ParseResult{Status: "work"}, nil
	}
	p.working = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.working = false; p.mu.Unlock() }()

	host := strings.TrimRight(p.Config.Rudub.Host, "/")
	if host == "" {
		return ParseResult{Status: "config missing"}, nil
	}
	if limitPage <= 0 {
		limitPage = 10
	}
	if limitPage > 100 {
		limitPage = 100
	}

	res := ParseResult{Status: "ok"}

	// Prove the run can actually produce magnets before walking 2 x N pages.
	// Without this a gated tracker parses every row and then fails every row,
	// which is what animelayer did (parsed=35 failed=35) and which names
	// nothing in the log.
	if err := p.authorize(ctx, host); err != nil {
		res.Status = core.StatusWorkLogin
		return res, err
	}

	for _, vf := range videoFormats {
		for page := 0; page < limitPage; page++ {
			if err := ctx.Err(); err != nil {
				res.Status = "canceled"
				return res, err
			}
			if page > 0 || vf != videoFormats[0] {
				if err := p.delay(ctx); err != nil {
					res.Status = "canceled"
					return res, err
				}
			}

			u := fmt.Sprintf("%s%s?incldead=0&sort=4&type=desc&videoformat=%d&page=%d",
				host, endpointBrowse, vf, page)
			body, err := p.fetchPage(u)
			if err != nil {
				log.Printf("rudub: vf=%d page %d failed: %v", vf, page, err)
				res.Failed++
				break
			}
			if !strings.Contains(body, validationMarker) {
				// Not a listing. Stopping is right for both causes — an empty
				// tail of the catalogue and a page we were not served.
				log.Printf("rudub: vf=%d page %d carries no cards, stopping this format", vf, page)
				break
			}

			cards := parseCards(body, host)
			if len(cards) == 0 {
				break
			}
			res.Fetched += len(cards)

			added, updated, skipped, failed, err := p.saveTorrents(ctx, host, cards)
			res.Added, res.Updated, res.Skipped, res.Failed = res.Added+added, res.Updated+updated, res.Skipped+skipped, res.Failed+failed
			if err != nil {
				if errors.Is(err, core.ErrNotAuthorized) {
					res.Status = core.StatusWorkLogin
				}
				return res, err
			}
			log.Printf("rudub: vf=%d page %d/%d fetched=%d added=%d updated=%d skipped=%d failed=%d",
				vf, page+1, limitPage, len(cards), added, updated, skipped, failed)
		}
	}

	log.Printf("rudub: done fetched=%d added=%d updated=%d skipped=%d failed=%d",
		res.Fetched, res.Added, res.Updated, res.Skipped, res.Failed)
	return res, nil
}

// ------------------------------------------------------------------ auth --

// authorize establishes whatever session is configured, and fails the run when
// credentials are present but unusable.
//
// Credentials are optional here, and that is a measured decision rather than an
// oversight: on 2026-09-25 both the listing and download2.php answered a guest
// in full, though upstream's docs call a login mandatory. So there is nothing
// to verify up front when none are configured — the gate, if the site puts one
// back, shows up on the first download, and saveTorrents aborts the whole run
// there rather than producing one failure per row (animelayer's parsed=35
// failed=35). That is the guarantee: one named failure, not thirty anonymous
// ones.
func (p *Parser) authorize(ctx context.Context, host string) error {
	if p.hasCredentials() && p.getCookie() == "" {
		if err := p.takeLogin(ctx, host); err != nil {
			return err
		}
	}
	return nil
}

func (p *Parser) hasCredentials() bool {
	return strings.TrimSpace(p.Config.Rudub.Cookie) != "" ||
		strings.TrimSpace(p.Config.Rudub.Login.U) != ""
}

func (p *Parser) getCookie() string {
	if c := strings.TrimSpace(p.Config.Rudub.Cookie); c != "" {
		return c
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cookie != "" && time.Since(p.cookieT) < 2*time.Hour {
		return p.cookie
	}
	return ""
}

// takeLogin returns an error on every failure path. A wrong password, a renamed
// cookie and "login is not configured" are three different problems, and an
// empty-cookie-and-nil-error return would make all three look like a run that
// simply continued unauthenticated.
func (p *Parser) takeLogin(ctx context.Context, host string) error {
	u := strings.TrimSpace(p.Config.Rudub.Login.U)
	pw := p.Config.Rudub.Login.P
	if u == "" {
		return fmt.Errorf("rudub: login is not configured: %w", core.ErrNotAuthorized)
	}

	form := url.Values{"username": {u}, "password": {pw}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+endpointLogin,
		strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("rudub: login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Referer", host+"/login.php")

	// The session arrives as Set-Cookie on the redirect, and FetchResult
	// carries only body+status — hence the raw client here, as in kinozal.
	resp, err := p.Client.Do(req)
	if err != nil {
		return fmt.Errorf("rudub: login failed: %w", err)
	}
	defer resp.Body.Close()

	var parts []string
	for _, line := range resp.Header.Values("Set-Cookie") {
		parts = append(parts, strings.SplitN(line, ";", 2)[0])
	}
	cookie := strings.Join(parts, "; ")
	// Cookie names only — a value in a log is a replayable credential, and
	// Data/log is kept for 14 days.
	if !strings.Contains(cookie, "uid=") || !strings.Contains(cookie, "pass=") {
		return fmt.Errorf("rudub: login rejected (got %s): %w",
			core.CookieNames(cookie), core.ErrNotAuthorized)
	}

	p.mu.Lock()
	p.cookie = cookie
	p.cookieT = time.Now()
	p.mu.Unlock()
	_ = core.DefaultSessionStore().SaveAuth(p.domain, cookie)
	log.Printf("rudub: login OK, got %s", core.CookieNames(cookie))
	return nil
}

func (p *Parser) invalidateCookie() {
	p.mu.Lock()
	p.cookie = ""
	p.cookieT = time.Time{}
	p.mu.Unlock()
	_ = core.DefaultSessionStore().DeleteAuth(p.domain)
}

// --------------------------------------------------------------- parsing --

func parseCards(body, host string) []card {
	host = strings.TrimRight(host, "/")
	decoded := html.UnescapeString(strings.ReplaceAll(body, "&nbsp;", " "))
	parts := cardSplitRe.Split(decoded, -1)

	out := make([]card, 0, len(parts))
	for _, part := range parts[1:] {
		m := detailsRe.FindStringSubmatch(part)
		if m == nil {
			continue
		}
		relURL, id, rawTitle := m[1], m[2], m[3]
		title := normalizeTitle(rawTitle)
		if title == "" || id == "" || !isPreferredQuality(title) {
			continue
		}
		dl := downloadRe.FindStringSubmatch(part)
		if dl == nil {
			continue
		}

		created := parseCardDate(part)
		name, original, relased := parseTitleFields(title)
		if name == "" {
			continue
		}

		c := card{
			url:        host + "/" + relURL,
			title:      title,
			name:       name,
			original:   original,
			relased:    relased,
			sid:        1,
			types:      detectTypes(title),
			createTime: created,
			downloadID: dl[1],
		}
		if a := activityRe.FindStringSubmatch(part); a != nil {
			c.sid = atoiOr(a[1], 1)
			c.pir = atoiOr(a[2], 0)
		}
		if s := sizeRe.FindStringSubmatch(part); s != nil {
			c.sizeName = strings.TrimSpace(whitespaceRe.ReplaceAllString(s[1], " "))
		}
		out = append(out, c)
	}
	return out
}

// isPreferredQuality keeps HD 1080/2160 and drops 720p and SD. A title that
// names both (a combined release) is kept.
func isPreferredQuality(title string) bool {
	if title == "" {
		return false
	}
	if badQualityRe.MatchString(title) && !goodQualityRe.MatchString(title) {
		return false
	}
	return goodQualityRe.MatchString(title)
}

func normalizeTitle(raw string) string {
	t := brRe.ReplaceAllString(raw, " ")
	t = whitespaceRe.ReplaceAllString(html.UnescapeString(t), " ")
	for _, tag := range []string{"(Обновляемая)", "(Оновлюється)", "(Золото)"} {
		t = replaceFold(t, tag, "")
	}
	return strings.TrimSpace(whitespaceRe.ReplaceAllString(t, " "))
}

// parseTitleFields splits "Русское (Original) Сезон 1 (HD1080p)" into its
// parts. The first parenthesised group that is not a bare year is taken as the
// original title, and everything before it as the name.
//
// relased is left at 0 when the title carries no "(YYYY)". Upstream falls back
// to the upload date here; measured across two live listing pages only 2 of 60
// titles carry a year at all, and the one that did said 2022 for a card
// uploaded in 2025 — so the fallback would stamp a wrong year on ~97% of
// records. A wrong year is worse than none: Jackett matches years within ±1,
// so it would both miss the real year and surface under the wrong one. The
// cost of 0 is exclusion from /api/v1.0/qualitys only; the main search and
// torznab are unaffected.
func parseTitleFields(title string) (name, original string, relased int) {
	if strings.TrimSpace(title) == "" {
		return "", "", 0
	}
	for _, g := range parenGroups(title) {
		if y, ok := parseYearGroup(g.inner); ok {
			if relased <= 0 {
				relased = y
			}
			continue
		}
		if original == "" {
			name = stripTrailingYearParens(strings.TrimSpace(title[:g.start]))
			original = strings.TrimSpace(g.inner)
		}
	}
	if strings.TrimSpace(name) == "" {
		name = strings.TrimSpace(splitFirst(title, "(", "/", "|"))
	}
	return strings.TrimSpace(name), strings.TrimSpace(original), relased
}

type parenGroup struct {
	start int
	inner string
}

// parenGroups walks balanced groups, so "(The Outlaws (US))" yields one group
// rather than a truncated one.
func parenGroups(title string) []parenGroup {
	var out []parenGroup
	runes := []rune(title)
	byteAt := make([]int, len(runes)+1)
	pos := 0
	for i, r := range runes {
		byteAt[i] = pos
		pos += len(string(r))
	}
	byteAt[len(runes)] = pos

	for i := 0; i < len(runes); i++ {
		if runes[i] != '(' {
			continue
		}
		depth, close := 0, -1
		for j := i; j < len(runes); j++ {
			switch runes[j] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					close = j
				}
			}
			if close >= 0 {
				break
			}
		}
		if close < 0 {
			break
		}
		out = append(out, parenGroup{start: byteAt[i], inner: string(runes[i+1 : close])})
		i = close
	}
	return out
}

func parseYearGroup(inner string) (int, bool) {
	t := strings.TrimSpace(inner)
	if !yearOnlyRe.MatchString(t) {
		return 0, false
	}
	y, err := strconv.Atoi(t[:4])
	if err != nil || y < 1900 || y > 2100 {
		return 0, false
	}
	return y, true
}

func stripTrailingYearParens(prefix string) string {
	for {
		prefix = strings.TrimRight(prefix, " \t")
		if !strings.HasSuffix(prefix, ")") {
			return prefix
		}
		open := strings.LastIndex(prefix, "(")
		if open < 0 {
			return prefix
		}
		if _, ok := parseYearGroup(prefix[open+1 : len(prefix)-1]); !ok {
			return prefix
		}
		prefix = prefix[:open]
	}
}

func detectTypes(title string) []string {
	if serialSeasonRe.MatchString(title) || serialEpisodeRe.MatchString(title) || serialSxxExxRe.MatchString(title) {
		return []string{"serial"}
	}
	return []string{"movie"}
}

func parseCardDate(part string) time.Time {
	m := dateRe.FindStringSubmatch(part)
	if m == nil {
		return time.Time{}
	}
	t, err := time.Parse("2006-01-02 15:04:05", strings.TrimSpace(m[1]))
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// ----------------------------------------------------------------- save ---

func (p *Parser) saveTorrents(ctx context.Context, host string, cards []card) (int, int, int, int, error) {
	added, updated, skipped, failed := 0, 0, 0, 0
	plog := core.NewParserLog(trackerName, filepath.Join(p.DB.DataDir, "log"), p.Config.LogParsers && p.Config.Rudub.Log)
	bucketCache := make(map[string]map[string]filedb.TorrentDetails, len(cards))
	changed := make(map[string]time.Time, len(cards))

	for _, c := range cards {
		key := p.DB.KeyDb(c.name, core.FirstNonEmpty(c.original, c.name))
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

		existing, exists := bucket[c.url]
		magnet := ""
		if exists {
			magnet = strings.TrimSpace(asString(existing["magnet"]))
		}
		// Reuse the stored magnet whenever the title is unchanged. Without
		// this every pass would re-download every .torrent on the pages it
		// re-reads, which is most of them.
		if magnet == "" || asString(existing["title"]) != c.title {
			if err := p.delay(ctx); err != nil {
				return added, updated, skipped, failed, err
			}
			data, err := p.download(host+endpointDownload+"?id="+c.downloadID, host+endpointBrowse)
			if err != nil {
				if errors.Is(err, core.ErrNotAuthorized) {
					// Downloads have started requiring a session. Abort the
					// run with one named failure instead of grinding out one
					// per row.
					p.invalidateCookie()
					return added, updated, skipped, failed, err
				}
				plog.WriteFailed(c.url, c.title)
				failed++
				continue
			}
			// NoTrackers is mandatory, not a preference: every .torrent here
			// carries a passkey in its announce — verified on anonymous
			// downloads too, where two different torrents came back with the
			// same key. Appending announces as tr= would republish it through
			// the search API, torznab and /sync.
			m, err := core.TorrentBytesToMagnetNoTrackersErr(data)
			if err != nil || strings.TrimSpace(m) == "" {
				plog.WriteFailed(c.url, c.title)
				failed++
				continue
			}
			magnet = m
		}

		incoming := filedb.TorrentRecord{
			TrackerName: trackerName,
			Types:       c.types,
			URL:         c.url,
			Title:       c.title,
			Name:        c.name,
			// Quality is deliberately not set: UpdateFullDetails skips a
			// record that already has quality and _sn, so presetting it makes
			// every new record lose seasons/videotype/voices/languages. The
			// title carries "1080p"/"2160p", which is what it reads.
			OriginalName: core.FirstNonEmpty(c.original, c.name),
			Sid:          c.sid,
			Pir:          c.pir,
			SizeName:     c.sizeName,
			Magnet:       magnet,
			Relased:      c.relased,
			CreateTime:   createOrNow(c.createTime).Format(time.RFC3339),
			UpdateTime:   time.Now().UTC().Format(time.RFC3339),
		}.ToMap()

		var ex filedb.TorrentDetails
		if exists {
			ex = existing
		}
		result := filedb.MergeTorrent(ex, incoming, p.Config.TracksAttempt)
		if !result.Changed {
			skipped++
			continue
		}
		bucket[c.url] = result.Torrent
		changed[key] = fileTime(result.Torrent)
		if !result.IsNew {
			plog.WriteUpdated(c.url, c.title)
			updated++
		} else {
			plog.WriteAdded(c.url, c.title)
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

const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"

// fetchPage returns the listing as UTF-8. The site serves cp1251, and a body
// decoded as UTF-8 would turn every Cyrillic title into replacement characters
// — the regexes keying off "Дата"/"Размер"/"Активность" would then match
// nothing and the page would look empty.
func (p *Parser) fetchPage(rawURL string) (string, error) {
	res, err := p.Fetcher.Do(rawURL, p.trackerSettings(), core.FetchOptions{
		UserAgent: browserUA,
	})
	if err != nil {
		return "", err
	}
	if res.StatusCode >= 400 {
		return "", fmt.Errorf("http %d", res.StatusCode)
	}
	return decodeBody(res.Body), nil
}

func (p *Parser) download(rawURL, referer string) ([]byte, error) {
	res, err := p.Fetcher.Do(rawURL, p.trackerSettings(), core.FetchOptions{
		UserAgent: browserUA,
		ExtraHeaders: map[string]string{
			"Referer": referer,
			"Accept":  "application/x-bittorrent,application/octet-stream;q=0.9,*/*;q=0.8",
		},
	})
	if err != nil {
		return nil, err
	}
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("rudub: download %d: %w", res.StatusCode, core.ErrNotAuthorized)
	}
	if res.StatusCode >= 400 {
		return nil, fmt.Errorf("http %d", res.StatusCode)
	}
	if !looksLikeTorrent(res.Body) {
		// The site answers 200 with an HTML page when it refuses, so the
		// payload is what decides — a status check alone would report success.
		if looksLikeHTML(res.Body) {
			return nil, fmt.Errorf("rudub: download2.php returned HTML, a session is now required: %w", core.ErrNotAuthorized)
		}
		return nil, errNotATorrent
	}
	return res.Body, nil
}

// trackerSettings folds the live session into the per-request config so the
// fetcher sends it, without mutating the parser's own config.
func (p *Parser) trackerSettings() app.TrackerSettings {
	t := p.Config.Rudub
	if c := p.getCookie(); c != "" {
		t.Cookie = core.MergeCookieStrings(t.Cookie, c)
	}
	return t
}

func (p *Parser) delay(ctx context.Context) error {
	d := p.Config.Rudub.ParseDelay
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

// looksLikeTorrent checks for a bencoded dictionary. A .torrent always starts
// with 'd'; anything else is an error page whatever its status said.
func looksLikeTorrent(data []byte) bool {
	return len(data) > 0 && data[0] == 'd'
}

func looksLikeHTML(data []byte) bool {
	head := strings.ToLower(string(data[:min(len(data), 512)]))
	return strings.Contains(head, "<html") || strings.Contains(head, "<!doctype") || strings.Contains(head, "<body")
}

// decodeBody converts cp1251 to UTF-8, leaving an already-valid UTF-8 body
// alone — the site is cp1251 today but sniffing costs nothing and a mixed
// deployment would otherwise mangle every title.
func decodeBody(b []byte) string {
	if utf8Valid(b) {
		return string(b)
	}
	return core.DecodeCP1251(b)
}

func utf8Valid(b []byte) bool {
	return strings.ToValidUTF8(string(b), "�") == string(b) && !strings.ContainsRune(string(b), '�')
}

func replaceFold(s, old, new string) string {
	for {
		i := strings.Index(strings.ToLower(s), strings.ToLower(old))
		if i < 0 {
			return s
		}
		s = s[:i] + new + s[i+len(old):]
	}
}

func splitFirst(s string, seps ...string) string {
	cut := len(s)
	for _, sep := range seps {
		if i := strings.Index(s, sep); i >= 0 && i < cut {
			cut = i
		}
	}
	return s[:cut]
}

func createOrNow(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t
}

func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
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
