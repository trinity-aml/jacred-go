package server

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"jacred/filedb"
)

// The regression this whole file exists for. Measured against production on
// 2026-11-13, before any of this: the Jackett JSON path answered 200 and the
// Torznab path answered 404, so a client configured as a "Torznab indexer" —
// which is how Sonarr, Radarr and Prowlarr are normally set up — could not be
// pointed at this server at all.
func TestTorznabPathsAreRecognised(t *testing.T) {
	served := []struct {
		path, indexer string
	}{
		{"/torznab/api", ""},
		{"/api/v2.0/indexers/all/results/torznab/api", "all"},
		{"/api/v2.0/indexers/rutor/results/torznab/api/", "rutor"},
		{"/API/V2.0/INDEXERS/all/RESULTS/TORZNAB/API", "all"},
		{"/api/v1/indexer/1/newznab", "1"},
	}
	for _, c := range served {
		indexer, ok := torznabIndexerFromPath(c.path)
		if !ok {
			t.Errorf("%s was not recognised as a Torznab path", c.path)
			continue
		}
		if indexer != c.indexer {
			t.Errorf("%s → indexer %q, want %q", c.path, indexer, c.indexer)
		}
	}
	// The JSON endpoint must keep its own path to itself: clients depend on it.
	for _, path := range []string{
		"/api/v2.0/indexers/all/results",
		"/api/v2.0/indexers",
		"/torznab",
		"/api/v1/indexer",
	} {
		if _, ok := torznabIndexerFromPath(path); ok {
			t.Errorf("%s was taken for a Torznab path", path)
		}
	}
}

func TestCapsIsWellFormedAndClaimsOnlyWhatWeDo(t *testing.T) {
	caps := torznabCapsXML("https://jacred.example/torznab/api")
	if err := xml.Unmarshal([]byte(caps), new(struct{})); err != nil {
		t.Fatalf("caps is not well-formed XML: %v\n%s", err, caps)
	}
	for _, want := range []string{`id="2000"`, `id="5000"`, `id="5070"`, `max="1000"`, `default="100"`, `tv-search available="yes"`} {
		if !strings.Contains(caps, want) {
			t.Errorf("caps is missing %s:\n%s", want, caps)
		}
	}
	// This search path has no id lookup — JackettSearch takes a title, and only
	// /api/v1.0/torrents resolves tt…/kp… through an external service. Claiming
	// imdbid would make a client trust an id search that silently became a
	// title match.
	for _, nope := range []string{"imdbid", "tvdbid"} {
		if strings.Contains(caps, nope) {
			t.Errorf("caps advertises %s, which this endpoint does not implement", nope)
		}
	}
}

func sampleRecord() filedb.TorrentDetails {
	return filedb.TorrentDetails{
		"title":       "Матрица / The Matrix (1999)",
		"trackerName": "rutor",
		"url":         "https://rutor.is/torrent/123",
		"magnet":      "magnet:?xt=urn:btih:0123456789ABCDEF0123456789ABCDEF01234567&dn=x",
		"size":        float64(4500000000),
		"sid":         12,
		"pir":         3,
		"createTime":  "2026-03-04T05:06:07Z",
		"types":       []any{"movie"},
		"voices":      []any{"Гаврилов", "Дубляж"},
		"languages":   []any{"rus", "eng"},
		"relased":     1999,
	}
}

func TestItemCarriesWhatAClientNeeds(t *testing.T) {
	s := newJobTestServer(t)
	item := s.torznabItem(sampleRecord(), false, "", true)

	// Well-formedness is the one thing a client cannot work around, and it is
	// exactly what a title full of quotes and ampersands breaks.
	if err := xml.Unmarshal([]byte(item), new(struct{})); err != nil {
		t.Fatalf("item is not well-formed XML: %v\n%s", err, item)
	}

	for _, want := range []string{
		`<guid isPermaLink="false">0123456789abcdef0123456789abcdef01234567</guid>`,
		`<link>magnet:?xt=urn:btih:0123456789ABCDEF0123456789ABCDEF01234567&amp;dn=x</link>`,
		`<comments>https://rutor.is/torrent/123</comments>`,
		`<size>4500000000</size>`,
		`type="application/x-bittorrent;x-scheme-handler/magnet"`,
		`name="seeders" value="12"`,
		`name="leechers" value="3"`,
		`name="peers" value="15"`,
		`name="infohash" value="0123456789abcdef0123456789abcdef01234567"`,
		`name="downloadvolumefactor" value="1"`,
		`name="uploadvolumefactor" value="1"`,
		`name="site" value="rutor"`,
		`name="category" value="2000"`,
		`name="year" value="1999"`,
		`<pubDate>Wed, 04 Mar 2026 05:06:07 GMT</pubDate>`,
	} {
		if !strings.Contains(item, want) {
			t.Errorf("item is missing %s\n%s", want, item)
		}
	}
}

