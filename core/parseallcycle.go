package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ParseAllTask sweeps a tracker's task map one page at a time, and until this
// existed it decided what was still outstanding by asking "was this page
// visited *today*".
//
// That answer changes at midnight, which makes a sweep forget its own progress.
// One uninterrupted run is fine — it walks a snapshot straight through and
// never re-reads the stamps it just wrote. The damage shows when a run stops
// early and a later one resumes across the boundary: every page finished before
// midnight now reads as "not done today" and is fetched again, so the sweep
// restarts instead of continuing. Runs stop early routinely here — a restart, a
// cancelled context, or rutracker abandoning the rest of its categories when
// flaresolverr goes into cooldown.
//
// A cycle id fixes the frame of reference. A page belongs to the sweep that
// last visited it, not to a calendar day, so progress survives midnight and the
// cycle ends only when every page has actually been seen.
//
// The shape is a port of upstream's ParseAllCycleStore, including the failure
// budget, and it is deliberately additive: the two fields below are optional in
// JSON and the cycle lives in its own file, so an existing <tracker>_taskParse.json
// keeps loading unchanged.

// ParseAllFailBudget is how many consecutive failures a single page may cost
// before the cycle gives up on it.
//
// Without a budget one permanently broken page stalls the cycle forever: it
// never settles, so the cycle never completes, so it never rotates, and every
// run retries the same page. Settling it after a few tries means a tracker that
// is down for a whole pass completes an empty cycle and retries on the next
// one — a bounded loss instead of an unbounded one.
const ParseAllFailBudget = 3

// ParseAllCycle identifies one full sweep of a task map.
type ParseAllCycle struct {
	CycleID     string `json:"cycleId"`
	StartedAt   string `json:"startedAt"`
	Fingerprint string `json:"mapFingerprint,omitempty"`
	MapCount    int    `json:"mapCount,omitempty"`
}

// CycleFields is embedded into a parser's Task to carry per-page cycle state.
//
// It is an anonymous embed with no JSON tag, so encoding/json inlines these
// fields: the stored task keeps its flat shape and only gains two optional
// keys. Embedding rather than copying the fields into all twelve Task structs
// also means *Task satisfies CycleSlot through promotion, with no per-parser
// methods to write or keep in step.
type CycleFields struct {
	ParseAllCycleID   string `json:"parseAllCycleId,omitempty"`
	ParseAllFailCount int    `json:"parseAllFailCount,omitempty"`
}

func (c *CycleFields) CycleID() string      { return c.ParseAllCycleID }
func (c *CycleFields) SetCycleID(id string) { c.ParseAllCycleID = id }
func (c *CycleFields) FailCount() int       { return c.ParseAllFailCount }
func (c *CycleFields) SetFailCount(n int)   { c.ParseAllFailCount = n }

// CycleSlot is one page of a task map, as the cycle logic sees it.
type CycleSlot interface {
	CycleID() string
	SetCycleID(string)
	FailCount() int
	SetFailCount(int)
}

// ParseAllCyclePath is where a tracker's cycle checkpoint lives, next to its
// task map.
func ParseAllCyclePath(dataDir, tracker string) string {
	return filepath.Join(dataDir, "temp", tracker+"_parseAllCycle.json")
}

// NewParseAllCycle starts a cycle. The id only has to be unique against the
// previous one, so a timestamp plus the fingerprint is enough and stays
// readable in the file.
func NewParseAllCycle(fingerprint string, mapCount int) *ParseAllCycle {
	now := time.Now().UTC()
	id := fmt.Sprintf("%s-%04x", now.Format("20060102T150405"), now.UnixNano()&0xffff)
	return &ParseAllCycle{
		CycleID:     id,
		StartedAt:   now.Format(time.RFC3339),
		Fingerprint: fingerprint,
		MapCount:    mapCount,
	}
}

// MapFingerprint summarises which pages a map contains, so a run can tell that
// the map itself changed under it rather than that work got done.
func MapFingerprint(keys []string) string {
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(sum[:])
}

// LoadParseAllCycle reads the checkpoint, returning nil when there is none or
// it cannot be read. A missing or corrupt cycle is never fatal: the caller
// starts a fresh one, which costs a repeated sweep and nothing else.
func LoadParseAllCycle(path string) *ParseAllCycle {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var c ParseAllCycle
	if err := json.Unmarshal(data, &c); err != nil || strings.TrimSpace(c.CycleID) == "" {
		return nil
	}
	return &c
}

