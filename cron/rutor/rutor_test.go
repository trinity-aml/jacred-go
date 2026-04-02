package rutor

import (
	"strings"
	"testing"
	"time"
)

// ----- parseTitle -----

type titleCase struct {
	cat      string
	title    string
	wantName string
	wantOrig string
	wantYear int
}

func TestParseTitle(t *testing.T) {
	cases := []titleCase{
		// cat 1 — movies
		{
			cat:      "1",
			title:    "Интерстеллар / Interstellar / Christopher Nolan (2014) BDRip",
			wantName: "Интерстеллар", wantOrig: "Christopher Nolan", wantYear: 2014,
		},
		{
			cat:      "1",
			title:    "Дюна / Dune (2021) BDRip 1080p",
			wantName: "Дюна", wantOrig: "Dune", wantYear: 2021,
		},
		// cat 5 — music
		{
			cat:      "5",
			title:    "Metallica - Black Album (1991) MP3",
			wantName: "Metallica - Black Album", wantOrig: "", wantYear: 1991,
		},
		// cat 4 — serials
		{
			cat:      "4",
			title:    "Во все тяжкие / Breaking Bad / Сезон 5 [16 серий] (2012) BDRip",
			wantName: "Во все тяжкие", wantOrig: "Сезон 5", wantYear: 2012,
		},
		// cat 16 — multi-serials
		{
			cat:      "16",
			title:    "Клан Сопрано [86 серий] (1999-2007) BDRip",
			wantName: "Клан Сопрано", wantOrig: "", wantYear: 1999,
		},
		// cat 12 — docu
		{
			cat:      "12",
			title:    "Земля / Earth / BBC (2007) BDRip",
			wantName: "Земля", wantOrig: "BBC", wantYear: 2007,
		},
		// cat 10 — anime
		{
			cat:      "10",
			title:    "Наруто / Naruto / Pierrot (2002) DVDRip",
			wantName: "Наруто", wantOrig: "Pierrot", wantYear: 2002,
		},
	}

	for _, tc := range cases {
		name, orig, year := parseTitle(tc.cat, tc.title)
		if name != tc.wantName {
			t.Errorf("parseTitle(cat=%s, %q): name=%q, want %q", tc.cat, tc.title, name, tc.wantName)
		}
		if orig != tc.wantOrig {
			t.Errorf("parseTitle(cat=%s, %q): orig=%q, want %q", tc.cat, tc.title, orig, tc.wantOrig)
		}
		if year != tc.wantYear {
			t.Errorf("parseTitle(cat=%s, %q): year=%d, want %d", tc.cat, tc.title, year, tc.wantYear)
		}
	}
}

// ----- parsePageHTML -----

func buildRow(urlPath, title, sid, pir, size, magnet, date string) string {
	return `<tr class="gai">` +
		`<td>` + date + `</td>` +
		`<td><a class="downgif" href="#">dl</a></td>` +
		`<td><a href="/` + urlPath + `">` + title + `</a></td>` +
		`<td><span class="green"><img src="s.gif">&nbsp;` + sid + `</span></td>` +
		`<td><span class="red">&nbsp;` + pir + `</span></td>` +
		`<td align="right">` + size + `</td>` +
		`<td><a href="` + magnet + `">magnet</a></td>` +
		`</tr>`
}

func TestParsePageHTML_basic(t *testing.T) {
	host := "https://rutor.is"
	cat := "1"
	magnet := "magnet:?xt=urn:btih:AABBCCDDEEFF00112233445566778899AABBCCDD&dn=test"
	row := buildRow(
		"torrent/12345/dune-2021",
		"Дюна / Dune (2021) BDRip 1080p",
		"150", "5", "14.0 GB",
		magnet,
		"01 янв 21",
	)
	html := `<html><body>` + row + `</body></html>`

	items := parsePageHTML(host, cat, html)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item := items[0]

	wantURL := host + "/torrent/12345/dune-2021"
	if item["url"] != wantURL {
		t.Errorf("url=%q, want %q", item["url"], wantURL)
	}
	if item["title"] != "Дюна / Dune (2021) BDRip 1080p" {
		t.Errorf("title=%q", item["title"])
	}
	if item["sid"] != 150 {
		t.Errorf("sid=%v, want 150", item["sid"])
	}
	if item["pir"] != 5 {
		t.Errorf("pir=%v, want 5", item["pir"])
	}
	if item["sizeName"] != "14.0 GB" {
		t.Errorf("sizeName=%q", item["sizeName"])
	}
	if item["magnet"] != magnet {
		t.Errorf("magnet mismatch")
	}
	if item["name"] != "Дюна" {
		t.Errorf("name=%q, want Дюна", item["name"])
	}
	if item["originalname"] != "Dune" {
		t.Errorf("originalname=%q, want Dune", item["originalname"])
	}
	if item["relased"] != 2021 {
		t.Errorf("relased=%v, want 2021", item["relased"])
	}
	if item["trackerName"] != "rutor" {
		t.Errorf("trackerName=%q", item["trackerName"])
	}
}

func TestParsePageHTML_trailerSkipped(t *testing.T) {
	host := "https://rutor.is"
	magnet := "magnet:?xt=urn:btih:AABBCCDDEEFF00112233445566778899AABBCCDD&dn=test"
	row := buildRow("torrent/99/trailer", "Трейлер фильма (2021)", "10", "2", "100 MB", magnet, "01 янв 21")
	items := parsePageHTML(host, "1", `<html>`+row+`</html>`)
	if len(items) != 0 {
		t.Errorf("trailer should be skipped, got %d items", len(items))
	}
}

