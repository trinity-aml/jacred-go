package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Parse and ParseAllTask must share one run flag. With a guard apiece they
// swept concurrently — the deployed schedule fires parsealltask every few
// minutes while a full sweep runs for hours — so two runs drove the same
// session, the same re-login path and the same rate limit at the tracker.
// toloka is where that was measured: it answered HTTP 429 to download.php 126
// times in a single eight-category run.
//
// This is a drift guard rather than a unit test because the flag is declared
// separately in each of the twelve parsers, and a hand-maintained duplicate
// that nothing checks is exactly how this codebase lost 35 rutracker forums.
func TestParsersKeepOneRunFlag(t *testing.T) {
	dirs, err := filepath.Glob("../cron/*")
	if err != nil {
		t.Fatal(err)
	}
	fieldRe := regexp.MustCompile(`(?m)^\s+(working|allWork|busy)\s+bool\s*$`)

	checked := 0
	for _, dir := range dirs {
		name := filepath.Base(dir)
		src, err := os.ReadFile(filepath.Join(dir, name+".go"))
		if err != nil {
			continue
		}
		body := string(src)
		// Only the parsers that have both entrypoints are in scope.
		if !strings.Contains(body, ") ParseAllTask(") {
			continue
		}
		checked++
		found := fieldRe.FindAllStringSubmatch(body, -1)
		var names []string
		for _, m := range found {
			names = append(names, m[1])
		}
		if len(names) != 1 {
			t.Errorf("%s: ожидался один флаг прогона, найдено %d %v — Parse и ParseAllTask снова могут идти параллельно",
				name, len(names), names)
		}
	}
	if checked < 12 {
		t.Errorf("проверено парсеров: %d, ожидалось не меньше 12 — тест перестал их находить", checked)
	}
}
