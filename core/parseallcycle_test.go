package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// task mirrors the shape all twelve parsers use.
type task struct {
	UpdateTime string `json:"updateTime"`
	Page       int    `json:"page"`
	CycleFields
}

func slots(tasks []task) []CycleSlot {
	out := make([]CycleSlot, len(tasks))
	for i := range tasks {
		out[i] = &tasks[i]
	}
	return out
}

// The embed must stay invisible in JSON, or every existing
// <tracker>_taskParse.json would have to be migrated.
func TestEmbeddedFieldsKeepTheTaskFileFlat(t *testing.T) {
	b, err := json.Marshal(task{UpdateTime: "2026-09-25T00:00:00Z", Page: 7})
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if got != `{"updateTime":"2026-09-25T00:00:00Z","page":7}` {
		t.Errorf("a clean task serialises as %s — it must match the old shape exactly", got)
	}
	if strings.Contains(got, "CycleFields") {
		t.Error("the embed leaked a nested object into the file")
	}

	// And an old file must still load.
	var back task
	if err := json.Unmarshal([]byte(`{"updateTime":"2026-09-01T00:00:00Z","page":3}`), &back); err != nil {
		t.Fatalf("an existing task file no longer loads: %v", err)
	}
	if back.Page != 3 || back.ParseAllCycleID != "" {
		t.Errorf("loaded %+v", back)
	}

	// A stamped task round-trips through the flat shape.
	stamped := task{UpdateTime: "x", Page: 1, CycleFields: CycleFields{ParseAllCycleID: "c1", ParseAllFailCount: 2}}
	b, _ = json.Marshal(stamped)
	if !strings.Contains(string(b), `"parseAllCycleId":"c1"`) || !strings.Contains(string(b), `"parseAllFailCount":2`) {
		t.Errorf("stamped task = %s", b)
	}
}

// The whole point: progress must not depend on the calendar.
func TestProgressSurvivesAcrossRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x_parseAllCycle.json")
	tasks := make([]task, 5)
	s := slots(tasks)

	cycle, pending := BeginParseAllCycle(path, "fp", s, nil, true)
	if pending != 5 {
		t.Fatalf("pending = %d, want 5", pending)
	}

	// Two pages done, then the run stops.
	NoteParseAllAttempt("x", s[0], cycle, true)
	NoteParseAllAttempt("x", s[1], cycle, true)

	// A later run — a different day, as far as the old check was concerned.
	cycle2, pending2 := BeginParseAllCycle(path, "fp", s, nil, true)
	if cycle2.CycleID != cycle.CycleID {
		t.Error("an unfinished cycle was rotated; the sweep would restart")
	}
	if pending2 != 3 {
		t.Errorf("pending = %d, want 3 — the finished pages were forgotten", pending2)
	}
}

// Once every page has been seen the cycle rotates, so the map is swept again.
func TestCycleRotatesOnlyWhenComplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x_parseAllCycle.json")
	tasks := make([]task, 3)
	s := slots(tasks)

	cycle, _ := BeginParseAllCycle(path, "fp", s, nil, true)
	for _, sl := range s {
		NoteParseAllAttempt("x", sl, cycle, true)
	}
	if n := CountPendingInCycle(s, cycle); n != 0 {
		t.Fatalf("pending = %d after finishing every page", n)
	}

	next, pending := BeginParseAllCycle(path, "fp", s, nil, true)
	if next.CycleID == cycle.CycleID {
		t.Error("a completed cycle did not rotate; the map would never be re-swept")
	}
	if pending != 3 {
		t.Errorf("pending = %d in the new cycle, want 3", pending)
	}
}

// ParseLatest joins the running cycle and must never rotate it — rotating would
// reopen every page the full sweep had already settled.
func TestPartialRunDoesNotRotate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x_parseAllCycle.json")
	tasks := make([]task, 2)
	s := slots(tasks)

	cycle, _ := BeginParseAllCycle(path, "fp", s, nil, true)
	for _, sl := range s {
		NoteParseAllAttempt("x", sl, cycle, true)
	}
	same, pending := BeginParseAllCycle(path, "fp", s, nil, false)
	if same.CycleID != cycle.CycleID {
		t.Error("a partial run rotated the cycle")
	}
	if pending != 0 {
		t.Errorf("pending = %d, want 0", pending)
	}
}

// Switching to cycles must not throw away the day's work already recorded by
// the old date stamps.
func TestFirstRunAdoptsTodaysProgress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x_parseAllCycle.json")
	tasks := make([]task, 4)
	s := slots(tasks)

	// Pages 0 and 2 were already done today under the old scheme.
	_, pending := BeginParseAllCycle(path, "fp", s, func(i int) bool { return i == 0 || i == 2 }, true)
	if pending != 2 {
		t.Errorf("pending = %d, want 2 — today's progress was discarded", pending)
	}
}

