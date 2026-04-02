package selezen

import (
	"strconv"
	"testing"
	"time"

	"jacred/filedb"
)

// ── buildRow — constructs a realistic DLE card HTML chunk ──────────────────

func buildRow(urlv, title, date, sid, pir, size string) string {
	return `<div class="card overflow-hidden">` +
		`<a href="` + urlv + `"><h4 class="card-title">` + title + `</h4></a>` +
		`<span class="bx bx-calendar"></span> ` + date + `</a>` +
		`<i class="bx bx-chevrons-up"></i>` + sid +
		`<i class="bx bx-chevrons-down"></i>` + pir +
		`<span class="bx bx-download"></span>` + size + `</a>` +
		`</div>`
}

// ── parsePageHTML ──────────────────────────────────────────────────────────

func TestParsePageHTML_basic(t *testing.T) {
	row := buildRow(
		"https://selezen.club/relizy-ot-selezen/12345-dune-2021.html",
		"Дюна / Dune (2021) BDRip 1080p",
		"15.03.2024 12:00",
		"120", "8", "14.0 GB",
	)
	items := parsePageHTML(`<html>` + row + `</html>`)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	it := items[0]

	if it["url"] != "https://selezen.club/relizy-ot-selezen/12345-dune-2021.html" {
		t.Errorf("url=%q", it["url"])
	}
	if it["title"] != "Дюна / Dune (2021) BDRip 1080p" {
		t.Errorf("title=%q", it["title"])
	}
	if it["name"] != "Дюна" {
		t.Errorf("name=%q, want Дюна", it["name"])
	}
	if it["originalname"] != "Dune" {
		t.Errorf("originalname=%q, want Dune", it["originalname"])
	}
	if it["relased"] != 2021 {
		t.Errorf("relased=%v, want 2021", it["relased"])
	}
	if it["sid"] != 120 {
		t.Errorf("sid=%v, want 120", it["sid"])
	}
	if it["pir"] != 8 {
		t.Errorf("pir=%v, want 8", it["pir"])
	}
	if it["sizeName"] != "14.0 GB" {
		t.Errorf("sizeName=%q", it["sizeName"])
	}
	if it["trackerName"] != "selezen" {
		t.Errorf("trackerName=%q", it["trackerName"])
	}
}

func TestParsePageHTML_urlWithoutHTML_skipped(t *testing.T) {
	// URL must contain ".html" to be accepted
	row := buildRow(
		"https://selezen.club/relizy-ot-selezen/12345-dune-2021",
		"Дюна / Dune (2021)", "15.03.2024 12:00", "10", "2", "5 GB",
	)
	items := parsePageHTML(`<html>` + row + `</html>`)
	if len(items) != 0 {
		t.Errorf("url without .html should be skipped, got %d items", len(items))
	}
}

func TestParsePageHTML_badDate_skipped(t *testing.T) {
	row := buildRow(
		"https://selezen.club/relizy-ot-selezen/1-test.html",
		"Тест / Test (2020)", "not-a-date", "5", "1", "1 GB",
	)
	items := parsePageHTML(`<html>` + row + `</html>`)
	if len(items) != 0 {
		t.Errorf("bad date should be skipped, got %d items", len(items))
	}
}

func TestParsePageHTML_animeSkipped(t *testing.T) {
	// badAnimeMarkerRe: ">Аниме</a>" in the row body
	row := buildRow(
		"https://selezen.club/relizy-ot-selezen/1-anime.html",
		"Аниме / Anime (2022)", "01.01.2024 10:00", "50", "3", "2 GB",
	) + `<a href="/cat/anime/">Аниме</a>`
	items := parsePageHTML(`<html>` + row + `</html>`)
	if len(items) != 0 {
		t.Errorf("anime row should be skipped, got %d items", len(items))
	}
}

