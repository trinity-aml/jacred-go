package animelayer

import (
	"context"
	"errors"
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

	"path/filepath"

	"jacred/app"
	"jacred/core"
	"jacred/filedb"
)

const trackerName = "animelayer"
const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"

// loginCooldown throttles repeated login attempts after a failure.
const loginCooldown = time.Minute

var (
	// errNoCredentials separates "login is not configured" from "login was
	// attempted and did not work". Both used to surface as an empty cookie.
	errNoCredentials = errors.New("animelayer: no cookie and no login credentials configured")
	// errUnauthorized marks a response animelayer served to a logged-out
	// visitor: the anonymous catalog page, or the topic page returned in
	// place of a .torrent attachment.
	errUnauthorized = errors.New("animelayer: session cookie is not authorized")
)

var (
	rowSplitRe       = regexp.MustCompile(`class="torrent-item torrent-item-medium panel"`)
	titleURLRe       = regexp.MustCompile(`<a href="/(torrent/[a-z0-9]+)/?">([^<]+)</a>`)
	sidRe            = regexp.MustCompile(`class="icon s-icons-upload"></i>([0-9]+)`)
	pirRe            = regexp.MustCompile(`class="icon s-icons-download"></i>([0-9]+)`)
	sizeNameRe       = regexp.MustCompile(`s-icons-download"></i>\s*[0-9]+\s*<span[^>]*>[\s\S]*?</span>\s*([0-9]+(?:[.,][0-9]+)?\s*(?:[KMGT]B|[КМГТ]Б))`)
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
	Config  app.Config
	DB      *filedb.DB
	Fetcher *core.Fetcher

	mu               sync.Mutex
	working          bool
	cookieMu         sync.Mutex
	cookie           string
	lastLoginAttempt time.Time
	domain           string
}

func New(cfg app.Config, db *filedb.DB) *Parser {
	p := &Parser{Config: cfg, DB: db, Fetcher: core.NewFetcher(cfg), domain: core.DomainFromHost(cfg.Animelayer.Host)}
	if saved, _ := core.DefaultSessionStore().LoadAuth(p.domain); saved != "" {
		p.cookie = saved
		log.Printf("animelayer: loaded saved cookie from disk")
	}
	return p
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
	// Authorize before parsing anything. animelayer serves the catalog to
	// logged-out visitors too, so an unauthorized run still parses a full page
	// of rows and only fails later, once per row, on the login-gated .torrent
	// attachment — "parsed=35 added=0 failed=35" with nothing naming the cause.
	// Reporting it once, up front, is the difference between a diagnosable
	// failure and a silent one.
	if err := p.authorize(ctx); err != nil {
		log.Printf("%v", err)
		return ParseResult{Status: "work_login"}, err
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
	log.Printf("animelayer: done parsed=%d added=%d skipped=%d failed=%d", res.Parsed, res.Added, res.Skipped, res.Failed)
	return res, nil
}

func (p *Parser) parsePage(ctx context.Context, page int) (int, int, int, int, int, error) {
	baseHost := ensureHTTPS(firstNonEmpty(strings.TrimSpace(p.Config.Animelayer.Alias), strings.TrimSpace(p.Config.Animelayer.Host)))
	if baseHost == "" {
		return 0, 0, 0, 0, 0, nil
	}
	rawURL := baseHost + "/torrents/anime/"
	if page > 1 {
		rawURL = fmt.Sprintf("%s/torrents/anime/?page=%d", baseHost, page)
	}
	cookie, body, err := p.fetchListing(ctx, rawURL, page)
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}
	if body == "" {
		return 0, 0, 0, 0, 0, nil
	}

	return p.saveTorrents(ctx, cookie, parseListing(body, baseHost, page))
}

