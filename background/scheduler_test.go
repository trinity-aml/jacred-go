package background

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func mustSchedule(t *testing.T, spec string) schedule {
	t.Helper()
	s, err := parseSchedule(strings.Fields(spec))
	if err != nil {
		t.Fatalf("parse %q: %v", spec, err)
	}
	return s
}

// The forms the repo's crontab actually uses.
func TestScheduleMatching(t *testing.T) {
	cases := []struct {
		spec, when string
		want       bool
	}{
		{"*/5 * * * *", "2026-09-25 10:05", true},
		{"*/5 * * * *", "2026-09-25 10:06", false},
		{"*/15 * * * *", "2026-09-25 10:45", true},
		{"25 * * * *", "2026-09-25 03:25", true},
		{"25 * * * *", "2026-09-25 03:26", false},
		{"30 4 * * *", "2026-09-25 04:30", true},
		{"30 4 * * *", "2026-09-25 05:30", false},
		{"10 */4 * * *", "2026-09-25 08:10", true},
		{"10 */4 * * *", "2026-09-25 09:10", false},
		{"15 */6 * * *", "2026-09-25 18:15", true},
		{"15 */6 * * *", "2026-09-25 17:15", false},
		// Ranges and lists are not in the file today, but an edit may add them.
		{"0 9-17 * * *", "2026-09-25 13:00", true},
		{"0 9-17 * * *", "2026-09-25 08:00", false},
		{"0,30 * * * *", "2026-09-25 07:30", true},
		{"0,30 * * * *", "2026-09-25 07:15", false},
		{"5/10 * * * *", "2026-09-25 07:25", true},
		{"5/10 * * * *", "2026-09-25 07:20", false},
	}
	for _, c := range cases {
		if got := mustSchedule(t, c.spec).matches(at(t, c.when)); got != c.want {
			t.Errorf("%q at %s = %v, want %v", c.spec, c.when, got, c.want)
		}
	}
}

// Day-of-month and day-of-week are ORed when both are restricted — cron's
// oddest rule, and the one a hand-rolled parser gets wrong.
func TestDayOfMonthAndWeekAreOred(t *testing.T) {
	s := mustSchedule(t, "0 0 13 * 5") // the 13th, or any Friday
	if !s.matches(at(t, "2026-09-13 00:00")) {
		t.Error("the 13th should match")
	}
	if !s.matches(at(t, "2026-09-25 00:00")) { // a Friday
		t.Error("a Friday should match")
	}
	if s.matches(at(t, "2026-09-24 00:00")) { // Thursday the 24th
		t.Error("neither day matched, so it should not fire")
	}
	// With only one of them restricted it is a plain AND.
	if mustSchedule(t, "0 0 13 * *").matches(at(t, "2026-09-25 00:00")) {
		t.Error("day-of-month alone must not match another day")
	}
}

func TestBadSchedulesAreRejected(t *testing.T) {
	for _, spec := range []string{
		"60 * * * *", "* 24 * * *", "* * 32 * *", "* * * 13 *", "* * * * 7",
		"*/0 * * * *", "x * * * *", "* * * *", "5-1 * * * *",
	} {
		if _, err := parseSchedule(strings.Fields(spec)); err == nil {
			t.Errorf("%q was accepted", spec)
		}
	}
}

// Only `curl -s "<url>"` is accepted, and nothing reaches a shell: the file is
// configuration, not a place to ask the process to run commands.
func TestOnlyCurlJobsAreAccepted(t *testing.T) {
	in := `
# a comment
*/5 * * * *    curl -s "http://127.0.0.1:9117/cron/rutor/parse"
30 4 * * *     curl -s "http://127.0.0.1:9117/cron/rudub/parse?limit_page=20"

*/5 * * * *    rm -rf /
*/5 * * * *    curl -s http://unquoted/
*/5 * * * *    /bin/sh -c "curl http://x/"
99 * * * *     curl -s "http://x/"
`
	jobs, warnings := parseCrontab(strings.NewReader(in))
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2: %+v", len(jobs), jobs)
	}
	if jobs[0].url != "http://127.0.0.1:9117/cron/rutor/parse" {
		t.Errorf("url = %q", jobs[0].url)
	}
	if !strings.Contains(jobs[1].url, "limit_page=20") {
		t.Errorf("query string lost: %q", jobs[1].url)
	}
	if len(warnings) != 4 {
		t.Errorf("got %d warnings, want 4 (shell command, unquoted url, sh wrapper, bad minute): %v", len(warnings), warnings)
	}
	for _, j := range jobs {
		if strings.Contains(j.url, "rm -rf") {
			t.Error("a shell command became a job")
		}
	}
}

