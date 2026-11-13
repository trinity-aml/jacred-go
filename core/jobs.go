package core

import (
	"context"
	"log"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Background job tracking: what a long-running cron op is doing right now, and
// the watchdog that stops one that has stopped doing anything.
//
// Two gaps, both met in production on 2026-11-13.
//
// **A sweep in flight was opaque.** `/trackers` records the outcome of the last
// *finished* run and a parser logs `processed=N/M` only once it is done, so the
// one question an operator asks during a multi-hour pass — how far along is it
// — had no answer anywhere. A rutracker `parsealltask` ran for 4511 s and ended
// in `cloudflare challenge (bypass failed)`; from outside there was no way to
// tell working from wedged, and `/cron/rutracker/parse` answering `work` says
// only that *something* holds the run flag.
//
// **Nothing could stop one either.** Every cron handler passed
// context.Background() (66 call sites, zero `context.WithTimeout` anywhere in
// server/, cron/ or background/), so no run had a deadline, a stall guard, or
// any way to be ended short of restarting the process.
//
// # Progress is derived, not reported
//
// The two functions that honestly know how a sweep is going are
// BeginParseAllCycle — how many pages this run owes — and NoteParseAllAttempt —
// a page just settled. Both live in parseallcycle.go and both are already
// called by all twelve sweeping parsers. Hooking them costs two lines in one
// file. Asking the parsers to report instead would be the same two lines copied
// into twelve packages, which is the hand-maintained duplicate that lost this
// codebase 35 rutracker forums and, twice over, a tracker missing from a
// registration list nothing checked.
//
// The server only starts and finishes jobs and reads snapshots; it never has to
// know what a page is.

const (
	// JobStallTimeout is how long a sweep may settle no page before the
	// watchdog calls it stuck rather than slow.
	//
	// The margin is wide on purpose, because a failing page still counts as
	// progress: the cycle's failure budget gives up on one after
	// ParseAllFailBudget attempts and settles it, so even a tracker refusing
	// every single request walks its map to the end and finishes. Reaching
	// this timeout means nothing is moving at all — not a slow page, not a
	// failing page, nothing.
	JobStallTimeout = 45 * time.Minute

	// JobUpdateTasksWallClock bounds updatetasksparse, which does not walk
	// the cycle and therefore offers no progress signal to watch. Its work
	// is bounded by construction — one listing fetch per category — so a
	// wall clock is the honest guard. For scale: rutracker's 98 forums take
	// 85 s when the cookie replay works and roughly five minutes when every
	// page is a browser render.
	JobUpdateTasksWallClock = 30 * time.Minute

	// jobWatchInterval is how often a watchdog re-checks. It is capped to a
	// quarter of the shortest limit so a test can use a limit of
	// milliseconds without waiting a minute for the first tick.
	jobWatchInterval = time.Minute
)

// JobLimits is the watchdog policy for one op. A zero field disables that half.
type JobLimits struct {
	// Stall cancels the job when no page has been attempted for this long.
	// Only meaningful for ops that walk the cycle, since that is where the
	// signal comes from.
	Stall time.Duration
	// Wall cancels the job this long after it started, whatever it is doing.
	Wall time.Duration
}

// sweepOps are the ops that walk a ParseAll cycle, and so the ones whose
// progress the hooks in parseallcycle.go can attribute. Parse and ParseAllTask
// share one run flag in all twelve sweeping parsers, so at most one sweep per
// tracker is ever in flight and a tracker name is enough to find it.
var sweepOps = []string{"parsealltask", "parselatest"}

// JobLimitsForOp maps a cron op to its watchdog policy.
//
// `parse` is deliberately left unguarded. A legitimate one runs for minutes
// with a working cookie replay and for hours when every page is a browser
// render — measured on rutracker, both in the same day — and it has no progress
// signal of its own, so any wall clock here would be a guess with a real chance
// of killing work that was going to succeed. Upstream limits UpdateTasks only,
// for the same reason.
func JobLimitsForOp(op string) JobLimits {
	switch strings.ToLower(strings.TrimSpace(op)) {
	case "parsealltask", "parselatest":
		return JobLimits{Stall: JobStallTimeout}
	case "updatetasksparse":
		return JobLimits{Wall: JobUpdateTasksWallClock}
	}
	return JobLimits{}
}

// Job is one running cron op.
type Job struct {
	tracker   string
	op        string
	startedAt time.Time
	limits    JobLimits
	cancel    context.CancelFunc

	// registered is false for a job that lost the race against one already
	// running under the same key; it keeps Finish from deleting the live
	// entry and keeps the watchdog from arming twice.
	registered bool
	stop       chan struct{}
	stopOnce   sync.Once

	mu           sync.Mutex
	pagesDone    int
	pagesTotal   int
	lastActivity time.Time
	cancelled    string
}

var (
	jobsMu sync.Mutex
	jobs   = map[string]*Job{}
)

func jobKey(tracker, op string) string {
	return strings.ToLower(strings.TrimSpace(tracker)) + ":" + strings.ToLower(strings.TrimSpace(op))
}

// StartJob registers a running cron op and arms its watchdog. The caller must
// call Finish when the op returns, whatever the outcome.
//
// A second start for the same tracker and op while the first still runs returns
// an **unregistered** job: the parser's own run flag has already answered that
// request with "work", and letting it overwrite the live entry would hide the
// real run and silently disarm its watchdog.
func StartJob(tracker, op string, cancel context.CancelFunc, limits JobLimits) *Job {
	now := time.Now()
	j := &Job{
		tracker:      strings.ToLower(strings.TrimSpace(tracker)),
		op:           strings.ToLower(strings.TrimSpace(op)),
		startedAt:    now,
		limits:       limits,
		cancel:       cancel,
		stop:         make(chan struct{}),
		lastActivity: now,
	}
	key := jobKey(tracker, op)

	jobsMu.Lock()
	if _, busy := jobs[key]; busy {
		jobsMu.Unlock()
		return j
	}
	jobs[key] = j
	j.registered = true
	jobsMu.Unlock()

	if limits.Stall > 0 || limits.Wall > 0 {
		go j.watch()
	}
	return j
}

// Finish deregisters the job and stops its watchdog.
func (j *Job) Finish() {
	if j == nil {
		return
	}
	j.stopOnce.Do(func() { close(j.stop) })
	if !j.registered {
		return
	}
	key := jobKey(j.tracker, j.op)
	jobsMu.Lock()
	if jobs[key] == j {
		delete(jobs, key)
	}
	jobsMu.Unlock()
}

// CancelReason is the watchdog's verdict, empty while the job is healthy. A
// handler reads it to say why a run ended, since a cancelled parser reports
// only that its context went away.
func (j *Job) CancelReason() string {
	if j == nil {
		return ""
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.cancelled
}

func (j *Job) watch() {
	tick := jobWatchInterval
	for _, limit := range []time.Duration{j.limits.Stall, j.limits.Wall} {
		if limit > 0 && limit/4 < tick {
			tick = limit / 4
		}
	}
	if tick <= 0 {
		tick = time.Millisecond
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-j.stop:
			return
		case <-t.C:
			if reason := j.overdue(time.Now()); reason != "" {
				j.cancelNow(reason)
				return
			}
		}
	}
}

// overdue names the limit the job has broken, or "" while it is healthy. Split
// out from watch so a test can drive it against a chosen clock.
func (j *Job) overdue(now time.Time) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.cancelled != "" {
		return ""
	}
	if j.limits.Wall > 0 && now.Sub(j.startedAt) > j.limits.Wall {
		return "wall-clock limit"
	}
	if j.limits.Stall > 0 && now.Sub(j.lastActivity) > j.limits.Stall {
		return "no progress"
	}
	return ""
}

