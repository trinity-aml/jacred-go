package toloka

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"jacred/app"
	"jacred/core"
	"jacred/filedb"
)

const trackerName = "toloka"

// errNoCredentials separates "login is not configured" from "login was attempted
// and did not work", so the reported status can tell them apart too.
var errNoCredentials = fmt.Errorf("toloka: login credentials missing: %w", core.ErrNotAuthorized)

// statusForError is deliberately thin: the cron handler derives the status from
// the error itself (core.ErrNotAuthorized → core.StatusWorkLogin), so all this
// has to do is stop res.Status from staying "ok" on a failed run.
func statusForError(err error) string {
	if errors.Is(err, core.ErrNotAuthorized) {
		return core.StatusWorkLogin
	}
	return "error"
}

var parseCats = []string{"16", "96", "19", "139", "32", "173", "174", "44"}
var taskCats = []string{
	"16", "32", "19", "44", "127",
	"84", "42", "124", "125",
	"96", "173", "139", "174", "140",
	"12", "131", "230", "226", "227", "228", "229",
	"132",
}

var (
	rowSplitRe      = regexp.MustCompile(`</tr>`)
	cleanSpaceRe    = regexp.MustCompile(`[\n\r\t\x{00A0} ]+`)
	maxPagesRe      = regexp.MustCompile(`>([0-9]+)</a>&nbsp;&nbsp;<a href="[^"]+">наступна</a>`)
	createTimeRe    = regexp.MustCompile(`class="postdetails">([0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2})`)
	topicURLRe      = regexp.MustCompile(`<a href="(t[0-9]+)" class="topictitle"`)
	titleRe         = regexp.MustCompile(`class="topictitle">([^<]+)</a>`)
	sidRe           = regexp.MustCompile(`<span class="seedmed" [^>]+><b>([0-9]+)</b></span>`)
	pirRe           = regexp.MustCompile(`<span class="leechmed" [^>]+><b>([0-9]+)</b></span>`)
	sizeNameRe      = regexp.MustCompile(`<a href="download\.php[^"]+" [^>]+>([^<]+)</a>`)
	downloadIDRe    = regexp.MustCompile(`href="download\.php\?id=([0-9]+)"`)
	firstNamePartRe = regexp.MustCompile(`(\[|/|\(|\|)`)

	movieMainRe     = regexp.MustCompile(`^([^/\(\[]+)/[^/\(\[]+/([^/\(\[]+) \(([0-9]{4})(\)|-)`)
	movieShortRe    = regexp.MustCompile(`^([^/\(\[]+)/([^/\(\[]+) \(([0-9]{4})(\)|-)`)
	movieSeriesRe   = regexp.MustCompile(`^([^/\(\[]+) \([^\)]+\) \(([0-9]{4})(\)|-)`)
	movieSingleRe   = regexp.MustCompile(`^([^/\(\[]+) \(([0-9]{4})(\)|-)`)
	serialLongRe    = regexp.MustCompile(`^([^/\(\[]+) \([^\)]+\) \([^\)]+\) ?/([^/\(\[]+) \([^\)]+\) \(([0-9]{4})(\)|-)`)
	serialBasicRe   = regexp.MustCompile(`^([^/\(\[]+) \([^\)]+\) ?/([^/\(\[]+) \([^\)]+\) \(([0-9]{4})(\)|-)`)
	serialBracketRe = regexp.MustCompile(`^([^/\(\[]+) (\(|\[)[^\)\]]+(\)|\]) ?/([^/\(\[]+) \(([0-9]{4})(\)|-)`)
	serialSlashRe   = regexp.MustCompile(`^([^/\(\[]+)/([^/\(\[]+) \([^\)]+\) \(([0-9]{4})(\)|-)`)
	serialTriRe     = regexp.MustCompile(`^([^/\(\[]+)/[^/\(\[]+/([^/\(\[]+) \([^\)]+\) \(([0-9]{4})(\)|-)`)
	serialRusOnlyRe = regexp.MustCompile(`^([^/\(\[]+) \([^\)]+\) \(([0-9]{4})(\)|-)`)

	inlineReB76eb1Re = regexp.MustCompile(`toloka_sid=([^;]+)(;|$)`)
	inlineReB96ce0Re = regexp.MustCompile(`(?i)Збір коштів`)
	inlineReEbc614Re = regexp.MustCompile(`toloka_data=([^;]+)(;|$)`)
	useridRe         = regexp.MustCompile(`"userid";i:(-?\d+)`)
)

type Task struct {
	UpdateTime string `json:"updateTime"`
	Page       int    `json:"page"`
	// Cycle bookkeeping; embedded so the stored map stays flat.
	core.CycleFields
}

type parseItem struct {
	Torrent    filedb.TorrentDetails
	DownloadID string
}

type ParseResult struct {
	Status      string         `json:"status"`
	Fetched     int            `json:"fetched"`
	Added       int            `json:"added"`
	Updated     int            `json:"updated"`
	Skipped     int            `json:"skipped"`
	Failed      int            `json:"failed"`
	PerCategory map[string]int `json:"by_category"`
}

type Parser struct {
	Config  app.Config
	DB      *filedb.DB
	DataDir string
	Fetcher *core.Fetcher
	loc     *time.Location

	mu               sync.Mutex
	working          bool
	allWork          bool
	latestMu         sync.Mutex
	tasks            map[string][]Task
	cookieMu         sync.Mutex
	cookie           string
	lastLoginAttempt time.Time
	sessionRejects   int
	loginUA          string
	domain           string
}

