package server

import (
	"context"
	"net/http"
	"time"

	"jacred/core"
)

// cronRunCtxKey carries the cancellable context the job tracker made for this
// request. It is a context value rather than a field on Server because it is
// per-request, and rather than r.Context() itself because the two must not be
// the same thing — see cronCtx.
type cronRunCtxKey struct{}

// cronCtx is the context a cron handler hands its parser.
//
// It is deliberately **not** derived from r.Context(). A curl that disconnects
// must not cancel a running parse, and for a multi-hour sweep the client
// hanging up is the normal case rather than the exception: the crontab drives
// these with `curl -s` and nothing waits for the body. That rule predates this
// file and is why every one of these handlers passed context.Background().
//
// What changed is only that Background has nothing that can ever cancel it, so
// no run had a stall guard or a deadline. This returns a context descended from
// Background whose single canceller is the watchdog in core/jobs.go. With no
// registered job — a handler reached outside the cron middleware, or a test
// calling it directly — it is context.Background(), exactly what was passed
// before.
func cronCtx(r *http.Request) context.Context {
	if r == nil {
		return context.Background()
	}
	if ctx, ok := r.Context().Value(cronRunCtxKey{}).(context.Context); ok && ctx != nil {
		return ctx
	}
	return context.Background()
}

// withCronRunCtx attaches the run context to a request.
func withCronRunCtx(r *http.Request, ctx context.Context) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), cronRunCtxKey{}, ctx))
}

// handleHealthBackgroundJobs reports the cron ops running right now.
//
// `/trackers` answers what the last finished run did; this answers what the
// current one is doing, which is the question an operator actually has during a
// multi-hour sweep and which nothing could answer before. A parser's own
// `{"status":"work"}` says only that something holds the run flag.
//
// Field names follow the original's `/health/background-jobs` so the two are
// readable with the same eyes, with `idleSeconds`, `stallsAtUtc` and
// `deadlineUtc` added: knowing a job will be cancelled in four minutes is worth
// more than knowing it has been idle for forty-one.
func (s *Server) handleHealthBackgroundJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET only"})
		return
	}
	running := core.RunningJobs()
	jobs := make([]map[string]any, 0, len(running))
	for _, j := range running {
		row := map[string]any{
			"id":             j.ID,
			"tracker":        j.Tracker,
			"job":            j.Op,
			"startedAtUtc":   j.StartedAt.Format(time.RFC3339),
			"elapsedSeconds": int(j.Elapsed.Seconds()),
			"pagesCompleted": j.PagesDone,
			"pagesTotal":     j.PagesTotal,
			"percent":        j.Percent,
			"idleSeconds":    int(j.Idle.Seconds()),
			"summary":        j.Summary,
		}
		if !j.StallsAt.IsZero() {
			row["stallsAtUtc"] = j.StallsAt.Format(time.RFC3339)
		}
		if !j.Deadline.IsZero() {
			row["deadlineUtc"] = j.Deadline.Format(time.RFC3339)
		}
		if j.Cancelled != "" {
			row["cancelled"] = j.Cancelled
		}
		jobs = append(jobs, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}
