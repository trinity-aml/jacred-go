package subsplease

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jacred/filedb"
)

// The fixtures in testdata/ are live captures from subsplease.org (2026-09-25),
// trimmed for size. There is no account anywhere in this tracker, so nothing
// needed scrubbing: the magnets carry public announce URLs (nyaa.tracker.wf,
// opentrackr) and no passkey — unlike mazepa or anibelka, where a logged-in
// download stamps the account into the announce.
func load(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

const host = "https://subsplease.org"

func TestParseLatest(t *testing.T) {
	items, err := parseLatestJSON(load(t, "latest.json"), host)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("no releases parsed")
	}
	for _, it := range items {
		title := asString(it["title"])
		if !strings.HasPrefix(title, "[SubsPlease] ") {
			t.Errorf("title %q lost its prefix", title)
		}
		if !strings.Contains(title, "(1080p)") {
			t.Errorf("title %q is not the 1080p variant", title)
		}
		// The parser must NOT preset quality: UpdateFullDetails skips any
		// record that already has quality and _sn, so presetting it makes a
		// new record lose seasons/videotype/voices/languages for good.
		if q, set := it["quality"]; set {
			t.Errorf("%s: parser preset quality=%v; leave it to UpdateFullDetails", title, q)
		}
		mag := asString(it["magnet"])
		if !strings.HasPrefix(mag, "magnet:?") {
			t.Errorf("%s: magnet = %.40q", title, mag)
		}
		if asString(it["sizeName"]) == "" {
			t.Errorf("%s: no sizeName — the magnet's xl= was not read", title)
		}
		if u := asString(it["url"]); !strings.Contains(u, "/shows/") || !strings.Contains(u, "ep=") {
			t.Errorf("%s: url = %q", title, u)
		}
		if asString(it["name"]) == "" || asString(it["originalname"]) == "" {
			t.Errorf("%s: name/originalname missing — the bucket key would be empty", title)
		}
	}
}

// Every release is published at 480/540, 720 and 1080. Storing all three would
// put three records under one name for the same episode.
func TestOnlyThePreferredResolutionIsKept(t *testing.T) {
	body := load(t, "latest.json")
	if !strings.Contains(body, `"res": "720"`) {
		t.Fatal("fixture has no 720p download, so this proves nothing")
	}
	items, err := parseLatestJSON(body, host)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		mag := asString(it["magnet"])
		if strings.Contains(mag, "720p") || strings.Contains(mag, "480p") {
			t.Errorf("a non-1080p magnet was stored: %.80q", mag)
		}
	}
}

// Distinct episodes must get distinct URLs: dedup is by URL, so a shared one
// would make every new episode overwrite the previous record.
func TestEpisodesGetDistinctURLs(t *testing.T) {
	items, err := parseLatestJSON(load(t, "latest.json"), host)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, it := range items {
		u, title := asString(it["url"]), asString(it["title"])
		if prev, dup := seen[u]; dup {
			t.Errorf("url %q shared by %q and %q", u, prev, title)
		}
		seen[u] = title
	}
}

func TestParseShowSections(t *testing.T) {
	items, err := parseShowJSON(load(t, "show_sid96.json"), host, "2-43-seiin-koukou-danshi-volley-bu")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var batches, episodes int
	for _, it := range items {
		if strings.Contains(asString(it["title"]), "[Batch]") {
			batches++
		} else {
			episodes++
		}
		// f=show entries carry no "page" of their own; without the slug being
		// substituted buildTorrent cannot form a URL and drops the row.
		if u := asString(it["url"]); !strings.Contains(u, "/shows/2-43-seiin-koukou-danshi-volley-bu/") {
			t.Errorf("url did not take the requested slug: %q", u)
		}
	}
	if batches == 0 {
		t.Error("the batch section produced nothing")
	}
	if episodes == 0 {
		t.Error("the episode section produced nothing")
	}
}