func (t Task) UpdatedToday(loc *time.Location) bool {
	tm := parseTaskTime(t.UpdateTime, loc)
	if tm.IsZero() {
		return false
	}
	if loc == nil {
		loc = time.Local
	}
	now := time.Now().In(loc)
	y1, m1, d1 := tm.In(loc).Date()
	y2, m2, d2 := now.Date()
	return y1 == y2 && m1 == m2 && d1 == d2
}

func (t *Task) MarkToday(loc *time.Location) {
	if loc == nil {
		loc = time.Local
	}
	now := time.Now().In(loc)
	t.UpdateTime = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).Format("2006-01-02T15:04:05Z07:00")
}

func (p *Parser) taskFilePath() string {
	return filepath.Join(p.DataDir, "temp", "toloka_taskParse.json")
}

func (p *Parser) loadTasks() error {
	path := p.taskFilePath()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			p.tasks = map[string][]Task{}
			return nil
		}
		return err
	}
	tasks := map[string][]Task{}
	if err := json.Unmarshal(b, &tasks); err != nil {
		return err
	}
	p.tasks = tasks
	return nil
}

func (p *Parser) saveTasksLocked() error {
	path := p.taskFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(p.tasks)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func (p *Parser) saveTasks() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tasks == nil {
		p.tasks = map[string][]Task{}
	}
	return p.saveTasksLocked()
}

func cloneTasks(in map[string][]Task) map[string][]Task {
	out := make(map[string][]Task, len(in))
	for k, v := range in {
		vv := make([]Task, len(v))
		copy(vv, v)
		out[k] = vv
	}
	return out
}

func (p *Parser) markTask(cat string, page int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tasks == nil {
		p.tasks = map[string][]Task{}
	}
	list := p.tasks[cat]
	for i := range list {
		if list[i].Page == page {
			list[i].MarkToday(p.loc)
			p.tasks[cat] = list
			return
		}
	}
	t := Task{Page: page}
	t.MarkToday(p.loc)
	p.tasks[cat] = append(list, t)
	sort.Slice(p.tasks[cat], func(i, j int) bool { return p.tasks[cat][i].Page < p.tasks[cat][j].Page })
}

func parseTaskTime(s string, loc *time.Location) time.Time {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "0001-01-01") {
		return time.Time{}
	}
	if loc == nil {
		loc = time.Local
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04:05Z07:00"} {
		if tm, err := time.ParseInLocation(layout, s, loc); err == nil {
			return tm.In(loc)
		}
	}
	return time.Time{}
}

func isDisabled(list []string, tracker string) bool {
	for _, item := range list {
		if strings.EqualFold(strings.TrimSpace(item), strings.TrimSpace(tracker)) {
			return true
		}
	}
	return false
}

func New(cfg app.Config, db *filedb.DB, dataDir string) *Parser {
	loc, err := time.LoadLocation("Asia/Jerusalem")
	if err != nil || loc == nil {
		loc = time.FixedZone("+0200", 2*3600)
	}
	p := &Parser{Config: cfg, DB: db, DataDir: dataDir, Fetcher: core.NewFetcher(cfg), loc: loc, tasks: map[string][]Task{}, domain: core.DomainFromHost(cfg.Toloka.Host)}
	_ = p.loadTasks()
	if saved, _ := core.DefaultSessionStore().LoadAuth(p.domain); saved != "" {
		p.cookie = saved
		log.Printf("toloka: loaded saved cookie from disk")
	}
	return p
}

func (p *Parser) Parse(ctx context.Context, page int) (ParseResult, error) {
	p.mu.Lock()
	if p.working {
		p.mu.Unlock()
		return ParseResult{Status: "work"}, nil
	}
	p.working = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.working = false; p.mu.Unlock() }()

	if isDisabled(p.Config.DisableTrackers, trackerName) {
		return ParseResult{Status: "disabled"}, nil
	}
	if strings.TrimSpace(p.Config.Toloka.Host) == "" {
		return ParseResult{Status: "config missing"}, nil
	}
	res := ParseResult{Status: "ok", PerCategory: map[string]int{}}
	for i, cat := range parseCats {
		if i > 0 {
			delay := time.Duration(p.Config.Toloka.ParseDelay) * time.Millisecond
			if delay <= 0 {
				delay = 2 * time.Second
			}
			select {
			case <-ctx.Done():
				res.Status = "canceled"
				return res, ctx.Err()
			case <-time.After(delay):
			}
		}
		items, err := p.parsePage(ctx, cat, page)
		if err != nil {
			res.Status = statusForError(err)
			return res, err
		}
		res.Fetched += len(items)
		res.PerCategory[cat] = len(items)
		if len(items) == 0 {
			continue
		}
		a, u, s, f, err := p.saveTorrents(ctx, items)
		if err != nil {
			res.Status = statusForError(err)
			return res, err
		}
		res.Added += a
		res.Updated += u
		res.Skipped += s
		res.Failed += f
		log.Printf("toloka: cat=%s fetched=%d added=%d skipped=%d failed=%d", cat, len(items), a, s, f)
	}
	log.Printf("toloka: done fetched=%d added=%d skipped=%d failed=%d", res.Fetched, res.Added, res.Skipped, res.Failed)
	return res, nil
}

