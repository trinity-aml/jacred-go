package background

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The crontab is 100 lines of which 59 are jobs; the rest explains why each
// tracker is polled the way it is — measured timings, cold-run costs, why
// subsplease runs hourly. An editor that parsed the jobs out and wrote them
// back would lose all of it on the first save, so rendering must be the exact
// inverse of parsing.
func TestCrontabRoundTripsUnchanged(t *testing.T) {
	path := filepath.Join("..", "crontab")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no crontab in the repo root: %v", err)
	}
	entries, warnings := ParseCrontabEntries(strings.NewReader(string(raw)))
	if len(warnings) != 0 {
		t.Errorf("the repo crontab produced warnings: %v", warnings)
	}
	if n := countJobs(entries); n < 50 {
		t.Errorf("only %d jobs parsed", n)
	}

	got := RenderCrontabEntries(entries)
	want := string(raw)
	if !strings.HasSuffix(want, "\n") {
		want += "\n"
	}
	if got != want {
		// Show the first differing line rather than a 100-line dump.
		g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
		for i := range w {
			if i >= len(g) || g[i] != w[i] {
				t.Fatalf("line %d differs after a round-trip:\n  got:  %q\n  want: %q", i+1, lineAt(g, i), w[i])
			}
		}
		t.Fatalf("rendered %d lines, original has %d", len(g), len(w))
	}
}

func lineAt(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return "<missing>"
}

// Comments and blank lines are carried through verbatim, in place.
func TestTextLinesSurviveVerbatim(t *testing.T) {
	src := "# a note\n\n*/5 * * * *    curl -s \"http://x/a\"\n# trailing note\n"
	entries, _ := ParseCrontabEntries(strings.NewReader(src))
	if len(entries) != 4 {
		t.Fatalf("got %d entries, want 4", len(entries))
	}
	if entries[0].Kind != "text" || entries[0].Text != "# a note" {
		t.Errorf("entry 0 = %+v", entries[0])
	}
	if entries[2].Kind != "job" || entries[2].URL != "http://x/a" {
		t.Errorf("entry 2 = %+v", entries[2])
	}
	if got := RenderCrontabEntries(entries); got != src {
		t.Errorf("round-trip changed the file:\n%q\n%q", got, src)
	}
}

// A commented-out job is cron's own idiom for "off" and is what the UI toggle
// writes. It must survive a round-trip as a disabled job, not as prose.
func TestDisabledJobRoundTrips(t *testing.T) {
	src := "# */5 * * * *    curl -s \"http://x/a\"\n"
	entries, warnings := ParseCrontabEntries(strings.NewReader(src))
	if len(warnings) != 0 {
		t.Errorf("warnings: %v", warnings)
	}
	if len(entries) != 1 || entries[0].Kind != "job" || !entries[0].Disabled {
		t.Fatalf("entries = %+v", entries)
	}
	if got := RenderCrontabEntries(entries); got != src {
		t.Errorf("got %q, want %q", got, src)
	}

	// Toggling it on removes the comment and nothing else.
	entries[0].Disabled = false
	if got, want := RenderCrontabEntries(entries), "*/5 * * * *    curl -s \"http://x/a\"\n"; got != want {
		t.Errorf("enabling gave %q, want %q", got, want)
	}
}

// Prose must never be mistaken for a disabled job, or a save would turn a
// comment into a live schedule.
func TestProseIsNotMistakenForADisabledJob(t *testing.T) {
	src := "# parse берёт свежие релизы; parseshows обходит каталог\n# Rudub (ex-BaibaKoTV) — cp1251, только HD 1080/2160\n"
	entries, _ := ParseCrontabEntries(strings.NewReader(src))
	for _, e := range entries {
		if e.Kind == "job" {
			t.Errorf("prose became a job: %+v", e)
		}
	}
}

// A line that was meant to be a job but is broken stays in the file as text, so
// a typo is reported rather than silently deleted on the next save.
func TestBrokenJobLineIsKeptAsText(t *testing.T) {
	src := "99 * * * *    curl -s \"http://x/a\"\n"
	entries, warnings := ParseCrontabEntries(strings.NewReader(src))
	if len(warnings) == 0 {
		t.Error("a bad minute produced no warning")
	}
	if len(entries) != 1 || entries[0].Kind != "text" {
		t.Fatalf("entries = %+v", entries)
	}
	if got := RenderCrontabEntries(entries); got != src {
		t.Errorf("the broken line was not preserved: %q", got)
	}
}