// The repo's own crontab must parse completely — it is the file this runs.
func TestRepoCrontabParses(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "crontab"))
	if err != nil {
		t.Skipf("no crontab in the repo root: %v", err)
	}
	defer f.Close()
	jobs, warnings := parseCrontab(f)
	if len(warnings) != 0 {
		t.Errorf("the repo crontab produced warnings: %v", warnings)
	}
	if len(jobs) < 50 {
		t.Errorf("only %d jobs parsed from the repo crontab", len(jobs))
	}
	for _, j := range jobs {
		if !strings.HasPrefix(j.url, "http://127.0.0.1:9117/") {
			t.Errorf("line %d points somewhere unexpected: %s", j.line, j.url)
		}
	}
}

// A job still running must not be started again: parsealltask is scheduled
// every few minutes while a full sweep takes hours.
func TestOverlappingRunIsSkipped(t *testing.T) {
	var inFlight, total int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&total, 1)
		atomic.AddInt32(&inFlight, 1)
		defer atomic.AddInt32(&inFlight, -1)
		<-release
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	s := NewScheduler("", srv.URL)
	j := &job{spec: "* * * * *", url: srv.URL, schedule: mustSchedule(t, "* * * * *")}
	s.jobs = []*job{j}

	now := time.Now()
	s.tick(t.Context(), now)
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&inFlight) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// Fire again while the first is still in the handler.
	s.tick(t.Context(), now)
	s.tick(t.Context(), now)

	close(release)
	for j.running.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&total); got != 1 {
		t.Errorf("the endpoint was hit %d times, want 1", got)
	}
	if j.skips.Load() != 2 {
		t.Errorf("skips = %d, want 2", j.skips.Load())
	}
}

// A job fires as a plain GET against the process's own listener, so it goes
// through the same middleware as a curl from the shell.
func TestJobFiresOverLoopbackHTTP(t *testing.T) {
	var gotPath, gotMethod string
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.RequestURI(), r.Method
		w.Write([]byte(`{"status":"ok"}`))
		close(done)
	}))
	defer srv.Close()

	s := NewScheduler("", srv.URL)
	j := &job{url: srv.URL + "/cron/rutor/parse?limit_page=2", schedule: mustSchedule(t, "* * * * *")}
	s.jobs = []*job{j}
	s.tick(t.Context(), time.Now())

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the job never fired")
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %s", gotMethod)
	}
	if gotPath != "/cron/rutor/parse?limit_page=2" {
		t.Errorf("path = %q", gotPath)
	}
}

// An edited file is picked up without a restart; an unreadable one leaves the
// previous jobs alone rather than emptying the schedule.
func TestReloadPicksUpEditsAndKeepsJobsOnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "crontab")
	write := func(body string) {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("*/5 * * * *  curl -s \"http://x/a\"\n")

	s := NewScheduler(path, "http://x")
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	if len(s.jobs) != 1 {
		t.Fatalf("jobs = %d", len(s.jobs))
	}

	time.Sleep(10 * time.Millisecond)
	write("*/5 * * * *  curl -s \"http://x/a\"\n*/10 * * * * curl -s \"http://x/b\"\n")
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	if len(s.jobs) != 2 {
		t.Errorf("after edit jobs = %d, want 2", len(s.jobs))
	}

	os.Remove(path)
	if err := s.reload(); err == nil {
		t.Error("a missing file should be reported")
	}
	if len(s.jobs) != 2 {
		t.Errorf("jobs were dropped when the file went missing: %d", len(s.jobs))
	}
}