func TestParsePageHTML_cat17_nonUkrSkipped(t *testing.T) {
	host := "https://rutor.is"
	magnet := "magnet:?xt=urn:btih:AABBCCDDEEFF00112233445566778899AABBCCDD&dn=test"
	// cat 17 requires " UKR" in title
	row := buildRow("torrent/88/film-2022", "Фильм / Film (2022) BDRip", "20", "3", "5 GB", magnet, "01 янв 22")
	items := parsePageHTML(host, "17", `<html>`+row+`</html>`)
	if len(items) != 0 {
		t.Errorf("non-UKR in cat 17 should be skipped, got %d items", len(items))
	}
}

func TestParsePageHTML_cat17_ukrKept(t *testing.T) {
	host := "https://rutor.is"
	magnet := "magnet:?xt=urn:btih:AABBCCDDEEFF00112233445566778899AABBCCDD&dn=test"
	row := buildRow("torrent/88/film-2022", "Фільм UKR / Film (2022) BDRip", "20", "3", "5 GB", magnet, "01 янв 22")
	items := parsePageHTML(host, "17", `<html>`+row+`</html>`)
	if len(items) != 1 {
		t.Errorf("UKR in cat 17 should be kept, got %d items", len(items))
	}
}

func TestParsePageHTML_КПКSkipped(t *testing.T) {
	host := "https://rutor.is"
	magnet := "magnet:?xt=urn:btih:AABBCCDDEEFF00112233445566778899AABBCCDD&dn=test"
	row := buildRow("torrent/77/pda", "Фильм КПК (2020) DVDRip", "5", "1", "200 MB", magnet, "01 янв 20")
	items := parsePageHTML(host, "1", `<html>`+row+`</html>`)
	if len(items) != 0 {
		t.Errorf("КПК title should be skipped, got %d items", len(items))
	}
}

// ----- browsePagesRe (pagination) -----

func TestBrowsePagesRe(t *testing.T) {
	// Simulate rutor page with pagination links
	html := `<a href="/browse/0/1/0/0">1</a>` +
		`<a href="/browse/1/1/0/0">2</a>` +
		`<a href="/browse/2/1/0/0">3</a>` +
		`<a href="/browse/15/1/0/0">16</a>`

	maxPage := 0
	for _, m := range browsePagesRe.FindAllStringSubmatch(html, -1) {
		if n, _ := parseInt(m[1]); n > maxPage {
			maxPage = n
		}
	}
	if maxPage != 15 {
		t.Errorf("maxPage=%d, want 15", maxPage)
	}
}

// ----- Task helpers -----

func TestTaskUpdatedToday(t *testing.T) {
	now := time.Now()
	todayStr := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).Format(time.RFC3339)

	tk := Task{UpdateTime: todayStr}
	if !tk.UpdatedToday() {
		t.Error("task marked today should report UpdatedToday=true")
	}

	yesterday := now.AddDate(0, 0, -1)
	yStr := time.Date(yesterday.Year(), yesterday.Month(), yesterday.Day(), 0, 0, 0, 0, time.Local).Format(time.RFC3339)
	tk2 := Task{UpdateTime: yStr}
	if tk2.UpdatedToday() {
		t.Error("task marked yesterday should report UpdatedToday=false")
	}

	tk3 := Task{UpdateTime: "0001-01-01T00:00:00"}
	if tk3.UpdatedToday() {
		t.Error("zero time task should report UpdatedToday=false")
	}
}

func TestTaskMarkToday(t *testing.T) {
	tk := Task{UpdateTime: "0001-01-01T00:00:00"}
	tk.MarkToday()
	if !tk.UpdatedToday() {
		t.Error("after MarkToday, UpdatedToday should be true")
	}
}

// ----- cloneTasks -----

func TestCloneTasks(t *testing.T) {
	original := map[string][]Task{
		"1": {{Page: 0, UpdateTime: "2024-01-01"}, {Page: 1, UpdateTime: "0001-01-01T00:00:00"}},
	}
	clone := cloneTasks(original)
	// Modifying clone should not affect original
	clone["1"][0].UpdateTime = "modified"
	if original["1"][0].UpdateTime != "2024-01-01" {
		t.Error("cloneTasks: modifying clone affected original")
	}
	clone["2"] = []Task{{Page: 0}}
	if _, ok := original["2"]; ok {
		t.Error("cloneTasks: adding to clone affected original")
	}
}

// ----- parseCreateTime -----

func TestParseCreateTime(t *testing.T) {
	cases := []struct {
		input string
		wantY int
		wantM time.Month
	}{
		{"14 Мар 26", 2026, time.March},
		{"01 янв 21", 2021, time.January},
		{"15 дек 23", 2023, time.December},
	}
	for _, tc := range cases {
		tm := parseCreateTime(tc.input, "02.01.06")
		if tm.IsZero() {
			t.Errorf("parseCreateTime(%q): got zero time", tc.input)
			continue
		}
		if tm.Year() != tc.wantY {
			t.Errorf("parseCreateTime(%q): year=%d, want %d", tc.input, tm.Year(), tc.wantY)
		}
		if tm.Month() != tc.wantM {
			t.Errorf("parseCreateTime(%q): month=%v, want %v", tc.input, tm.Month(), tc.wantM)
		}
	}
}

// ----- fallbackName -----

func TestFallbackName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Фильм / Film (2021) BDRip", "Фильм "},
		{"Фильм (2021) BDRip", "Фильм "},
		{"[Fansub] Аниме 2023", ""},
		{"Просто название", "Просто название"},
	}
	for _, tc := range cases {
		got := fallbackName(tc.in)
		got = strings.TrimSpace(got)
		want := strings.TrimSpace(tc.want)
		if got != want {
			t.Errorf("fallbackName(%q)=%q, want %q", tc.in, got, want)
		}
	}
}

// helper for test (avoids importing strconv in test)
func parseInt(s string) (int, bool) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}