func TestParsePageHTML_multfilm(t *testing.T) {
	row := buildRow(
		"https://selezen.club/relizy-ot-selezen/2-mult.html",
		"Мультфильм / Cartoon (2023)", "05.05.2024 08:00", "30", "2", "3 GB",
	) + `<a href="/cat/mult/">Мульт</a>`
	items := parsePageHTML(`<html>` + row + `</html>`)
	if len(items) != 1 {
		t.Fatalf("multfilm should be kept, got %d items", len(items))
	}
	types, _ := items[0]["types"].([]string)
	if len(types) == 0 || types[0] != "multfilm" {
		t.Errorf("types=%v, want [multfilm]", types)
	}
}

func TestParsePageHTML_serialBySeasonTag(t *testing.T) {
	// [S01] in title → serial
	row := buildRow(
		"https://selezen.club/relizy-ot-selezen/3-serial.html",
		"Сериал [S01] / Serial (2022)", "10.06.2024 14:00", "80", "5", "8 GB",
	)
	items := parsePageHTML(`<html>` + row + `</html>`)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	types, _ := items[0]["types"].([]string)
	if len(types) == 0 || types[0] != "serial" {
		t.Errorf("types=%v, want [serial]", types)
	}
}

func TestParsePageHTML_multipleSidSpaces(t *testing.T) {
	// sid/pir with spaces like "1 234" → should parse as 1234
	row := buildRow(
		"https://selezen.club/relizy-ot-selezen/4-film.html",
		"Фильм / Film (2023)", "20.07.2024 09:00", "1 234", "56", "10 GB",
	)
	items := parsePageHTML(`<html>` + row + `</html>`)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0]["sid"] != 1234 {
		t.Errorf("sid=%v, want 1234", items[0]["sid"])
	}
}

// ── parseNames ─────────────────────────────────────────────────────────────

type namesCase struct {
	title    string
	wantName string
	wantOrig string
	wantYear int
}

func TestParseNames(t *testing.T) {
	cases := []namesCase{
		// movieMain: A / B / C (year)
		{
			"Интерстеллар / Interstellar / Nolan (2014) BDRip",
			"Интерстеллар", "Nolan", 2014,
		},
		// movieShort: A / B (year)
		{
			"Дюна / Dune (2021) BDRip 1080p",
			"Дюна", "Dune", 2021,
		},
		// no match → fallback via fallbackName
		{
			"Просто Название без года",
			"", "", 0,
		},
	}
	for _, tc := range cases {
		name, orig, year := parseNames(tc.title)
		if name != tc.wantName {
			t.Errorf("parseNames(%q): name=%q, want %q", tc.title, name, tc.wantName)
		}
		if orig != tc.wantOrig {
			t.Errorf("parseNames(%q): orig=%q, want %q", tc.title, orig, tc.wantOrig)
		}
		if year != tc.wantYear {
			t.Errorf("parseNames(%q): year=%d, want %d", tc.title, year, tc.wantYear)
		}
	}
}

// ── fallbackName ───────────────────────────────────────────────────────────