func (p *Parser) UpdateTasksParse(ctx context.Context) (map[string][]Task, error) {
	if _, err := p.ensureCookie(ctx); err != nil {
		_ = p.saveTasks()
		return nil, err
	}
	p.mu.Lock()
	if p.tasks == nil {
		p.tasks = map[string][]Task{}
	}
	p.mu.Unlock()
	for _, cat := range taskCats {
		htmlBody, err := p.fetchPageHTML(ctx, cat, 0)
		if err != nil || htmlBody == "" {
			continue
		}
		maxPages := 0
		if m := maxPagesRe.FindStringSubmatch(htmlBody); len(m) > 1 {
			maxPages, _ = strconv.Atoi(strings.TrimSpace(m[1]))
		}
		p.mu.Lock()
		existing := p.tasks[cat]
		pagesMap := map[int]Task{}
		for _, t := range existing {
			pagesMap[t.Page] = t
		}
		for page := 0; page <= maxPages; page++ {
			if _, ok := pagesMap[page]; !ok {
				pagesMap[page] = Task{Page: page, UpdateTime: "0001-01-01T00:00:00"}
			}
		}
		merged := make([]Task, 0, len(pagesMap))
		for _, t := range pagesMap {
			merged = append(merged, t)
		}
		sort.Slice(merged, func(i, j int) bool { return merged[i].Page < merged[j].Page })
		// The map was additive only, so a category that shrank kept its old
		// slots and ParseAllTask re-fetched each one every sweep. maxPages is
		// what this loop just read off the live pager; when it could not be
		// read it is 0 and PruneTaskPages deliberately changes nothing.
		merged, pruned := core.PruneTaskPages(merged, func(t Task) int { return t.Page }, maxPages)
		if pruned > 0 {
			log.Printf("toloka: updatetasksparse cat=%s maxPage=%d pruned=%d remaining=%d", cat, maxPages, pruned, len(merged))
		}
		p.tasks[cat] = merged
		p.mu.Unlock()
	}
	if err := p.saveTasks(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return cloneTasks(p.tasks), nil
}

// beginCycleLocked opens (or rotates) the sweep cycle. Caller holds p.mu.
func (p *Parser) beginCycleLocked() (*core.ParseAllCycle, int, int) {
	slots, keys := core.CycleSlotsFromMap[Task](p.tasks, func(t Task) int { return t.Page })
	cycle, pending := core.BeginParseAllCycle(
		core.ParseAllCyclePath(p.DataDir, trackerName),
		core.MapFingerprint(keys),
		slots,
		func(i int) bool { return slots[i].(*Task).UpdatedToday(p.loc) },
		true,
	)
	if pending != len(slots) {
		_ = p.saveTasksLocked()
	}
	return cycle, pending, len(slots)
}

// settle records one page's outcome. A failure spends its budget instead of
// settling the page, so a transient error is retried while a permanently
// broken page cannot hold the cycle open forever.
func (p *Parser) settle(cycle *core.ParseAllCycle, key string, page int, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if list, found := p.tasks[key]; found {
		for i := range list {
			if list[i].Page == page {
				if core.NoteParseAllAttempt(trackerName, &list[i], cycle, ok) && ok {
					list[i].MarkToday(p.loc)
				}
			}
		}
		p.tasks[key] = list
	}
	_ = p.saveTasksLocked()
}

func (p *Parser) ParseAllTask(ctx context.Context, force bool) (string, error) {
	p.mu.Lock()
	if p.allWork {
		p.mu.Unlock()
		return "work", nil
	}
	p.allWork = true
	snapshot := cloneTasks(p.tasks)
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.allWork = false; p.mu.Unlock() }()

	if len(snapshot) == 0 {
		log.Printf("toloka: parsealltask — tasks empty, running updatetasksparse first")
		if _, err := p.UpdateTasksParse(ctx); err != nil {
			return "", err
		}
		p.mu.Lock()
		snapshot = cloneTasks(p.tasks)
		p.mu.Unlock()
	}

	totalPages := 0
	for _, list := range snapshot {
		totalPages += len(list)
	}
	// Opened against p.tasks rather than the snapshot: the first run stamps
	// the day's existing progress into the cycle, and that must be persisted
	// before the snapshot is taken.
	p.mu.Lock()
	cycle, pendingAtStart, cycleTotal := p.beginCycleLocked()
	snapshot = cloneTasks(p.tasks)
	p.mu.Unlock()

	processed, fetched, added, updated, skipped, failed, errs := 0, 0, 0, 0, 0, 0, 0
	for cat, list := range snapshot {
		for _, task := range list {
			if !force && !core.PendingInCycle(&task, cycle) {
				skipped++
				continue
			}
			if p.Config.Toloka.ParseDelay > 0 {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(time.Duration(p.Config.Toloka.ParseDelay) * time.Millisecond):
				}
			}
			items, err := p.parsePage(ctx, cat, task.Page)
			if err != nil {
				log.Printf("toloka: parsealltask cat=%s page=%d error: %v", cat, task.Page, err)
				p.settle(cycle, cat, task.Page, false)
				errs++
				continue
			}
			processed++
			if len(items) == 0 {
				log.Printf("toloka: parsealltask cat=%s page=%d empty (marking today)", cat, task.Page)
				p.settle(cycle, cat, task.Page, true)
				_ = p.saveTasks()
				continue
			}
			a, u, s, f, err := p.saveTorrents(ctx, items)
			if err != nil {
				log.Printf("toloka: parsealltask cat=%s page=%d save error: %v", cat, task.Page, err)
				errs++
				continue
			}
			fetched += len(items)
			added += a
			updated += u
			skipped += s
			failed += f
			log.Printf("toloka: parsealltask cat=%s page=%d fetched=%d added=%d skipped=%d failed=%d", cat, task.Page, len(items), a, s, f)
			p.settle(cycle, cat, task.Page, true)
			if err := p.saveTasks(); err != nil {
				return "", err
			}
		}
	}
	log.Printf("toloka: parsealltask done processed=%d/%d fetched=%d added=%d updated=%d skipped=%d failed=%d errors=%d", processed, totalPages, fetched, added, updated, skipped, failed, errs)
	p.mu.Lock()
	cycleSlots, _ := core.CycleSlotsFromMap[Task](p.tasks, func(t Task) int { return t.Page })
	pendingLeft := core.CountPendingInCycle(cycleSlots, cycle)
	p.mu.Unlock()
	log.Printf("toloka: parsealltask cycle=%s pending=%d->%d/%d", cycle.CycleID, pendingAtStart, pendingLeft, cycleTotal)
	return "ok", nil
}

