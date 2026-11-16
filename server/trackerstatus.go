package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"jacred/app"
	"jacred/core"
)

// TrackerRun is the outcome of one cron invocation.
//
// It is recorded by watching the HTTP response of /cron/<tracker>/<op> rather
// than by instrumenting the parsers: every parser has its own ParseResult type
// in its own package, and the handlers already normalise them into the same
// JSON. Watching the response means one interception point instead of edits in
// 23 packages, and it records exactly what the endpoint reported — the number a
// human would have seen — rather than a parallel accounting that could drift.
type TrackerRun struct {
	Tracker string    `json:"tracker"`
	Op      string    `json:"op"`
	At      time.Time `json:"at"`
	Seconds float64   `json:"seconds"`
	HTTP    int       `json:"http"`
	Status  string    `json:"status"`
	Error   string    `json:"error,omitempty"`
	Fetched int       `json:"fetched"`
	Added   int       `json:"added"`
	Updated int       `json:"updated"`
	Skipped int       `json:"skipped"`
	Failed  int       `json:"failed"`
}

// Ok reports whether the run is one a human can ignore.
func (r TrackerRun) Ok() bool {
	return r.HTTP >= 200 && r.HTTP < 300 && r.Error == "" &&
		r.Status != "error" && r.Status != core.StatusWorkLogin
}

type runStore struct {
	mu   sync.RWMutex
	runs map[string]map[string]TrackerRun // tracker -> op -> last run
	path string
}

func newRunStore(dataDir string) *runStore {
	s := &runStore{runs: map[string]map[string]TrackerRun{}}
	if dataDir != "" {
		s.path = filepath.Join(dataDir, "temp", "tracker_runs.json")
		s.load()
	}
	return s
}

func (s *runStore) load() {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var flat []TrackerRun
	if json.Unmarshal(b, &flat) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range flat {
		if s.runs[r.Tracker] == nil {
			s.runs[r.Tracker] = map[string]TrackerRun{}
		}
		s.runs[r.Tracker][r.Op] = r
	}
}

// record stores a run and persists the whole set. Cron fires at most a few
// times a minute across 23 trackers, so writing the file each time is cheaper
// than the bookkeeping a debounce would need — and it means a restart does not
// lose the one thing the page exists to show.
func (s *runStore) record(r TrackerRun) {
	s.mu.Lock()
	if s.runs[r.Tracker] == nil {
		s.runs[r.Tracker] = map[string]TrackerRun{}
	}
	// A "work" answer means the previous run is still going and this request
	// did nothing. Only one run per tracker per op is kept, so recording it
	// would erase the last real result — and a long sweep is exactly when that
	// happens: parsealltask is scheduled every few minutes while a rutor pass
	// takes hours, so a hundred no-ops would bury the outcome the page exists
	// to show. The informative record is the one already there.
	if r.Status == "work" {
		if _, seen := s.runs[r.Tracker][r.Op]; seen {
			s.mu.Unlock()
			return
		}
	}
	s.runs[r.Tracker][r.Op] = r
	flat := s.snapshotLocked()
	path := s.path
	s.mu.Unlock()

	if path == "" {
		return
	}
	b, err := json.Marshal(flat)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}

func (s *runStore) snapshotLocked() []TrackerRun {
	out := make([]TrackerRun, 0, len(s.runs)*2)
	for _, ops := range s.runs {
		for _, r := range ops {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tracker != out[j].Tracker {
			return out[i].Tracker < out[j].Tracker
		}
		return out[i].Op < out[j].Op
	})
	return out
}

