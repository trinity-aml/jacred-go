package viruseproject

import (
	"context"
	"os"
	"testing"
)

func TestBuildRecordsFromFixtures(t *testing.T) {
	cases := []struct {
		name        string
		path        string
		cat         string
		wantTitlePrefix string
		wantQualities  int
		wantYear    int
		wantVideoType string
	}{
		{
			name: "Witcher 2025 (year in title, 2 qualities)",
			path: "/home/trinity1980/11111/Viruse Project - Ведьмак_ Сирены глубин _ The Witcher_ Sirens of the Deep _ 2025.html",
			cat: "cartoons",
			wantTitlePrefix: "Ведьмак: Сирены глубин",
			wantQualities: 2,
			wantYear: 2025,
			wantVideoType: "WEBRip",
		},
		{
			name: "Apex 2026 (year in title, 2 qualities)",
			path: "/home/trinity1980/11111/Viruse Project - Вершина _ Apex _ 2026.html",
			cat: "movies",
			wantTitlePrefix: "Вершина",
			wantQualities: 2,
			wantYear: 2026,
			wantVideoType: "WEB-DLRip",
		},
		{
			name: "Hell's Kitchen (no year in title)",
			path: "/home/trinity1980/11111/Viruse Project - Адская Кухня 11 (Hell's Kitchen 11).html",
			cat: "reality-show",
			wantTitlePrefix: "Адская Кухня 11",
			wantQualities: 1,
			wantYear: 2013,
			wantVideoType: "PDTVRip",
		},
		{
			name: "Peaky Blinders 2026 (1 quality)",
			path: "/home/trinity1980/11111/Viruse Project - Острые козырьки_ Бессмертный человек _ Peaky Blinders_ The Immortal Man _ 2026.html",
			cat: "movies",
			wantTitlePrefix: "Острые козырьки: Бессмертный человек",
			wantQualities: 1,
			wantYear: 2026,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile(tc.path)
			if err != nil {
				t.Skipf("fixture missing: %v", err)
			}
			p := &Parser{}
			records := p.buildRecords(context.Background(), "https://viruseproject.tv/post/example", string(data), "https://viruseproject.tv", tc.cat, []string{"movie"})
			if len(records) != tc.wantQualities {
				t.Fatalf("expected %d records, got %d", tc.wantQualities, len(records))
			}
			for i, r := range records {
				t.Logf("record[%d]: title=%q url=%q size=%v(%v) q=%v vt=%v name=%q orig=%q year=%v create=%v _tid=%v _downloadURI=%v",
					i, r["title"], r["url"], r["size"], r["sizeName"], r["quality"], r["videotype"],
					r["name"], r["originalname"], r["relased"], r["createTime"], r["_tid"], r["_downloadURI"])
				if title, _ := r["title"].(string); title == "" {
					t.Errorf("empty title in record %d", i)
				}
				if y, _ := r["relased"].(int); y != tc.wantYear {
					t.Errorf("record %d: relased=%d, want %d", i, y, tc.wantYear)
				}
				if tc.wantVideoType != "" {
					if vt, _ := r["videotype"].(string); vt != tc.wantVideoType {
						t.Errorf("record %d: videotype=%q, want %q", i, vt, tc.wantVideoType)
					}
				}
			}
		})
	}
}

func TestParseNames(t *testing.T) {
	cases := []struct {
		raw  string
		wantRu string
		wantEn string
	}{
		{"Ведьмак: Сирены глубин / The Witcher: Sirens of the Deep / 2025", "Ведьмак: Сирены глубин", "The Witcher: Sirens of the Deep"},
		{"Вершина / Apex / 2026", "Вершина", "Apex"},
		{"Соперники / Rivals / сезон 2 / 1-3 из 12", "Соперники", "Rivals"},
		{"Адская Кухня 11 (Hell's Kitchen 11)", "Адская Кухня 11", "Hell's Kitchen 11"},
		{"Шествие смерти (Death Parade)", "Шествие смерти", "Death Parade"},
		{"Фоллаут / Fallout / сезон 2", "Фоллаут", "Fallout"},
	}
	for _, tc := range cases {
		ru, en := parseNames(tc.raw)
		if ru != tc.wantRu || en != tc.wantEn {
			t.Errorf("parseNames(%q): got (%q, %q), want (%q, %q)", tc.raw, ru, en, tc.wantRu, tc.wantEn)
		}
	}
}

func TestParseRussianDate(t *testing.T) {
	cases := []struct {
		raw       string
		wantYear  int
	}{
		{"Четверг, 13 Февраль 2025 00:00", 2025},
		{"Вторник, 12 Май 2026 00:00", 2026},
		{"Понедельник, 23 Март 2026 00:00", 2026},
		{"Среда, 07 Май 2014 00:00", 2014},
	}
	for _, tc := range cases {
		got := parseRussianDate(tc.raw)
		if got.Year() != tc.wantYear {
			t.Errorf("parseRussianDate(%q): year=%d, want %d", tc.raw, got.Year(), tc.wantYear)
		}
	}
}