func (p *Parser) ParseLatest(ctx context.Context, pages int) (string, error) {
	if !p.latestMu.TryLock() {
		return "work", nil
	}
	defer p.latestMu.Unlock()
	if pages <= 0 {
		pages = 5
	}
	p.mu.Lock()
	snapshot := cloneTasks(p.tasks)
	p.mu.Unlock()
	if len(snapshot) == 0 {
		if _, err := p.UpdateTasksParse(ctx); err != nil {
			return "", err
		}
		p.mu.Lock()
		snapshot = cloneTasks(p.tasks)
		p.mu.Unlock()
	}
	var lines []string
	processed, fetched, added, updated, skipped, failed, errs := 0, 0, 0, 0, 0, 0, 0
	for cat, list := range snapshot {
		sort.Slice(list, func(i, j int) bool { return list[i].Page < list[j].Page })
		if len(list) > pages {
			list = list[:pages]
		}
		for _, task := range list {
			if p.Config.Toloka.ParseDelay > 0 {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(time.Duration(p.Config.Toloka.ParseDelay) * time.Millisecond):
				}
			}
			items, err := p.parsePage(ctx, cat, task.Page)
			if err != nil {
				log.Printf("toloka: parselatest cat=%s page=%d error: %v", cat, task.Page, err)
				errs++
				continue
			}
			processed++
			if len(items) == 0 {
				log.Printf("toloka: parselatest cat=%s page=%d empty (marking today)", cat, task.Page)
				p.markTask(cat, task.Page)
				_ = p.saveTasks()
				continue
			}
			a, u, s, f, err := p.saveTorrents(ctx, items)
			if err != nil {
				log.Printf("toloka: parselatest cat=%s page=%d save error: %v", cat, task.Page, err)
				errs++
				continue
			}
			fetched += len(items)
			added += a
			updated += u
			skipped += s
			failed += f
			log.Printf("toloka: parselatest cat=%s page=%d fetched=%d added=%d skipped=%d failed=%d", cat, task.Page, len(items), a, s, f)
			p.markTask(cat, task.Page)
			if err := p.saveTasks(); err != nil {
				return "", err
			}
			lines = append(lines, fmt.Sprintf("%s - %d", cat, task.Page))
		}
	}
	log.Printf("toloka: parselatest done processed=%d fetched=%d added=%d updated=%d skipped=%d failed=%d errors=%d", processed, fetched, added, updated, skipped, failed, errs)
	if len(lines) == 0 {
		return "ok", nil
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func (p *Parser) parsePage(ctx context.Context, cat string, page int) ([]parseItem, error) {
	if _, err := p.ensureCookie(ctx); err != nil {
		return nil, err
	}
	htmlBody, err := p.fetchPageHTML(ctx, cat, page)
	if err != nil {
		return nil, err
	}
	if htmlBody == "" {
		return nil, nil
	}
	// Expired-cookie detection: when the session cookie no longer authenticates,
	// toloka serves /login.php in place of the forum view. The login page is
	// also <html lang="uk">, so a language-tag check is not enough — match the
	// form's action attribute (name="username"/"password" inputs only appear
	// on /login.php, never on a listing). The previous lang-only check missed
	// expiry and the parser kept fetching 0 rows until config reload.
	if looksLikeTolokaLoginForm(htmlBody) {
		p.cookieMu.Lock()
		p.sessionRejects++
		n := p.sessionRejects
		p.cookieMu.Unlock()
		log.Printf("toloka: cat=%s page=%d returned login form (session refused, %d/%d)", cat, page, n, maxSessionRejects)
		p.logSessionRejection(cat, page)
		p.invalidateCookie()
		// A session the site refuses on the very next request after issuing it
		// will not be fixed by logging in again, and each attempt is another
		// credentials POST. Unbounded, this is one login per category: measured
		// in production 2026-09-26 as four logins in 45 seconds, each reporting
		// success. Repeated login attempts are how a tracker's anti-bruteforce
		// gets triggered — that is how rutracker ended up showing a CAPTCHA.
		if n >= maxSessionRejects {
			return nil, fmt.Errorf("toloka: the site refused a freshly created session %d times — "+
				"logging in again will not help, a phpBB session is bound to the User-Agent that created it "+
				"and the pages are fetched by the flaresolverr browser: %w", n, core.ErrNotAuthorized)
		}
		return nil, nil
	}
	if !strings.Contains(htmlBody, `<html lang="uk"`) {
		// Body is neither the login form nor a Ukrainian forum view — toloka
		// probably returned an error/maintenance page. Don't drop the cookie
		// (it may still be valid), just skip this page.
		log.Printf("toloka: cat=%s page=%d unexpected body (bodyLen=%d, not login form, not uk forum)", cat, page, len(htmlBody))
		return nil, nil
	}
	// The session was accepted, so a later refusal starts its count over.
	p.cookieMu.Lock()
	p.sessionRejects = 0
	p.cookieMu.Unlock()
	return parsePageHTML(strings.TrimRight(p.Config.Toloka.Host, "/"), cat, htmlBody), nil
}

// maxSessionRejects bounds the re-login loop described in parsePage.
const maxSessionRejects = 2

// looksLikeTolokaLoginForm returns true when the response is /login.php
// rendered in place of the requested listing. Matches the login form's
// signature: a form posting to login.php with username/password inputs.
// Listing pages never contain this combination.
func looksLikeTolokaLoginForm(htmlBody string) bool {
	if !strings.Contains(htmlBody, `action="login.php"`) &&
		!strings.Contains(htmlBody, `action='login.php'`) {
		return false
	}
	return strings.Contains(htmlBody, `name="username"`) &&
		strings.Contains(htmlBody, `name="password"`)
}

func matchDecode(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(cleanText(m[1]))
}

func cleanText(s string) string {
	s = html.UnescapeString(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "\u00a0", " ")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = strings.ReplaceAll(s, "&#160;", " ")
	return strings.TrimSpace(cleanSpaceRe.ReplaceAllString(s, " "))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if v != "" {
			return v
		}
	}
	return ""
}

