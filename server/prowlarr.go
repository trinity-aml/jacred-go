package server

import (
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"jacred/filedb"
)

// Prowlarr's own REST API, next to the Torznab XML it also speaks.
//
// Torznab alone is enough to add this server as a "Generic Torznab" indexer,
// which is why it came first. These three endpoints are what Prowlarr uses when
// it treats jacred as a *backend* rather than a feed: it lists indexers, reads
// one, and pulls results as JSON release resources instead of RSS. Clients
// built on Prowlarr's API (and anything pointed at a Prowlarr-compatible
// backend) need them.
//
// One aggregated indexer, id 1, exactly as upstream: the per-tracker filter
// lives in the Torznab path segment, and advertising 25 separate indexers would
// multiply every search a client makes by 25 for the same rows.
//
// All three sit behind `torznab.enable`, like the XML — they are the same
// feature wearing a different protocol, and the Jackett JSON endpoint is the
// one that has no switch.

const (
	prowlarrIndexerID   = 1
	prowlarrIndexerName = "jacred (all trackers)"
	prowlarrIndexerDesc = "Aggregated jacred search across all configured trackers"
)

var prowlarrIndexerIDRe = regexp.MustCompile(`^/api/v1/indexer/(\d+)/?$`)

// prowlarrCategoryNames names the Newznab ids this server actually emits — the
// same set server.go's categories() maps records onto, no more.
var prowlarrCategoryNames = map[int]string{
	2000: "Movies",
	2010: "Movies/Foreign",
	5000: "TV",
	5020: "TV/Foreign",
	5070: "TV/Anime",
	5080: "TV/Documentary",
}

// handleProwlarrIndexer serves both the list and the detail, and hands
// /api/v1/indexer/<id>/newznab to the Torznab handler.
//
// One mux pattern owns the whole /api/v1/indexer/ prefix, so the dispatch has
// to happen here rather than by registering three overlapping patterns.
func (s *Server) handleProwlarrIndexer(w http.ResponseWriter, r *http.Request) {
	if _, ok := torznabIndexerFromPath(r.URL.Path); ok {
		s.handleTorznab(w, r)
		return
	}
	if !s.GetConfig().Torznab.Enable {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET only"})
		return
	}

	trimmed := strings.TrimRight(r.URL.Path, "/")
	if strings.EqualFold(trimmed, "/api/v1/indexer") {
		writeCanonicalJSON(w, http.StatusOK, []any{prowlarrIndexerSummary()})
		return
	}
	m := prowlarrIndexerIDRe.FindStringSubmatch(r.URL.Path)
	if m == nil {
		http.NotFound(w, r)
		return
	}
	if id, _ := strconv.Atoi(m[1]); id != prowlarrIndexerID {
		// Only one indexer exists. Answering 404 rather than the same object
		// for every id is what stops a client from happily configuring id 7
		// and then wondering why its "other" indexer returns identical rows.
		http.NotFound(w, r)
		return
	}
	detail := prowlarrIndexerSummary()
	detail["fields"] = []any{}
	writeCanonicalJSON(w, http.StatusOK, detail)
}

func prowlarrIndexerSummary() map[string]any {
	return map[string]any{
		"id":                 prowlarrIndexerID,
		"name":               prowlarrIndexerName,
		"description":        prowlarrIndexerDesc,
		"implementation":     "Torznab",
		"implementationName": "Torznab",
		"enable":             true,
		"protocol":           "torrent",
	}
}

// handleProwlarrSearch is the Search Feed: the same search as everywhere else,
// rendered as Prowlarr release resources.
func (s *Server) handleProwlarrSearch(w http.ResponseWriter, r *http.Request) {
	cfg := s.GetConfig()
	if !cfg.Torznab.Enable {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET only"})
		return
	}
	q := r.URL.Query()

	// indexerIds selects which indexers to ask. 1 is this server and -2 means
	// "all torrent indexers"; -1 is Usenet, which this is not, so an explicit
	// Usenet-only request has to come back empty rather than with torrents.
	if ids, ok := q["indexerIds"]; ok && len(ids) > 0 {
		wanted := false
		for _, raw := range ids {
			for _, part := range strings.Split(raw, ",") {
				switch strings.TrimSpace(part) {
				case strconv.Itoa(prowlarrIndexerID), "-2", "":
					wanted = true
				}
			}
		}
		if !wanted {
			writeCanonicalJSON(w, http.StatusOK, []any{})
			return
		}
	}

	query := firstQueryURL(q, "query", "q")
	title := q.Get("title")
	titleOriginal := q.Get("title_original")
	if strings.TrimSpace(query) == "" && strings.TrimSpace(title) == "" && strings.TrimSpace(titleOriginal) == "" {
		writeCanonicalJSON(w, http.StatusOK, []any{})
		return
	}

	cacheKey := cacheKeyFor("prowlarr", r.URL.RawQuery)
	if cached, ok := s.cache.Get(cacheKey); ok {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Cache", "HIT")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(cached)
		return
	}

	params := filedb.SearchParams{
		APIKey:        firstQueryURL(q, "apikey", "apiKey"),
		Query:         query,
		Title:         title,
		TitleOriginal: titleOriginal,
		Year:          atoi(q.Get("year")),
		IsSerial:      prowlarrIsSerial(q),
		CategoryRaw:   firstCategoryValue(q),
		UserAgent:     r.UserAgent(),
	}
	res, err := s.DB.JackettSearch(params)
	if err != nil {
		writeCanonicalJSON(w, http.StatusInternalServerError, []any{})
		return
	}
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

	out := make([]map[string]any, 0, len(res.Results))
	for _, src := range res.Results {
		out = append(out, s.prowlarrRelease(src, res.RqNum, cfg.Torznab.EnrichTitles))
	}
	if data := writeCanonicalJSONCached(w, http.StatusOK, out); data != nil {
		s.cache.Set(cacheKey, data)
	}
}