// An empty section arrives as [] rather than {}, which must not abort the
// other section.
func TestShowToleratesEmptySectionArray(t *testing.T) {
	items, err := parseShowJSON(`{"batch":[],"episode":{"X - 1":{"show":"X","episode":"1","page":"x",
		"downloads":[{"res":"1080","magnet":"magnet:?xt=urn:btih:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA&xl=1048576"}]}}}`,
		host, "x")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d records, want 1", len(items))
	}
}

func TestIsBatchEpisode(t *testing.T) {
	for _, c := range []struct {
		ep   string
		want bool
	}{
		{"01-12", true}, {"13-24", true}, {"1 ~ 12", true},
		{"12", false}, {"07", false}, {"", false},
	} {
		if got := isBatchEpisode(c.ep); got != c.want {
			t.Errorf("isBatchEpisode(%q) = %v, want %v", c.ep, got, c.want)
		}
	}
}

func TestExtractShowSid(t *testing.T) {
	if got := extractShowSid(load(t, "show_page_sid96.html")); got != "96" {
		t.Errorf("sid = %q, want 96", got)
	}
	// Attribute order is not guaranteed by the markup.
	if got := extractShowSid(`<table sid="7" id="show-release-table">`); got != "7" {
		t.Errorf("reversed attribute order: sid = %q, want 7", got)
	}
	if got := extractShowSid("<html><body>no table</body></html>"); got != "" {
		t.Errorf("sid invented from a page without one: %q", got)
	}
}

func TestParseShowSlugs(t *testing.T) {
	slugs := parseShowSlugsFromIndexHTML(load(t, "shows_index.html"))
	if len(slugs) == 0 {
		t.Fatal("no slugs parsed from the catalogue")
	}
	seen := map[string]bool{}
	for _, s := range slugs {
		if seen[s] {
			t.Errorf("duplicate slug %q", s)
		}
		seen[s] = true
		if strings.Contains(s, "/") || s == "" {
			t.Errorf("bad slug %q", s)
		}
	}
}

func TestParseSchedule(t *testing.T) {
	slugs := parseSchedulePageSlugs(load(t, "schedule.json"))
	if len(slugs) == 0 {
		t.Fatal("no airing shows parsed — the sweep would lose its priority order")
	}
}

// limit_reached arrives as HTTP 200 with a body, so without this it reads as an
// empty catalogue: a throttled run and a quiet day would look identical, which
// is how a stalled tracker stays invisible for weeks.
func TestLimitReachedIsNotAnEmptyResult(t *testing.T) {
	if _, err := parseLatestJSON(`{"limit_reached":true}`, host); !errors.Is(err, errLimitReached) {
		t.Errorf("latest: err = %v, want errLimitReached", err)
	}
	if _, err := parseShowJSON(`{"limit_reached":true}`, host, "x"); !errors.Is(err, errLimitReached) {
		t.Errorf("show: err = %v, want errLimitReached", err)
	}
	if statusFor(errLimitReached) != "rate_limited" {
		t.Error("a throttled run must be reported as rate_limited on /trackers")
	}
}

// A genuinely empty response is not an error — it just ends the page walk.
func TestEmptyResponseIsNotAnError(t *testing.T) {
	for _, body := range []string{"", "[]", "   "} {
		items, err := parseLatestJSON(body, host)
		if err != nil || len(items) != 0 {
			t.Errorf("body %q: items=%d err=%v", body, len(items), err)
		}
	}
}

func TestMalformedJSONIsReported(t *testing.T) {
	if _, err := parseLatestJSON(`{"broken":`, host); err == nil {
		t.Error("malformed JSON parsed as success")
	}
}

// A release with no 1080p download is dropped rather than stored without a
// magnet — a record with no magnet is useless to every consumer.
func TestReleaseWithoutPreferredResIsDropped(t *testing.T) {
	items, err := parseLatestJSON(`{"X - 1":{"show":"X","episode":"1","page":"x",
		"downloads":[{"res":"720","magnet":"magnet:?xt=urn:btih:BBBB"}]}}`, host)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("got %d records, want 0", len(items))
	}
}

