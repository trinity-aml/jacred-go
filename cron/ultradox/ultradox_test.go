package ultradox

import (
	"os"
	"strings"
	"testing"
	"time"

	"jacred/core"
)

// The pages in testdata/ were captured from ultradox.onl (which redirects to a
// numbered ultadox.space mirror) after the 2026 domain move. They exist so a
// markup change is caught here rather than as a silent zero-row parse in
// production.

func loadTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	return string(b)
}

func TestParseListingHTML(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Moscow")
	items := parseListingHTML(loadTestdata(t, "listing_serial-hd.html"), loc)

	if len(items) != 18 {
		t.Fatalf("parsed %d rows, want 18", len(items))
	}
	first := items[0]
	if first.detailURL != "/serial-hd/54741-jejforija-3-sezon.html" {
		t.Errorf("detailURL = %q", first.detailURL)
	}
	if first.title != "Эйфория (3 сезон) [+9 серия] [Ultradox]" {
		t.Errorf("title = %q", first.title)
	}
	if first.imdb != "tt8772296" {
		t.Errorf("imdb = %q", first.imdb)
	}
	if first.createTime.IsZero() {
		t.Error("createTime is zero")
	}
	// The closing </a> of a title link is split across lines as "</a\n\t\t>".
	// Every row must still yield a usable title and link.
	for i, it := range items {
		if strings.TrimSpace(it.title) == "" {
			t.Errorf("row %d: empty title", i)
		}
		if !strings.HasPrefix(it.detailURL, "/") {
			t.Errorf("row %d: detailURL = %q, want site-relative", i, it.detailURL)
		}
		if strings.Contains(it.title, "magnet:") {
			t.Errorf("row %d: title leaked the magnet cell: %q", i, it.title)
		}
	}
}

func TestDetailMagnetExtraction(t *testing.T) {
	body := loadTestdata(t, "detail_serial.html")
	ms := detailMagnetRe.FindAllStringSubmatch(body, -1)
	if len(ms) != 3 {
		t.Fatalf("found %d magnets, want 3 quality variants", len(ms))
	}
	wantQuality := map[string]bool{"1080p": true, "720p": true, "400p": true}
	for _, m := range ms {
		hash, dn := m[1], m[3]
		if len(hash) != 40 {
			t.Errorf("hash %q is not a 40-char btih", hash)
		}
		if strings.Trim(hash, "0") == "" {
			t.Errorf("hash %q is the empty placeholder the listing carries", hash)
		}
		q := extractQuality(dn)
		if !wantQuality[q] {
			t.Errorf("unexpected quality %q from dn %q", q, dn)
		}
		delete(wantQuality, q)
	}
	if len(wantQuality) != 0 {
		t.Errorf("missing quality variants: %v", wantQuality)
	}
}

// The listing's own magnets have an empty btih, which is exactly why the
// parser follows detail links. If this ever stops being true the detail fetch
// could be skipped.
func TestListingMagnetsAreStillPlaceholders(t *testing.T) {
	body := loadTestdata(t, "listing_serial-hd.html")
	if strings.Contains(body, "magnet:?xt=urn:btih:&") == false {
		t.Skip("listing magnets are no longer empty — detail fetch may be avoidable")
	}
}

func TestParseRowDate(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Moscow")
	if got := parseRowDate("02-04-2025, 14:32", loc); got.IsZero() {
		t.Error("absolute date failed to parse")
	}
	if got := parseRowDate("Сегодня, 10:06", loc); got.IsZero() {
		t.Error("relative today failed to parse")
	}
	if got := parseRowDate("Вчера, 22:05", loc); got.IsZero() {
		t.Error("relative yesterday failed to parse")
	}
	if got := parseRowDate("позавчера", loc); !got.IsZero() {
		t.Errorf("unknown shape returned %v, want zero", got)
	}
}

// Requests without a search-engine Referer get a 503 from the site's nginx.
func TestBrowserHeadersCarryReferer(t *testing.T) {
	ref := browserHeaders["Referer"]
	if !strings.Contains(ref, "google.") && !strings.Contains(ref, "yandex.") {
		t.Errorf("Referer = %q, want a search engine — the site 503s otherwise", ref)
	}
	if _, ok := browserHeaders["Accept-Encoding"]; ok {
		t.Error("Accept-Encoding must stay unset so net/http can decompress the body itself")
	}
}