// prowlarrIsSerial maps a Prowlarr search type onto the is_serial this search
// path already understands: 1 is a movie, 2 a serial, -1 "either". Lampa's
// Jackett client uses the same two values, which is why the field exists.
func prowlarrIsSerial(q map[string][]string) int {
	t := ""
	if v, ok := q["type"]; ok && len(v) > 0 {
		t = strings.ToLower(strings.TrimSpace(v[0]))
	}
	switch t {
	case "movie", "moviesearch":
		return 1
	case "tv", "tvsearch":
		return 2
	}
	// No type, or one this server has nothing to say about (music, book): fall
	// back to the categories, and otherwise do not constrain the search.
	cats := firstCategoryValue(q)
	switch {
	case strings.Contains(cats, "5000"), strings.Contains(cats, "5070"), strings.Contains(cats, "5080"):
		return 2
	case strings.Contains(cats, "2000"), strings.Contains(cats, "2010"):
		return 1
	}
	return -1
}

func (s *Server) prowlarrRelease(src filedb.TorrentDetails, rqnum, enrich bool) map[string]any {
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
	infoHash := infoHashFromMagnet(magnet)
	guid := infoHash
	if guid == "" {
		sum := md5.Sum([]byte(display))
		guid = hex.EncodeToString(sum[:])
	}

	published := filedbTime(t, "createTime")
	if published.IsZero() || published.Year() < 2000 {
		published = time.Now()
	}
	published = published.UTC()
	age := time.Since(published)
	if age < 0 {
		age = 0
	}

	var magnetURL, infoURL any
	if strings.HasPrefix(strings.ToLower(magnet), "magnet:") {
		magnetURL = magnet
	}
	if strings.HasPrefix(strings.ToLower(details), "http") {
		infoURL = details
	}
	download := magnet
	if download == "" {
		download = details
	}

	indexer := asString(t["trackerName"])
	if indexer == "" {
		indexer = prowlarrIndexerName
	}

	row := map[string]any{
		"guid":        guid,
		"age":         int(age.Hours() / 24),
		"ageHours":    age.Hours(),
		"ageMinutes":  age.Minutes(),
		"size":        int64(parseSize(t)),
		"indexerId":   prowlarrIndexerID,
		"indexer":     indexer,
		"title":       display,
		"sortTitle":   display,
		"publishDate": published.Format(time.RFC3339),
		"downloadUrl": download,
		"magnetUrl":   magnetURL,
		"infoUrl":     infoURL,
		"commentUrl":  infoURL,
		"categories":  prowlarrCategories(t),
		"protocol":    "torrent",
		"infoHash":    emptyToNil(infoHash),
		"seeders":     asInt(t["sid"]),
		"leechers":    asInt(t["pir"]),
		// The same three extensions the Jackett JSON endpoint carries, so a
		// client that already reads those does not lose them by moving here.
		"ffprobe":   nullableField(rqnum, t["ffprobe"]),
		"languages": nullableField(rqnum, t["languages"]),
		"info":      nil,
	}
	if !rqnum {
		row["info"] = map[string]any{
			"name":         t["name"],
			"originalname": t["originalname"],
			"sizeName":     t["sizeName"],
			"relased":      asInt(t["relased"]),
			"videotype":    asString(t["videotype"]),
			"quality":      asInt(t["quality"]),
			"voices":       emptyToNil(t["voices"]),
			"seasons":      emptyToNil(t["seasons"]),
			"types":        t["types"],
		}
	}
	return row
}

func prowlarrCategories(t filedb.TorrentDetails) []map[string]any {
	ids, desc := categories(t)
	sort.Ints(ids)
	out := make([]map[string]any, 0, len(ids)+1)
	for _, id := range ids {
		name := prowlarrCategoryNames[id]
		if name == "" {
			name = strconv.Itoa(id)
		}
		if len(ids) == 1 && desc != "" {
			name = desc
		}
		out = append(out, map[string]any{"id": id, "name": name, "subCategories": []any{}})
	}
	if len(out) == 0 {
		// A record whose types say nothing still has to land somewhere, or a
		// client with a category filter drops it entirely.
		name := prowlarrCategoryNames[2000]
		if desc != "" {
			name = desc
		}
		out = append(out, map[string]any{"id": 2000, "name": name, "subCategories": []any{}})
	}
	return out
}
