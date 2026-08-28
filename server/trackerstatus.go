package server

import (
	"bytes"
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

// recordCronRuns remembers the outcome of every /cron/<tracker>/<op> call.
//
// Until this existed, the only record of a parser run was the process log: to
// learn that kinozal had been returning zero for a week, or that animelayer was
// failing every row, someone had to ssh in and grep. Several silent breakages
// survived that long for exactly this reason.
func (s *Server) recordCronRuns(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tracker, op := cronPathParts(r.URL.Path)
		if tracker == "" || r.Method != http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		cw := &captureWriter{ResponseWriter: w}
		next.ServeHTTP(cw, r)

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
			// Nineteen parsers report "fetched", four ("anidub", "selezen",
			// "aniliberty", "animelayer") report "parsed" for the same number.
			run.Fetched = jsonInt(body, "fetched", "parsed")
			run.Added = jsonInt(body, "added")
			run.Updated = jsonInt(body, "updated")
			run.Skipped = jsonInt(body, "skipped")
			run.Failed = jsonInt(body, "failed")
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
}

// trackerNames is the set the cron routes expose, in the order the settings
// page lists them. bitruapi deliberately has no config section of its own — it
// reads cfg.Bitru — so it takes Bitru's host here too.
var trackerNames = []string{
	"anibelka", "anidub", "anifilm", "aniliberty", "animelayer", "anistar",
	"bitru", "bitruapi", "kinozal", "knaben", "korsars", "leproduction",
	"lostfilm", "mazepa", "megapeer", "nnmclub", "rutor", "rutracker",
	"selezen", "toloka", "torrentby", "ultradox", "viruseproject",
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