// A page that always fails must not hold the cycle open forever.
func TestFailureBudgetSettlesABrokenPage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x_parseAllCycle.json")
	tasks := make([]task, 1)
	s := slots(tasks)
	cycle, _ := BeginParseAllCycle(path, "fp", s, nil, true)

	for i := 1; i < ParseAllFailBudget; i++ {
		if NoteParseAllAttempt("x", s[0], cycle, false) {
			t.Fatalf("settled after %d failures, before the budget was spent", i)
		}
		if !PendingInCycle(s[0], cycle) {
			t.Fatal("page stopped being pending too early")
		}
	}
	if !NoteParseAllAttempt("x", s[0], cycle, false) {
		t.Error("the page never settled; the cycle would never complete")
	}
	if PendingInCycle(s[0], cycle) {
		t.Error("a settled page is still pending")
	}
	if s[0].FailCount() != 0 {
		t.Errorf("fail count = %d after settling, want 0", s[0].FailCount())
	}
}

// A success in between clears the count, so transient errors do not accumulate
// into a page being abandoned.
func TestSuccessResetsTheFailureCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x_parseAllCycle.json")
	tasks := make([]task, 1)
	s := slots(tasks)
	cycle, _ := BeginParseAllCycle(path, "fp", s, nil, true)

	NoteParseAllAttempt("x", s[0], cycle, false)
	NoteParseAllAttempt("x", s[0], cycle, false)
	NoteParseAllAttempt("x", s[0], cycle, true)
	if s[0].FailCount() != 0 {
		t.Fatalf("fail count = %d after a success", s[0].FailCount())
	}
}

// A missing or corrupt checkpoint must make everything pending, never nothing.
func TestMissingCycleLeavesEverythingPending(t *testing.T) {
	tasks := make([]task, 3)
	s := slots(tasks)
	if n := CountPendingInCycle(s, nil); n != 3 {
		t.Errorf("pending = %d with no cycle, want 3", n)
	}
	if !PendingInCycle(s[0], &ParseAllCycle{}) {
		t.Error("an empty cycle id must not mark pages done")
	}

	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if LoadParseAllCycle(bad) != nil {
		t.Error("corrupt checkpoint was accepted")
	}
	_, pending := BeginParseAllCycle(bad, "fp", s, nil, true)
	if pending != 3 {
		t.Errorf("pending = %d after a corrupt checkpoint, want 3", pending)
	}
}

func TestCycleRoundTripsThroughDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	c := NewParseAllCycle("fp", 42)
	if err := SaveParseAllCycle(path, c); err != nil {
		t.Fatal(err)
	}
	back := LoadParseAllCycle(path)
	if back == nil || back.CycleID != c.CycleID || back.MapCount != 42 {
		t.Errorf("round trip gave %+v", back)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("the temp file was left behind")
	}
}

func TestFingerprintIsOrderIndependent(t *testing.T) {
	a := MapFingerprint([]string{"1/0", "1/1", "2/0"})
	b := MapFingerprint([]string{"2/0", "1/1", "1/0"})
	if a != b {
		t.Error("fingerprint depends on key order")
	}
	if a == MapFingerprint([]string{"1/0", "1/1"}) {
		t.Error("a different map produced the same fingerprint")
	}
}

func TestCyclePath(t *testing.T) {
	if got := ParseAllCyclePath("Data", "rutor"); got != filepath.Join("Data", "temp", "rutor_parseAllCycle.json") {
		t.Errorf("path = %q", got)
	}
}

// Slots must point into the caller's map, or settling a page would update a
// copy and the cycle would never advance.
func TestSlotsWriteThroughToTheMap(t *testing.T) {
	m := map[string][]task{
		"2": {{Page: 1}, {Page: 0}},
		"1": {{Page: 0}},
	}
	s, keys := CycleSlotsFromMap[task](m, func(t task) int { return t.Page })

	want := []string{"1/0", "2/0", "2/1"}
	if len(keys) != len(want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("keys = %v, want %v (category then page)", keys, want)
		}
	}

	cycle := NewParseAllCycle("fp", len(s))
	for _, sl := range s {
		NoteParseAllAttempt("x", sl, cycle, true)
	}
	for cat, list := range m {
		for _, tk := range list {
			if tk.ParseAllCycleID != cycle.CycleID {
				t.Errorf("%s page %d was not settled in the caller's map", cat, tk.Page)
			}
		}
	}
}

// Iteration order of a Go map is random, so the fingerprint has to be taken
// over a sorted flattening or it would change on every run.
func TestSlotOrderIsStableAcrossRuns(t *testing.T) {
	m := map[string][]task{"b": {{Page: 2}, {Page: 1}}, "a": {{Page: 3}}, "c": {{Page: 0}}}
	_, first := CycleSlotsFromMap[task](m, func(t task) int { return t.Page })
	for i := 0; i < 20; i++ {
		_, again := CycleSlotsFromMap[task](m, func(t task) int { return t.Page })
		if MapFingerprint(first) != MapFingerprint(again) {
			t.Fatal("fingerprint changed between identical runs")
		}
	}
}
