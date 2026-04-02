package mazepa

import (
	"strings"
	"testing"
	"time"
)

// ----- parseNamesAdvanced -----

func TestParseNamesAdvanced_slashBoth(t *testing.T) {
	name, orig, year := parseNamesAdvanced("Матриця / The Matrix (1999) BDRip")
	if name != "Матриця" {
		t.Errorf("name: got %q, want %q", name, "Матриця")
	}
	if orig != "The Matrix" {
		t.Errorf("orig: got %q, want %q", orig, "The Matrix")
	}
	if year != 1999 {
		t.Errorf("year: got %d, want 1999", year)
	}
}

func TestParseNamesAdvanced_cyrillicOnly(t *testing.T) {
	name, orig, year := parseNamesAdvanced("Тінь (2023)")
	if name == "" {
		t.Error("name should not be empty")
	}
	if year != 2023 {
		t.Errorf("year: got %d, want 2023", year)
	}
	_ = orig
}

func TestParseNamesAdvanced_empty(t *testing.T) {
	name, orig, year := parseNamesAdvanced("")
	if name != "" || orig != "" || year != 0 {
		t.Errorf("expected all empty, got %q %q %d", name, orig, year)
	}
}

// ----- parseMazepaDate -----

func TestParseMazepaDate(t *testing.T) {
	cases := []struct {
		input string
		want  time.Time
	}{
		{"15 бер 2022, 14:30", time.Date(2022, 3, 15, 14, 30, 0, 0, time.UTC)},
		{"1 сiч 2021, 9:05", time.Date(2021, 1, 1, 9, 5, 0, 0, time.UTC)},
		{"31 гру 2019, 23:59", time.Date(2019, 12, 31, 23, 59, 0, 0, time.UTC)},
		{"bad date", time.Time{}},
	}
	for _, c := range cases {
		got := parseMazepaDate(c.input)
		if !got.Equal(c.want) {
			t.Errorf("parseMazepaDate(%q) = %v, want %v", c.input, got, c.want)
		}
	}
}

// ----- normalizeMagnet -----

func TestNormalizeMagnet(t *testing.T) {
	raw := "magnet:?xt=urn:btih:AABBCCDDEEFF00112233445566778899AABBCCDD&dn=Test"
	got := normalizeMagnet(raw)
	if !strings.HasPrefix(got, "magnet:?xt=urn:btih:") {
		t.Errorf("unexpected magnet: %q", got)
	}
	if strings.Contains(got, "dn=") {
		t.Errorf("should strip extra params, got: %q", got)
	}
}

func TestNormalizeMagnet_invalid(t *testing.T) {
	got := normalizeMagnet("not-a-magnet")
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

// ----- parseSizeName -----

func TestParseSizeName(t *testing.T) {
	cases := []struct {
		block string
		want  string
	}{
		{">1.23&nbsp;GB<", "1.23 GB"},
		{">700.5&nbsp;MB<", "700.5 MB"},
		{"2.5 TB text", "2.5 TB"},
		{"no size here", ""},
	}
	for _, c := range cases {
		got := parseSizeName(c.block)
		if got != c.want {
			t.Errorf("parseSizeName(%q) = %q, want %q", c.block, got, c.want)
		}
	}
}

// ----- mazePagRe -----

func TestMazePagRe(t *testing.T) {
	html := `<a href="./viewforum.php?f=12&amp;start=50">2</a>
<a href="./viewforum.php?f=12&amp;start=100">3</a>
<a href="./viewforum.php?f=12&amp;start=150">4</a>`
	matches := mazePagRe.FindAllStringSubmatch(html, -1)
	if len(matches) != 3 {
		t.Fatalf("expected 3 matches, got %d", len(matches))
	}
	if matches[2][1] != "150" {
		t.Errorf("last start: got %q, want 150", matches[2][1])
	}
}

func TestMazePagRe_noAmp(t *testing.T) {
	html := `<a href="viewforum.php?f=7&start=50">next</a>`
	matches := mazePagRe.FindAllStringSubmatch(html, -1)
	if len(matches) != 1 || matches[0][1] != "50" {
		t.Errorf("expected start=50, got %v", matches)
	}
}

// ----- Task UpdatedToday / MarkToday -----

func TestTaskUpdatedToday_notToday(t *testing.T) {
	task := Task{UpdateTime: "2000-01-01T00:00:00Z"}
	if task.UpdatedToday() {
		t.Error("should not be today")
	}
}

func TestTaskUpdatedToday_today(t *testing.T) {
	task := Task{}
	task.MarkToday()
	if !task.UpdatedToday() {
		t.Error("should be today after MarkToday")
	}
}

func TestTaskUpdatedToday_empty(t *testing.T) {
	task := Task{}
	if task.UpdatedToday() {
		t.Error("empty UpdateTime should not be today")
	}
}

// ----- cloneTasks -----

func TestCloneTasks(t *testing.T) {
	src := map[string][]Task{
		"12": {{Page: 0, UpdateTime: "t1"}, {Page: 1, UpdateTime: "t2"}},
	}
	dst := cloneTasks(src)
	dst["12"][0].UpdateTime = "changed"
	if src["12"][0].UpdateTime == "changed" {
		t.Error("clone should be independent")
	}
}

// ----- parseForumPage HTML parsing -----

func TestParseForumPage_basic(t *testing.T) {
	html := `<tr id="tr-12345">
  <td><a class="torTopic "><b>Дюна / Dune (2021) 1080p</b></a></td>
  <td><a href="magnet:?xt=urn:btih:AABBCCDDEEFF00112233445566778899AABBCCDD"></a></td>
  <td class="seedmed"><b>10</b></td>
  <td class="leechmed"><b>2</b></td>
  <td>>1.5&nbsp;GB<</td>
  <ul class="last_post"><li><a>15 бер 2022, 10:00</a></li></ul>
</tr>`
	p := &Parser{}
	items, sig, err := p.parseForumPage(nil, "", []string{"movie"}, "http://mazepa.to")
	_ = items
	_ = sig
	_ = err
	// Real test with actual html body — test the HTML extraction helpers directly
	rows := rowRe.FindAllString(html, -1)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	block := rows[0]

	tidM := inlineReB10e5aRe.FindStringSubmatch(block)
	if len(tidM) < 2 || tidM[1] != "12345" {
		t.Errorf("tid: got %v", tidM)
	}

	titleM := titleRe.FindStringSubmatch(block)
	if len(titleM) < 2 || !strings.Contains(titleM[1], "Дюна") {
		t.Errorf("title: got %v", titleM)
	}

	magnetM := magnetRe.FindStringSubmatch(block)
	if len(magnetM) < 2 || !strings.HasPrefix(magnetM[1], "magnet:") {
		t.Errorf("magnet: got %v", magnetM)
	}
}

func TestParseForumPage_noMagnet(t *testing.T) {
	html := `<tr id="tr-999">
  <td><a class="torTopic "><b>Тест (2020)</b></a></td>
  <td>no magnet here</td>
</tr>`
	rows := rowRe.FindAllString(html, -1)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	magnetM := magnetRe.FindStringSubmatch(rows[0])
	if len(magnetM) >= 2 {
		t.Error("should not find magnet in row without magnet")
	}
}