func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func fileTime(td filedb.TorrentDetails) time.Time {
	for _, key := range []string{"updateTime", "createTime"} {
		if s := strings.TrimSpace(asString(td[key])); s != "" {
			for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
				if tm, err := time.Parse(layout, s); err == nil {
					return tm.UTC()
				}
			}
		}
	}
	return time.Now().UTC()
}

// defaultUA returns the User-Agent for standard-mode requests. Empty means
// "let Fetcher pick", which is the right default: core.defaultUserAgent is
// pinned to the tls-client's Chrome impersonation profile, and the two have to
// agree. What used to sit here was a truncated `Mozilla/5.0 (Windows NT 10.0;
// Win64; x64) AppleWebKit/537.36` — no Chrome token at all, next to a Chrome
// ClientHello. It was invisible while toloka ran through flaresolverr, because
// the flare path uses the browser's UA and ignores this one (see GetExt), but
// every standard-mode request carried the contradiction.
func defaultUA() string { return "" }

func parsePageHTML(host, cat, htmlBody string) []parseItem {
	rows := rowSplitRe.Split(replaceBadNames(htmlBody), -1)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	out := make([]parseItem, 0, len(rows))
	for _, row := range rows[1:] {
		if strings.TrimSpace(row) == "" || inlineReB96ce0Re.MatchString(row) {
			continue
		}
		createRaw := matchDecode(createTimeRe, row)
		createRaw = strings.ReplaceAll(createRaw, "-", ".")
		createTime, err := time.ParseInLocation("2006.01.02 15:04", createRaw, time.UTC)
		if err != nil || createTime.IsZero() {
			continue
		}
		urlPath := matchDecode(topicURLRe, row)
		title := matchDecode(titleRe, row)
		sidRaw := matchDecode(sidRe, row)
		pirRaw := matchDecode(pirRe, row)
		sizeName := strings.ReplaceAll(matchDecode(sizeNameRe, row), "&nbsp;", " ")
		downloadID := matchDecode(downloadIDRe, row)
		if urlPath == "" || title == "" || sidRaw == "" || pirRaw == "" || strings.TrimSpace(sizeName) == "" || sizeName == "0 B" || downloadID == "" {
			continue
		}
		name, original, relased := parseTitle(cat, title)
		if strings.TrimSpace(name) == "" {
			name = fallbackName(title)
		}
		types := typesForCategory(cat)
		if len(types) == 0 || strings.TrimSpace(name) == "" {
			continue
		}
		sid, _ := strconv.Atoi(sidRaw)
		pir, _ := strconv.Atoi(pirRaw)
		td := filedb.TorrentRecord{
			TrackerName:  trackerName,
			Types:        types,
			URL:          host + "/" + strings.TrimLeft(urlPath, "/"),
			Title:        title,
			Sid:          sid,
			Pir:          pir,
			SizeName:     strings.TrimSpace(sizeName),
			CreateTime:   createTime.UTC().Format(time.RFC3339Nano),
			UpdateTime:   now,
			Name:         strings.TrimSpace(name),
			OriginalName: strings.TrimSpace(original),
			Relased:      relased,
			SearchName:   core.SearchName(name),
			SearchOrig:   core.SearchName(firstNonEmpty(original, name)),
		}.ToMap()
		out = append(out, parseItem{Torrent: td, DownloadID: downloadID})
	}
	return out
}

