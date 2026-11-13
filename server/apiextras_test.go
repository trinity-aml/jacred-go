package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jacred/core"
	"jacred/filedb"
)

// ---------------------------------------------------------------- indexer filter

// The Jackett JSON endpoint matched the indexer segment with a regex and then
// never read it, so /indexers/rutor/results answered with every tracker while
// looking like it had filtered — and the README's own example URL uses that
// segment. Fixed alongside the Torznab endpoint, which never had the bug.
func TestJackettJSONHonoursTheIndexerSegment(t *testing.T) {
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
	// Two trackers carrying the same title. Real records are needed here, not
	// an empty index: with no results both responses are byte-identical, so a
	// cache key that drops the indexer is invisible — which is exactly how the
	// Torznab endpoint shipped the same bug and only a live run caught it.
	// Both rows go into one call: they share a name, so KeyDb maps them to the
	// same bucket and a second SaveBucket would replace the first.
	seedSearchable(t, s, "theindexertest", "rutor", "kinozal")

	const q = "?q=theindexertest"
	all := ask("/api/v2.0/indexers/all/results" + q)
	if n := countJackettResults(t, all); n != 2 {
		t.Fatalf("aggregate request returned %d results, want 2: %s", n, truncate(all, 300))
	}
	one := ask("/api/v2.0/indexers/rutor/results" + q)
	if n := countJackettResults(t, one); n != 1 {
		t.Errorf("/indexers/rutor/results returned %d results, want 1 — the segment is no filter", n)
	}
	if strings.Contains(one, "kinozal") {
		t.Errorf("the rutor request carried a kinozal row:\n%s", truncate(one, 300))
	}
	if n := countJackettResults(t, ask("/api/v2.0/indexers/nnmclub/results"+q)); n != 0 {
		t.Errorf("a tracker with no rows returned %d results", n)
	}

	// The filter itself, on records: an aggregate name must not be read as a
	// tracker, and a real one must exclude the others.
	items := []filedb.TorrentDetails{
		{"trackerName": "rutor", "title": "a"},
		{"trackerName": "kinozal", "title": "b"},
	}
	if got := filterByTracker(items, "rutor"); len(got) != 1 {
		t.Errorf("filterByTracker(rutor) = %d rows, want 1", len(got))
	}
	for _, name := range []string{"all", "jacred", "status"} {
		if !aggregateIndexerNames[name] {
			t.Errorf("%q should mean every tracker", name)
		}
	}
}

// ---------------------------------------------------------------- prowlarr

func TestProwlarrIndexerListAndDetail(t *testing.T) {
	s := newJobTestServer(t)
	h := s.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/indexer", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list → HTTP %d", w.Code)
	}
	var list []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("list is not a JSON array: %v: %s", err, w.Body.String())
	}
	if len(list) != 1 {
		t.Fatalf("want exactly one aggregated indexer, got %d", len(list))
	}
	if list[0]["id"] != float64(prowlarrIndexerID) || list[0]["protocol"] != "torrent" {
		t.Errorf("indexer summary = %v", list[0])
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/indexer/1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("detail → HTTP %d", w.Code)
	}
	var detail map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("detail is not JSON: %v", err)
	}
	if _, ok := detail["fields"]; !ok {
		t.Errorf("detail is missing the fields array Prowlarr expects: %v", detail)
	}

	// Only one indexer exists. Answering the same object for every id would
	// let a client configure id 7 and then wonder why its "other" indexer
	// returns identical rows.
	for _, path := range []string{"/api/v1/indexer/2", "/api/v1/indexer/99"} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s → HTTP %d, want 404", path, w.Code)
		}
	}
	// And /api/v1/indexer/<id>/newznab still has to reach the Torznab handler.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/indexer/1/newznab?t=caps", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<caps>") {
		t.Errorf("newznab under the indexer prefix broke: HTTP %d %s", w.Code, truncate(w.Body.String(), 150))
	}
}

func TestProwlarrSearchFeed(t *testing.T) {
	s := newJobTestServer(t)
	h := s.Handler()

	// -1 is Usenet. This server has none, so an explicit Usenet-only request
	// must come back empty rather than with torrents.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/search?query=x&indexerIds=-1", nil))
	if body := strings.TrimSpace(w.Body.String()); body != "[]" {
		t.Errorf("indexerIds=-1 returned %q", truncate(body, 120))
	}
	// A query-less request is what a client's test button sends.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/search", nil))
	if body := strings.TrimSpace(w.Body.String()); body != "[]" {
		t.Errorf("empty query returned %q", truncate(body, 120))
	}
	// And a real one answers a JSON array.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/search?query=whatever&indexerIds=1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("search → HTTP %d", w.Code)
	}
	var rows []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("search feed is not a JSON array: %v: %s", err, truncate(w.Body.String(), 200))
	}
}