// The scheduler skips a line it cannot parse, so saving a typo would silently
// stop that job from ever running. Refuse instead.
func TestValidationRejectsUnusableJobs(t *testing.T) {
	cases := []struct {
		name  string
		entry CrontabEntry
	}{
		{"bad minute", CrontabEntry{Kind: "job", Spec: "99 * * * *", URL: "http://x/"}},
		{"four fields", CrontabEntry{Kind: "job", Spec: "* * * *", URL: "http://x/"}},
		{"empty url", CrontabEntry{Kind: "job", Spec: "* * * * *", URL: ""}},
		{"url with a space", CrontabEntry{Kind: "job", Spec: "* * * * *", URL: "http://x/ a"}},
		{"url with a quote", CrontabEntry{Kind: "job", Spec: "* * * * *", URL: `http://x/"`}},
		{"scheme-less url", CrontabEntry{Kind: "job", Spec: "* * * * *", URL: "x/a"}},
		{"not a flag", CrontabEntry{Kind: "job", Spec: "* * * * *", URL: "http://x/", Flags: "rm"}},
		{"newline in text", CrontabEntry{Kind: "text", Text: "a\nb"}},
	}
	for _, c := range cases {
		if errs := ValidateCrontabEntries([]CrontabEntry{c.entry}); len(errs) == 0 {
			t.Errorf("%s was accepted", c.name)
		}
	}

	ok := []CrontabEntry{
		{Kind: "job", Spec: "*/5 * * * *", URL: "http://127.0.0.1:9117/cron/rutor/parse", Flags: "-s"},
		{Kind: "job", Spec: "0 9-17 * * 1-5", URL: "/cron/rutor/parse?page=1"},
		{Kind: "text", Text: "# fine"},
	}
	if errs := ValidateCrontabEntries(ok); len(errs) != 0 {
		t.Errorf("valid entries rejected: %v", errs)
	}
}

func TestWriteRefusesAnInvalidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crontab")
	if err := WriteCrontabEntries(path, []CrontabEntry{{Kind: "job", Spec: "99 * * * *", URL: "http://x/"}}); err == nil {
		t.Fatal("an invalid schedule was written")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("a file was created despite the refusal")
	}
}

func TestWriteIsAtomicAndReadsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crontab")
	in := []CrontabEntry{
		{Kind: "text", Text: "# schedule"},
		{Kind: "job", Spec: "*/5 * * * *", URL: "http://127.0.0.1:9117/cron/rutor/parse", Flags: "-s"},
		{Kind: "job", Spec: "30 4 * * *", URL: "http://127.0.0.1:9117/cron/rudub/parse?limit_page=20", Flags: "-s", Disabled: true},
	}
	if err := WriteCrontabEntries(path, in); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("the temp file was left behind")
	}

	back, warnings, err := ReadCrontabFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings on read-back: %v", warnings)
	}
	if len(back) != 3 || back[1].URL != in[1].URL || !back[2].Disabled {
		t.Fatalf("read back %+v", back)
	}

	// And the scheduler's own loader must see the enabled job and not the
	// disabled one — the two parsers have to agree.
	jobs, _ := parseCrontab(strings.NewReader(RenderCrontabEntries(in)))
	if len(jobs) != 1 || jobs[0].url != in[1].URL {
		t.Errorf("the scheduler loaded %d job(s) from the same file", len(jobs))
	}
}

func TestNextRun(t *testing.T) {
	from := time.Date(2026, 9, 26, 10, 3, 0, 0, time.UTC)
	got := NextRun("*/15 * * * *", from)
	if got.Minute() != 15 || got.Hour() != 10 {
		t.Errorf("next = %s, want 10:15", got)
	}
	if got := NextRun("30 4 * * *", from); got.Hour() != 4 || got.Minute() != 30 || got.Day() != 27 {
		t.Errorf("next = %s, want the 27th at 04:30", got)
	}
	if !NextRun("bad spec", from).IsZero() {
		t.Error("a bad spec produced a time")
	}
	// A date that never comes round must not hang or lie.
	if !NextRun("0 0 30 2 *", from).IsZero() {
		t.Error("Feb 30 produced a time")
	}
}
