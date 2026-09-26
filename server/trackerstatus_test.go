package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"jacred/app"
	"jacred/core"
)

func TestCronPathParts(t *testing.T) {
	cases := map[string][2]string{
		"/cron/animelayer/parse":       {"animelayer", "parse"},
		"/cron/rutracker/parsealltask": {"rutracker", "parsealltask"},
		"/CRON/Kinozal/ParseLatest":    {"kinozal", "parselatest"},
		"/cron/animelayer/parse/":      {"animelayer", "parse"},
		"/stats/parsers":               {"", ""},
		"/cron/animelayer":             {"", ""},
		"/cron/animelayer/parse/extra": {"", ""},
		"/api/v1.0/torrents":           {"", ""},
		"/dev/fixanimelayerduplicates": {"", ""},
		"/cron//parse":                 {"", "parse"},
	}
	for path, want := range cases {
		gotT, gotOp := cronPathParts(path)
		if want[0] == "" && gotT != "" {
			t.Errorf("%s: expected no tracker, got %q", path, gotT)
			continue
		}
		if want[0] != "" && (gotT != want[0] || gotOp != want[1]) {
			t.Errorf("%s: got (%q,%q), want (%q,%q)", path, gotT, gotOp, want[0], want[1])
		}
	}
}

// The recorder reads the handler's own JSON rather than instrumenting parsers,
// so the field aliases matter: nineteen parsers report "fetched" and four report
// "parsed" for the same number.
func TestRecordCronRunsReadsTheResponse(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		code   int
		body   string
		want   TrackerRun
		wantOk bool
	}{
		{
			name: "fetched",
			path: "/cron/torrentby/parse",
			code: 200,
			body: `{"status":"ok","fetched":908,"added":891,"updated":0,"skipped":17,"failed":0}`,
			want: TrackerRun{Tracker: "torrentby", Op: "parse", HTTP: 200, Status: "ok",
				Fetched: 908, Added: 891, Skipped: 17},
			wantOk: true,
		},
		{
			name: "parsed alias",
			path: "/cron/selezen/parse",
			code: 200,
			body: `{"status":"ok","parsed":42,"added":7}`,
			want: TrackerRun{Tracker: "selezen", Op: "parse", HTTP: 200, Status: "ok",
				Fetched: 42, Added: 7},
			wantOk: true,
		},
		{
			// The string-returning ops answer {"status":"ok","text":"work"}
			// when a run is already in flight. Read literally that is a
			// successful run with zero records, and it buried the last real
			// result on /trackers — which became the common case once Parse
			// and ParseAllTask started sharing one run flag.
			name:   "skipped run reported in text",
			path:   "/cron/toloka/parsealltask",
			code:   200,
			body:   `{"status":"ok","text":"work"}`,
			want:   TrackerRun{Tracker: "toloka", Op: "parsealltask", HTTP: 200, Status: "work"},
			wantOk: true,
		},
		{
			name: "authorization failure",
			path: "/cron/animelayer/parse",
			code: 500,
			body: `{"error":"animelayer: no credentials: not authorized","status":"work_login"}`,
			want: TrackerRun{Tracker: "animelayer", Op: "parse", HTTP: 500,
				Status: core.StatusWorkLogin, Error: "animelayer: no credentials: not authorized"},
			wantOk: false,
		},
	}

	for _, c := range cases {
		s := &Server{Runs: newRunStore("")}
		h := s.recordCronRuns(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(c.code)
			_, _ = w.Write([]byte(c.body))
		}))

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))

		// The response must reach the client untouched.
		if rec.Code != c.code || rec.Body.String() != c.body {
			t.Errorf("%s: response altered: %d %q", c.name, rec.Code, rec.Body.String())
		}

		runs := s.Runs.forTracker(c.want.Tracker)
		if len(runs) != 1 {
			t.Fatalf("%s: recorded %d runs, want 1", c.name, len(runs))
		}
		got := runs[0]
		if got.Op != c.want.Op || got.HTTP != c.want.HTTP || got.Status != c.want.Status ||
			got.Error != c.want.Error || got.Fetched != c.want.Fetched ||
			got.Added != c.want.Added || got.Skipped != c.want.Skipped {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
		if got.Ok() != c.wantOk {
			t.Errorf("%s: Ok() = %v, want %v", c.name, got.Ok(), c.wantOk)
		}
		if got.At.IsZero() {
			t.Errorf("%s: run has no timestamp", c.name)
		}
	}
}

// Anything that is not a cron GET passes through without being recorded.
func TestRecordCronRunsIgnoresOtherRequests(t *testing.T) {
	s := &Server{Runs: newRunStore("")}
	h := s.recordCronRuns(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","fetched":1}`))
	}))
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/stats/parsers", nil),
		httptest.NewRequest(http.MethodPost, "/cron/rutor/parse", nil),
	} {
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if n := len(s.Runs.snapshotLocked()); n != 0 {
		t.Errorf("recorded %d runs for non-cron traffic", n)
	}
}

// The page exists to answer "is this tracker still working", so losing the
// history on every restart would defeat it.
func TestRunStoreSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	first := newRunStore(dir)
	first.record(TrackerRun{Tracker: "rutracker", Op: "parse", At: time.Now().UTC(),
		Status: "ok", HTTP: 200, Fetched: 3008, Added: 33})
	first.record(TrackerRun{Tracker: "rutracker", Op: "parselatest", At: time.Now().UTC(),
		Status: "ok", HTTP: 200, Fetched: 12})

	if _, err := readStateFile(filepath.Join(dir, "temp", "tracker_runs.json")); err != nil {
		t.Fatalf("state file not written or malformed: %v", err)
	}

	second := newRunStore(dir)
	runs := second.forTracker("rutracker")
	if len(runs) != 2 {
		t.Fatalf("reloaded %d runs, want 2", len(runs))
	}
	byOp := map[string]TrackerRun{}
	for _, r := range runs {
		byOp[r.Op] = r
	}
	if byOp["parse"].Fetched != 3008 || byOp["parselatest"].Fetched != 12 {
		t.Errorf("reloaded wrong values: %+v", byOp)
	}
}

// A later run of the same op replaces the earlier one; a different op does not.
func TestRunStoreKeepsLastRunPerOp(t *testing.T) {
	s := newRunStore("")
	s.record(TrackerRun{Tracker: "kinozal", Op: "parse", Fetched: 1})
	s.record(TrackerRun{Tracker: "kinozal", Op: "parse", Fetched: 2})
	s.record(TrackerRun{Tracker: "kinozal", Op: "parselatest", Fetched: 3})
	runs := s.forTracker("kinozal")
	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(runs))
	}
	for _, r := range runs {
		if r.Op == "parse" && r.Fetched != 2 {
			t.Errorf("parse kept %d, want the later 2", r.Fetched)
		}
	}
}

// Every cron route must map to a config section, or the page shows a tracker
// with no host and no settings.
func TestEveryTrackerNameResolvesSettings(t *testing.T) {
	cfg := app.DefaultConfig()
	for _, name := range trackerNames {
		if trackerSettings(cfg, name).Host == "" {
			t.Errorf("%s resolves no config section", name)
		}
	}
}

// readStateFile reads the persisted run file and proves it is well-formed.
func readStateFile(path string) ([]TrackerRun, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v []TrackerRun
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return v, nil
}
