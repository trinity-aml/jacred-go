package kinozal

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Titles below were taken from the live kinozal.guru RSS feed after the move
// from kinozal.tv, and confirm the listing title format survived the domain
// change. browse.php itself is behind the login wall, so its table markup is
// not covered here — only the title grammar, which is where the name,
// original name and year come from.
func TestParseTitleLiveShapes(t *testing.T) {
	tests := []struct {
		cat      string
		title    string
		name     string
		original string
		year     int
	}{
		// Movie categories: "Рус / Original / YYYY / переводы / качество".
		{"8", "Профессионалы / The Professionals / 1960 / ЛО  СТ / BDRip (AVC)",
			"Профессионалы", "The Professionals", 1960},
		{"8", "Братья / Brothers / 2009 / ДБ  ПМ  АП (Немахов)  СТ / BDRip (AVC)",
			"Братья", "Brothers", 2009},
		// Foreign serial with a season block and an original name.
		{"46", "Укрытие (Бункер) (3 сезон: 1-4 серия из 10) / Silo / 2026 / 5 x ПМ / WEB-DL (1080p)",
			"Укрытие", "Silo", 2026},
		// Russian serial: no original name, and the year may be a range —
		// only the first year is taken.
		{"45", "Бим (Пёс в законе) (3 сезон: 1-25 серии из 30) / 2023-2024 / РУ  СТ / WEB-DL (1080p)",
			"Бим", "", 2023},
		{"45", "Тайга (1 сезон: 1-8 серии из 8) / 2026 / РУ / WEB-DL (1080p)",
			"Тайга", "", 2026},
		// TV show category.
		{"49", "NG. Самая большая акула-мако / Worlds Biggest Mako / 2026 / ПД / WEB-DL (1080p)",
			"NG. Самая большая акула-мако", "Worlds Biggest Mako", 2026},
	}
	for _, tc := range tests {
		name, original, year := parseTitle(tc.cat, tc.title, tc.title)
		if name != tc.name || original != tc.original || year != tc.year {
			t.Errorf("parseTitle(cat=%s, %q)\n  got  %q / %q (%d)\n  want %q / %q (%d)",
				tc.cat, tc.title, name, original, year, tc.name, tc.original, tc.year)
		}
	}
}

// The login form on kinozal.guru still posts username/password to
// /takelogin.php, which is what takeLogin builds.
func TestLoginFieldNames(t *testing.T) {
	for _, field := range []string{"username", "password"} {
		if field == "" {
			t.Fatal("empty field name")
		}
	}
	if inlineReC4d16cRe.FindStringSubmatch("uid=123456; path=/") == nil {
		t.Error("uid cookie regex no longer matches a Set-Cookie value")
	}
	if m := inlineReF31405Re.FindStringSubmatch("pass=abcdef0123; path=/"); m == nil || m[1] != "abcdef0123" {
		t.Errorf("pass cookie regex failed: %v", m)
	}
}

// browse_cat46.html is a trimmed capture from kinozal.guru: the <title>, the
// logout marker, eight listing rows (four with relative "сегодня/вчера" dates,
// four absolute) and the pagination block. The uploader column and every
// account identifier were stripped before committing.
func loadBrowse(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/browse_cat46.html")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	return string(b)
}

// The guard in parsePage must accept the current brand. Pinning the literal
// "Кинозал.ТВ</title>" made every page fail it after the rebrand, and because
// parsePage returns (nil, nil) on that path the parser reported zero torrents
// instead of an error.
func TestTitleGuardAcceptsRebrand(t *testing.T) {
	for _, ok := range []string{
		"<title>Раздачи :: Кинозал.GURU</title>",
		"<title>Раздачи :: Кинозал.ТВ</title>",
		"<title>Кинозал.АНY</title>",
	} {
		if !kinozalTitleRe.MatchString(ok) {
			t.Errorf("title guard rejected %q", ok)
		}
	}
	for _, bad := range []string{
		"<title>502 Bad Gateway</title>",
		"<title>Вход :: Другой сайт</title>",
		"",
	} {
		if kinozalTitleRe.MatchString(bad) {
			t.Errorf("title guard accepted %q", bad)
		}
	}
	if !kinozalTitleRe.MatchString(loadBrowse(t)) {
		t.Error("title guard rejected the captured live page")
	}
}

// Every listing row must yield all five fields; a miss on any one of them
// makes parsePage drop the row silently.
func TestListingRowFields(t *testing.T) {
	body := loadBrowse(t)
	rows := rowSplitRe.Split(replaceBadNames(body), -1)
	if len(rows)-1 != 8 {
		t.Fatalf("split produced %d rows, want 8", len(rows)-1)
	}
	for i, row := range rows[1:] {
		for _, f := range []struct {
			name string
			re   *regexp.Regexp
		}{
			{"detail url", mp1Re}, {"title", mp2Re},
			{"seeders", mp3Re}, {"leechers", mp4Re}, {"size", mp5Re},
		} {
			if m := f.re.FindStringSubmatch(row); len(m) < 2 {
				t.Errorf("row %d: %s regex did not match", i, f.name)
			}
		}
		// Date: either a relative marker or the absolute form.
		relative := strings.Contains(row, "<td class='s'>сегодня") || strings.Contains(row, "<td class='s'>вчера")
		if !relative && dateOnlyRe.FindStringSubmatch(row) == nil {
			t.Errorf("row %d: no usable date", i)
		}
	}
}

// The pagination regex feeds UpdateTasksParse; without it every category
// collapses to a single page.
func TestBrowsePagination(t *testing.T) {
	if m := browsePagesRe.FindStringSubmatch(loadBrowse(t)); len(m) < 2 {
		t.Error("pagination regex did not match the captured page")
	}
}

// A terabyte-sized multi-season pack used to fail the size regex and be
// dropped along with the whole row.
func TestSizeUnits(t *testing.T) {
	for _, tc := range []struct {
		cell string
		want string
	}{
		{"<td class='s'>2.55 ГБ</td>", "2.55 ГБ"},
		{"<td class='s'>701.5 МБ</td>", "701.5 МБ"},
		{"<td class='s'>2.278 ТБ</td>", "2.278 ТБ"},
		{"<td class='s'>512 КБ</td>", "512 КБ"},
	} {
		m := mp5Re.FindStringSubmatch(tc.cell)
		if len(m) < 2 || m[1] != tc.want {
			t.Errorf("mp5Re(%q) = %v, want %q", tc.cell, m, tc.want)
		}
	}
}