func (j *Job) cancelNow(reason string) {
	j.mu.Lock()
	if j.cancelled != "" {
		j.mu.Unlock()
		return
	}
	j.cancelled = reason
	done, total := j.pagesDone, j.pagesTotal
	idle := time.Since(j.lastActivity)
	j.mu.Unlock()

	log.Printf("%s: %s cancelled by the watchdog — %s (%d/%d pages, running %s, idle %s)",
		j.tracker, j.op, reason, done, total,
		time.Since(j.startedAt).Round(time.Second), idle.Round(time.Second))
	if j.cancel != nil {
		j.cancel()
	}
}

// --------------------------------------------------------------- progress

// sweepJob finds the tracker's running sweep, if any.
func sweepJob(tracker string) *Job {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	for _, op := range sweepOps {
		if j, ok := jobs[jobKey(tracker, op)]; ok {
			return j
		}
	}
	return nil
}

// noteSweepTotal records how many pages the run owes. Called from
// BeginParseAllCycle, the only place that computes it.
func noteSweepTotal(tracker string, pending int) {
	j := sweepJob(tracker)
	if j == nil || pending < 0 {
		return
	}
	j.mu.Lock()
	j.pagesTotal = pending
	j.lastActivity = time.Now()
	j.mu.Unlock()
}