// A hostile title must not be able to close an attribute or an element.
func TestItemEscapesTheTitle(t *testing.T) {
	s := newJobTestServer(t)
	rec := sampleRecord()
	rec["title"] = `Bad "quoted" & <tagged> '`
	rec["voices"] = nil
	item := s.torznabItem(rec, false, "", true)
	if err := xml.Unmarshal([]byte(item), new(struct{})); err != nil {
		t.Fatalf("a quoted title broke the XML: %v\n%s", err, item)
	}
	if strings.Contains(item, "<tagged>") {
		t.Errorf("the title was not escaped:\n%s", item)
	}
}

// Sonarr and Radarr match on the title, and for a Russian release the dub is
// most of its identity — two otherwise identical rows are told apart by it.
func TestEnrichTitlesAppendsTheVoices(t *testing.T) {
	s := newJobTestServer(t)
	on := s.torznabItem(sampleRecord(), false, "", true)
	if !strings.Contains(on, "| [Гаврилов Дубляж].rus") {
		t.Errorf("voices not appended:\n%s", on)
	}
	off := s.torznabItem(sampleRecord(), false, "", false)
	if strings.Contains(off, "Гаврилов]") || strings.Contains(off, ".rus<") {
		t.Errorf("enrichTitles: false still rewrote the title:\n%s", off)
	}
	if !strings.Contains(off, "<title>Матрица / The Matrix (1999)</title>") {
		t.Errorf("the stored title was not emitted verbatim:\n%s", off)
	}
}

// The indexer segment of the path is a tracker filter. The older JSON endpoint
// matches it with a regex and then ignores it, so /indexers/rutor/results
// answers with every tracker; a brand-new endpoint must not repeat that.
func TestIndexerSegmentFiltersByTracker(t *testing.T) {
	items := []filedb.TorrentDetails{
		{"trackerName": "rutor", "title": "a"},
		{"trackerName": "kinozal", "title": "b"},
		// A merged duplicate stores both names in one comma-joined field, so
		// an equality test would drop exactly the rows two trackers agreed on.
		{"trackerName": "kinozal, rutracker", "title": "c"},
	}
	got := filterByTracker(items, "rutracker")
	if len(got) != 1 || asString(got[0]["title"]) != "c" {
		t.Errorf("comma-joined trackerName not matched: %v", got)
	}
	if got := filterByTracker(items, "RuTor"); len(got) != 1 {
		t.Errorf("case-insensitive match failed: %v", got)
	}
	if got := filterByTracker(items, "nosuchtracker"); len(got) != 0 {
		t.Errorf("unknown tracker returned %d rows", len(got))
	}
	// And the aggregate names must not be mistaken for a tracker.
	for _, name := range []string{"", "all", "jacred", "ALL"} {
		if !aggregateIndexerNames[strings.ToLower(name)] {
			t.Errorf("%q should mean every tracker", name)
		}
	}
}

func TestWindowAppliesOffsetAndLimit(t *testing.T) {
	items := make([]filedb.TorrentDetails, 250)
	for i := range items {
		items[i] = filedb.TorrentDetails{"title": strconv.Itoa(i)}
	}
	if got := applyTorznabWindow(items, map[string][]string{}); len(got) != 100 {
		t.Errorf("no limit given → %d rows, want the advertised default of 100", len(got))
	}
	if got := applyTorznabWindow(items, map[string][]string{"limit": {"5"}}); len(got) != 5 {
		t.Errorf("limit=5 → %d rows", len(got))
	}
	if got := applyTorznabWindow(items, map[string][]string{"limit": {"9999"}}); len(got) != 250 {
		t.Errorf("limit above the advertised max → %d rows, want all 250", len(got))
	}
	got := applyTorznabWindow(items, map[string][]string{"offset": {"248"}, "limit": {"10"}})
	if len(got) != 2 || asString(got[0]["title"]) != "248" {
		t.Errorf("offset past the tail → %v", got)
	}
	if got := applyTorznabWindow(items, map[string][]string{"offset": {"9999"}}); len(got) != 0 {
		t.Errorf("offset beyond the end → %d rows", len(got))
	}
}