func (p *Parser) saveTorrents(ctx context.Context, items []parseItem) (int, int, int, int, error) {
	added, updated, skipped, failed := 0, 0, 0, 0
	plog := core.NewParserLog(trackerName, filepath.Join(p.DB.DataDir, "log"), p.Config.LogParsers && p.Config.Toloka.Log)
	bucketCache := make(map[string]map[string]filedb.TorrentDetails, len(items))
	changed := make(map[string]time.Time, len(items))
	cookie, err := p.ensureCookie(ctx)
	if err != nil {
		return added, updated, skipped, failed, err
	}
	for _, item := range items {
		incoming := item.Torrent
		key := p.DB.KeyDb(asString(incoming["name"]), asString(incoming["originalname"]))
		if strings.TrimSpace(key) == "" || key == ":" {
			skipped++
			continue
		}
		bucket, ok := bucketCache[key]
		if !ok {
			loaded, err := p.DB.OpenReadOrEmpty(key)
			if err != nil {
				return added, updated, skipped, failed, err
			}
			bucket = loaded
			bucketCache[key] = bucket
		}
		urlv := asString(incoming["url"])
		if strings.TrimSpace(urlv) == "" {
			skipped++
			continue
		}
		existing, exists := bucket[urlv]
		// Only resolve magnet if title changed or existing has no magnet
		needMagnet := strings.TrimSpace(asString(incoming["magnet"])) == "" &&
			(!exists || strings.TrimSpace(asString(existing["title"])) != strings.TrimSpace(asString(incoming["title"])) || strings.TrimSpace(asString(existing["magnet"])) == "")
		if needMagnet {
			select {
			case <-ctx.Done():
				return added, updated, skipped, failed, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
			magnet, err := p.downloadMagnet(ctx, item.DownloadID, cookie)
			if err != nil {
				failed++
				continue
			}
			if strings.TrimSpace(magnet) == "" {
				failed++
				continue
			}
			incoming["magnet"] = magnet
		}
		var ex filedb.TorrentDetails
		if exists {
			ex = existing
		}
		result := filedb.MergeTorrent(ex, incoming, p.Config.TracksAttempt)
		if !result.Changed {
			skipped++
			continue
		}
		bucket[urlv] = result.Torrent
		changed[key] = fileTime(result.Torrent)
		if !result.IsNew {
			plog.WriteUpdated(urlv, asString(incoming["title"]))
			updated++
		} else {
			plog.WriteAdded(urlv, asString(incoming["title"]))
			added++
		}
	}
	for key, when := range changed {
		if err := p.DB.SaveBucket(key, bucketCache[key], when); err != nil {
			return added, updated, skipped, failed, err
		}
	}
	return added, updated, skipped, failed, nil
}

func (p *Parser) ensureCookie(ctx context.Context) (string, error) {
	p.cookieMu.Lock()
	cookie := strings.TrimSpace(p.cookie)
	if cookie != "" {
		p.cookieMu.Unlock()
		return cookie, nil
	}
	if !p.lastLoginAttempt.IsZero() && time.Since(p.lastLoginAttempt) < 5*time.Minute {
		remaining := 5*time.Minute - time.Since(p.lastLoginAttempt)
		p.cookieMu.Unlock()
		log.Printf("toloka: login on cooldown for %s after recent failure", remaining.Round(time.Second))
		return "", fmt.Errorf("toloka: login on cooldown for %s: %w", remaining.Round(time.Second), core.ErrNotAuthorized)
	}
	p.lastLoginAttempt = time.Now()
	p.cookieMu.Unlock()

	cookie, err := p.takeLogin(ctx)
	if err != nil {
		return "", err
	}
	p.cookieMu.Lock()
	p.cookie = cookie
	p.lastLoginAttempt = time.Time{} // clear cooldown on success
	p.cookieMu.Unlock()
	_ = core.DefaultSessionStore().SaveAuth(p.domain, cookie)
	return cookie, nil
}

func (p *Parser) invalidateCookie() {
	p.cookieMu.Lock()
	p.cookie = ""
	p.lastLoginAttempt = time.Time{} // allow immediate re-login
	p.cookieMu.Unlock()
	_ = core.DefaultSessionStore().DeleteAuth(p.domain)
}

func (p *Parser) takeLogin(ctx context.Context) (string, error) {
	host := strings.TrimRight(p.Config.Toloka.Host, "/")
	user := strings.TrimSpace(p.Config.Toloka.Login.U)
	pass := strings.TrimSpace(p.Config.Toloka.Login.P)
	if user == "" || pass == "" {
		log.Printf("toloka: login skipped — username/password empty in config")
		return "", errNoCredentials
	}
	vals := url.Values{}
	vals.Set("username", user)
	vals.Set("password", pass)
	vals.Set("autologin", "on")
	vals.Set("ssl", "on")
	vals.Set("redirect", "index.php?")
	vals.Set("login", "Вхід")
	loginURL := host + "/login.php"

	// Through Fetcher, not a raw net/http client. toloka is behind Cloudflare
	// (server: cloudflare, cf-ray) and simply is not challenging today —
	// measured 2026-09-26, login.php answers 200 with the form intact. kinozal
	// was described the same way in this repo until it moved behind a managed
	// challenge, at which point its raw-client login became impossible rather
	// than merely fragile: a raw client can hold no cf_clearance and does not
	// impersonate Chrome's ClientHello. Fetcher carries both, so if toloka ever
	// gates, CF auto-detect handles it with no code change. The two things that
	// used to require the raw client — Set-Cookie off the 302, and not
	// following it — are FetchResult.Header and FetchOptions.NoRedirect.
	//
	// The login POST carries the same User-Agent the pages are fetched with.
	//
	// The symptom is measured; the mechanism is not. Production 2026-09-26:
	// `login OK userid=444179` at 16:31:46, then `cat=96 page=0 returned login
	// form` at 16:31:54, then a fresh login per category, indefinitely, each
	// one reporting success. The leading explanation is phpBB's
	// `session_browser` check, which drops a session to guest when the browser
	// string changes — and the login used to go out on a raw client with a
	// hand-written `Mozilla/5.0 … AppleWebKit/537.36` while the pages come from
	// the flaresolverr browser, a guaranteed mismatch. That is NOT confirmed
	// against the live site: the obvious test (replay a guest sid under a
	// different UA) proves nothing, because toloka hands a guest a fresh sid on
	// almost every request whatever the UA — measured over three runs, where
	// the *same* UA also produced a new sid.
	//
	// A competing explanation fits equally well: our cookie never reaches the
	// rendered page at all. logSessionRejection separates the two on the next
	// production run.
	ua := strings.TrimSpace(p.Config.Toloka.UserAgent)
	postCookie := strings.TrimSpace(p.Config.Toloka.Cookie)
	flareCookie, flareUA := p.Fetcher.GetFlareCookies(loginURL)
	if flareCookie != "" {
		// Hand the browser's own session to the POST as well: phpBB upgrades
		// that session in place, so the sid the browser already holds is the
		// one that becomes authenticated.
		postCookie = core.MergeCookieStrings(postCookie, flareCookie)
	}
	if ua == "" {
		ua = flareUA
	}
	if ua == "" {
		log.Printf("toloka: login is going out without the browser's User-Agent — " +
			"if pages are fetched through flaresolverr the site will refuse this session on the next request")
	}
	log.Printf("toloka: login as user=%s host=%s ua=%q", user, host, ua)

	tracker := p.Config.Toloka
	tracker.Cookie = postCookie
	// Standard mode unless CF auto-detect says otherwise; for a non-GET both
	// routes end in a plain POST over the impersonating client either way.
	tracker.FetchMode = "standard"
	resp, err := p.Fetcher.Do(loginURL, tracker, core.FetchOptions{
		Method:      http.MethodPost,
		Body:        []byte(vals.Encode()),
		ContentType: "application/x-www-form-urlencoded",
		UserAgent:   ua,
		NoRedirect:  true,
		ExtraHeaders: map[string]string{
			"Referer": loginURL,
			"Origin":  host,
		},
	})
	if err != nil {
		log.Printf("toloka: login HTTP error: %v", err)
		return "", err
	}
	location := resp.Header.Get("Location")
	var sid, data string
	for _, setCookie := range resp.Header.Values("Set-Cookie") {
		for _, part := range strings.Split(setCookie, ",") {
			if m := inlineReB76eb1Re.FindStringSubmatch(part); len(m) > 1 {
				sid = strings.TrimSpace(m[1])
			}
			if m := inlineReEbc614Re.FindStringSubmatch(part); len(m) > 1 {
				data = strings.TrimSpace(m[1])
			}
		}
	}
	if sid == "" || data == "" {
		log.Printf("toloka: login FAILED — no toloka_sid/toloka_data cookies (status=%d location=%q)", resp.StatusCode, location)
		// Names only — toloka_data is the session credential itself.
		return "", fmt.Errorf("toloka: login.php set [%s], need toloka_sid+toloka_data: %w", core.CookieNames(resp.Header.Values("Set-Cookie")...), core.ErrNotAuthorized)
	}
	// Detect guest cookie: toloka_data is URL-encoded PHP serialized blob; on
	// failed credentials toloka still returns a session but with userid=-1.
	// PHP-encoded variants of "userid";i:-1 — match either raw or URL-encoded.
	dataDecoded, _ := url.QueryUnescape(data)
	if strings.Contains(dataDecoded, `"userid";i:-1`) || strings.Contains(data, "%22userid%22%3Bi%3A-1") {
		log.Printf("toloka: login FAILED — server returned guest cookie (userid=-1, wrong credentials?)")
		return "", fmt.Errorf("toloka: login returned a guest session (userid=-1, credentials refused): %w", core.ErrNotAuthorized)
	}
	useridStr := ""
	if m := useridRe.FindStringSubmatch(dataDecoded); len(m) > 1 {
		useridStr = m[1]
	}
	cookie := fmt.Sprintf("toloka_sid=%s; toloka_ssl=1; toloka_data=%s;", sid, data)
	p.cookieMu.Lock()
	p.loginUA = ua
	p.cookieMu.Unlock()
	log.Printf("toloka: login OK userid=%s sid=%s ua=%q", useridStr, sidFingerprint(cookie), ua)
	return cookie, nil
}

// fetchPageHTML routes through the shared Fetcher so the tracker's fetchmode
// (standard / flaresolverr) takes effect. The login cookie is merged with any
// config-side cookie inside Fetcher.GetExt. Retries on 429 with backoff.
func (p *Parser) fetchPageHTML(ctx context.Context, cat string, page int) (string, error) {
	host := strings.TrimRight(p.Config.Toloka.Host, "/")
	cookie, err := p.ensureCookie(ctx)
	if err != nil {
		return "", err
	}
	urlv := fmt.Sprintf("%s/f%s", host, cat)
	if page > 0 {
		urlv = fmt.Sprintf("%s/f%s-%d", host, cat, page*45)
	}
	urlv += "?sort=8"
	for attempt := 1; attempt <= 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		body, status, err := p.Fetcher.GetStringExt(urlv, p.Config.Toloka, cookie, defaultUA())
		if err != nil {
			return "", err
		}
		if status == 429 && attempt < 3 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt*5) * time.Second):
			}
			continue
		}
		if status < 200 || status >= 300 {
			return "", fmt.Errorf("toloka status %d", status)
		}
		return body, nil
	}
	return "", fmt.Errorf("toloka: max retries exceeded")
}

