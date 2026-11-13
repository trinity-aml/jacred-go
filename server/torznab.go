package server

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"jacred/filedb"
)

// Torznab XML — the format Sonarr, Radarr and Prowlarr actually speak.
//
// What existed before was the Jackett **JSON** API at
// /api/v2.0/indexers/<x>/results, answering {"Results":[…],"jacred":true}. That
// is a different protocol: a client configured as a "Torznab indexer" asks for
// /results/torznab/api?t=caps first, and this server answered 404, so it could
// not be added at all. Measured against the production instance on 2026-11-13
// before this landed:
//
//	200  /api/v2.0/indexers/all/results?Query=matrix
//	404  /api/v2.0/indexers/all/results/torznab/api?t=caps
//
// The wire format here follows the original's TorznabXmlFormatter field for
// field — same attrs, same guid rule, same enclosure type, same caps — because
// clients are already configured against it and "readable with the same eyes"
// is worth more than any improvement I might invent.
//
// # Magnets, not torrent files
//
// Every record here carries a magnet and nothing else; this server never has a
// .torrent to serve. So <link> and <enclosure url> are the magnet, which is
// what Jackett itself emits for a magnet-only indexer. That is not a shortcut:
// three parsers (anibelka, mazepa, rudub) deliberately strip announce URLs
// because each .torrent embeds the account's passkey, and republishing it
// through this endpoint is precisely the leak they exist to prevent.

// torznabPathRe matches the Jackett-style Torznab path. The segment is the
// indexer name, which — unlike the older JSON path — this endpoint honours.
var torznabPathRe = regexp.MustCompile(`(?i)^/api/v2\.0/indexers/([^/]+)/results/torznab/api/?$`)

// newznabPathRe is the Prowlarr spelling of the same thing. Its own REST
// discovery (/api/v1/indexer) is a separate feature and is not served here;
// Prowlarr's "Generic Torznab" needs only this URL.
var newznabPathRe = regexp.MustCompile(`(?i)^/api/v1/indexer/([^/]+)/newznab/?$`)

var infoHashRe = regexp.MustCompile(`(?i)btih:([a-f0-9]{32,40})`)

// torznabIndexerFromPath reads the indexer segment, "" meaning "every tracker".
func torznabIndexerFromPath(path string) (indexer string, ok bool) {
	for _, re := range []*regexp.Regexp{torznabPathRe, newznabPathRe} {
		if m := re.FindStringSubmatch(path); m != nil {
			return m[1], true
		}
	}
	if strings.EqualFold(strings.TrimRight(path, "/"), "/torznab/api") {
		return "", true
	}
	return "", false
}

// aggregateIndexerNames are the segment values that mean "all trackers" rather
// than a tracker called that. "all" is what the original's own indexer id is,
// and Jackett's convention; "jacred" is what this project's README has always
// used in its example URL.
var aggregateIndexerNames = map[string]bool{
	"":       true,
	"all":    true,
	"jacred": true,
	"status": true, // Jackett's /indexers/status:healthy/results spelling
}

