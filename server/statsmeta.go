package server

import (
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// /stats/meta — how old the numbers are.
//
// Every stats and health surface here is served from a file that a timer
// rebuilds: `/stats/torrents` reads `stats.json`, `/trackers` reads
// `tracker_runs.json`, the CF routing shown on both comes from `cf_auto.json`.
// None of them says when it was written, so a stale figure and a wrong one look
// the same — and the one question worth asking first, when a number looks off,
// is whether it is simply old. `stats.json` is refreshed on a timer and by
// `/stats/refresh`, so "three hours stale" is a complete explanation on its own.
//
// Nothing here is derived from the database, only from file metadata, so it
// costs no index walk and is safe to poll.

type statsMetaFile struct {
	Name       string `json:"name"`
	Exists     bool   `json:"exists"`
	ModifiedAt string `json:"modifiedAt,omitempty"`
	AgeSeconds int    `json:"ageSeconds,omitempty"`
	Bytes      int64  `json:"bytes,omitempty"`
	Serves     string `json:"serves"`
	Error      string `json:"error,omitempty"`
}

// statsMetaArtefacts are the derived files, each named with what reads it —
// a bare filename tells an operator nothing about which page went stale.
var statsMetaArtefacts = []struct{ file, serves string }{
	{"stats.json", "/stats/torrents, /stats and the stats page"},
	{"tracker_runs.json", "/stats/parsers and the /trackers page"},
	{"cf_auto.json", "/admin/cf-domains and the route column on /trackers"},
	{"tracker_runs.json.tmp", "a leftover temp file; its presence means a write was interrupted"},
}

func (s *Server) handleStatsMeta(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET only"})
		return
	}
	dir := filepath.Join(s.dataDir(), "temp")
	now := time.Now()

	files := make([]statsMetaFile, 0, len(statsMetaArtefacts))
	for _, a := range statsMetaArtefacts {
		row := statsMetaFile{Name: a.file, Serves: a.serves}
		info, err := os.Stat(filepath.Join(dir, a.file))
		switch {
		case err == nil:
			row.Exists = true
			row.ModifiedAt = info.ModTime().UTC().Format(time.RFC3339)
			row.AgeSeconds = int(now.Sub(info.ModTime()).Seconds())
			row.Bytes = info.Size()
		case os.IsNotExist(err):
			// Absent is normal for several of these — a box that has never
			// met a Cloudflare challenge has no cf_auto.json — so it is not
			// an error, just a fact.
		default:
			row.Error = err.Error()
		}
		files = append(files, row)
	}

	out := map[string]any{
		"now":          now.UTC().Format(time.RFC3339),
		"lastupdatedb": s.DB.LastUpdateDB(),
		"files":        files,
	}
	// The stats refresh interval, so an age can be read against what it should
	// be rather than against a guess.
	if m := s.GetConfig().TimeStatsUpdate; m > 0 {
		out["statsRefreshMinutes"] = m
	}
	writeJSON(w, http.StatusOK, out)
}