// SaveParseAllCycle writes the checkpoint through a temp file, so a run killed
// mid-write cannot leave truncated JSON where the cycle id should be.
func SaveParseAllCycle(path string, c *ParseAllCycle) error {
	if c == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// PendingInCycle reports whether a page still has to be visited in this cycle.
//
// With no cycle every page is pending, which is the safe direction: the worst
// case is one repeated sweep, never a sweep that silently skips its tail.
func PendingInCycle(s CycleSlot, c *ParseAllCycle) bool {
	if c == nil || strings.TrimSpace(c.CycleID) == "" {
		return true
	}
	return s.CycleID() != c.CycleID
}

// CountPendingInCycle is PendingInCycle over a whole map.
func CountPendingInCycle(slots []CycleSlot, c *ParseAllCycle) int {
	n := 0
	for _, s := range slots {
		if PendingInCycle(s, c) {
			n++
		}
	}
	return n
}

// BeginParseAllCycle returns the cycle a full ParseAllTask run should use, and
// how many pages it still owes.
//
// rotateIfComplete is what separates a full sweep from ParseLatest: a full run
// may start the next cycle once the current one is finished, while a partial
// task must join the cycle in progress and never rotate it — rotating there
// would reopen every page that the full sweep had already settled.
//
// updatedToday is consulted only on the very first run, to carry the old
// date-stamped progress into the first cycle. Without it, switching to cycles
// would re-fetch every page already done that day.
func BeginParseAllCycle(path, fingerprint string, slots []CycleSlot, updatedToday func(int) bool, rotateIfComplete bool) (*ParseAllCycle, int) {
	state := LoadParseAllCycle(path)
	if state == nil {
		state = NewParseAllCycle(fingerprint, len(slots))
		if updatedToday != nil {
			for i, s := range slots {
				if updatedToday(i) {
					s.SetCycleID(state.CycleID)
				}
			}
		}
		_ = SaveParseAllCycle(path, state)
		return state, CountPendingInCycle(slots, state)
	}

	state.Fingerprint = fingerprint
	state.MapCount = len(slots)

	pending := CountPendingInCycle(slots, state)
	if rotateIfComplete && pending == 0 && len(slots) > 0 {
		state = NewParseAllCycle(fingerprint, len(slots))
		pending = len(slots)
	}
	_ = SaveParseAllCycle(path, state)
	return state, pending
}

// NoteParseAllAttempt settles a page after an attempt and reports whether it is
// now done for this cycle.
//
// A success clears the failure count and settles the page. A failure settles it
// only once it has burned the budget, so a transient error is retried while a
// permanently broken page cannot hold the cycle open indefinitely.
func NoteParseAllAttempt(tracker string, s CycleSlot, c *ParseAllCycle, ok bool) bool {
	if c == nil {
		return ok
	}
	if ok {
		s.SetFailCount(0)
		s.SetCycleID(c.CycleID)
		return true
	}
	s.SetFailCount(s.FailCount() + 1)
	if s.FailCount() < ParseAllFailBudget {
		return false
	}
	s.SetFailCount(0)
	s.SetCycleID(c.CycleID)
	log.Printf("%s: parsealltask giving up on a page after %d consecutive failures", tracker, ParseAllFailBudget)
	return true
}

// CycleSlotsFromMap flattens a parser's task map into the slot list the cycle
// logic works on, plus the canonical keys its fingerprint is taken over.
//
// The slots point into the map's own backing arrays, so settling a page through
// one updates the parser's state directly — the caller only has to persist it.
//
// Order is deterministic (category, then page) because the fingerprint is taken
// over it and Go randomises map iteration.
//
// PT is the "pointer to the type parameter" form: every parser's Task embeds
// CycleFields, so *Task satisfies CycleSlot by promotion and no parser needs a
// single method of its own.
func CycleSlotsFromMap[T any, PT interface {
	*T
	CycleSlot
}](m map[string][]T, pageOf func(T) int) ([]CycleSlot, []string) {
	cats := make([]string, 0, len(m))
	for k := range m {
		cats = append(cats, k)
	}
	sort.Strings(cats)

	slots := make([]CycleSlot, 0, len(m))
	keys := make([]string, 0, len(m))
	for _, cat := range cats {
		list := m[cat]
		order := make([]int, len(list))
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(a, b int) bool { return pageOf(list[order[a]]) < pageOf(list[order[b]]) })
		for _, i := range order {
			slots = append(slots, PT(&list[i]))
			keys = append(keys, cat+"/"+strconv.Itoa(pageOf(list[i])))
		}
	}
	return slots, keys
}

// CycleSlotsFromSlice is CycleSlotsFromMap for a parser that keeps one flat
// list of pages instead of a map of categories (selezen).
func CycleSlotsFromSlice[T any, PT interface {
	*T
	CycleSlot
}](list []T, pageOf func(T) int) ([]CycleSlot, []string) {
	order := make([]int, len(list))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return pageOf(list[order[a]]) < pageOf(list[order[b]]) })

	slots := make([]CycleSlot, 0, len(list))
	keys := make([]string, 0, len(list))
	for _, i := range order {
		slots = append(slots, PT(&list[i]))
		keys = append(keys, strconv.Itoa(pageOf(list[i])))
	}
	return slots, keys
}

// CycleSlotsFromNestedMap is CycleSlotsFromMap for a parser whose map is keyed
// by category and then by a second argument (kinozal).
func CycleSlotsFromNestedMap[T any, PT interface {
	*T
	CycleSlot
}](m map[string]map[string][]T, pageOf func(T) int) ([]CycleSlot, []string) {
	cats := make([]string, 0, len(m))
	for k := range m {
		cats = append(cats, k)
	}
	sort.Strings(cats)

	var slots []CycleSlot
	var keys []string
	for _, cat := range cats {
		inner := m[cat]
		args := make([]string, 0, len(inner))
		for k := range inner {
			args = append(args, k)
		}
		sort.Strings(args)
		for _, arg := range args {
			sub, subKeys := CycleSlotsFromSlice[T, PT](inner[arg], pageOf)
			slots = append(slots, sub...)
			for _, k := range subKeys {
				keys = append(keys, cat+"/"+arg+"/"+k)
			}
		}
	}
	return slots, keys
}
