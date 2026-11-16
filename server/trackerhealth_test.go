package server

import (
	"os"
	"strings"
	"testing"
	"time"

	"jacred/core"
)

func at(hhmmss string) time.Time {
	t, err := time.Parse("15:04:05", hhmmss)
	if err != nil {
		panic(err)
	}
	return time.Date(2026, 11, 16, t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
}

// TestAnAuthFailureOlderThanTheLoginIsSuperseded is the regression guard for
// what the page actually showed on 2026-11-16: rutracker reported "нет входа"
// while the log had it parsing. All three recorded runs said work_login — at
// 14:10, 14:45 and 14:50 — and the session store said the login had succeeded
// at 14:55:24. Every verdict on the page was older than the authorisation that
// disproved it, and the data to see that was already in the response.
func TestAnAuthFailureOlderThanTheLoginIsSuperseded(t *testing.T) {
	authed := at("14:55:24")
	st := TrackerState{
		Name:      "rutracker",
		Auth:      "session",
		AuthSince: &authed,
		Runs: []TrackerRun{
			{Op: "parsealltask", At: at("14:50:00"), HTTP: 500, Status: core.StatusWorkLogin, Error: "not authorized"},
			{Op: "parse", At: at("14:45:00"), HTTP: 500, Status: core.StatusWorkLogin, Error: "not authorized"},
			{Op: "updatetasksparse", At: at("14:10:00"), HTTP: 500, Status: core.StatusWorkLogin, Error: "not authorized"},
		},
	}

	got := trackerHealth(st)
	if got.Attention {
		t.Error("a tracker that logged in after its last failed run is still being reported as needing a human")
	}
	if got.Key != "authed-since" {
		t.Errorf("key = %q, want authed-since", got.Key)
	}
	if !strings.Contains(got.Why, "14:55:24") && !strings.Contains(got.Why, authed.Local().Format("15:04:05")) {
		t.Errorf("the verdict does not say when the login happened: %q", got.Why)
	}
}

// TestAnAuthFailureNewerThanTheLoginStillNeedsAHuman is the other direction,
// and the one that matters more: suppressing a real failure would hide a dead
// tracker, which is the whole thing /trackers exists to prevent.
func TestAnAuthFailureNewerThanTheLoginStillNeedsAHuman(t *testing.T) {
	authed := at("09:00:00")
	st := TrackerState{
		Name:      "rutracker",
		Auth:      "session",
		AuthSince: &authed,
		Runs: []TrackerRun{
			{Op: "parse", At: at("14:45:00"), HTTP: 500, Status: core.StatusWorkLogin, Error: "not authorized"},
		},
	}
	got := trackerHealth(st)
	if !got.Attention || got.Key != "auth" {
		t.Errorf("an authorisation failure newer than the last login was suppressed: %+v", got)
	}
}

// TestALaterSuccessfulRunWinsOutright: once a run after the login has actually
// succeeded there is measured evidence, so report it plainly.
func TestALaterSuccessfulRunWinsOutright(t *testing.T) {
	authed := at("14:55:24")
	st := TrackerState{
		AuthSince: &authed,
		Runs: []TrackerRun{
			{Op: "parsealltask", At: at("14:50:00"), HTTP: 500, Status: core.StatusWorkLogin, Error: "not authorized"},
			{Op: "parse", At: at("15:05:00"), HTTP: 200, Status: "ok", Fetched: 120},
		},
	}
	// Runs[0] is the failure (the page is handed them newest-first, but this
	// asserts the scan does not depend on that ordering for the success).
	if got := trackerHealth(st); got.Attention || got.Key != "ok" {
		t.Errorf("a successful run after the login did not win: %+v", got)
	}
}

// TestTheVerdictIsDecidedOnlyOnTheServer keeps the two renderers from growing
// their own opinions again. The page and the sidebar badge each used to
// classify a tracker from the raw run, and they had already drifted: the badge
// counted `failed > 0` as needing a human while the page did not.
func TestTheVerdictIsDecidedOnlyOnTheServer(t *testing.T) {
	for _, path := range []string{"wwwroot/trackers.html", "wwwroot/assets/app.js"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var code []string
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			code = append(code, line)
		}
		body := strings.Join(code, "\n")
		for _, reDerived := range []string{"'work_login'", "\"work_login\"", "'cf-challenge'", "\"cf-challenge\""} {
			if strings.Contains(body, reDerived) {
				t.Errorf("%s classifies %s itself; the verdict belongs to trackerHealth so the page and the badge cannot disagree", path, reDerived)
			}
		}
	}
}