// parseListing turns one catalog page into records. Split out from parsePage so
// the markup contract can be pinned against a captured page.
func parseListing(body, baseHost string, page int) []filedb.TorrentDetails {
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
		torrents = append(torrents, filedb.TorrentRecord{
			TrackerName:  trackerName,
			Types:        []string{"anime"},
			URL:          fullURL,
			Title:        title,
			Sid:          sid,
			Pir:          pir,
			CreateTime:   createTime.UTC().Format(time.RFC3339Nano),
			UpdateTime:   time.Now().UTC().Format(time.RFC3339Nano),
			Name:         name,
			OriginalName: original,
			Relased:      relased,
			SizeName:     rowSizeName(row),
			SearchName:   core.SearchName(name),
			SearchOrig:   core.SearchName(firstNonEmpty(original, name)),
		}.ToMap())
	}
	return torrents
}

func (p *Parser) saveTorrents(ctx context.Context, cookie string, torrents []filedb.TorrentDetails) (int, int, int, int, int, error) {
	parsedCount := len(torrents)
	addedCount, updatedCount, skippedCount, failedCount := 0, 0, 0, 0
	plog := core.NewParserLog(trackerName, filepath.Join(p.DB.DataDir, "log"), p.Config.LogParsers && p.Config.Animelayer.Log)
	bucketCache := make(map[string]map[string]filedb.TorrentDetails, len(torrents))
	changed := make(map[string]time.Time, len(torrents))
	var abortErr error

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
		needMagnet := !exists || asString(existing["title"]) != asString(t["title"]) || strings.TrimSpace(asString(existing["magnet"])) == ""
		if needMagnet {
			torrentBytes, err := p.downloadTorrent(ctx, urlv+"download/", urlv, cookie)
			if errors.Is(err, errUnauthorized) {
				// The catalog is public but attachments are not, so this is not
				// one bad row — every remaining download in the run will fail
				// the same way. Stop and report it instead of grinding through
				// the page to produce failed=N.
				log.Printf("animelayer: .torrent download returned HTML — cookie is not authorized, aborting run")
				p.invalidateCookie()
				plog.WriteFailed(urlv, asString(t["title"]))
				failedCount++
				abortErr = err
				break
			}
			if err != nil || len(torrentBytes) == 0 {
				plog.WriteFailed(urlv, asString(t["title"]))
				failedCount++
				continue
			}
			magnet := core.TorrentBytesToMagnet(torrentBytes)
			sizeName := torrentBytesToSizeName(torrentBytes)
			if strings.TrimSpace(sizeName) == "" {
				// The listing prints the size next to the leecher count, so a
				// bencode that carries no usable length still yields a record.
				sizeName = asString(t["sizeName"])
			}
			if strings.TrimSpace(magnet) == "" || strings.TrimSpace(sizeName) == "" {
				plog.WriteFailed(urlv, asString(t["title"]))
				failedCount++
				continue
			}
			t["magnet"] = magnet
			t["sizeName"] = sizeName
		}
		var ex filedb.TorrentDetails
		if exists {
			ex = existing
		}
		result := filedb.MergeTorrent(ex, t, p.Config.TracksAttempt)
		if !result.Changed {
			skippedCount++
			continue
		}
		bucket[urlv] = result.Torrent
		changed[key] = fileTime(result.Torrent)
		if !result.IsNew {
			plog.WriteUpdated(urlv, asString(t["title"]))
			updatedCount++
		} else {
			plog.WriteAdded(urlv, asString(t["title"]))
			addedCount++
		}
	}
	for key, tm := range changed {
		if err := p.DB.SaveBucket(key, bucketCache[key], tm); err != nil {
			return parsedCount, addedCount, updatedCount, skippedCount, failedCount, err
		}
	}
	return parsedCount, addedCount, updatedCount, skippedCount, failedCount, abortErr
}