func TestFallbackName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Фильм / Film (2021)", "Фильм"},
		{"Фильм (2021) BDRip", "Фильм"},
		{"[Fansub] Аниме 2023", ""},
		{"Просто название", "Просто название"},
	}
	for _, tc := range cases {
		got := fallbackName(tc.in)
		if got != tc.want {
			t.Errorf("fallbackName(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

// ── itemID ─────────────────────────────────────────────────────────────────

func TestItemID(t *testing.T) {
	cases := []struct{ url, want string }{
		{"https://selezen.club/relizy-ot-selezen/12345-dune-2021.html", "12345"},
		{"https://selezen.club/relizy-ot-selezen/9-film.html", "9"},
		{"https://selezen.club/other/path.html", ""},
	}
	for _, tc := range cases {
		got := itemID(tc.url)
		if got != tc.want {
			t.Errorf("itemID(%q)=%q, want %q", tc.url, got, tc.want)
		}
	}
}

// ── typesForRow ────────────────────────────────────────────────────────────

func TestTypesForRow(t *testing.T) {
	cases := []struct {
		row, title, url string
		want            string
	}{
		{`>Мульт<`, "Мультфильм (2020)", "x.html", "multfilm"},
		{"", "Сериал [S02] (2022)", "x.html", "serial"},
		{"", "Сериал [1x01] (2022)", "x.html", "serial"},
		{"", "Фильм / Film (2021)", "x.html", "movie"},
		{"", "Шоу", "tvshows/show.html", "serial"},
	}
	for _, tc := range cases {
		types := typesForRow(tc.row, tc.title, tc.url)
		if len(types) == 0 || types[0] != tc.want {
			t.Errorf("typesForRow(row=%q, title=%q, url=%q)=%v, want [%s]",
				tc.row, tc.title, tc.url, types, tc.want)
		}
	}
}

// ── pageLinksRe (pagination detection) ───────────────────────────────────

func TestPageLinksRe(t *testing.T) {
	html := `<a href="/relizy-ot-selezen/page/2/">2</a>` +
		`<a href="/relizy-ot-selezen/page/5/">5</a>` +
		`<a href="/relizy-ot-selezen/page/10/">10</a>` +
		`<a href="/relizy-ot-selezen/page/3/">3</a>`

	maxPage := 1
	for _, m := range pageLinksRe.FindAllStringSubmatch(html, -1) {
		n, _ := strconv.Atoi(m[1])
		if n > maxPage {
			maxPage = n
		}
	}
	if maxPage != 10 {
		t.Errorf("maxPage=%d, want 10", maxPage)
	}
}

func TestPageLinksRe_noLinks(t *testing.T) {
	// Single page — no pagination links → maxPage stays 1
	html := `<div>page content without pagination</div>`
	maxPage := 1
	for _, m := range pageLinksRe.FindAllStringSubmatch(html, -1) {
		n, _ := strconv.Atoi(m[1])
		if n > maxPage {
			maxPage = n
		}
	}
	if maxPage != 1 {
		t.Errorf("maxPage=%d, want 1 (no links = single page)", maxPage)
	}
}

// ── Task helpers ───────────────────────────────────────────────────────────

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
		t.Error("zero-time task should report UpdatedToday=false")
	}
}

func TestTaskMarkToday(t *testing.T) {
	tk := Task{UpdateTime: "0001-01-01T00:00:00"}
	tk.MarkToday()
	if !tk.UpdatedToday() {
		t.Error("after MarkToday, UpdatedToday should return true")
	}
}

// ── cloneTasks ─────────────────────────────────────────────────────────────

func TestCloneTasks(t *testing.T) {
	original := []Task{
		{Page: 1, UpdateTime: "2024-01-01"},
		{Page: 2, UpdateTime: "0001-01-01T00:00:00"},
	}
	clone := cloneTasks(original)
	clone[0].UpdateTime = "modified"
	if original[0].UpdateTime != "2024-01-01" {
		t.Error("modifying clone should not affect original")
	}
	clone = append(clone, Task{Page: 3})
	if len(original) != 2 {
		t.Error("appending to clone should not affect original length")
	}
}

// ── isDisabled ─────────────────────────────────────────────────────────────

func TestIsDisabled(t *testing.T) {
	list := []string{"Rutor", "selezen", "NNMClub"}
	if !isDisabled(list, "selezen") {
		t.Error("selezen should be disabled")
	}
	if !isDisabled(list, "SELEZEN") {
		t.Error("SELEZEN (case-insensitive) should be disabled")
	}
	if isDisabled(list, "kinozal") {
		t.Error("kinozal should not be disabled")
	}
}

// ── samePrimary ────────────────────────────────────────────────────────────

func TestSamePrimary(t *testing.T) {
	a := filedb.TorrentDetails{
		"title": "Дюна (2021)", "magnet": "magnet:?xt=aaa", "sid": 100, "pir": 5,
	}
	b := filedb.TorrentDetails{
		"title": "Дюна (2021)", "magnet": "magnet:?xt=aaa", "sid": 100, "pir": 5,
	}
	if !samePrimary(a, b) {
		t.Error("identical entries should be samePrimary=true")
	}

	b["sid"] = 101
	if samePrimary(a, b) {
		t.Error("different sid should be samePrimary=false")
	}
}