// A client's search mode wins over the record's own types: a tvsearch result
// filed under Movies is dropped by the client.
func TestSearchModeDecidesTheCategory(t *testing.T) {
	cases := []struct{ t, cat, want string }{
		{"tvsearch", "", "5000"},
		{"tv", "", "5000"},
		{"movie", "", "2000"},
		{"moviesearch", "", "2000"},
		{"search", "5070,5000", "5070"},
		{"search", "", ""},
	}
	for _, c := range cases {
		q := map[string][]string{}
		if c.cat != "" {
			q["cat"] = []string{c.cat}
		}
		if got := assignedCategory(c.t, q); got != c.want {
			t.Errorf("assignedCategory(%q, cat=%q) = %q, want %q", c.t, c.cat, got, c.want)
		}
	}
	// With no assignment the record's own category is used.
	s := newJobTestServer(t)
	if item := s.torznabItem(sampleRecord(), false, "", false); !strings.Contains(item, "<category>2000</category>") {
		t.Errorf("record category not used:\n%s", item)
	}
	if item := s.torznabItem(sampleRecord(), false, "5000", false); !strings.Contains(item, "<category>5000</category>") {
		t.Errorf("assigned category did not win:\n%s", item)
	}
}

// A zero pubDate sorts an item to the bottom for good, and some clients refuse
// a feed that carries one.
func TestPubDateFallsBackToNow(t *testing.T) {
	for _, raw := range []any{nil, "", "0001-01-01T00:00:00Z", "not a date"} {
		rec := sampleRecord()
		rec["createTime"] = raw
		got := torznabPubDate(rec)
		when, err := time.Parse(http.TimeFormat, got)
		if err != nil {
			t.Fatalf("createTime=%v produced an unparseable pubDate %q: %v", raw, got, err)
		}
		if when.Year() < 2000 {
			t.Errorf("createTime=%v produced %q", raw, got)
		}
	}
}

// Without an infohash the guid still has to be stable, or a client re-grabs the
// same row every time it refreshes.
func TestGuidIsStableWithoutAnInfohash(t *testing.T) {
	s := newJobTestServer(t)
	rec := sampleRecord()
	rec["magnet"] = ""
	first := s.torznabItem(rec, false, "", true)
	second := s.torznabItem(rec, false, "", true)
	if first != second {
		t.Error("two renders of the same record differ")
	}
	if strings.Contains(first, `<guid isPermaLink="false"></guid>`) {
		t.Errorf("empty guid:\n%s", first)
	}
	// With no magnet the detail URL is the only thing left to hand a client.
	if !strings.Contains(first, "<link>https://rutor.is/torrent/123</link>") {
		t.Errorf("no magnet and no link:\n%s", first)
	}
}

// The alphabet guess comes first and is never replaced by the record's
// `languages`, which is derived by heuristics and misses external audio tracks.
func TestLanguageGuessLeadsAndIsNotOverwritten(t *testing.T) {
	cases := []struct {
		title, tracker string
		known          []string
		wantFirst      string
	}{
		{"Матрица", "rutor", nil, "rus"},
		{"The Matrix", "rutor", nil, "eng"},
		{"Матриця", "toloka", nil, "ukr"},
		{"Матриця", "mazepa", []string{"rus"}, "ukr"},
		{"The Matrix", "rutor", []string{"rus"}, "eng"},
		{"1917", "rutor", nil, "rus"},
	}
	for _, c := range cases {
		got := torznabLanguages(c.title, c.tracker, c.known)
		if len(got) == 0 || got[0] != c.wantFirst {
			t.Errorf("torznabLanguages(%q, %q, %v) = %v, want %q first", c.title, c.tracker, c.known, got, c.wantFirst)
		}
	}
	// Known languages are added after the guess, deduplicated.
	got := torznabLanguages("The Matrix", "rutor", []string{"eng", "rus", "rus"})
	if strings.Join(got, ",") != "eng,rus" {
		t.Errorf("languages = %v, want eng then rus once", got)
	}
}

