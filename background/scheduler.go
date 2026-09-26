package background

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Scheduler runs the jobs in the repo's `crontab` file from inside the process,
// so a deployment does not need system cron.
//
// Three decisions shape it:
//
//   - **It fires jobs over loopback HTTP, not by calling parsers directly.**
//     The run history behind /trackers is built by the recordCronRuns
//     middleware watching HTTP responses, so a scheduler that reached past it
//     would quietly empty the parser-health page. Going through the listener
//     also means a job behaves identically however it was triggered — system
//     cron, this scheduler, or a person with curl.
//
//   - **It reads the existing crontab file** rather than introducing a second
//     place to describe the schedule. That file is already tuned and already
//     in the repo; the only change when switching over is which process reads
//     it.
//
//   - **It is off by default.** The deployed system cron keeps working, and
//     turning both on would run every job twice.
//
// Only one command shape is accepted — `curl -s "<url>"` — and nothing is ever
// handed to a shell. The file is configuration, so it should not be able to
// ask the process to execute arbitrary commands.
type Scheduler struct {
	path    string
	baseURL string
	client  *http.Client
	// enabled is consulted on every tick rather than at startup, so flipping
	// `scheduler` in the settings takes effect within a minute instead of
	// needing a restart — and the page can report the real state rather than
	// whatever was true when the process launched.
	enabled func() bool

	mu      sync.Mutex
	jobs    []*job
	lastMod time.Time
	lastSz  int64
}

type job struct {
	line     int
	spec     string
	url      string
	schedule schedule
	running  atomic.Bool
	runs     atomic.Int64
	skips    atomic.Int64
}

// jobLineRe matches the one shape the crontab uses: five schedule fields, then
// a bare curl of a quoted URL.
var jobLineRe = regexp.MustCompile(`^(\S+)\s+(\S+)\s+(\S+)\s+(\S+)\s+(\S+)\s+curl\s+((?:-[a-zA-Z]+\s+)*)["']([^"']+)["']\s*$`)

// NewScheduler prepares a scheduler for the jobs in path, firing them at base
// (the process's own listen address).
func NewScheduler(path, base string, enabled func() bool) *Scheduler {
	if enabled == nil {
		enabled = func() bool { return true }
	}
	return &Scheduler{
		path:    path,
		baseURL: strings.TrimRight(base, "/"),
		enabled: enabled,
		// No timeout: a cron parser legitimately runs for hours, which is why
		// the HTTP server sets WriteTimeout to 0 as well. An in-flight job is
		// bounded by the run guard below, not by a clock.
		client: &http.Client{},
	}
}

// Run ticks once a minute, on the minute, until ctx is cancelled. It runs even
// when the scheduler is disabled, firing nothing — that is what lets the flag
// be flipped at runtime.
func (s *Scheduler) Run(ctx context.Context) {
	if err := s.reload(); err != nil {
		log.Printf("scheduler: %v", err)
	}
	wasOn := s.enabled()
	if wasOn {
		s.mu.Lock()
		n := len(s.jobs)
		s.mu.Unlock()
		log.Printf("scheduler: %d job(s) from %s, firing against %s", n, s.path, s.baseURL)
	}

	for {
		// Align to the next minute so a job written for :05 runs at :05 rather
		// than at whatever second the process happened to start.
		next := time.Now().Truncate(time.Minute).Add(time.Minute)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
		on := s.enabled()
		if on != wasOn {
			s.mu.Lock()
			n := len(s.jobs)
			s.mu.Unlock()
			if on {
				log.Printf("scheduler: enabled, %d job(s) from %s", n, s.path)
			} else {
				log.Printf("scheduler: disabled, nothing will fire until it is turned back on")
			}
			wasOn = on
		}
		if !on {
			continue
		}
		if err := s.reload(); err != nil {
			log.Printf("scheduler: reload failed, keeping the previous jobs: %v", err)
		}
		s.tick(ctx, time.Now())
	}
}

func (s *Scheduler) tick(ctx context.Context, now time.Time) {
	s.mu.Lock()
	jobs := append([]*job(nil), s.jobs...)
	s.mu.Unlock()

	for _, j := range jobs {
		if !j.schedule.matches(now) {
			continue
		}
		// A sweep can outlast its own schedule — parsealltask is scheduled
		// every few minutes while a full pass takes hours. The parser itself
		// would answer "work", but firing anyway would spend a request and put
		// a no-op in the run history for no gain.
		if !j.running.CompareAndSwap(false, true) {
			j.skips.Add(1)
			continue
		}
		go func(j *job) {
			defer j.running.Store(false)
			s.fire(ctx, j)
		}(j)
	}
}

func (s *Scheduler) fire(ctx context.Context, j *job) {
	started := time.Now()
	url := j.url
	if strings.HasPrefix(url, "/") {
		url = s.baseURL + url
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		log.Printf("scheduler: %s: %v", url, err)
		return
	}
	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down
		}
		log.Printf("scheduler: %s failed after %s: %v", url, took(started), err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	j.runs.Add(1)

	// The endpoint already logs what it did; this line exists so a failure is
	// attributable to the schedule rather than looking like a stray request.
	if resp.StatusCode >= 400 {
		log.Printf("scheduler: %s answered %d after %s: %s", url, resp.StatusCode, took(started), strings.TrimSpace(string(body)))
	}
}

