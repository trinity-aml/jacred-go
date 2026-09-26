package background

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The crontab file is 100 lines of which only 59 are jobs; the rest is prose
// explaining why a tracker is polled the way it is — measured timings, cold-run
// costs, why subsplease runs hourly rather than daily. An editor that parsed
// out the jobs and wrote them back would throw all of that away on the first
// save.
//
// So the file is modelled as an ordered list of entries where anything that is
// not a job passes through verbatim, and rendering is the exact inverse of
// parsing. A round-trip with no edits must reproduce the file byte for byte —
// CrontabRoundTripsUnchanged pins that against the repo's own crontab.

// CrontabEntry is one line: either a job the UI can edit and run, or text that
// is carried through untouched.
type CrontabEntry struct {
	Line int    `json:"line"`
	Kind string `json:"kind"` // "job" or "text"

	// Text is the raw line, for Kind == "text".
	Text string `json:"text,omitempty"`

	// Job fields, for Kind == "job".
	Spec string `json:"spec,omitempty"`
	URL  string `json:"url,omitempty"`
	// Flags are the curl flags as written ("-s"), kept so a round-trip does not
	// silently rewrite someone's command.
	Flags string `json:"flags,omitempty"`
	// Disabled is a job commented out with a leading '#', which is cron's own
	// idiom for "off" and what the UI's toggle writes.
	Disabled bool `json:"disabled"`
	// Indent is the leading whitespace of the line, preserved on render.
	Indent string `json:"-"`
	// Gap is the whitespace between the schedule and the command, so the
	// file's column alignment survives an edit to a neighbour.
	Gap string `json:"-"`
}

// ParseCrontabEntries reads the whole file. Warnings name lines that look like
// they were meant to be jobs but are not usable; those lines still round-trip
// as text, so a typo is never silently deleted.
func ParseCrontabEntries(r io.Reader) ([]CrontabEntry, []string) {
	var entries []CrontabEntry
	var warnings []string

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		raw := sc.Text()
		body := strings.TrimSpace(raw)

		// A commented-out job is a disabled job, not prose. Everything the
		// repo's crontab comments are today is Russian prose that cannot parse
		// as a job, so this cannot swallow documentation.
		disabled := false
		candidate := body
		if strings.HasPrefix(body, "#") {
			candidate = strings.TrimSpace(strings.TrimPrefix(body, "#"))
			disabled = true
		}

		if e, ok := parseJobLine(candidate); ok {
			e.Line = n
			e.Disabled = disabled
			e.Indent = leadingSpace(raw)
			entries = append(entries, e)
			continue
		}

		if !disabled && body != "" {
			warnings = append(warnings, fmt.Sprintf(
				"line %d is not a `<schedule> curl \"<url>\"` job and is kept as text: %s", n, body))
		}
		entries = append(entries, CrontabEntry{Line: n, Kind: "text", Text: raw})
	}
	if err := sc.Err(); err != nil {
		warnings = append(warnings, "read error: "+err.Error())
	}
	return entries, warnings
}

func parseJobLine(s string) (CrontabEntry, bool) {
	m := jobLineRe.FindStringSubmatch(s)
	if m == nil {
		return CrontabEntry{}, false
	}
	if _, err := parseSchedule(m[1:6]); err != nil {
		return CrontabEntry{}, false
	}
	// Keep the schedule exactly as written, spacing included. The file aligns
	// its columns ("*/5 *   *   *   *"), and joining the five fields with
	// single spaces would reflow every line in the file on the first save.
	// Validation splits on whitespace, so any spacing parses the same.
	spec := strings.Join(m[1:6], " ")
	gap := "    "
	if i := strings.Index(s, "curl"); i > 0 {
		head := s[:i]
		spec = strings.TrimRight(head, " \t")
		gap = head[len(spec):]
	}
	return CrontabEntry{
		Kind:  "job",
		Spec:  spec,
		URL:   m[7],
		Flags: strings.TrimSpace(m[6]),
		Gap:   gap,
	}, true
}

func leadingSpace(s string) string {
	return s[:len(s)-len(strings.TrimLeft(s, " \t"))]
}

