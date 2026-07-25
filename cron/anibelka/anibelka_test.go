package anibelka

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Fixtures were captured anonymously from anibelka.com on 2026-07-25:
// forum_f33.html is the first listing page of "Аниме с озвучкой", the two
// topic_*.html are topic pages from the dubbed and the feature-film sections.

func load(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	return string(b)
}

func TestParseListingHTML(t *testing.T) {
	items := parseListingHTML(load(t, "forum_f33.html"))
	if len(items) == 0 {
		t.Fatal("no topics parsed")
	}
	for i, it := range items {
		if it.topicID == "" {
			t.Errorf("row %d: empty topic id", i)
		}
		// Pinned service topics ("Информация по разделу", "Большие аниме …")
		// hold no torrent and must not reach the per-topic fetch.
		if !strings.HasPrefix(it.title, "[") {
			t.Errorf("row %d: service topic leaked into the list: %q", i, it.title)
		}
	}
	first := items[0]
	if first.title != "[rus] Фермерская жизнь в ином мире / Isekai Nonbiri Nouka [2TV][2023-2026, повседневность, фэнтези]" {
		t.Errorf("first title = %q", first.title)
	}
	if first.topicID != "1849" {
		t.Errorf("first topicID = %q, want 1849", first.topicID)
	}
}

func TestParseTopicHTML(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Moscow")
	info, ok := parseTopicHTML(load(t, "topic_rus.html"), loc)
	if !ok {
		t.Fatal("torrent attachment not found")
	}
	// The topic also carries a poster served by the same endpoint; picking the
	// wrong id yields a JPEG that no bencode parser will accept.
	if info.torrentID != "7316" {
		t.Errorf("torrentID = %q, want 7316 (the .torrent, not the poster)", info.torrentID)
	}
	if info.sizeName != "4.75 ГБ" {
		t.Errorf("sizeName = %q, want %q", info.sizeName, "4.75 ГБ")
	}
	if info.sid != 5 {
		t.Errorf("sid = %d, want 5", info.sid)
	}
	if info.pir != 0 {
		t.Errorf("pir = %d, want 0", info.pir)
	}
	if info.createTime.IsZero() {
		t.Error("createTime not parsed")
	}
	if got := info.createTime.UTC().Format("2006-01-02"); got != "2026-07-23" {
		t.Errorf("createTime = %s, want 2026-07-23", got)
	}
}

func TestParseTopicHTMLFeatureFilm(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Moscow")
	info, ok := parseTopicHTML(load(t, "topic_mv.html"), loc)
	if !ok {
		t.Fatal("torrent attachment not found in the film topic")
	}
	if info.torrentID == "" || info.sizeName == "" {
		t.Errorf("incomplete info: %+v", info)
	}
}

func TestParseTitle(t *testing.T) {
	tests := []struct {
		title    string
		name     string
		original string
		year     int
	}{
		// One slash: Russian / romaji.
		{"[rus] Фермерская жизнь в ином мире / Isekai Nonbiri Nouka [2TV][2023-2026, повседневность]",
			"Фермерская жизнь в ином мире", "Isekai Nonbiri Nouka", 2023},
		{"[mv] Вторая страна / Ni no Kuni [R,S][2019, приключения, фэнтези]",
			"Вторая страна", "Ni no Kuni", 2019},
		// Two slashes: the third part is an English alias, the original stays
		// the first Latin one.
		{"[uni] Вампир не умеет правильно сосать / Chanto Suenai Kyuuketsuki-chan / Li'l Miss Vampire [TV][2024, комедия]",
			"Вампир не умеет правильно сосать", "Chanto Suenai Kyuuketsuki-chan", 2024},
		// Three slashes: a second Russian variant precedes the romaji, so
		// "second part" would be wrong — the first Latin part is right.
		{"[rus] Туалетный мальчик Ханако / Ханако после школы / Jibaku Shounen Hanako-kun / Houkago Shounen Hanako-kun [TV][2020, мистика]",
			"Туалетный мальчик Ханако", "Jibaku Shounen Hanako-kun", 2020},
		// Both parts Latin.
		{"[uni] P-15 / R-15 [TV+OVA][2011, комедия, школа, этти]", "P-15", "R-15", 2011},
		// No metadata brackets at all.
		{"[psp] Хёка / Hyouka", "Хёка", "Hyouka", 0},
	}
	for _, tc := range tests {
		name, original, year := parseTitle(tc.title)
		if name != tc.name || original != tc.original || year != tc.year {
			t.Errorf("parseTitle(%q)\n  got  %q / %q (%d)\n  want %q / %q (%d)",
				tc.title, name, original, year, tc.name, tc.original, tc.year)
		}
	}
}