func (p *Parser) downloadMagnet(ctx context.Context, downloadID, cookie string) (string, error) {
	if strings.TrimSpace(downloadID) == "" {
		return "", nil
	}
	host := strings.TrimRight(p.Config.Toloka.Host, "/")
	rawURL := fmt.Sprintf("%s/download.php?id=%s", host, url.QueryEscape(downloadID))
	for attempt := 1; attempt <= 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		data, status, err := p.Fetcher.DownloadExt(rawURL, p.Config.Toloka, cookie, defaultUA())
		if err != nil {
			return "", err
		}
		if status == 429 && attempt < 3 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt*5) * time.Second):
			}
			continue
		}
		if status < 200 || status >= 300 {
			return "", fmt.Errorf("toloka download status %d", status)
		}
		return core.TorrentBytesToMagnet(data), nil
	}
	return "", fmt.Errorf("toloka download: max retries")
}

func parseTitle(cat, title string) (string, string, int) {
	var name, original string
	relased := 0
	parseYear := func(s string) {
		if relased == 0 {
			relased, _ = strconv.Atoi(strings.TrimSpace(s))
		}
	}
	if isMovieCat(cat) {
		if g := movieMainRe.FindStringSubmatch(title); len(g) > 3 {
			name, original = strings.TrimSpace(g[1]), strings.TrimSpace(g[2])
			parseYear(g[3])
		} else if g := movieShortRe.FindStringSubmatch(title); len(g) > 3 {
			name, original = strings.TrimSpace(g[1]), strings.TrimSpace(g[2])
			parseYear(g[3])
		} else if g := movieSeriesRe.FindStringSubmatch(title); len(g) > 2 {
			name = strings.TrimSpace(g[1])
			parseYear(g[2])
		} else if g := movieSingleRe.FindStringSubmatch(title); len(g) > 2 {
			name = strings.TrimSpace(g[1])
			parseYear(g[2])
		}
	} else if isSerialCat(cat) {
		if g := serialLongRe.FindStringSubmatch(title); len(g) > 3 {
			name, original = strings.TrimSpace(g[1]), strings.TrimSpace(g[2])
			parseYear(g[3])
		} else if g := serialBasicRe.FindStringSubmatch(title); len(g) > 3 {
			name, original = strings.TrimSpace(g[1]), strings.TrimSpace(g[2])
			parseYear(g[3])
		} else if g := serialBracketRe.FindStringSubmatch(title); len(g) > 5 {
			name, original = strings.TrimSpace(g[1]), strings.TrimSpace(g[4])
			parseYear(g[5])
		} else if g := serialSlashRe.FindStringSubmatch(title); len(g) > 3 {
			name, original = strings.TrimSpace(g[1]), strings.TrimSpace(g[2])
			parseYear(g[3])
		} else if g := serialTriRe.FindStringSubmatch(title); len(g) > 3 {
			name, original = strings.TrimSpace(g[1]), strings.TrimSpace(g[2])
			parseYear(g[3])
		} else if g := serialRusOnlyRe.FindStringSubmatch(title); len(g) > 2 {
			name = strings.TrimSpace(g[1])
			parseYear(g[2])
		}
	}
	return strings.TrimSpace(name), strings.TrimSpace(original), relased
}