// forTracker returns every recorded op for one tracker, newest first.
func (s *runStore) forTracker(name string) []TrackerRun {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ops := s.runs[name]
	out := make([]TrackerRun, 0, len(ops))
	for _, r := range ops {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

// ---------------------------------------------------------------- recording

// cronPathParts splits /cron/<tracker>/<op> into its two names. Anything else
// yields empty strings and is not recorded.
func cronPathParts(path string) (tracker, op string) {
	p := strings.Trim(strings.ToLower(path), "/")
	parts := strings.Split(p, "/")
	if len(parts) != 3 || parts[0] != "cron" {
		return "", ""
	}
	return parts[1], parts[2]
}

// captureWriter passes the response through untouched while keeping a copy of
// the body, which is a few hundred bytes of JSON for every cron endpoint.
type captureWriter struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

const captureLimit = 64 << 10

func (w *captureWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *captureWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.buf.Len() < captureLimit {
		w.buf.Write(b[:min(len(b), captureLimit-w.buf.Len())])
	}
	return w.ResponseWriter.Write(b)
}

// recordCronRuns remembers the outcome of every /cron/<tracker>/<op> call, and
// brackets it in the background-job tracker.
//
// Until this existed, the only record of a parser run was the process log: to
// learn that kinozal had been returning zero for a week, or that animelayer was
// failing every row, someone had to ssh in and grep. Several silent breakages
// survived that long for exactly this reason.
//
// It owns the job bracket too (core.StartJob / Finish) rather than leaving that
// to a second middleware, because the scope is identical — the same path parse,
// the same GET check, the same before-and-after — and the recorded run needs
// the job's verdict. A watchdog cancellation otherwise reaches /trackers as a
// bare `context canceled`, on the one page whose whole purpose is naming why a
// parser stopped.
func (s *Server) recordCronRuns(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tracker, op := cronPathParts(r.URL.Path)
		// Only real trackers. /cron/maintenance/... has the same shape and is
		// not a parser, so recording it would put a row in the store that no
		// page ever reads and register a job nobody asked about — and a typo'd
		// URL would do the same.
		if tracker == "" || !isKnownTracker(tracker) || r.Method != http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		cw := &captureWriter{ResponseWriter: w}

		// Descended from Background, never from r.Context(): a disconnecting
		// client must not cancel the run. cronCtx (server/jobs.go) explains
		// the rule; the watchdog is the only thing that may cancel this.
		runCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		job := core.StartJob(tracker, op, cancel, s.jobLimitsFor(op))

		next.ServeHTTP(cw, withCronRunCtx(r, runCtx))
		job.Finish()

		run := TrackerRun{
			Tracker: tracker,
			Op:      op,
			At:      started.UTC(),
			Seconds: time.Since(started).Seconds(),
			HTTP:    cw.status,
		}
		if run.HTTP == 0 {
			run.HTTP = http.StatusOK
		}
		var body map[string]any
		if json.Unmarshal(cw.buf.Bytes(), &body) == nil {
			run.Status, _ = body["status"].(string)
			run.Error, _ = body["error"].(string)
			// The string-returning entrypoints — parsealltask, parselatest,
			// updatetasksparse — answer {"status":"ok","text":"work"} when a
			// run is already in flight: the "work" lands in text, not status,
			// so record's guard below could not see it and the no-op
			// overwrote the last real result. Normalise it here, so that
			// guard stays the single place that decides. This got far more
			// frequent once Parse and ParseAllTask started sharing one run
			// flag — a skip is now the normal answer during a long sweep.
			if txt, _ := body["text"].(string); txt == "work" && run.Status == "ok" {
				run.Status = "work"
			}
			// Nineteen parsers report "fetched", four ("anidub", "selezen",
			// "aniliberty", "animelayer") report "parsed" for the same number.
			run.Fetched = jsonInt(body, "fetched", "parsed")
			run.Added = jsonInt(body, "added")
			run.Updated = jsonInt(body, "updated")
			run.Skipped = jsonInt(body, "skipped")
			run.Failed = jsonInt(body, "failed")
		}
		// The watchdog's reason beats whatever the parser managed to say on
		// its way out, which is some form of "context canceled".
		if reason := job.CancelReason(); reason != "" {
			run.Status = "canceled"
			run.Error = tracker + ": " + op + " stopped by the watchdog — " + reason
		}
		s.Runs.record(run)
	})
}

func jsonInt(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if f, ok := m[k].(float64); ok {
			return int(f)
		}
	}
	return 0
}

// ------------------------------------------------------------------ reading

// TrackerState is one row of /stats/parsers: what the config says, what CF
// routing is actually doing, and how the last runs went.
type TrackerState struct {
	Name      string       `json:"name"`
	Host      string       `json:"host"`
	Disabled  bool         `json:"disabled"`
	FetchMode string       `json:"fetchmode,omitempty"`
	Route     string       `json:"route"`
	CFSince   *time.Time   `json:"cfSince,omitempty"`
	Auth      string       `json:"auth"`
	AuthSince *time.Time   `json:"authSince,omitempty"`
	Runs      []TrackerRun `json:"runs"`

	// Running is the sweep in flight for this tracker, if any. Taken from the
	// same job tracker /health/background-jobs reads, because a run happening
	// *now* is the strongest evidence there is and the page had none of it: on
	// 2026-11-16 rutracker was 572/10862 pages into a parsealltask, moving
	// (idle 0s), while the row read "нет входа" off a record from an hour
	// earlier. The original's web has a whole page for these; this puts them
	// where the verdict is formed.
	Running *TrackerJob `json:"running,omitempty"`

	// Health is the verdict both the page and the sidebar badge render. It is
	// computed here, once, because it used to be computed twice — in
	// trackers.html and again in assets/app.js — and the two had already
	// drifted: the badge counted `failed > 0` as needing a human while the page
	// did not. That is the same hand-maintained duplicate this codebase has
	// been bitten by before.
	Health TrackerHealth `json:"health"`
}