func TestParseRuDate(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Moscow")
	got := parseRuDate("23 июл 2026, 08:56", loc)
	if got.IsZero() {
		t.Fatal("failed to parse a normal stamp")
	}
	if s := got.UTC().Format("2006-01-02 15:04"); s != "2026-07-23 05:56" {
		t.Errorf("got %s UTC, want 2026-07-23 05:56 (МСК 08:56)", s)
	}
	for _, bad := range []string{"вчера", "", "32 abc 2026"} {
		if !parseRuDate(bad, loc).IsZero() {
			t.Errorf("parseRuDate(%q) should be zero", bad)
		}
	}
	// Every month name the site can emit must resolve.
	for _, mon := range []string{"янв", "фев", "мар", "апр", "май", "июн", "июл", "авг", "сен", "окт", "ноя", "дек"} {
		if parseRuDate("01 "+mon+" 2026, 00:00", loc).IsZero() {
			t.Errorf("month %q not recognised", mon)
		}
	}
}

func TestLastPageFromHTML(t *testing.T) {
	// The captured listing paginates to start=600, i.e. page 40 zero-based.
	if got := lastPageFromHTML(load(t, "forum_f33.html")); got != 40 {
		t.Errorf("lastPageFromHTML = %d, want 40", got)
	}
	if got := lastPageFromHTML("<html>no pagination</html>"); got != 0 {
		t.Errorf("lastPageFromHTML(no links) = %d, want 0", got)
	}
}

func TestBuildTorrent(t *testing.T) {
	it := listingItem{
		topicID: "1849",
		title:   "[rus] Фермерская жизнь в ином мире / Isekai Nonbiri Nouka [2TV][2023-2026, повседневность]",
	}
	info := topicInfo{torrentID: "7316", sizeName: "4.75 ГБ", sid: 5, pir: 0, createTime: time.Now().UTC()}
	magnet := "magnet:?xt=urn:btih:a2e092da06e84fe18b9dc5ca20bf5cc896fceaeb"

	rec := buildTorrent("https://anibelka.com", sections[1], it, info, magnet, "")
	if rec == nil {
		t.Fatal("buildTorrent returned nil")
	}
	if got := asString(rec["url"]); got != "https://anibelka.com/viewtopic.php?t=1849" {
		t.Errorf("url = %q", got)
	}
	if got := asString(rec["name"]); got != "Фермерская жизнь в ином мире" {
		t.Errorf("name = %q", got)
	}
	if got := asString(rec["originalname"]); got != "Isekai Nonbiri Nouka" {
		t.Errorf("originalname = %q", got)
	}
	if got := rec["sid"]; got != 5 {
		t.Errorf("sid = %v, want 5", got)
	}
	types, _ := rec["types"].([]string)
	if len(types) != 1 || types[0] != "anime" {
		t.Errorf("types = %v, want [anime]", rec["types"])
	}
	// A record without a magnet is useless and must not be stored.
	if buildTorrent("https://anibelka.com", sections[1], it, info, "", "") != nil {
		t.Error("record built despite an empty magnet")
	}
}

// The parser must stay anonymous: a logged-in .torrent embeds the account's
// personal passkey in the announce, which would then travel into every magnet
// served by the search API and /sync.
func TestNoLoginIsUsed(t *testing.T) {
	src, err := os.ReadFile("anibelka.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"takeLogin", "ucp.php?mode=login", "Login.U", "Login.P"} {
		if strings.Contains(string(src), forbidden) {
			t.Errorf("parser references %q — it is meant to fetch anonymously", forbidden)
		}
	}
}