func typesForCategory(cat string) []string {
	switch cat {
	case "16", "96", "42":
		return []string{"movie"}
	case "19", "139", "84":
		return []string{"multfilm"}
	case "32", "173", "124":
		return []string{"serial"}
	case "174", "44", "125":
		return []string{"multserial"}
	case "226", "227", "228", "229", "230", "12", "131":
		return []string{"docuserial", "documovie"}
	case "127":
		return []string{"anime"}
	case "132":
		return []string{"tvshow"}
	default:
		return nil
	}
}

func isMovieCat(cat string) bool {
	switch cat {
	case "16", "96", "19", "139", "12", "131", "84", "42":
		return true
	default:
		return false
	}
}
func isSerialCat(cat string) bool {
	switch cat {
	case "32", "173", "174", "44", "230", "226", "227", "228", "229", "127", "124", "125", "132":
		return true
	default:
		return false
	}
}

func fallbackName(title string) string {
	parts := firstNamePartRe.Split(title, 2)
	if len(parts) == 0 {
		return strings.TrimSpace(title)
	}
	return strings.TrimSpace(parts[0])
}

func replaceBadNames(s string) string {
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = strings.ReplaceAll(s, "&#160;", " ")
	s = strings.ReplaceAll(s, "\u00a0", " ")
	s = strings.ReplaceAll(s, "Ё", "Е")
	s = strings.ReplaceAll(s, "ё", "е")
	return strings.TrimSpace(cleanSpaceRe.ReplaceAllString(s, " "))
}

// sidFingerprint reduces a session id to a short, non-reversible tag, so two log
// lines can be compared without ever printing the credential. A toloka_sid in a
// log is a replayable session, the same rule cf_clearance follows.
func sidFingerprint(cookie string) string {
	sid := ""
	for _, part := range strings.Split(cookie, ";") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(part), "toloka_sid="); ok {
			sid = v
			break
		}
	}
	if sid == "" {
		return "none"
	}
	sum := sha256.Sum256([]byte(sid))
	return hex.EncodeToString(sum[:3])
}

// logSessionRejection records what the run actually sent when the site refused
// the session, which is what separates the two candidate explanations for the
// re-login loop. If this sid tag matches the one logged at login, our cookie
// did reach the request and toloka rejected it anyway — pointing at the session
// being bound to something the fetch path does not reproduce, the User-Agent
// being the leading suspect. If it differs, or reads `none`, then the cookie
// never made it into the fetch and the bug is entirely ours.
func (p *Parser) logSessionRejection(cat string, page int) {
	p.cookieMu.Lock()
	cookie, ua := p.cookie, p.loginUA
	p.cookieMu.Unlock()
	log.Printf("toloka: cat=%s page=%d — session we sent: cookies=[%s] sid=%s loginUA=%q",
		cat, page, core.CookieNames(cookie), sidFingerprint(cookie), ua)
}
