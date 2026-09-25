package rutor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jacred/core"
)

func newTestParser(t *testing.T) *Parser {
	t.Helper()
	return &Parser{DataDir: t.TempDir(), tasks: map[string][]Task{
		"1": {{Page: 0}, {Page: 1}, {Page: 2}},
		"5": {{Page: 0}, {Page: 1}},
	}}
}

func pendingNow(p *Parser, cycle *core.ParseAllCycle) int {
	slots, _ := core.CycleSlotsFromMap[Task](p.tasks, func(t Task) int { return t.Page })
	return core.CountPendingInCycle(slots, cycle)
}

// The defect this replaced: a sweep that stopped early lost everything it had
// done as soon as the date rolled over, because "done" meant "stamped today".
// Progress is now tied to the cycle, so a later run continues.
func TestSweepResumesInsteadOfRestarting(t *testing.T) {
	p := newTestParser(t)

	p.mu.Lock()
	cycle, pending, total := p.beginCycleLocked()
	p.mu.Unlock()
	if pending != 5 || total != 5 {
		t.Fatalf("pending=%d total=%d, want 5/5", pending, total)
	}

	p.settle(cycle, "1", 0, true)
	p.settle(cycle, "1", 1, true)

	// A second run — the calendar is irrelevant now.
	p.mu.Lock()
	cycle2, pending2, _ := p.beginCycleLocked()
	p.mu.Unlock()
	if cycle2.CycleID != cycle.CycleID {
		t.Error("an unfinished sweep rotated its cycle and would start over")
	}
	if pending2 != 3 {
		t.Errorf("pending = %d, want 3 — two finished pages were forgotten", pending2)
	}
}

// Finishing every page rotates the cycle, so the map is swept again rather than
// the parser going idle forever.
func TestCompletedSweepRotates(t *testing.T) {
	p := newTestParser(t)
	p.mu.Lock()
	cycle, _, _ := p.beginCycleLocked()
	p.mu.Unlock()

	for cat, list := range p.tasks {
		for _, tk := range list {
			p.settle(cycle, cat, tk.Page, true)
		}
	}
	if n := pendingNow(p, cycle); n != 0 {
		t.Fatalf("pending = %d after settling everything", n)
	}

	p.mu.Lock()
	next, pending, _ := p.beginCycleLocked()
	p.mu.Unlock()
	if next.CycleID == cycle.CycleID {
		t.Error("a finished cycle did not rotate")
	}
	if pending != 5 {
		t.Errorf("pending = %d in the new cycle, want 5", pending)
	}
}

// A page that keeps failing must settle once its budget is spent, or it holds
// the cycle open and every run retries the same page.
func TestFailingPageSettlesAfterItsBudget(t *testing.T) {
	p := newTestParser(t)
	p.mu.Lock()
	cycle, _, _ := p.beginCycleLocked()
	p.mu.Unlock()

	for i := 0; i < core.ParseAllFailBudget-1; i++ {
		p.settle(cycle, "1", 0, false)
	}
	if pendingNow(p, cycle) != 5 {
		t.Error("a page settled before its budget was spent")
	}
	p.settle(cycle, "1", 0, false)
	if pendingNow(p, cycle) != 4 {
		t.Error("a page that exhausted its budget is still pending")
	}
}

// Success still stamps updateTime, because ParseLatest and the task map's own
// housekeeping keep reading it.
func TestSettleStillStampsUpdateTime(t *testing.T) {
	p := newTestParser(t)
	p.mu.Lock()
	cycle, _, _ := p.beginCycleLocked()
	p.mu.Unlock()

	p.settle(cycle, "1", 2, true)
	for _, tk := range p.tasks["1"] {
		if tk.Page != 2 {
			continue
		}
		if !tk.UpdatedToday() {
			t.Error("a settled page no longer reports UpdatedToday")
		}
	}
}

// Existing rutor_taskParse.json files must keep loading, and new ones must stay
// flat — the cycle lives in its own file.
func TestTaskFileStaysFlat(t *testing.T) {
	p := newTestParser(t)
	p.mu.Lock()
	cycle, _, _ := p.beginCycleLocked()
	p.mu.Unlock()
	p.settle(cycle, "1", 0, true)

	raw, err := os.ReadFile(filepath.Join(p.DataDir, "temp", "rutor_taskParse.json"))
	if err != nil {
		t.Fatal(err)
	}
	var back map[string][]Task
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("the written map no longer loads: %v", err)
	}
	if strings.Contains(string(raw), "CycleFields") {
		t.Errorf("the embed leaked a nested object:\n%s", raw)
	}

	// An old file, with no cycle keys at all, still loads.
	var old map[string][]Task
	if err := json.Unmarshal([]byte(`{"1":[{"updateTime":"2026-01-01T00:00:00Z","page":0}]}`), &old); err != nil {
		t.Fatalf("an existing task file no longer loads: %v", err)
	}
	if old["1"][0].Page != 0 || old["1"][0].ParseAllCycleID != "" {
		t.Errorf("loaded %+v", old["1"][0])
	}

	if _, err := os.Stat(core.ParseAllCyclePath(p.DataDir, trackerName)); err != nil {
		t.Errorf("no cycle checkpoint was written: %v", err)
	}
}

// Switching an existing deployment over must not re-fetch everything that was
// already done today.
func TestFirstRunAdoptsTodaysStamps(t *testing.T) {
	p := newTestParser(t)
	p.tasks["1"][0].MarkToday()
	p.tasks["5"][1].MarkToday()

	p.mu.Lock()
	_, pending, total := p.beginCycleLocked()
	p.mu.Unlock()
	if total != 5 {
		t.Fatalf("total = %d", total)
	}
	if pending != 3 {
		t.Errorf("pending = %d, want 3 — today's progress was thrown away", pending)
	}
}