func (s *Server) handleTorznab(w http.ResponseWriter, r *http.Request) {
	indexer, ok := torznabIndexerFromPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	cfg := s.GetConfig()
	if !cfg.Torznab.Enable {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	t := strings.ToLower(strings.TrimSpace(q.Get("t")))
	apiURL := absoluteURL(r, r.URL.Path)
	origin := absoluteURL(r, "")

	// Prowlarr probes with HEAD before it will save an indexer.
	if r.Method == http.MethodHead {
		writeXML(w, "")
		return
	}

	switch t {
	case "caps":
		writeXML(w, torznabCapsXML(apiURL))
		return
	case "indexers":
		if cfgd := strings.ToLower(q.Get("configured")); cfgd != "" && cfgd != "true" {
			writeXML(w, xmlHeader+"<indexers></indexers>")
			return
		}
		writeXML(w, torznabIndexersXML(trackerNames))
		return
	}

	query := firstQueryURL(q, "q", "query", "Query")
	title := q.Get("title")
	titleOriginal := q.Get("title_original")
	if strings.TrimSpace(query) == "" && strings.TrimSpace(title) == "" && strings.TrimSpace(titleOriginal) == "" {
		// A bare feed request with nothing to search for. Clients send this on
		// "Test", and an empty channel is a valid answer that keeps the test
		// green without running a wildcard query across the whole database.
		writeXML(w, torznabRSS("", origin, apiURL, 0, 0))
		return
	}

	// The indexer is part of the key, not just the query string. It comes
	// from the *path*, so two requests differing only in which tracker they
	// ask for are otherwise identical keys — found live on 2026-11-13, where
	// /indexers/kinozal/... was served the cached /indexers/all/... feed and
	// the tracker filter looked like it did nothing.
	cacheKey := cacheKeyFor("torznab", strings.ToLower(indexer)+"|"+r.URL.RawQuery)
	if cached, ok := s.cache.Get(cacheKey); ok {
		w.Header().Set("X-Cache", "HIT")
		writeXML(w, string(cached))
		return
	}

	params := filedb.SearchParams{
		APIKey:        firstQueryURL(q, "apikey", "apiKey"),
		Query:         query,
		Title:         title,
		TitleOriginal: titleOriginal,
		Year:          atoi(q.Get("year")),
		IsSerial:      parseOptionalInt(q, "is_serial", -1),
		CategoryRaw:   firstCategoryValue(q),
		UserAgent:     r.UserAgent(),
	}
	res, err := s.DB.JackettSearch(params)
	if err != nil {
		// A broken search must not look like "nothing found": a client would
		// cache the empty feed and stop asking.
		writeXMLStatus(w, http.StatusInternalServerError, torznabRSS("", origin, apiURL, 0, 0))
		return
	}
	// Same fallbacks as the other two search paths — one list, in
	// searchfallback.go, so the three cannot drift.
	if len(res.Results) == 0 {
		for _, fb := range searchFallbacks(query) {
			retryParams := params
			retryParams.Query = fb.Query
			if retryParams.Year == 0 {
				retryParams.Year = fb.Year
			}
			if retry, retryErr := s.DB.JackettSearch(retryParams); retryErr == nil && len(retry.Results) > 0 {
				res = retry
				break
			}
		}
	}

	items := res.Results
	if !aggregateIndexerNames[strings.ToLower(indexer)] {
		items = filterByTracker(items, indexer)
	}
	// Drop what a client cannot act on, and do it before the window so that
	// `total` describes what is actually on offer. Taken from PR #1: a details
	// page is not a downloadable enclosure, and emitting one as <link> hands
	// the client a URL it will fail on. Every record here normally carries a
	// magnet, so this is a guard rather than a filter.
	items = withDownloadableLink(items)
	total := len(items)
	items, offset := applyTorznabWindow(items, q)

	cat := assignedCategory(t, q)
	body := torznabRSS(s.torznabItems(items, res.RqNum, cat, cfg.Torznab.EnrichTitles), origin, apiURL, offset, total)
	s.cache.Set(cacheKey, []byte(body))
	writeXML(w, body)
}

// filterByTracker honours the indexer segment of the path.
//
// A stored trackerName can be a comma-joined list after duplicate merging
// (`mergeduplicates`), e.g. "kinozal, rutracker", so this splits before
// comparing — an equality test would drop exactly the rows that two trackers
// agreed on.
func filterByTracker(items []filedb.TorrentDetails, indexer string) []filedb.TorrentDetails {
	want := strings.ToLower(strings.TrimSpace(indexer))
	if want == "" {
		return items
	}
	out := make([]filedb.TorrentDetails, 0, len(items))
	for _, it := range items {
		for _, name := range strings.Split(asString(it["trackerName"]), ",") {
			if strings.EqualFold(strings.TrimSpace(name), want) {
				out = append(out, it)
				break
			}
		}
	}
	return out
}

// applyTorznabWindow applies offset/limit, which caps advertises as max 1000
// and default 100. The search itself returns everything it found, so without
// this a client asking for a page would get the lot.
func applyTorznabWindow(items []filedb.TorrentDetails, q map[string][]string) ([]filedb.TorrentDetails, int) {
	get := func(k string) int {
		if v, ok := q[k]; ok && len(v) > 0 {
			return atoi(v[0])
		}
		return 0
	}
	offset := get("offset")
	if offset < 0 {
		offset = 0
	}
	if offset >= len(items) {
		return nil, offset
	}
	items = items[offset:]
	limit := get("limit")
	if limit <= 0 {
		limit = torznabDefaultLimit
	}
	if limit > torznabMaxLimit {
		limit = torznabMaxLimit
	}
	if limit < len(items) {
		items = items[:limit]
	}
	return items, offset
}

// assignedCategory is the category a client's search mode implies, which wins
// over whatever the record's own types say — a tvsearch result must be filed
// under TV even when the record also looks like a movie, or the client drops it.
func assignedCategory(t string, q map[string][]string) string {
	switch t {
	case "tvsearch", "tv":
		return "5000"
	case "movie", "moviesearch":
		return "2000"
	}
	if raw, ok := q["cat"]; ok && len(raw) > 0 {
		if first := strings.TrimSpace(strings.Split(raw[0], ",")[0]); first != "" {
			return first
		}
	}
	return ""
}

const (
	xmlHeader           = `<?xml version="1.0" encoding="UTF-8"?>` + "\n"
	torznabDefaultLimit = 100
	torznabMaxLimit     = 1000
)

func torznabCapsXML(apiURL string) string {
	return xmlHeader + `<caps>
  <server version="1.0" title="jacred" strapline="Native Torznab API" email="info@localhost" url="` + xmlEscape(strings.TrimRight(apiURL, "/")) + `"/>
  <limits max="` + strconv.Itoa(torznabMaxLimit) + `" default="` + strconv.Itoa(torznabDefaultLimit) + `"/>
  <searching>
    <search available="yes" supportedParams="q"/>
    <tv-search available="yes" supportedParams="q,season,ep"/>
    <movie-search available="yes" supportedParams="q"/>
  </searching>
  <categories>
    <category id="2000" name="Movies"/>
    <category id="5000" name="TV"/>
    <category id="5070" name="TV/Anime"/>
  </categories>
</caps>`
}

// torznabCapsXML deliberately does not advertise imdbid, tvdbid or music and
// book modes. The original lists imdbid/tvdbid; this search path has no id
// lookup of its own — /api/v1.0/torrents resolves `tt…`/`kp…` through an
// external service, JackettSearch does not — and advertising a mode that falls
// back to a plain title match makes a client trust an id search that never
// happened.

func torznabIndexersXML(trackers []string) string {
	var b strings.Builder
	b.WriteString(xmlHeader + "<indexers>\n")
	b.WriteString(torznabIndexerEntry("all", "jacred (all trackers)", "Aggregated jacred search across all configured trackers"))
	names := append([]string(nil), trackers...)
	sort.Strings(names)
	for _, name := range names {
		b.WriteString(torznabIndexerEntry(name, name, "jacred tracker: "+name))
	}
	b.WriteString("</indexers>")
	return b.String()
}

func torznabIndexerEntry(id, title, description string) string {
	return `  <indexer id="` + xmlEscape(id) + `" configured="true">
    <title>` + xmlEscape(title) + `</title>
    <description>` + xmlEscape(description) + `</description>
    <link>https://github.com/jacred-fdb/jacred</link>
    <language>ru-RU</language>
    <type>public</type>
  </indexer>
`
}

// torznabRSS wraps the items. offset and total are reported in
// <torznab:response>, which is how a client knows there is a next page: a feed
// that simply ends looks identical to one that was truncated by `limit`.
// Mine shipped without it; PR #1 had it.
func torznabRSS(itemsXML, origin, apiURL string, offset, total int) string {
	return xmlHeader + `<rss version="2.0" xmlns:atom="http://www.w3.org/2005/Atom" xmlns:torznab="http://torznab.com/schemas/2015/feed">
  <channel>
    <atom:link href="` + xmlEscape(strings.TrimRight(apiURL, "/")) + `" rel="self" type="application/rss+xml"/>
    <title>jacred</title>
    <description>Torznab API</description>
    <link>` + xmlEscape(strings.TrimRight(origin, "/")) + `/</link>
    <language>en-us</language>
    <category>search</category>
    <torznab:response offset="` + strconv.Itoa(offset) + `" total="` + strconv.Itoa(total) + `"/>
` + itemsXML + `  </channel>
</rss>`
}

// withDownloadableLink keeps only rows carrying a magnet.
func withDownloadableLink(items []filedb.TorrentDetails) []filedb.TorrentDetails {
	out := make([]filedb.TorrentDetails, 0, len(items))
	for _, it := range items {
		if strings.HasPrefix(strings.ToLower(asString(it["magnet"])), "magnet:") {
			out = append(out, it)
		}
	}
	return out
}

func (s *Server) torznabItems(items []filedb.TorrentDetails, rqnum bool, assignedCat string, enrich bool) string {
	var b strings.Builder
	for _, src := range items {
		b.WriteString(s.torznabItem(src, rqnum, assignedCat, enrich))
	}
	return b.String()
}

func (s *Server) torznabItem(src filedb.TorrentDetails, rqnum bool, assignedCat string, enrich bool) string {
	t := s.enrichTorrentWithTracks(cloneMap(src))

	title := asString(t["title"])
	if title == "" {
		title = "Unknown"
	}
	display := title
	if enrich {
		if voices := stringsFrom(t["voices"]); len(voices) > 0 {
			display = title + " | [" + strings.Join(voices, " ") + "].rus"
		}
	}

	magnet := asString(t["magnet"])
	details := asString(normalizeDetailURL(t["url"]))
	tracker := asString(t["trackerName"])
	if tracker == "" {
		tracker = "jacred"
	}

	seeders := asInt(t["sid"])
	leechers := asInt(t["pir"])
	peers := seeders
	if leechers > 0 {
		peers = seeders + leechers
	}

	cat := assignedCat
	if cat == "" {
		if ids, _ := categories(t); len(ids) > 0 {
			sort.Ints(ids)
			cat = strconv.Itoa(ids[0])
		}
	}
	if cat == "" {
		cat = "2000"
	}

	size := int64(parseSize(t))
	infohash := infoHashFromMagnet(magnet)
	guid := infohash
	if guid == "" {
		// A stable id matters more than a pretty one: a client that sees a new
		// guid for a row it already has grabs it again.
		sum := md5.Sum([]byte(display))
		guid = hex.EncodeToString(sum[:])
	}

	enclosureType := "application/x-bittorrent"
	if strings.HasPrefix(strings.ToLower(magnet), "magnet:") {
		enclosureType = "application/x-bittorrent;x-scheme-handler/magnet"
	}

	var attrs strings.Builder
	attr := func(name, value string) {
		if value == "" {
			return
		}
		attrs.WriteString(`      <torznab:attr name="` + name + `" value="` + xmlEscape(value) + `"/>` + "\n")
	}
	attr("magneturl", magnet)
	attr("size", strconv.FormatInt(size, 10))
	attr("seeders", strconv.Itoa(seeders))
	if leechers > 0 {
		attr("leechers", strconv.Itoa(leechers))
	}
	attr("peers", strconv.Itoa(peers))
	attr("infohash", infohash)
	// Not a private tracker and nothing here knows about freeleech, so the
	// honest answer to both is 1 — reporting 0 would tell a client a download
	// is free when nobody has said so.
	attr("downloadvolumefactor", "1")
	attr("uploadvolumefactor", "1")
	attr("site", tracker)
	attr("category", cat)
	for _, lang := range torznabLanguages(title, tracker, stringsFrom(t["languages"])) {
		attr("language", lang)
	}
	if !rqnum {
		if year := asInt(t["relased"]); year > 0 {
			attr("year", strconv.Itoa(year))
		}
	}
	if seasons := intsFrom(t["seasons"]); len(seasons) == 1 {
		// Only an unambiguous single season is reported. A pack covering
		// several would otherwise claim to be one of them, and a client
		// filtering on season would take the wrong file.
		attr("season", strconv.Itoa(seasons[0]))
	}

	comments := ""
	if strings.HasPrefix(strings.ToLower(details), "http") {
		comments = "\n      <comments>" + xmlEscape(details) + "</comments>"
	}

	return `    <item>
      <title>` + xmlEscape(display) + `</title>
      <guid isPermaLink="false">` + xmlEscape(guid) + `</guid>
      <jackettindexer id="all">` + xmlEscape(tracker) + `</jackettindexer>
      <link>` + xmlEscape(magnet) + `</link>` + comments + `
      <pubDate>` + torznabPubDate(t) + `</pubDate>
      <category>` + xmlEscape(cat) + `</category>
      <size>` + strconv.FormatInt(size, 10) + `</size>
      <enclosure url="` + xmlEscape(magnet) + `" length="` + strconv.FormatInt(size, 10) + `" type="` + enclosureType + `"/>
` + attrs.String() + `    </item>
`
}

// torznabLanguages guesses the audio language and adds what the record knows.
//
// The alphabet guess comes first and is never replaced: `languages` is derived
// by heuristics and misses external audio tracks, so an empty or partial list
// must not turn a Russian release into an unlabelled one. Ukrainian trackers
// are called out by name because their titles are Cyrillic too and would
// otherwise all read as Russian.
func torznabLanguages(title, tracker string, known []string) []string {
	primary := "rus"
	switch {
	case ukrainianTrackers[strings.ToLower(strings.TrimSpace(tracker))]:
		primary = "ukr"
	case !hasCyrillic(title) && hasLatin(title):
		primary = "eng"
	}
	out := []string{primary}
	seen := map[string]bool{primary: true}
	for _, k := range known {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// ukrainianTrackers is the set whose Cyrillic titles are Ukrainian, not
// Russian. toloka is Ukrainian by nature and mazepa's listings are Ukrainian —
// both are the reason the cyrillic subset of the self-hosted Inter font is
// bundled at all.
var ukrainianTrackers = map[string]bool{"toloka": true, "mazepa": true}

var (
	cyrillicRe = regexp.MustCompile(`[\p{Cyrillic}]`)
	latinRe    = regexp.MustCompile(`[A-Za-z]`)
)

func hasCyrillic(s string) bool { return cyrillicRe.MatchString(s) }
func hasLatin(s string) bool    { return latinRe.MatchString(s) }

// torznabPubDate formats createTime as RFC1123 GMT, which is what an RSS
// pubDate is. A missing or implausible date falls back to now rather than to
// the zero time: a 1-January-0001 item sorts to the bottom forever and some
// clients refuse the feed outright.
func torznabPubDate(t filedb.TorrentDetails) string {
	when := filedbTime(t, "createTime")
	if when.IsZero() || when.Year() < 2000 {
		when = time.Now()
	}
	return when.UTC().Format(http.TimeFormat)
}

func infoHashFromMagnet(magnet string) string {
	if m := infoHashRe.FindStringSubmatch(magnet); m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}

func stringsFrom(v any) []string {
	switch vv := v.(type) {
	case []string:
		return vv
	case []any:
		out := make([]string, 0, len(vv))
		for _, it := range vv {
			if s := asString(it); s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if vv != "" {
			return []string{vv}
		}
	}
	return nil
}

func intsFrom(v any) []int {
	switch vv := v.(type) {
	case []int:
		return vv
	case []any:
		out := make([]int, 0, len(vv))
		for _, it := range vv {
			if n := asInt(it); n > 0 {
				out = append(out, n)
			}
		}
		return out
	}
	return nil
}

// absoluteURL builds a URL on this request's own host, honouring the proxy
// headers, because a self-hosted instance has no canonical address and the
// value goes into the feed a client reads back. Same reasoning and the same
// escaping as handleOpenSearch.
func absoluteURL(r *http.Request, path string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); v != "" {
		scheme = strings.Split(v, ",")[0]
	}
	host := r.Host
	if v := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); v != "" {
		host = strings.TrimSpace(strings.Split(v, ",")[0])
	}
	return fmt.Sprintf("%s://%s%s", scheme, host, path)
}

func writeXML(w http.ResponseWriter, body string) {
	writeXMLStatus(w, http.StatusOK, body)
}

func writeXMLStatus(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}