func TestTorznabCanBeTurnedOff(t *testing.T) {
	s := newJobTestServer(t)
	cfg := s.GetConfig()
	cfg.Torznab.Enable = false
	s.Config = cfg

	w := httptest.NewRecorder()
	s.handleTorznab(w, httptest.NewRequest(http.MethodGet, "/torznab/api?t=caps", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("torznab.enable: false still answered HTTP %d", w.Code)
	}
}

// Prowlarr sends HEAD before it will save an indexer, and a search mode it does
// not recognise must not become a wildcard query over the whole database.
func TestHeadAndEmptyQueryAnswerAnEmptyFeed(t *testing.T) {
	s := newJobTestServer(t)

	w := httptest.NewRecorder()
	s.handleTorznab(w, httptest.NewRequest(http.MethodHead, "/torznab/api", nil))
	if w.Code != http.StatusOK {
		t.Errorf("HEAD answered HTTP %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/xml") {
		t.Errorf("HEAD Content-Type = %q", ct)
	}

	w = httptest.NewRecorder()
	s.handleTorznab(w, httptest.NewRequest(http.MethodGet, "/torznab/api?t=search", nil))
	body, _ := io.ReadAll(w.Body)
	if w.Code != http.StatusOK {
		t.Fatalf("empty search answered HTTP %d", w.Code)
	}
	if err := xml.Unmarshal(body, new(struct{})); err != nil {
		t.Fatalf("empty feed is not well-formed: %v\n%s", err, body)
	}
	if strings.Contains(string(body), "<item>") {
		t.Errorf("a query-less request returned items:\n%s", body)
	}
}

// The feed advertises its own URL back to the client, and a self-hosted
// instance has no canonical address — so it has to come from the request,
// proxy headers included, like /opensearch.xml already does.
func TestFeedURLComesFromTheRequest(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/torznab/api?t=caps", nil)
	r.Host = "box.lan:9118"
	if got := absoluteURL(r, r.URL.Path); got != "http://box.lan:9118/torznab/api" {
		t.Errorf("absoluteURL = %q", got)
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "jacred.example, inner")
	if got := absoluteURL(r, r.URL.Path); got != "https://jacred.example/torznab/api" {
		t.Errorf("behind a proxy absoluteURL = %q", got)
	}
}

// Through the real router, because the thing that was broken in production was
// the routing, not a helper: /api/v2.0/indexers/all/results/torznab/api
// answered 404 while the JSON path beside it answered 200. A test of
// torznabIndexerFromPath alone does not notice the dispatch being removed —
// verified by removing it and watching this fail while that one passed.
func TestTorznabIsReachableThroughTheRouter(t *testing.T) {
	s := newJobTestServer(t)
	h := s.Handler()

	for _, path := range []string{
		"/torznab/api?t=caps",
		"/api/v2.0/indexers/all/results/torznab/api?t=caps",
		"/api/v1/indexer/1/newznab?t=caps",
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Errorf("%s → HTTP %d, want 200", path, w.Code)
			continue
		}
		body := w.Body.String()
		if !strings.Contains(body, "<caps>") {
			t.Errorf("%s did not return caps:\n%s", path, truncate(body, 200))
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/xml") {
			t.Errorf("%s Content-Type = %q", path, ct)
		}
	}

	// And the JSON endpoint must still be JSON on its own path.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v2.0/indexers/all/results?Query=nothing", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"jacred"`) {
		t.Errorf("the Jackett JSON path changed: HTTP %d %s", w.Code, truncate(w.Body.String(), 200))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// The indexer comes from the path and the cache key was built from the query
// string alone, so /indexers/kinozal/… was served the cached /indexers/all/…
// feed and the tracker filter appeared to do nothing. Found live, not by the
// unit tests above — they never touch the cache.
func TestTheCacheKeyIncludesTheIndexer(t *testing.T) {
	s := newJobTestServer(t)
	h := s.Handler()

	ask := func(path string) string {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s → HTTP %d", path, w.Code)
		}
		return w.Body.String()
	}

	// Same query, different indexer: the second must not be the first's feed.
	const q = "?t=search&q=whatever"
	all := ask("/api/v2.0/indexers/all/results/torznab/api" + q)
	one := ask("/api/v2.0/indexers/rutor/results/torznab/api" + q)

	if !strings.Contains(all, "indexers/all/results") {
		t.Errorf("the aggregate feed does not name its own URL:\n%s", truncate(all, 300))
	}
	if strings.Contains(one, "indexers/all/results") {
		t.Errorf("the per-tracker request was served the cached aggregate feed:\n%s", truncate(one, 300))
	}
	if !strings.Contains(one, "indexers/rutor/results") {
		t.Errorf("the per-tracker feed does not name its own URL:\n%s", truncate(one, 300))
	}
}