// RenderCrontabEntries is the exact inverse of ParseCrontabEntries.
func RenderCrontabEntries(entries []CrontabEntry) string {
	var b strings.Builder
	for _, e := range entries {
		if e.Kind != "job" {
			b.WriteString(e.Text)
			b.WriteByte('\n')
			continue
		}
		if e.Disabled {
			b.WriteString("# ")
		}
		b.WriteString(e.Indent)
		b.WriteString(strings.TrimRight(strings.TrimLeft(e.Spec, " \t"), " \t"))
		gap := e.Gap
		if gap == "" {
			gap = "    "
		}
		b.WriteString(gap)
		b.WriteString("curl")
		if f := strings.TrimSpace(e.Flags); f != "" {
			b.WriteString(" " + f)
		}
		b.WriteString(" \"" + e.URL + "\"\n")
	}
	return b.String()
}

// ValidateCrontabEntries reports everything wrong with a proposed file, so the
// UI can show all the problems at once instead of one per save.
//
// A job is refused rather than accepted-and-ignored: the scheduler skips a line
// it cannot parse, so saving a typo would silently stop that job from ever
// running again.
func ValidateCrontabEntries(entries []CrontabEntry) []string {
	var errs []string
	for i, e := range entries {
		if e.Kind != "job" {
			if strings.ContainsAny(e.Text, "\n\r") {
				errs = append(errs, fmt.Sprintf("entry %d: a text line cannot contain a line break", i+1))
			}
			continue
		}
		spec := strings.TrimSpace(e.Spec)
		fields := strings.Fields(spec)
		if len(fields) != 5 {
			errs = append(errs, fmt.Sprintf("%q: a schedule needs 5 fields, got %d", spec, len(fields)))
		} else if _, err := parseSchedule(fields); err != nil {
			errs = append(errs, fmt.Sprintf("%q: %v", spec, err))
		}

		u := strings.TrimSpace(e.URL)
		switch {
		case u == "":
			errs = append(errs, fmt.Sprintf("%q: the URL is empty", spec))
		case strings.ContainsAny(u, "\"'\n\r \t"):
			errs = append(errs, fmt.Sprintf("%q: the URL contains a quote, space or line break", spec))
		case !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "/"):
			errs = append(errs, fmt.Sprintf("%q: the URL must start with http://, https:// or /", spec))
		}

		// Flags reach a rendered line that the parser must accept again, and
		// they are never handed to a shell — but a stray quote would still
		// produce a line that no longer round-trips.
		if f := strings.TrimSpace(e.Flags); f != "" {
			for _, part := range strings.Fields(f) {
				if !strings.HasPrefix(part, "-") || strings.ContainsAny(part, "\"'") {
					errs = append(errs, fmt.Sprintf("%q: %q is not a curl flag", spec, part))
				}
			}
		}
	}
	return errs
}

// WriteCrontabEntries renders and writes the file through a temp file, so a
// crash mid-write cannot leave a truncated schedule.
func WriteCrontabEntries(path string, entries []CrontabEntry) error {
	if errs := ValidateCrontabEntries(entries); len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	body := RenderCrontabEntries(entries)

	// Re-parse what is about to be written. The file is what the scheduler
	// executes, so a render that no longer parses would disable every job in
	// it — better to refuse the save than to write it.
	back, _ := ParseCrontabEntries(strings.NewReader(body))
	if got, want := countJobs(back), countJobs(entries); got != want {
		return fmt.Errorf("refusing to write: %d job(s) would no longer parse back", want-got)
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func countJobs(entries []CrontabEntry) int {
	n := 0
	for _, e := range entries {
		if e.Kind == "job" {
			n++
		}
	}
	return n
}

// ReadCrontabFile loads the file from disk.
func ReadCrontabFile(path string) ([]CrontabEntry, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	entries, warnings := ParseCrontabEntries(f)
	return entries, warnings, nil
}

// NextRun reports when a schedule fires next after `from`, or the zero time if
// it cannot within a year (a date like Feb 30 never comes round).
func NextRun(spec string, from time.Time) time.Time {
	sch, err := parseSchedule(strings.Fields(strings.TrimSpace(spec)))
	if err != nil {
		return time.Time{}
	}
	t := from.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 366*24*60; i++ {
		if sch.matches(t) {
			return t
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}
}