func TestParseTitle(t *testing.T) {
	tests := []struct {
		title    string
		name     string
		original string
		year     int
	}{
		// Movie sections: the "(YYYY)" block marks where the title ends.
		{"Ип Ман: Битва кланов (2026) (ПМ) [BDRip]", "Ип Ман: Битва кланов", "", 2026},
		{"30 ночей с бывшим (2025) (Дубляж [Чистый звук]) [BDRip]", "30 ночей с бывшим", "", 2025},
		{"Мотор Сити (Автомобильный город) (2025) (Дубляж) [Telecine]", "Мотор Сити (Автомобильный город)", "", 2025},
		{"Трасса «Море - море» (2026) (Оригинал) [Telecine]", "Трасса «Море - море»", "", 2026},
		// Serial and anime sections carry no year — the cut has to happen on
		// the season block, and the episode counter must not survive.
		{"Эйфория (3 сезон) [+9 серия] [Ultradox]", "Эйфория", "", 0},
		{"Боевой петух (1 сезон) [+12 серия] (ПМ) [WEB-DL]", "Боевой петух", "", 0},
		{"Триган: Наблюдая за звёздами (2 сезон) [+12 серия] [Ultradox]", "Триган: Наблюдая за звёздами", "", 0},
		{"Проект Пуля/Пуля (1 сезон) [+12 серия] [Ultradox]", "Проект Пуля/Пуля", "", 0},
		{"Губка Боб квадратные штаны (17 сезон) [+6 серия] [Ultradox]", "Губка Боб квадратные штаны", "", 0},
		// No season block, only bracketed markers.
		{"Некий сериал [+5 серия] [Ultradox]", "Некий сериал", "", 0},
		// Slashed original name survives the cut.
		{"Астрид и Рафаэлла / Astrid et Raphaëlle (2025) [WEB-DL]", "Астрид и Рафаэлла", "Astrid et Raphaëlle", 2025},
	}
	for _, tc := range tests {
		name, original, year := parseTitle(tc.title)
		if name != tc.name || original != tc.original || year != tc.year {
			t.Errorf("parseTitle(%q)\n  got  name=%q original=%q year=%d\n  want name=%q original=%q year=%d",
				tc.title, name, original, year, tc.name, tc.original, tc.year)
		}
	}
}

// The bucket key is core.NameToHash(name, originalname). Every quality variant
// of one release must land on the same key, otherwise a single show is spread
// over three buckets and never merges with the other trackers' copy.
func TestQualityVariantsShareBucketKey(t *testing.T) {
	item := listingItem{
		title:     "Эйфория (3 сезон) [+9 серия] [Ultradox]",
		detailURL: "/serial-hd/54741-jejforija-3-sezon.html",
	}
	sec := section{path: "serial-hd", types: []string{"serial"}}
	variants := []magnetVariant{
		{hash: "0474f44b58fbec31ec145d610a74488a8231f214", magnet: "magnet:?a", bytes: 21648023723, dn: "x.1080p.torrent", quality: "1080p"},
		{hash: "e89466561abc2312894f75d39e6783d4712fa0e4", magnet: "magnet:?b", bytes: 13940626498, dn: "x.720p.torrent", quality: "720p"},
		{hash: "19ca78e954b198c8c86d5b7f76cf1aa625514e3e", magnet: "magnet:?c", bytes: 8619991040, dn: "x.400p.torrent", quality: "400p"},
	}

	keys := map[string]bool{}
	urls := map[string]bool{}
	for _, v := range variants {
		rec := buildTorrent("https://ultradox.onl", sec, item, v, detailInfo{year: 2026, original: "Euphoria"}, "")
		if rec == nil {
			t.Fatalf("buildTorrent returned nil for %s", v.quality)
		}
		if got := asString(rec["name"]); got != "Эйфория" {
			t.Errorf("%s: name = %q, want %q", v.quality, got, "Эйфория")
		}
		if got := asString(rec["originalname"]); got != "Euphoria" {
			t.Errorf("%s: originalname = %q, want %q", v.quality, got, "Euphoria")
		}
		if got := rec["relased"]; got != 2026 {
			t.Errorf("%s: relased = %v, want 2026 from the detail page", v.quality, got)
		}
		// The display title still has to carry the quality for UpdateFullDetails.
		if !strings.Contains(asString(rec["title"]), v.quality) {
			t.Errorf("%s: title %q lost the quality marker", v.quality, rec["title"])
		}
		keys[core.NameToHash(asString(rec["name"]), asString(rec["originalname"]))] = true
		urls[asString(rec["url"])] = true
	}
	if len(keys) != 1 {
		t.Errorf("quality variants produced %d bucket keys, want 1: %v", len(keys), keys)
	}
	// They still need distinct URLs so all three survive inside that bucket.
	if len(urls) != 3 {
		t.Errorf("quality variants produced %d urls, want 3 distinct", len(urls))
	}
}

// A new episode changes "[+9 серия]" to "[+10 серия]". The name — and so the
// bucket — must not move.
func TestEpisodeCounterDoesNotChangeIdentity(t *testing.T) {
	before, _, _ := parseTitle("Эйфория (3 сезон) [+9 серия] [Ultradox]")
	after, _, _ := parseTitle("Эйфория (3 сезон) [+10 серия] [Ultradox]")
	if before != after {
		t.Errorf("episode bump changed the name: %q -> %q", before, after)
	}
	if core.NameToHash(before, "") != core.NameToHash(after, "") {
		t.Error("episode bump changed the bucket key")
	}
}

