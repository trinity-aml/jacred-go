package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"jacred/core"
)

// /cron/maintenance/parseallstatus — how much of each sweep is still owed, read
// off disk.
//
// This is the other half of /health/background-jobs, and the two answer
// different questions on purpose. That endpoint lists what is running *now*,
// from process memory, so it is empty after a restart. This one reads the cycle
// checkpoints and task maps, which survive a restart, so it answers "how far
// through its map is rutracker" whether or not anything is running.
//
// # The count is derived from the files, not from the parsers
//
// Each parser keeps its task map in its own shape — a flat list (selezen), a
// map of category to pages (rutor), a nested map (kinozal) — so there is no
// shared accessor to ask. What they do share is the storage format:
// core.CycleFields is embedded with no JSON tag, so encoding/json inlines
// `parseAllCycleId` into every page object whatever the surrounding shape is.
// That is what this walks: every object carrying a "page" key is a page, and it
// belongs to the current cycle when its `parseAllCycleId` matches.
//
// So this works for all twelve without a twelfth copy of anything, and a parser
// added later is covered by existing code. The cost is a heuristic — an object
// with a "page" key that is not a task page would be miscounted — which is why
// the response reports `pages` next to `mapCount` from the checkpoint: if those
// two disagree, the walk found something it should not have.

type parseAllTrackerStatus struct {
	Tracker   string `json:"tracker"`
	CycleID   string `json:"cycleId"`
	StartedAt string `json:"startedAt,omitempty"`
	MapCount  int    `json:"mapCount"`
	Pages     int    `json:"pages"`
	Done      int    `json:"done"`
	Pending   int    `json:"pending"`
	Percent   int    `json:"percent"`
	Note      string `json:"note,omitempty"`
}

func (s *Server) handleParseAllStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "GET only"})
		return
	}
	dir := filepath.Join(s.dataDir(), "temp")
	paths, _ := filepath.Glob(filepath.Join(dir, "*_parseAllCycle.json"))
	sort.Strings(paths)

	only := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("tracker")))
	out := make([]parseAllTrackerStatus, 0, len(paths))
	for _, p := range paths {
		tracker := core.TrackerFromCyclePath(p)
		if tracker == "" || (only != "" && tracker != only) {
			continue
		}
		cycle := core.LoadParseAllCycle(p)
		if cycle == nil {
			// A missing or corrupt checkpoint means every page is pending —
			// the same direction core.PendingInCycle takes, because the worst
			// case is one repeated sweep rather than a silently skipped tail.
			out = append(out, parseAllTrackerStatus{Tracker: tracker, Note: "no readable cycle checkpoint — every page counts as pending"})
			continue
		}
		st := parseAllTrackerStatus{
			Tracker:   tracker,
			CycleID:   cycle.CycleID,
			StartedAt: cycle.StartedAt,
			MapCount:  cycle.MapCount,
		}
		pages, done, err := countCyclePages(filepath.Join(dir, tracker+"_taskParse.json"), cycle.CycleID)
		switch {
		case err != nil:
			st.Note = "task map unreadable: " + err.Error()
		default:
			st.Pages = pages
			st.Done = done
			st.Pending = pages - done
			if pages > 0 {
				st.Percent = done * 100 / pages
			}
			if cycle.MapCount > 0 && pages != cycle.MapCount {
				// Either the map changed under the cycle — which is what the
				// fingerprint exists to notice — or the walk below counted
				// something that is not a page.
				st.Note = "the map holds a different number of pages than the checkpoint recorded"
			}
		}
		out = append(out, st)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "trackers": out})
}

// countCyclePages walks a task map and counts its pages, and how many of them
// belong to cycleID.
func countCyclePages(path, cycleID string) (pages, done int, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		return 0, 0, err
	}
	walkTaskPages(doc, func(page map[string]any) {
		pages++
		if id, _ := page["parseAllCycleId"].(string); id != "" && id == cycleID {
			done++
		}
	})
	return pages, done, nil
}

// walkTaskPages visits every object that looks like a task page, whatever
// shape the surrounding map has.
func walkTaskPages(node any, visit func(map[string]any)) {
	switch v := node.(type) {
	case map[string]any:
		if _, isPage := v["page"]; isPage {
			visit(v)
			return
		}
		// Deterministic order, so a response is comparable between calls.
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			walkTaskPages(v[k], visit)
		}
	case []any:
		for _, it := range v {
			walkTaskPages(it, visit)
		}
	}
}
