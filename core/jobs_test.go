package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// jobTestLimits are short enough to drive a watchdog in a test. The watchdog
// derives its tick from a quarter of the shortest limit, so these fire in
// milliseconds rather than in the 45 minutes production uses.
const (
	testStall = 120 * time.Millisecond
	testWall  = 120 * time.Millisecond
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A sweep that stops making progress has to be stopped. Before the watchdog
// existed nothing could: every cron handler passed context.Background(), so a
// wedged run held the parser's flag until someone restarted the process.
func TestStallWatchdogCancelsASweepWithNoProgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	job := StartJob("stalltracker", "parsealltask", cancel, JobLimits{Stall: testStall})
	defer job.Finish()

	waitFor(t, "the stalled sweep to be cancelled", func() bool { return ctx.Err() != nil })
	if got := job.CancelReason(); got != "no progress" {
		t.Errorf("CancelReason() = %q, want %q", got, "no progress")
	}
}

// The opposite direction, and the one that matters more: a sweep that is slow
// but alive must be left alone. A failing page is still activity — the cycle's
// failure budget gives up on it after three attempts — so a tracker refusing
// every request walks its whole map, and cancelling that would throw away the
// pass the budget exists to let finish.
func TestActivityKeepsASweepAlive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	job := StartJob("livetracker", "parsealltask", cancel, JobLimits{Stall: testStall})
	defer job.Finish()

	// Three stall windows' worth of failing pages: never settled, always busy.
	deadline := time.Now().Add(3 * testStall)
	for time.Now().Before(deadline) {
		noteSweepAttempt("livetracker", false)
		if ctx.Err() != nil {
			t.Fatalf("a sweep that was attempting pages got cancelled: %s", job.CancelReason())
		}
		time.Sleep(testStall / 8)
	}
}

// A wall clock ignores progress by design. updatetasksparse does not walk the
// cycle, so it has no progress to report; what bounds it is that its work is
// finite — one listing fetch per category.
func TestWallClockCancelsEvenWhileWorking(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	job := StartJob("walltracker", "updatetasksparse", cancel, JobLimits{Wall: testWall})
	defer job.Finish()

	go func() {
		for i := 0; i < 200; i++ {
			noteSweepAttempt("walltracker", true)
			time.Sleep(testWall / 20)
		}
	}()

	waitFor(t, "the wall clock to fire", func() bool { return ctx.Err() != nil })
	if got := job.CancelReason(); got != "wall-clock limit" {
		t.Errorf("CancelReason() = %q, want %q", got, "wall-clock limit")
	}
}

// Progress must come from the cycle store, not from the parsers. This drives
// the two real entry points — BeginParseAllCycle and NoteParseAllAttempt — and
// requires the job to have learned the size of the run and the pages done
// without any parser saying a word.
func TestProgressComesFromTheCycleNotTheParsers(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "temp"), 0o755); err != nil {
		t.Fatal(err)
	}
	const tracker = "cycletracker"

	job := StartJob(tracker, "parsealltask", func() {}, JobLimits{})
	defer job.Finish()

	slots := make([]CycleSlot, 4)
	fields := make([]*CycleFields, 4)
	for i := range slots {
		fields[i] = &CycleFields{}
		slots[i] = fields[i]
	}

	path := ParseAllCyclePath(dir, tracker)
	cycle, pending := BeginParseAllCycle(path, "fp", slots, nil, true)
	if pending != 4 {
		t.Fatalf("pending = %d, want 4", pending)
	}

	if got := find(t, tracker).PagesTotal; got != 4 {
		t.Errorf("PagesTotal = %d, want 4 — BeginParseAllCycle did not report the run size", got)
	}

	NoteParseAllAttempt(tracker, slots[0], cycle, true)
	NoteParseAllAttempt(tracker, slots[1], cycle, true)
	// A failure inside the budget is activity but not progress.
	NoteParseAllAttempt(tracker, slots[2], cycle, false)

	st := find(t, tracker)
	if st.PagesDone != 2 {
		t.Errorf("PagesDone = %d, want 2 (two settled, one failed inside its budget)", st.PagesDone)
	}
	if st.Percent == nil || *st.Percent != 50 {
		t.Errorf("Percent = %v, want 50", st.Percent)
	}
	if st.Summary == "" || st.Summary == "running" {
		t.Errorf("Summary = %q, want a progress line", st.Summary)
	}
}

func find(t *testing.T, tracker string) JobStatus {
	t.Helper()
	for _, st := range RunningJobs() {
		if st.Tracker == tracker {
			return st
		}
	}
	t.Fatalf("no running job for %q", tracker)
	return JobStatus{}
}

// A request that arrives while a sweep is running is answered "work" by the
// parser's own run flag. Registering it must not replace the live entry: that
// would hide the real run from /health/background-jobs and, worse, silently
// disarm its watchdog.
func TestASecondStartDoesNotClobberARunningJob(t *testing.T) {
	const tracker = "dupetracker"
	firstCancelled := false
	first := StartJob(tracker, "parsealltask", func() { firstCancelled = true }, JobLimits{})
	defer first.Finish()

	noteSweepTotal(tracker, 10)
	noteSweepAttempt(tracker, true)

	second := StartJob(tracker, "parsealltask", func() {}, JobLimits{})
	// The loser must not deregister the winner.
	second.Finish()

	st := find(t, tracker)
	if st.PagesDone != 1 || st.PagesTotal != 10 {
		t.Errorf("after a duplicate start: %d/%d pages, want 1/10 — the live job was replaced",
			st.PagesDone, st.PagesTotal)
	}
	if firstCancelled {
		t.Error("the duplicate start cancelled the running job")
	}
}

// Attribution depends on reading the tracker name back out of the checkpoint
// path, because BeginParseAllCycle is handed a path and not a name. If the file
// name ever changes, this fails rather than progress silently going nowhere.
func TestCyclePathRoundTripsTheTrackerName(t *testing.T) {
	for _, tracker := range []string{"rutracker", "nnmclub", "torrentby", "a"} {
		path := ParseAllCyclePath("/srv/jacred/Data", tracker)
		if got := TrackerFromCyclePath(path); got != tracker {
			t.Errorf("TrackerFromCyclePath(%q) = %q, want %q", path, got, tracker)
		}
	}
	for _, junk := range []string{"", "Data/temp/stats.json", "rutracker_taskParse.json", "/"} {
		if got := TrackerFromCyclePath(junk); got != "" {
			t.Errorf("TrackerFromCyclePath(%q) = %q, want \"\"", junk, got)
		}
	}
}

// The policy itself, including the deliberate hole: `parse` is unguarded,
// because a legitimate one runs for minutes with a working cookie replay and
// for hours when every page is a browser render, and it reports no progress a
// stall watchdog could read.
func TestJobLimitsMatchTheOp(t *testing.T) {
	cases := []struct {
		op          string
		stall, wall time.Duration
	}{
		{"parsealltask", JobStallTimeout, 0},
		{"parselatest", JobStallTimeout, 0},
		{"ParseAllTask", JobStallTimeout, 0},
		{"updatetasksparse", 0, JobUpdateTasksWallClock},
		{"parse", 0, 0},
		{"parsepages", 0, 0},
		{"", 0, 0},
	}
	for _, c := range cases {
		got := JobLimitsForOp(c.op)
		if got.Stall != c.stall || got.Wall != c.wall {
			t.Errorf("JobLimitsForOp(%q) = %+v, want stall=%s wall=%s", c.op, got, c.stall, c.wall)
		}
	}
}