func TestFormatSizeRoundTripsThroughComputeSize(t *testing.T) {
	// The spelling matters: filedb.computeSize reads "gb"/"tb" and treats
	// anything else as megabytes, so a unit it cannot place silently gives
	// size=0 for the whole tracker.
	for _, c := range []struct {
		bytes int64
		want  string
	}{
		{1048576, "1.00 Mb"},
		{1073741824, "1.00 GB"},
		{1099511627776, "1.00 TB"},
		{0, ""},
		{-1, ""},
	} {
		if got := formatSize(c.bytes); got != c.want {
			t.Errorf("formatSize(%d) = %q, want %q", c.bytes, got, c.want)
		}
	}
}

func TestMagnetSizeBytes(t *testing.T) {
	if got := magnetSizeBytes("magnet:?xt=urn:btih:AA&xl=394037490&dn=x"); got != 394037490 {
		t.Errorf("xl = %d", got)
	}
	if got := magnetSizeBytes("magnet:?xt=urn:btih:AA"); got != 0 {
		t.Errorf("missing xl should give 0, got %d", got)
	}
}

func TestParseReleaseDate(t *testing.T) {
	got := parseReleaseDate("Thu, 10 Sep 2026 17:32:07 +0000")
	if got.IsZero() {
		t.Fatal("the API's own date format did not parse")
	}
	if got.Year() != 2026 || got.Month() != 9 || got.Day() != 10 {
		t.Errorf("parsed %s", got)
	}
	if !parseReleaseDate("nonsense").IsZero() {
		t.Error("nonsense produced a date")
	}
}

// The save path is MergeTorrent then UpdateFullDetails (SaveBucket runs the
// latter over the whole bucket). This pins the interaction end to end, because
// it is subtle in exactly the way that hides: UpdateFullDetails returns early
// when a record already carries both quality and _sn, mergeNew always sets
// _sn, and so a parser that fills in quality itself makes every new record
// look "already processed" — losing seasons, videotype, voices and languages
// permanently, while size still looks right because it is computed before the
// early return. Measured on live records before the fix: quality=1080 and
// size=1428076625 were correct while videotype and seasons stayed empty.
func TestRecordGetsComputedFieldsThroughTheSavePath(t *testing.T) {
	rel := release{
		Show: "Link Click S3", Episode: "08", Page: "link-click-s3",
		ReleaseDate: "Thu, 25 Sep 2026 03:02:55 +0000",
		Downloads: []download{
			{Res: "720", Magnet: "magnet:?xt=urn:btih:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB&xl=1"},
			{Res: "1080", Magnet: "magnet:?xt=urn:btih:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA&xl=1424579162"},
		},
	}
	rec := buildTorrent(rel, host, "")
	if rec == nil {
		t.Fatal("buildTorrent returned nil")
	}
	if _, set := rec["quality"]; set {
		t.Fatal("the parser preset quality; UpdateFullDetails would then skip this record")
	}

	merged := filedb.MergeTorrent(nil, rec, 0)
	if !merged.NeedsFull {
		t.Error("a new record should be flagged for UpdateFullDetails")
	}
	filedb.UpdateFullDetails(merged.Torrent)

	got := merged.Torrent
	if got["quality"] != 1080 {
		t.Errorf("quality = %v, want 1080 derived from the title", got["quality"])
	}
	if asString(got["videotype"]) == "" {
		t.Error("videotype is empty — UpdateFullDetails did not run to completion")
	}
	if seasons, ok := got["seasons"].([]int); !ok || len(seasons) != 1 || seasons[0] != 3 {
		t.Errorf("seasons = %v, want [3] from \"S3\" in the title", got["seasons"])
	}
	if got["size"] == nil {
		t.Error("size was not derived from sizeName")
	}
}