// TrackerJob is the live progress of a sweep, in the same field names
// /health/background-jobs uses so the two cannot drift.
type TrackerJob struct {
	Op             string `json:"job"`
	PagesCompleted int    `json:"pagesCompleted"`
	PagesTotal     int    `json:"pagesTotal"`
	Percent        *int   `json:"percent,omitempty"`
	ElapsedSeconds int    `json:"elapsedSeconds"`
	IdleSeconds    int    `json:"idleSeconds"`
	Summary        string `json:"summary"`
}

// jobMovingWindow is how long a sweep may go without settling a page before its
// progress stops counting as evidence that the tracker is alive.
//
// Generous on purpose, and for the reason the watchdog's own margin is
// generous: a *failing* page still counts as activity, and rutracker through
// the browser takes minutes per page. Shorter than the watchdog's 45-minute
// stall limit, because this only decides what a row says.
const jobMovingWindow = 10 * time.Minute

// TrackerHealth is the decision, not its presentation: the page maps Key to a
// label and a colour, and the badge counts Attention.
type TrackerHealth struct {
	Key       string `json:"key"`
	Attention bool   `json:"attention"`
	// Why explains a verdict a reader would otherwise have to reconstruct from
	// timestamps — today only the superseded case uses it.
	Why string `json:"why,omitempty"`
}

// trackerHealth reads the freshest *meaningful* evidence about a tracker.
//
// The case that forced this: on 2026-11-16 the page reported rutracker as "нет
// входа" while the log showed it parsing. All three of its recorded runs said
// work_login — at 14:10, 14:45 and 14:50 — and the session store said the
// login had succeeded at **14:55:24**. Every one of those verdicts was older
// than the authorisation that disproved it, and the page had the data to see
// that and did not use it.
//
// So an authorisation failure older than AuthSince is superseded: the tracker
// has logged in since. It is deliberately not reported as "работает" either —
// no run has been observed since the login, and claiming success nobody
// measured is how the old `fetched=0 failed=0` silent zeroes read as healthy.
// It is reported as its own state, and it does not call for a human.
func trackerHealth(st TrackerState) TrackerHealth {
	if st.Disabled {
		return TrackerHealth{Key: "disabled"}
	}
	if len(st.Runs) == 0 {
		return TrackerHealth{Key: "unknown"}
	}

	run := st.Runs[0] // forTracker sorts newest first
	authFailed := run.Status == core.StatusWorkLogin

	// A sweep that is running *and has settled at least one page* outranks any
	// recorded verdict older than it. Nothing else the page knows is this
	// direct: the tracker is working while you read the row.
	//
	// PagesCompleted > 0 is load-bearing, and the first deploy of this rule
	// proved it. A restart fires the whole crontab at once, and eight seconds
	// later nineteen of twenty trackers reported `running · elapsed 8s` with no
	// pages done — IdleSeconds is 0 at that moment only because nothing has
	// settled yet, so the window test passed trivially and every row went
	// quiet, including any that was genuinely in trouble. A job that has never
	// settled a page is not evidence of anything; ops that walk no cycle at all
	// (`parse`) never report pages and so never suppress a verdict.
	if j := st.Running; j != nil && j.PagesCompleted > 0 &&
		time.Duration(j.IdleSeconds)*time.Second < jobMovingWindow {
		return TrackerHealth{Key: "running", Why: j.Summary}
	}

	if authFailed && st.AuthSince != nil && st.AuthSince.After(run.At) {
		// Prefer a later run that actually succeeded, if the store has one.
		for _, r := range st.Runs {
			if r.At.After(*st.AuthSince) && r.Ok() {
				return TrackerHealth{Key: "ok"}
			}
		}
		return TrackerHealth{
			Key: "authed-since",
			Why: "вход выполнен после последнего прогона (" +
				st.AuthSince.Local().Format("15:04:05") + ")",
		}
	}

	switch {
	case authFailed:
		return TrackerHealth{Key: "auth", Attention: true}
	case run.Status == "cf-challenge":
		return TrackerHealth{Key: "cf", Attention: true}
	case run.HTTP >= 400 || run.Error != "" || run.Status == "error":
		return TrackerHealth{Key: "error", Attention: true}
	case run.Failed > 0:
		return TrackerHealth{Key: "partial", Attention: true}
	}
	return TrackerHealth{Key: "ok"}
}

// trackerNames is the set the cron routes expose, in the order the settings
// page lists them. bitruapi deliberately has no config section of its own — it
// reads cfg.Bitru — so it takes Bitru's host here too.
// isKnownTracker reports whether name is one of the parsers. Derived from the
// same trackerNames the /trackers page is built from, so there is no second
// roster to keep in step.
func isKnownTracker(name string) bool {
	for _, t := range trackerNames {
		if t == name {
			return true
		}
	}
	return false
}