// noteSweepAttempt records that the run is still doing something — a page was
// attempted, settled or not.
//
// Activity and progress are deliberately separate. A page that fails has not
// progressed, but the run is plainly alive, and treating a run of failures as a
// stall would cancel exactly the pass that the cycle's failure budget exists to
// let finish.
func noteSweepAttempt(tracker string, settled bool) {
	j := sweepJob(tracker)
	if j == nil {
		return
	}
	j.mu.Lock()
	j.lastActivity = time.Now()
	if settled {
		j.pagesDone++
	}
	j.mu.Unlock()
}

// --------------------------------------------------------------- reading

// JobStatus is one row of /health/background-jobs.
//
// There is deliberately no currentCategory/currentPage, which upstream reports:
// the hooks this is built on see a cycle slot, not a category name, and getting
// those two fields would mean a reporting call in each of the twelve parsers —
// the duplicate this design exists to avoid. The parser's own log already
// prints `cat=… page=…` per page.
type JobStatus struct {
	ID           string
	Tracker      string
	Op           string
	StartedAt    time.Time
	Elapsed      time.Duration
	PagesDone    int
	PagesTotal   int
	Percent      *int
	LastActivity time.Time
	Idle         time.Duration
	StallsAt     time.Time
	Deadline     time.Time
	Cancelled    string
	Summary      string
}

// RunningJobs snapshots every registered job, oldest first.
//
// The list lives in process memory, so it is empty after a restart until
// something starts again. What is still owed on disk survives a restart and is
// a different question — that is the cycle checkpoint.
func RunningJobs() []JobStatus {
	jobsMu.Lock()
	live := make([]*Job, 0, len(jobs))
	for _, j := range jobs {
		live = append(live, j)
	}
	jobsMu.Unlock()

	now := time.Now()
	out := make([]JobStatus, 0, len(live))
	for _, j := range live {
		out = append(out, j.status(now))
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].StartedAt.Equal(out[b].StartedAt) {
			return out[a].ID < out[b].ID
		}
		return out[a].StartedAt.Before(out[b].StartedAt)
	})
	return out
}

func (j *Job) status(now time.Time) JobStatus {
	j.mu.Lock()
	done, total, last, cancelled := j.pagesDone, j.pagesTotal, j.lastActivity, j.cancelled
	j.mu.Unlock()

	st := JobStatus{
		ID:           j.tracker + ":" + j.op,
		Tracker:      j.tracker,
		Op:           j.op,
		StartedAt:    j.startedAt.UTC(),
		Elapsed:      now.Sub(j.startedAt),
		PagesDone:    done,
		PagesTotal:   total,
		LastActivity: last.UTC(),
		Idle:         now.Sub(last),
		Cancelled:    cancelled,
	}
	if total > 0 {
		pct := done * 100 / total
		if pct > 100 {
			pct = 100
		}
		st.Percent = &pct
	}
	if j.limits.Stall > 0 {
		st.StallsAt = last.Add(j.limits.Stall).UTC()
	}
	if j.limits.Wall > 0 {
		st.Deadline = j.startedAt.Add(j.limits.Wall).UTC()
	}
	st.Summary = jobSummary(st)
	return st
}

func jobSummary(st JobStatus) string {
	var b strings.Builder
	if st.PagesTotal > 0 {
		b.WriteString(strconv.Itoa(st.PagesDone))
		b.WriteString("/")
		b.WriteString(strconv.Itoa(st.PagesTotal))
		b.WriteString(" pages")
		if st.Percent != nil {
			b.WriteString(" · ")
			b.WriteString(strconv.Itoa(*st.Percent))
			b.WriteString("%")
		}
	} else if st.PagesDone > 0 {
		b.WriteString(strconv.Itoa(st.PagesDone))
		b.WriteString(" pages")
	} else {
		b.WriteString("running")
	}
	b.WriteString(" · elapsed ")
	b.WriteString(st.Elapsed.Round(time.Second).String())
	if st.Cancelled != "" {
		b.WriteString(" · cancelled: ")
		b.WriteString(st.Cancelled)
	}
	return b.String()
}

// TrackerFromCyclePath is the inverse of ParseAllCyclePath.
//
// The pair exists because BeginParseAllCycle is handed a path, not a tracker,
// and it is the one place that knows how large a run is. Deriving the name here
// keeps that knowledge in one function instead of adding a tracker argument to
// twelve call sites. TestCyclePathRoundTripsTheTrackerName pins the two
// together, so a change to the file name cannot quietly break attribution.
func TrackerFromCyclePath(path string) string {
	base := filepath.Base(strings.TrimSpace(path))
	const suffix = "_parseAllCycle.json"
	if !strings.HasSuffix(base, suffix) {
		return ""
	}
	return strings.TrimSuffix(base, suffix)
}