// invalidateCookie clears the in-memory and on-disk cookie and resets the
// login-attempt cooldown so the next ensureCookie call re-authenticates
// immediately.
func (p *Parser) invalidateCookie() {
	p.cookieMu.Lock()
	p.cookie = ""
	p.lastLoginAttempt = time.Time{}
	p.cookieMu.Unlock()
	_ = core.DefaultSessionStore().DeleteAuth(p.domain)
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
	if since := time.Since(p.lastLoginAttempt); since < loginCooldown {
		p.cookieMu.Unlock()
		// Returning an empty cookie and a nil error here let the caller carry
		// on unauthenticated for a whole minute after every failed login.
		return "", fmt.Errorf("animelayer: login cooldown, retry in %s", (loginCooldown - since).Round(time.Second))
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
	if cookie != "" {
		_ = core.DefaultSessionStore().SaveAuth(p.domain, cookie)
	}
	return cookie, nil
}

// loginConfigured reports whether init.yaml supplies something to authenticate
// with — either a ready-made cookie or a username/password pair.
func (p *Parser) loginConfigured() bool {
	return strings.TrimSpace(p.Config.Animelayer.Cookie) != "" ||
		(strings.TrimSpace(p.Config.Animelayer.Login.U) != "" && strings.TrimSpace(p.Config.Animelayer.Login.P) != "")
}

// authorize resolves a session cookie and proves it is actually logged in
// before the run starts, re-authenticating once if the stored one has expired.
func (p *Parser) authorize(ctx context.Context) error {
	if !p.loginConfigured() {
		return errNoCredentials
	}
	cookie, err := p.ensureCookie(ctx)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cookie) == "" {
		return fmt.Errorf("animelayer: login produced no session cookie")
	}
	ok, err := p.validateCookie(ctx, cookie)
	if err != nil {
		return fmt.Errorf("animelayer: cookie check failed: %w", err)
	}
	if ok {
		return nil
	}
	if strings.TrimSpace(p.Config.Animelayer.Cookie) != "" {
		// A cookie pinned in init.yaml cannot be refreshed from here.
		return fmt.Errorf("%w: replace the cookie in init.yaml", errUnauthorized)
	}
	log.Printf("animelayer: stored cookie is no longer authorized, re-logging in")
	p.invalidateCookie()
	cookie, err = p.ensureCookie(ctx)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cookie) == "" {
		return fmt.Errorf("animelayer: re-login produced no session cookie")
	}
	if ok, err := p.validateCookie(ctx, cookie); err != nil {
		return fmt.Errorf("animelayer: cookie check failed after re-login: %w", err)
	} else if !ok {
		p.invalidateCookie()
		return fmt.Errorf("%w: a freshly issued cookie was still served the anonymous catalog", errUnauthorized)
	}
	return nil
}

// validateCookie fetches the catalog and reports whether animelayer treated the
// request as logged in.
func (p *Parser) validateCookie(ctx context.Context, cookie string) (bool, error) {
	baseHost := ensureHTTPS(firstNonEmpty(strings.TrimSpace(p.Config.Animelayer.Alias), strings.TrimSpace(p.Config.Animelayer.Host)))
	if baseHost == "" {
		return false, fmt.Errorf("animelayer: host is not configured")
	}
	body, err := p.fetchHTML(ctx, baseHost+"/torrents/anime/", cookie)
	if err != nil {
		return false, err
	}
	if body == "" {
		return false, fmt.Errorf("animelayer: empty response from %s", baseHost+"/torrents/anime/")
	}
	return strings.Contains(body, `id="wrapper"`) && !hasAnonymousMarkers(body), nil
}

// fetchListing gets one catalog page with an authorized cookie, re-logging in
// and retrying once if animelayer answers with the anonymous page mid-run.
func (p *Parser) fetchListing(ctx context.Context, rawURL string, page int) (string, string, error) {
	var lastCookie string
	for attempt := 0; attempt < 2; attempt++ {
		cookie, err := p.ensureCookie(ctx)
		if err != nil {
			return "", "", err
		}
		lastCookie = cookie
		body, err := p.fetchHTML(ctx, rawURL, cookie)
		if err != nil {
			log.Printf("animelayer: fetchHTML error page=%d url=%s err=%v", page, rawURL, err)
			return "", "", err
		}
		if body == "" || !strings.Contains(body, `id="wrapper"`) {
			log.Printf("animelayer: page %d empty or no wrapper, url=%s bodyLen=%d", page, rawURL, len(body))
			return cookie, "", nil
		}
		if !hasAnonymousMarkers(body) {
			return cookie, body, nil
		}
		marker := anonymousMarker(body)
		if attempt == 0 && strings.TrimSpace(p.Config.Animelayer.Cookie) == "" {
			log.Printf("animelayer: page %d was served anonymously (%s) — cookie expired, re-logging in", page, marker)
			p.invalidateCookie()
			continue
		}
		log.Printf("animelayer: page %d is still served anonymously (%s) — giving up", page, marker)
		break
	}
	return lastCookie, "", nil
}