// TestARunningSweepOutranksAnOlderFailure is the other half of the misleading
// display, and the stronger half.
//
// Measured on production 2026-11-16 17:11: rutracker was 572 of 10 862 pages
// into a parsealltask, elapsed 1h31m, idle 0s — working while you read the row
// — and /trackers said "нет входа" from a record an hour older. The page had no
// access to the job tracker at all; the original's web has a whole page for
// these, and this puts them where the verdict is formed.
func TestARunningSweepOutranksAnOlderFailure(t *testing.T) {
	pct := 5
	st := TrackerState{
		Name: "rutracker",
		Runs: []TrackerRun{
			{Op: "parsealltask", At: at("14:50:00"), HTTP: 500, Status: core.StatusWorkLogin, Error: "not authorized"},
		},
		Running: &TrackerJob{
			Op: "parsealltask", PagesCompleted: 572, PagesTotal: 10862,
			Percent: &pct, ElapsedSeconds: 5494, IdleSeconds: 0,
			Summary: "572/10862 pages · 5% · elapsed 1h31m34s",
		},
	}
	got := trackerHealth(st)
	if got.Attention {
		t.Error("a tracker with a sweep in flight and settling pages is reported as needing a human")
	}
	if got.Key != "running" {
		t.Errorf("key = %q, want running", got.Key)
	}
	if !strings.Contains(got.Why, "572/10862") {
		t.Errorf("the verdict does not carry the progress: %q", got.Why)
	}
}

// TestAStalledSweepDoesNotExcuseAFailure keeps the rule from covering for a
// wedged run. The watchdog cancels at 45 minutes of no progress; this window is
// shorter because it only decides what a row says, and it is still generous
// because a *failing* page counts as activity and a browser-rendered rutracker
// page takes minutes.
func TestAStalledSweepDoesNotExcuseAFailure(t *testing.T) {
	st := TrackerState{
		Runs: []TrackerRun{
			{Op: "parse", At: at("14:50:00"), HTTP: 500, Status: core.StatusWorkLogin, Error: "not authorized"},
		},
		Running: &TrackerJob{Op: "parse", IdleSeconds: int((jobMovingWindow + time.Minute).Seconds())},
	}
	if got := trackerHealth(st); !got.Attention || got.Key != "auth" {
		t.Errorf("a sweep that stopped settling pages still suppressed the failure: %+v", got)
	}
}

// TestTheLiveSweepIsRenderedFromTheServersField pins that the page reads the
// field rather than polling a second endpoint and re-deciding — the same
// one-decision rule as the verdict itself.
func TestTheLiveSweepIsRenderedFromTheServersField(t *testing.T) {
	raw, err := os.ReadFile("wwwroot/trackers.html")
	if err != nil {
		t.Fatalf("read trackers.html: %v", err)
	}
	page := string(raw)
	// The branch, not just the name: replacing `if (t.running)` with a dead
	// condition leaves the identifier behind inside it, and a guard that only
	// looked for the name passed against exactly that.
	if !strings.Contains(page, "if (t.running)") {
		t.Error("the trackers page no longer branches on the live sweep, so a run in flight is invisible again")
	}
	if strings.Contains(page, "/health/background-jobs") {
		t.Error("the page polls the jobs endpoint itself; the snapshot comes with /stats/parsers so the row and the verdict cannot disagree")
	}
}

// TestASweepWithNoProgressYetDoesNotSuppressAVerdict is the regression guard for
// a flaw the first deploy exposed. A restart fires the whole crontab at once,
// and eight seconds later production reported `running · elapsed 8s` with zero
// pages done for nineteen of twenty trackers — IdleSeconds is 0 then only
// because nothing has settled yet, so the window test passed trivially and
// every row went quiet. A job that has never settled a page proves nothing.
func TestASweepWithNoProgressYetDoesNotSuppressAVerdict(t *testing.T) {
	st := TrackerState{
		Runs: []TrackerRun{
			{Op: "parse", At: at("14:50:00"), HTTP: 500, Status: core.StatusWorkLogin, Error: "not authorized"},
		},
		Running: &TrackerJob{Op: "parsealltask", PagesCompleted: 0, PagesTotal: 1129, IdleSeconds: 0},
	}
	if got := trackerHealth(st); !got.Attention || got.Key != "auth" {
		t.Errorf("a sweep that has settled no pages suppressed the verdict: %+v", got)
	}

	// One settled page is enough — that is measured progress.
	st.Running.PagesCompleted = 1
	if got := trackerHealth(st); got.Attention || got.Key != "running" {
		t.Errorf("a sweep that has settled a page is not being trusted: %+v", got)
	}
}
