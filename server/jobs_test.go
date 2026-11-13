package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jacred/app"
	"jacred/core"
	"jacred/filedb"
	"jacred/tracks"
)

func newJobTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "Data", "temp"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := app.DefaultConfig()
	db := filedb.New(cfg, filepath.Join(dir, "Data"))
	return New(cfg, db, tracks.New(filepath.Join(dir, "Data")), "")
}

// The rule this pins predates the watchdog and is the reason every cron handler
// used to pass context.Background(): a client that hangs up must not cancel a
// running parse. For a multi-hour sweep the client hanging up is the normal
// case — the crontab drives these with `curl -s` and nothing waits for the
// body — so deriving the run context from r.Context() would cancel sweeps for a
// living.
func TestCronContextIsNotTheRequestContext(t *testing.T) {
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()

	reqCtx, disconnect := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodGet, "/cron/rutor/parsealltask", nil).WithContext(reqCtx)
	r = withCronRunCtx(r, runCtx)

	got := cronCtx(r)
	disconnect() // the client goes away mid-sweep

	if got.Err() != nil {
		t.Fatalf("the run context died with the request: %v — cronCtx must not descend from r.Context()", got.Err())
	}

	// And the watchdog's cancel still has to reach it, or the guard is inert.
	cancelRun()
	if got.Err() == nil {
		t.Error("cancelling the run context did not reach the handler's context")
	}
}

// Without a registered job the handlers must behave exactly as they did before:
// a live context that nothing cancels.
func TestCronContextFallsBackToBackground(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/cron/rutor/parse", nil)
	if ctx := cronCtx(r); ctx == nil || ctx.Err() != nil {
		t.Errorf("cronCtx with no job = %v", ctx)
	}
	if ctx := cronCtx(nil); ctx == nil || ctx.Err() != nil {
		t.Errorf("cronCtx(nil) = %v", ctx)
	}
}

// /trackers answers what the last finished run did; this answers what the
// current one is doing. Nothing could answer that before — a parser's
// {"status":"work"} says only that something holds the run flag.
func TestBackgroundJobsEndpointShowsARunningSweep(t *testing.T) {
	s := newJobTestServer(t)

	job := core.StartJob("rutracker", "parsealltask", func() {}, core.JobLimits{Stall: 45 * time.Minute})
	defer job.Finish()

	w := httptest.NewRecorder()
	s.handleHealthBackgroundJobs(w, httptest.NewRequest(http.MethodGet, "/health/background-jobs", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d", w.Code)
	}

	var body struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad JSON: %v: %s", err, w.Body.String())
	}
	var row map[string]any
	for _, j := range body.Jobs {
		if j["tracker"] == "rutracker" {
			row = j
		}
	}
	if row == nil {
		t.Fatalf("the running sweep is not listed: %s", w.Body.String())
	}
	if row["id"] != "rutracker:parsealltask" || row["job"] != "parsealltask" {
		t.Errorf("id/job = %v/%v", row["id"], row["job"])
	}
	for _, k := range []string{"startedAtUtc", "elapsedSeconds", "pagesCompleted", "pagesTotal", "idleSeconds", "summary", "stallsAtUtc"} {
		if _, ok := row[k]; !ok {
			t.Errorf("field %q missing from %v", k, row)
		}
	}
	// An op with no stall watchdog must not advertise a stall time.
	plain := core.StartJob("rutor", "parse", func() {}, core.JobLimits{})
	defer plain.Finish()
	w2 := httptest.NewRecorder()
	s.handleHealthBackgroundJobs(w2, httptest.NewRequest(http.MethodGet, "/health/background-jobs", nil))
	if err := json.Unmarshal(w2.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, j := range body.Jobs {
		if j["tracker"] == "rutor" {
			if _, ok := j["stallsAtUtc"]; ok {
				t.Errorf("an unguarded op advertises a stall deadline: %v", j)
			}
		}
	}
}

// End to end: a handler that never finishes is cancelled by the watchdog, and
// the reason reaches /trackers by name. Without the last part a cancellation
// arrives there as a bare "context canceled" on the one page whose whole
// purpose is saying why a parser stopped.
func TestAWedgedRunIsCancelledAndNamedOnTrackers(t *testing.T) {
	s := newJobTestServer(t)
	s.jobLimits = func(string) core.JobLimits {
		return core.JobLimits{Stall: 100 * time.Millisecond}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/cron/rutor/parsealltask", func(w http.ResponseWriter, r *http.Request) {
		// A parser that is wedged: it waits on its context and reports what
		// every parser reports when that context dies.
		<-cronCtx(r).Done()
		writeJSON(w, http.StatusInternalServerError,
			map[string]any{"error": "rutor: " + context.Canceled.Error(), "status": "canceled"})
	})
	h := s.recordCronRuns(mux)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/cron/rutor/parsealltask", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("HTTP %d, want 500", w.Code)
	}
	runs := s.Runs.forTracker("rutor")
	if len(runs) == 0 {
		t.Fatal("no run recorded")
	}
	last := runs[0]
	if last.Status != "canceled" {
		t.Errorf("status = %q, want canceled", last.Status)
	}
	if last.Error == "" || last.Error == "rutor: context canceled" {
		t.Errorf("recorded error = %q — the watchdog's reason did not reach /trackers", last.Error)
	}
	if !strings.Contains(last.Error, "watchdog") || !strings.Contains(last.Error, "no progress") {
		t.Errorf("recorded error = %q, want it to name the watchdog and the reason", last.Error)
	}
}