// hasAnonymousMarkers reports whether animelayer rendered the page for a
// logged-out visitor. The catalog itself is public, so an unauthorized request
// still returns a full listing with id="wrapper" and every row intact — the
// header is the only difference, offering login and registration instead of the
// account menu. That is why a *link* is the signal: the login form lives only on
// /auth/login/, so the action="/auth/login/" this parser used to look for can
// never appear on a listing page, and the check could not fire at all.
func hasAnonymousMarkers(body string) bool {
	return anonymousMarker(body) != ""
}

// anonymousMarker returns the marker that gave the page away, so a log line can
// say why a session was judged unauthorized instead of only that it was.
func anonymousMarker(body string) string {
	for _, marker := range []string{`id="loginForm"`, "/auth/login/", "/auth/register/"} {
		if strings.Contains(body, marker) {
			return marker
		}
	}
	return ""
}

// looksLikeHTML reports whether a supposed .torrent payload is really a web
// page — what animelayer returns when the session is not authorized to download.
func looksLikeHTML(data []byte) bool {
	for i, b := range data {
		if i >= 64 {
			break
		}
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		}
		return b == '<'
	}
	return false
}

// rowSizeName reads the human-readable size the catalog prints next to the
// leecher count, so a record has one without downloading the attachment.
func rowSizeName(row string) string {
	return strings.ReplaceAll(matchFirst(sizeNameRe, row), ",", ".")
}

func (p *Parser) takeLogin(ctx context.Context) (string, error) {
	host := ensureHTTPS(strings.TrimSpace(p.Config.Animelayer.Host))
	if host == "" {
		return "", fmt.Errorf("animelayer: host is not configured")
	}
	if strings.TrimSpace(p.Config.Animelayer.Login.U) == "" || strings.TrimSpace(p.Config.Animelayer.Login.P) == "" {
		return "", errNoCredentials
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
		return "", fmt.Errorf("animelayer: login request failed: %w", err)
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
		// A wrong password, a renamed cookie and an error page all end up here.
		// Returning an empty cookie with a nil error made all three look exactly
		// like "login is not configured", and the run carried on anonymously.
		return "", fmt.Errorf("animelayer: login POST returned %d without layer_hash/layer_id cookies", resp.StatusCode)
	}
	cookie := fmt.Sprintf("layer_hash=%s;layer_id=%s", layerHash, layerID)
	if phpsess != "" {
		cookie += ";PHPSESSID=" + phpsess
	}
	return cookie, nil
}

func (p *Parser) fetchHTML(ctx context.Context, rawURL, cookie string) (string, error) {
	ts := p.Config.Animelayer
	if cookie != "" {
		ts.Cookie = cookie
	}
	body, status, err := p.Fetcher.GetString(rawURL, ts)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", nil
	}
	return body, nil
}

func (p *Parser) downloadTorrent(ctx context.Context, rawURL, referer, cookie string) ([]byte, error) {
	ts := p.Config.Animelayer
	if cookie != "" {
		ts.Cookie = cookie
	}
	res, err := p.Fetcher.Do(rawURL, ts, core.FetchOptions{
		ExtraHeaders: map[string]string{
			// The attachment is reached from the topic page; animelayer bounces
			// requests that do not look like a browser following that link.
			"Referer": referer,
			"Accept":  "application/x-bittorrent,application/octet-stream,*/*",
		},
	})
	if err != nil {
		return nil, err
	}
	if res.StatusCode == http.StatusFound || res.StatusCode == http.StatusMovedPermanently {
		// A logged-out download is redirected back to the topic page.
		return nil, errUnauthorized
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("animelayer: download %s returned status %d", rawURL, res.StatusCode)
	}
	if looksLikeHTML(res.Body) {
		return nil, errUnauthorized
	}
	return res.Body, nil
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