func took(start time.Time) string {
	d := time.Since(start)
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// reload re-reads the file when it has changed on disk, so editing the schedule
// does not need a restart. A file that fails to parse leaves the previous jobs
// in place — losing the whole schedule over one bad line would be worse than
// running a stale one.
func (s *Scheduler) reload() error {
	st, err := os.Stat(s.path)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", s.path, err)
	}
	s.mu.Lock()
	unchanged := st.ModTime().Equal(s.lastMod) && st.Size() == s.lastSz && s.jobs != nil
	s.mu.Unlock()
	if unchanged {
		return nil
	}

	f, err := os.Open(s.path)
	if err != nil {
		return err
	}
	defer f.Close()
	jobs, warnings := parseCrontab(f)
	for _, w := range warnings {
		log.Printf("scheduler: %s", w)
	}
	if len(jobs) == 0 {
		return fmt.Errorf("%s has no usable jobs", s.path)
	}

	s.mu.Lock()
	// Carry the in-flight flag across a reload, or an edit would let a second
	// copy of a running job start.
	prev := map[string]*job{}
	for _, j := range s.jobs {
		prev[j.spec+" "+j.url] = j
	}
	for _, j := range jobs {
		if old, ok := prev[j.spec+" "+j.url]; ok && old.running.Load() {
			j.running.Store(true)
			go func(old, cur *job) {
				for old.running.Load() {
					time.Sleep(time.Second)
				}
				cur.running.Store(false)
			}(old, j)
		}
	}
	s.jobs = jobs
	s.lastMod = st.ModTime()
	s.lastSz = st.Size()
	s.mu.Unlock()
	return nil
}

// Jobs reports the loaded schedule, for diagnostics.
func (s *Scheduler) Jobs() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, map[string]any{
			"spec": j.spec, "url": j.url, "line": j.line,
			"runs": j.runs.Load(), "skipped": j.skips.Load(), "running": j.running.Load(),
		})
	}
	return out
}

func parseCrontab(r io.Reader) ([]*job, []string) {
	var jobs []*job
	var warnings []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := jobLineRe.FindStringSubmatch(line)
		if m == nil {
			warnings = append(warnings, fmt.Sprintf("line %d is not a `<schedule> curl \"<url>\"` job, ignoring: %s", n, line))
			continue
		}
		sch, err := parseSchedule(m[1:6])
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("line %d: %v, ignoring", n, err))
			continue
		}
		jobs = append(jobs, &job{
			line:     n,
			spec:     strings.Join(m[1:6], " "),
			url:      m[7],
			schedule: sch,
		})
	}
	if err := sc.Err(); err != nil {
		warnings = append(warnings, "read error: "+err.Error())
	}
	return jobs, warnings
}

// schedule is a parsed five-field cron expression, one bitset per field.
type schedule struct {
	minute [60]bool
	hour   [24]bool
	dom    [32]bool // 1..31
	month  [13]bool // 1..12
	dow    [7]bool  // 0..6, Sunday = 0
	// domRestricted/dowRestricted carry cron's oddest rule: when *both*
	// day-of-month and day-of-week are given, a day matches if *either* does.
	domRestricted bool
	dowRestricted bool
}

func parseSchedule(f []string) (schedule, error) {
	var s schedule
	if len(f) != 5 {
		return s, fmt.Errorf("want 5 fields, got %d", len(f))
	}
	if err := fillField(s.minute[:], f[0], 0); err != nil {
		return s, fmt.Errorf("minute: %w", err)
	}
	if err := fillField(s.hour[:], f[1], 0); err != nil {
		return s, fmt.Errorf("hour: %w", err)
	}
	if err := fillField(s.dom[1:], f[2], 1); err != nil {
		return s, fmt.Errorf("day of month: %w", err)
	}
	if err := fillField(s.month[1:], f[3], 1); err != nil {
		return s, fmt.Errorf("month: %w", err)
	}
	if err := fillField(s.dow[:], f[4], 0); err != nil {
		return s, fmt.Errorf("day of week: %w", err)
	}
	s.domRestricted = f[2] != "*"
	s.dowRestricted = f[4] != "*"
	return s, nil
}

// fillField parses one cron field into bits. base is the value bits[0] stands
// for, so day-of-month can be 1-based while minutes are 0-based.
func fillField(bits []bool, field string, base int) error {
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return fmt.Errorf("empty element in %q", field)
		}
		step := 1
		if i := strings.Index(part, "/"); i >= 0 {
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n <= 0 {
				return fmt.Errorf("bad step in %q", part)
			}
			step = n
			part = part[:i]
		}
		lo, hi := base, base+len(bits)-1
		if part != "*" {
			if i := strings.Index(part, "-"); i >= 0 {
				a, err1 := strconv.Atoi(strings.TrimSpace(part[:i]))
				b, err2 := strconv.Atoi(strings.TrimSpace(part[i+1:]))
				if err1 != nil || err2 != nil {
					return fmt.Errorf("bad range in %q", part)
				}
				lo, hi = a, b
			} else {
				n, err := strconv.Atoi(part)
				if err != nil {
					return fmt.Errorf("bad value %q", part)
				}
				// A bare value with a step means "from here on", as cron does:
				// 5/10 is 5,15,25,...
				lo = n
				if step == 1 {
					hi = n
				}
			}
		}
		if lo < base || hi > base+len(bits)-1 || lo > hi {
			return fmt.Errorf("%q is out of range", part)
		}
		for v := lo; v <= hi; v += step {
			bits[v-base] = true
		}
	}
	return nil
}

func (s schedule) matches(t time.Time) bool {
	if !s.minute[t.Minute()] || !s.hour[t.Hour()] || !s.month[int(t.Month())] {
		return false
	}
	dom := s.dom[t.Day()]
	dow := s.dow[int(t.Weekday())]
	switch {
	case s.domRestricted && s.dowRestricted:
		return dom || dow
	case s.domRestricted:
		return dom
	case s.dowRestricted:
		return dow
	default:
		return true
	}
}
