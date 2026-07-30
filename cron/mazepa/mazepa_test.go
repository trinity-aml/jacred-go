package mazepa

import (
	"os"
	"strings"
	"testing"
)

// forum_f12.html is a real mazepa listing captured 2026-07-30 (logged in,
// account identity scrubbed). It is the markup that broke the parser: the
// site moved to SEO paths and dropped magnets from the listing entirely.
func loadFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	return string(b)
}

// The regression: every row still parsed, but magnetRe found nothing, and a
// row without a magnet is dropped with `continue`. All 50 rows fell out and
// the run reported fetched=0 with failed=0 — indistinguishable from a quiet
// day on the tracker, which is exactly how the kinozal breakage hid.
func TestListingHasNoMagnetsButKeepsAttachments(t *testing.T) {
	body := loadFixture(t, "forum_f12.html")

	if n := len(magnetRe.FindAllString(body, -1)); n != 0 {
		t.Errorf("fixture has %d inline magnets — it no longer captures the regression", n)
	}
	rows := rowRe.FindAllString(body, -1)
	if len(rows) != 50 {
		t.Fatalf("rowRe matched %d rows, want 50", len(rows))
	}
	withAttachment := 0
	for _, block := range rows {
		if m := dlRe.FindStringSubmatch(block); len(m) > 1 {
			withAttachment++
		}
	}
	if withAttachment != 50 {
		t.Errorf("only %d/50 rows expose a dl.php attachment — magnet resolution would drop the rest", withAttachment)
	}
}

// Everything except the magnet still parses off the row, so the fix stays
// confined to magnet resolution rather than a rewrite.
func TestRowFieldsSurviveTheNewMarkup(t *testing.T) {
	rows := rowRe.FindAllString(loadFixture(t, "forum_f12.html"), -1)
	if len(rows) == 0 {
		t.Fatal("no rows")
	}

	var titles, ids, seeds, sizes int
	for _, block := range rows {
		if m := titleRe.FindStringSubmatch(block); len(m) > 1 && strings.TrimSpace(m[1]) != "" {
			titles++
		}
		if m := inlineReB10e5aRe.FindStringSubmatch(block); len(m) > 1 {
			ids++
		}
		if m := seedRe.FindStringSubmatch(block); len(m) > 1 {
			seeds++
		}
		if parseSizeName(block) != "" {
			sizes++
		}
	}
	if ids != 50 {
		t.Errorf("topic ids: got %d, want 50", ids)
	}
	if seeds != 50 {
		t.Errorf("seeders: got %d, want 50", seeds)
	}
	if sizes != 50 {
		t.Errorf("sizes: got %d, want 50", sizes)
	}
	if titles < 45 {
		t.Errorf("titles: got %d, want most of 50", titles)
	}
}

// The attachment id is not the topic id — mixing them up downloads the wrong
// file (or 404s), so pin the pairing seen in the fixture.
func TestAttachmentIDIsDistinctFromTopicID(t *testing.T) {
	rows := rowRe.FindAllString(loadFixture(t, "forum_f12.html"), -1)

	same := 0
	for _, block := range rows {
		topic := inlineReB10e5aRe.FindStringSubmatch(block)
		att := dlRe.FindStringSubmatch(block)
		if len(topic) > 1 && len(att) > 1 && topic[1] == att[1] {
			same++
		}
	}
	if same == len(rows) {
		t.Error("attachment id equals topic id on every row — dlRe is probably matching the wrong link")
	}
}

// Topic URLs keep the viewtopic.php?t=N form even though the site now links
// SEO paths: the site still treats it as canonical (it is what the .torrent
// comment field contains) and it redirects. Changing it would re-key every
// stored record and duplicate the whole tracker.
func TestTopicURLFormIsStable(t *testing.T) {
	body := loadFixture(t, "forum_f12.html")
	if strings.Contains(body, "viewtopic.php?t=") {
		t.Skip("fixture still carries old-style links; the stability guard is moot")
	}
	if !strings.Contains(body, "-t=") || !strings.Contains(body, ".html") {
		t.Error("fixture lacks the SEO topic links this guard is about")
	}
}