func TestDetailYear(t *testing.T) {
	body := loadTestdata(t, "detail_serial.html")
	m := detailYearRe.FindStringSubmatch(body)
	if len(m) < 2 {
		t.Fatal("year not found on detail page")
	}
	if m[1] != "2026" {
		t.Errorf("year = %q, want 2026", m[1])
	}
}

func TestOriginalFromFilename(t *testing.T) {
	tests := []struct{ dn, want string }{
		// Serials and anime: the title ends at the season token.
		{"Euphoria.US.S03.1080p.Ru.Ultradox.torrent", "Euphoria"},
		{"Taakstraf.S01.1080p.Ru.Ultradox.torrent", "Taakstraf"},
		{"SpongeBob.SquarePants.S17.720p.Ru.Ultradox.torrent", "SpongeBob SquarePants"},
		{"Trigun.Stargaze.S02.720p.Ru.Ultradox.torrent", "Trigun Stargaze"},
		{"Shumatsu.no.Valkyrie.S03.1080p.Ultradox.torrent", "Shumatsu no Valkyrie"},
		{"Life.Larry.and.the.Pursuit.of.Unhappiness.An.Almost.History.of.America.S01.1080p.Ru.Ultradox.torrent",
			"Life Larry and the Pursuit of Unhappiness An Almost History of America"},
		// Movies: the title ends at the year.
		{"The.Death.Of.Robin.Hood.2026.D.BDRip.avi.torrent", "The Death Of Robin Hood"},
		{"Yellow.Letters.2026.Pm.BDRip.1O8Op.mkv.torrent", "Yellow Letters"},
		{"30.Notti.con.il.mio.ex.2025.D.BDRip.avi.torrent", "30 Notti con il mio ex"},
		{"Game.of.Shark.2024.Pk.WEB-DL.1O8Op.mkv.torrent", "Game of Shark"},
		{"State.of.Ramadhani.Dharyu.Dhani.Nu.Thay.2026.Pk.TELECINE.avi.torrent",
			"State of Ramadhani Dharyu Dhani Nu Thay"},
		// The site sometimes drops the separator before the year and writes
		// O for zero, so the year has to be matched as a token prefix.
		{"Trassa.more.more.2026O.TELECINE.1O8Op.mkv.torrent", "Trassa more more"},
		// Nothing usable in front of the stop token.
		{"S01.1080p.Ru.Ultradox.torrent", ""},
		{"2026.D.BDRip.torrent", ""},
		{"", ""},
	}
	for _, tc := range tests {
		if got := originalFromFilename(tc.dn); got != tc.want {
			t.Errorf("originalFromFilename(%q) = %q, want %q", tc.dn, got, tc.want)
		}
	}
}

// All quality variants of one release must yield the same original, otherwise
// the bucket key splits again — which is the whole reason the value is
// resolved once per item in fetchDetail rather than per variant.
func TestOriginalIsStableAcrossVariants(t *testing.T) {
	groups := [][]string{
		{"30.Notti.con.il.mio.ex.2025.D.BDRip.avi.torrent", "30.Notti.con.il.mio.ex.2025.D.BDRip.1O8Op.mkv.torrent"},
		{"Svoya.v.dosku.2026.O.WEB-DLRip.avi.torrent", "Svoya.v.dosku.2026.O.WEB-DL.1O8Op.mkv.torrent"},
		{"Euphoria.US.S03.1080p.Ru.Ultradox.torrent", "Euphoria.US.S03.720p.Ru.Ultradox.torrent", "Euphoria.US.S03.400p.Ru.Ultradox.torrent"},
	}
	for _, g := range groups {
		first := originalFromFilename(g[0])
		for _, dn := range g[1:] {
			if got := originalFromFilename(dn); got != first {
				t.Errorf("variants disagree: %q -> %q, %q -> %q", g[0], first, dn, got)
			}
		}
	}
}

// rufilm is domestic cinema: its filenames hold a transliteration of the
// Russian title, not a foreign original, so it must not become originalname.
func TestRufilmKeepsOriginalEmpty(t *testing.T) {
	item := listingItem{title: "Своя в доску (2026) (Оригинал) [WEB-DL]", detailURL: "/rufilm/1-x.html"}
	v := magnetVariant{hash: "abc123", magnet: "magnet:?x", dn: "Svoya.v.dosku.2026.O.WEB-DL.1O8Op.mkv.torrent", quality: "1080p"}
	info := detailInfo{year: 2026, original: "Svoya v dosku"}

	ru := buildTorrent("https://ultradox.onl", section{path: "rufilm", types: []string{"movie"}}, item, v, info, "")
	if got := asString(ru["originalname"]); got != "" {
		t.Errorf("rufilm originalname = %q, want empty", got)
	}
	// The same data in a foreign section does take the original.
	hd := buildTorrent("https://ultradox.onl", section{path: "hd", types: []string{"movie"}}, item, v, info, "")
	if got := asString(hd["originalname"]); got != "Svoya v dosku" {
		t.Errorf("hd originalname = %q, want %q", got, "Svoya v dosku")
	}
}