// The release resource is what Prowlarr reads, so the field names matter more
// than anything else here.
func TestProwlarrReleaseShape(t *testing.T) {
	s := newJobTestServer(t)
	row := s.prowlarrRelease(sampleRecord(), false, true)

	for _, k := range []string{
		"guid", "age", "ageHours", "ageMinutes", "size", "indexerId", "indexer",
		"title", "sortTitle", "publishDate", "downloadUrl", "magnetUrl",
		"infoUrl", "commentUrl", "categories", "protocol", "infoHash",
		"seeders", "leechers",
	} {
		if _, ok := row[k]; !ok {
			t.Errorf("release is missing %q: %v", k, row)
		}
	}
	if row["protocol"] != "torrent" {
		t.Errorf("protocol = %v", row["protocol"])
	}
	if row["guid"] != "0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("guid = %v, want the infohash", row["guid"])
	}
	if !strings.Contains(row["title"].(string), "| [Гаврилов Дубляж].rus") {
		t.Errorf("enrichTitles not applied: %v", row["title"])
	}
	cats, _ := row["categories"].([]map[string]any)
	if len(cats) != 1 || cats[0]["id"] != 2000 {
		t.Errorf("categories = %v", cats)
	}
	// A record whose types say nothing still has to land in a category, or a
	// client filtering on one drops it entirely.
	bare := sampleRecord()
	delete(bare, "types")
	cats, _ = s.prowlarrRelease(bare, false, false)["categories"].([]map[string]any)
	if len(cats) != 1 || cats[0]["id"] != 2000 {
		t.Errorf("a typeless record got categories %v", cats)
	}
}

func TestProwlarrFollowsTheTorznabSwitch(t *testing.T) {
	s := newJobTestServer(t)
	cfg := s.GetConfig()
	cfg.Torznab.Enable = false
	s.Config = cfg
	h := s.Handler()

	for _, path := range []string{"/api/v1/indexer", "/api/v1/indexer/1", "/api/v1/search?query=x"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s with torznab disabled → HTTP %d, want 404", path, w.Code)
		}
	}
}

// ---------------------------------------------------------------- parseall status

// The count has to work for every task-map shape without a per-parser
// accessor: a flat list, a category map, and a nested map are all in use.
// What they share is that CycleFields is inlined, so `parseAllCycleId` sits on
// each page object whatever surrounds it.
func TestCyclePageCountHandlesEveryMapShape(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name, body  string
		pages, done int
	}{
		{"flat list", `[{"page":1,"parseAllCycleId":"C"},{"page":2}]`, 2, 1},
		{"category map", `{"1":[{"page":0,"parseAllCycleId":"C"},{"page":1,"parseAllCycleId":"OLD"}],"2":[{"page":0,"parseAllCycleId":"C"}]}`, 3, 2},
		{"nested map", `{"a":{"&d=2024":[{"page":0,"parseAllCycleId":"C"}],"&d=2025":[{"page":0}]}}`, 2, 1},
		{"empty", `{}`, 0, 0},
	}
	for _, c := range cases {
		path := filepath.Join(dir, strings.ReplaceAll(c.name, " ", "_")+".json")
		if err := os.WriteFile(path, []byte(c.body), 0o644); err != nil {
			t.Fatal(err)
		}
		pages, done, err := countCyclePages(path, "C")
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if pages != c.pages || done != c.done {
			t.Errorf("%s: pages=%d done=%d, want %d/%d", c.name, pages, done, c.pages, c.done)
		}
	}
	if _, _, err := countCyclePages(filepath.Join(dir, "missing.json"), "C"); err == nil {
		t.Error("a missing task map should be an error, not a silent zero")
	}
}