var trackerNames = []string{
	"anibelka", "anidub", "anifilm", "aniliberty", "animelayer", "anistar",
	"bitru", "bitruapi", "kinozal", "knaben", "korsars", "leproduction",
	"lostfilm", "mazepa", "megapeer", "nnmclub", "rudub", "rutor", "rutracker",
	"selezen", "subsplease", "toloka", "torrentby", "ultradox", "viruseproject",
}

// trackerSettings maps a cron route name to its config section. Explicit rather
// than reflective so a rename shows up as a compile error.
func trackerSettings(cfg app.Config, name string) app.TrackerSettings {
	switch name {
	case "anibelka":
		return cfg.Anibelka
	case "anidub":
		return cfg.Anidub
	case "anifilm":
		return cfg.Anifilm
	case "aniliberty":
		return cfg.Aniliberty
	case "animelayer":
		return cfg.Animelayer
	case "anistar":
		return cfg.Anistar
	case "bitru", "bitruapi":
		return cfg.Bitru
	case "kinozal":
		return cfg.Kinozal
	case "knaben":
		return cfg.Knaben
	case "korsars":
		return cfg.Korsars
	case "leproduction":
		return cfg.Leproduction
	case "lostfilm":
		return cfg.Lostfilm
	case "mazepa":
		return cfg.Mazepa
	case "megapeer":
		return cfg.Megapeer
	case "nnmclub":
		return cfg.NNMClub
	case "rutor":
		return cfg.Rutor
	case "rutracker":
		return cfg.Rutracker
	case "selezen":
		return cfg.Selezen
	case "rudub":
		return cfg.Rudub
	case "subsplease":
		return cfg.SubsPlease
	case "toloka":
		return cfg.Toloka
	case "torrentby":
		return cfg.TorrentBy
	case "ultradox":
		return cfg.Ultradox
	case "viruseproject":
		return cfg.Viruseproject
	}
	return app.TrackerSettings{}
}

func (s *Server) handleStatsParsers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	cfg := s.GetConfig()
	cf := core.CFAutoSnapshot()
	store := core.DefaultSessionStore()

	disabled := map[string]bool{}
	for _, d := range cfg.DisableTrackers {
		disabled[strings.ToLower(strings.TrimSpace(d))] = true
	}

	out := make([]TrackerState, 0, len(trackerNames))
	// One snapshot for the whole response: RunningJobs takes a lock, and the
	// page would otherwise ask once per tracker.
	running := map[string]TrackerJob{}
	for _, j := range core.RunningJobs() {
		if j.Cancelled != "" {
			continue
		}
		running[j.Tracker] = TrackerJob{
			Op:             j.Op,
			PagesCompleted: j.PagesDone,
			PagesTotal:     j.PagesTotal,
			Percent:        j.Percent,
			ElapsedSeconds: int(j.Elapsed.Seconds()),
			IdleSeconds:    int(j.Idle.Seconds()),
			Summary:        j.Summary,
		}
	}

	for _, name := range trackerNames {
		ts := trackerSettings(cfg, name)
		st := TrackerState{
			Name:      name,
			Host:      firstNonEmptyStr(ts.Alias, ts.Host),
			Disabled:  disabled[name],
			FetchMode: ts.FetchMode,
			Route:     "standard",
			Auth:      "none",
			Runs:      s.Runs.forTracker(name),
		}

		// Routing is decided at runtime by CF auto-detect, not by fetchmode —
		// per the same rule that governs Data/temp/cf_auto.json, config is only
		// a hint here and the registry is the authority.
		domain := core.DomainFromHost(st.Host)
		if t, ok := cf[domain]; ok {
			st.Route = "flare"
			tt := t.UTC()
			st.CFSince = &tt
		} else if strings.EqualFold(ts.FetchMode, "flaresolverr") {
			st.Route = "flare"
		}

		switch {
		case strings.TrimSpace(ts.Cookie) != "":
			st.Auth = "cookie"
		case strings.TrimSpace(ts.Login.U) != "" && strings.TrimSpace(ts.Login.P) != "":
			st.Auth = "login"
		}
		if st.Auth != "none" && domain != "" {
			// Names and timestamps only — a cookie value in a response is a
			// replayable credential, the same rule that governs the logs.
			if cookie, at := store.LoadAuth(domain); strings.TrimSpace(cookie) != "" {
				st.Auth = "session"
				if !at.IsZero() {
					tt := at.UTC()
					st.AuthSince = &tt
				}
			}
		}
		if j, ok := running[st.Name]; ok {
			st.Running = &j
		}
		st.Health = trackerHealth(st)
		out = append(out, st)
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(out), "trackers": out})
}

func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