func TestParseAllStatusReadsTheCheckpoints(t *testing.T) {
	s := newJobTestServer(t)
	dir := filepath.Join(s.dataDir(), "temp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cycle := core.NewParseAllCycle("fp", 3)
	if err := core.SaveParseAllCycle(core.ParseAllCyclePath(s.dataDir(), "rutor"), cycle); err != nil {
		t.Fatal(err)
	}
	body := `{"1":[{"page":0,"parseAllCycleId":"` + cycle.CycleID + `"},{"page":1},{"page":2}]}`
	if err := os.WriteFile(filepath.Join(dir, "rutor_taskParse.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handleParseAllStatus(w, httptest.NewRequest(http.MethodGet, "/cron/maintenance/parseallstatus", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d", w.Code)
	}
	var out struct {
		Trackers []parseAllTrackerStatus `json:"trackers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON: %v: %s", err, w.Body.String())
	}
	if len(out.Trackers) != 1 {
		t.Fatalf("want one tracker, got %d: %s", len(out.Trackers), w.Body.String())
	}
	got := out.Trackers[0]
	if got.Tracker != "rutor" || got.Pages != 3 || got.Done != 1 || got.Pending != 2 {
		t.Errorf("status = %+v", got)
	}
	if got.CycleID != cycle.CycleID {
		t.Errorf("cycleId = %q, want %q", got.CycleID, cycle.CycleID)
	}
	if got.Percent != 33 {
		t.Errorf("percent = %d, want 33", got.Percent)
	}
}

// /cron/maintenance/... has the shape of a cron path but is not a parser, so
// recording it would put a row in the run store that no page reads — and a
// typo'd tracker name would do the same.
func TestOnlyRealTrackersAreRecorded(t *testing.T) {
	for _, name := range []string{"rutor", "rutracker", "bitruapi"} {
		if !isKnownTracker(name) {
			t.Errorf("%q should be a known tracker", name)
		}
	}
	for _, name := range []string{"maintenance", "rutorr", "", "cloudflare"} {
		if isKnownTracker(name) {
			t.Errorf("%q should not be a known tracker", name)
		}
	}

	s := newJobTestServer(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/cron/maintenance/parseallstatus", s.handleParseAllStatus)
	h := s.recordCronRuns(mux)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/cron/maintenance/parseallstatus", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d", w.Code)
	}
	if runs := s.Runs.forTracker("maintenance"); len(runs) != 0 {
		t.Errorf("a maintenance call was recorded as a parser run: %+v", runs)
	}
}

// ---------------------------------------------------------------- stats meta

func TestStatsMetaReportsStaleness(t *testing.T) {
	s := newJobTestServer(t)
	dir := filepath.Join(s.dataDir(), "temp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stats.json"), []byte(`{"x":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handleStatsMeta(w, httptest.NewRequest(http.MethodGet, "/stats/meta", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d", w.Code)
	}
	var out struct {
		Now          string          `json:"now"`
		LastUpdateDB string          `json:"lastupdatedb"`
		Files        []statsMetaFile `json:"files"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON: %v: %s", err, w.Body.String())
	}
	if out.Now == "" || out.LastUpdateDB == "" {
		t.Errorf("missing now/lastupdatedb: %s", w.Body.String())
	}
	var stats *statsMetaFile
	for i := range out.Files {
		if out.Files[i].Name == "stats.json" {
			stats = &out.Files[i]
		}
		// Every row has to say what reads it: a bare filename tells an
		// operator nothing about which page went stale.
		if out.Files[i].Serves == "" {
			t.Errorf("%q does not say what it serves", out.Files[i].Name)
		}
	}
	if stats == nil || !stats.Exists || stats.Bytes == 0 || stats.ModifiedAt == "" {
		t.Errorf("stats.json row = %+v", stats)
	}
	// An absent file is normal, not an error — a box that has never met a
	// Cloudflare challenge has no cf_auto.json.
	for _, f := range out.Files {
		if !f.Exists && f.Error != "" {
			t.Errorf("absent %q reported an error %q", f.Name, f.Error)
		}
	}
}

// seedSearchable writes one record per tracker, all under one name, so
// JackettSearch finds them together and a per-tracker request has something to
// filter out.
func seedSearchable(t *testing.T, s *Server, name string, trackers ...string) {
	t.Helper()
	key := s.DB.KeyDb(name, name)
	bucket := map[string]filedb.TorrentDetails{}
	for _, tracker := range trackers {
		url := "https://" + tracker + ".example/t/" + name
		bucket[url] = filedb.TorrentDetails{
			"title":        name + " (2026)",
			"name":         name,
			"originalname": name,
			"trackerName":  tracker,
			"magnet":       "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef0123456" + tracker[:1],
			"url":          url,
			"size":         float64(1000),
			"sid":          1,
			"createTime":   "2026-03-04T05:06:07Z",
			"types":        []any{"movie"},
		}
	}
	if err := s.DB.SaveBucket(key, bucket, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.SaveChangesToFileNow(); err != nil {
		t.Fatal(err)
	}
}

func countJackettResults(t *testing.T, body string) int {
	t.Helper()
	var out struct {
		Results []map[string]any `json:"Results"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("not the Jackett JSON shape: %v: %s", err, truncate(body, 200))
	}
	return len(out.Results)
}
