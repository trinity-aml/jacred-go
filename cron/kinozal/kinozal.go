package kinozal

import (
	"context"
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

const trackerName = "kinozal"

var parseCats = []string{"45", "46", "8", "6", "15", "17", "35", "39", "13", "14", "24", "11", "9", "47", "18", "37", "12", "10", "7", "16", "49", "50", "21", "22", "20"}

var (
	browsePagesRe = regexp.MustCompile(`page=([0-9]+)[^"]*"[^>]*>[0-9]+</a></li><li><a rel="next"`)
	rowSplitRe    = regexp.MustCompile(`<tr class=('first bg'|bg)>`)
	cleanSpaceRe  = regexp.MustCompile(`[\n\r\t\x{00A0} ]+`)
	dateOnlyRe    = regexp.MustCompile(`<td class='s'>([0-9]{2}\.[0-9]{2}\.[0-9]{4}) в [0-9]{2}:[0-9]{2}</td>`)
	movieMainRe   = regexp.MustCompile(`^([^\(/]+) (\([^\)/]+\) )?/ ([^\(/]+) (\([^\)/]+\) )?/ ([0-9]{4})`)
	movieShortRe  = regexp.MustCompile(`^([^/\(]+) / ([0-9]{4})`)
	rusSeasonRe   = regexp.MustCompile(`^([^\(/]+) (\([^\)/]+\) )?\([0-9\-]+ сезоны?: [^\)/]+\) ([^/]+ )?/ ([0-9]{4})`)
	rusSeriesRe   = regexp.MustCompile(`^([^\(/]+) (\([^\)/]+\) )?\([^\)/]+\) ([^/]+ )?/ ([0-9]{4})`)
	mp1Re         = regexp.MustCompile(`(?is)href="/(details.php\?id=[0-9]+)"`)
	mp2Re         = regexp.MustCompile(`(?is)class="r[0-9]+">([^<]+)</a>`)
	mp3Re         = regexp.MustCompile(`(?is)<td class='sl_s'>([0-9]+)</td>`)
	mp4Re         = regexp.MustCompile(`(?is)<td class='sl_p'>([0-9]+)</td>`)
	// КБ and ТБ are included because a row whose size falls outside the listed
	// units fails the "all fields present" guard in parsePage and is dropped
	// entirely. Multi-season packs do reach terabytes; filedb/fulldetails.go
	// and every sibling parser already understand ТБ.
	mp5Re       = regexp.MustCompile(`(?is)<td class='s'>([0-9\.,]+ (КБ|МБ|ГБ|ТБ))</td>`)
	forSeasonRe = regexp.MustCompile(`^([^\(/]+) (\([^\)/]+\) )?\([0-9\-]+ сезоны?: [^\)/]+\) ([^/]+ )?/ ([^\(/]+) / ([0-9]{4})`)
	forSeriesRe = regexp.MustCompile(`^([^\(/]+) (\([^\)/]+\) )?\([^\)/]+\) ([^/]+ )?/ ([^\(/]+) / ([0-9]{4})`)
	forShortRe  = regexp.MustCompile(`^([^\(/]+) / ([^\(/]+) / ([0-9]{4})`)
	tvMainRe    = regexp.MustCompile(`^([^\(/]+) (\([^\)/]+\) )?/ ([^\(/]+) / ([0-9]{4})`)
	tvShortRe   = regexp.MustCompile(`^([^/\(]+) (\([^\)/]+\) )?/ ([0-9]{4})`)
	idURLRe     = regexp.MustCompile(`\?id=([0-9]+)`)
	// Sanity check that a browse response is a real tracker page rather than
	// an error or a redirect body. Matched on the brand root only: the site
	// rebranded "Кинозал.ТВ" -> "Кинозал.GURU" with the 2026 domain move, and
	// pinning the full old name made every page fail this guard — parsePage
	// then returned zero records with no error, which read as "no new
	// torrents" instead of "parser is broken".
	kinozalTitleRe = regexp.MustCompile(`Кинозал\.[^<]*</title>`)
	hashRe         = regexp.MustCompile(`<ul><li>Инфо хеш: +([^<]+)</li>`)

	inlineReC4d16cRe = regexp.MustCompile(`uid=([0-9]+)`)
	inlineReF31405Re = regexp.MustCompile(`pass=([^;]+)(;|$)`)

	fallbackSplitRe = regexp.MustCompile(`(\[|/|\(|\|)`)
	dateLeadingZero = regexp.MustCompile(`^[0-9]\.`)
)

// monthSubsts is the precompiled set of month-name normalizers used by
// parseCreateTime. Replacing 36+ MustCompile calls per invocation with a
// single shared slice cuts the per-row regex cost on kinozal listings.
type monthSubst struct {
	re *regexp.Regexp
	to string
}

var monthSubsts = func() []monthSubst {
	raw := []struct{ pat, to string }{
		{` янв\.? `, `.01.`}, {` февр?\.? `, `.02.`}, {` март?\.? `, `.03.`}, {` апр\.? `, `.04.`},
		{` май `, `.05.`}, {` июнь?\.? `, `.06.`}, {` июль?\.? `, `.07.`}, {` авг\.? `, `.08.`},
		{` сент?\.? `, `.09.`}, {` окт\.? `, `.10.`}, {` нояб?\.? `, `.11.`}, {` дек\.? `, `.12.`},
		{` январ(ь|я)?\.? `, `.01.`}, {` феврал(ь|я)?\.? `, `.02.`}, {` марта?\.? `, `.03.`}, {` апрел(ь|я)?\.? `, `.04.`},
		{` май?я?\.? `, `.05.`}, {` июн(ь|я)?\.? `, `.06.`}, {` июл(ь|я)?\.? `, `.07.`}, {` августа?\.? `, `.08.`},
		{` сентябр(ь|я)?\.? `, `.09.`}, {` октябр(ь|я)?\.? `, `.10.`}, {` ноябр(ь|я)?\.? `, `.11.`}, {` декабр(ь|я)?\.? `, `.12.`},
		{` jan `, `.01.`}, {` feb `, `.02.`}, {` mar `, `.03.`}, {` apr `, `.04.`}, {` may `, `.05.`}, {` jun `, `.06.`},
		{` jul `, `.07.`}, {` aug `, `.08.`}, {` sep `, `.09.`}, {` oct `, `.10.`}, {` nov `, `.11.`}, {` dec `, `.12.`},
	}
	out := make([]monthSubst, len(raw))
	for i, r := range raw {
		out[i] = monthSubst{re: regexp.MustCompile(r.pat), to: r.to}
	}
	return out
}()

type Task struct {
	UpdateTime string `json:"updateTime"`
	Page       int    `json:"page"`
	// Cycle bookkeeping; embedded so the stored map stays flat.
	core.CycleFields
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
	tasks            map[string]map[string][]Task
	cookieMu         sync.Mutex
	cookie           string
	lastLoginAttempt time.Time
	loginBlockUntil  time.Time
	loginBlockReason string
}

// domainKey is the session-store key for the configured host. It is derived on
// every call rather than cached at construction: a live config reload replaces
// Config in place (server.UpdateConfig) without rebuilding the parser, so a
// cached value would keep pointing at the previous domain after a host change
// and the auth cookie would be written to the wrong Data/cookie/<domain>.json.
func (p *Parser) domainKey() string {
	return core.DomainFromHost(requestHost(p.Config.Kinozal))
}

func New(cfg app.Config, db *filedb.DB, dataDir string) *Parser {
	loc, _ := time.LoadLocation("Asia/Jerusalem")
	if loc == nil {
		loc = time.Local
	}
	p := &Parser{Config: cfg, DB: db, DataDir: dataDir, Fetcher: core.NewFetcher(cfg), loc: loc, tasks: map[string]map[string][]Task{}}
	_ = p.loadTasks()
	if saved, _ := core.DefaultSessionStore().LoadAuth(p.domainKey()); saved != "" {
		p.cookie = saved
		log.Printf("kinozal: loaded saved cookie from disk")
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
	// Login before parsing — resolveMagnet requires auth
	if err := p.requireLogin(ctx); err != nil {
		return ParseResult{Status: core.StatusWorkLogin}, err
	}
	res := ParseResult{Status: "ok", PerCategory: map[string]int{}}
	{
		log.Printf("kinozal: starting parse, cookie=[%s]", core.CookieNames(p.getCookie()))
	}
	for _, cat := range parseCats {
		items, err := p.parsePage(ctx, cat, page, "")
		if err != nil {
			return res, err
		}
		res.Fetched += len(items)
		res.PerCategory[cat] = len(items)
		if len(items) == 0 {
			continue
		}
		a, u, s, f, err := p.saveTorrents(ctx, items)
		if err != nil {
			return res, err
		}
		res.Added += a
		res.Updated += u
		res.Skipped += s
		res.Failed += f
		log.Printf("kinozal: cat=%s fetched=%d added=%d skipped=%d failed=%d", cat, len(items), a, s, f)
	}
	log.Printf("kinozal: done fetched=%d added=%d skipped=%d failed=%d", res.Fetched, res.Added, res.Skipped, res.Failed)
	return res, nil
}

func (p *Parser) UpdateTasksParse(ctx context.Context) (map[string]map[string][]Task, error) {
	if err := p.requireLogin(ctx); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tasks == nil {
		p.tasks = map[string]map[string][]Task{}
	}
	for _, cat := range parseCats {
		for year := time.Now().In(p.loc).Year(); year >= 1990; year-- {
			arg := fmt.Sprintf("&d=%d&t=1", year)
			htmlBody, err := p.fetchBrowse(ctx, cat, 0, arg)
			if err != nil || htmlBody == "" {
				continue
			}
			maxPages := 0
			if m := browsePagesRe.FindStringSubmatch(htmlBody); len(m) > 1 {
				maxPages, _ = strconv.Atoi(strings.TrimSpace(m[1]))
			}
			if _, ok := p.tasks[cat]; !ok {
				p.tasks[cat] = map[string][]Task{}
			}
			// Keep every existing slot here and prune once, below, through the
			// guarded helper. Filtering by `t.Page <= maxPages` at this point
			// looks equivalent but is not: browsePagesRe fails to match on any
			// body that is not a listing — a Cloudflare page, the login wall,
			// a changed layout — maxPages stays 0, and the whole sweep plan for
			// the category is dropped to page 0 without a word.
			pagesMap := map[int]Task{}
			for _, t := range p.tasks[cat][arg] {
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
			merged, pruned := core.PruneTaskPages(merged, func(t Task) int { return t.Page }, maxPages)
			if pruned > 0 {
				log.Printf("kinozal: updatetasksparse cat=%s arg=%q maxPage=%d pruned=%d remaining=%d", cat, arg, maxPages, pruned, len(merged))
			}
			p.tasks[cat][arg] = merged
		}
	}
	if err := p.saveTasksLocked(); err != nil {
		return nil, err
	}
	return cloneTasks(p.tasks), nil
}

// beginCycleLocked opens (or rotates) the sweep cycle. Caller holds p.mu.
// kinozal's map is nested: category, then the browse argument, then pages.
func (p *Parser) beginCycleLocked() (*core.ParseAllCycle, int, int) {
	slots, keys := core.CycleSlotsFromNestedMap[Task](p.tasks, func(t Task) int { return t.Page })
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
func (p *Parser) settle(cycle *core.ParseAllCycle, cat, arg string, page int, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if argMap, found := p.tasks[cat]; found {
		if list, found := argMap[arg]; found {
			for i := range list {
				if list[i].Page == page {
					if core.NoteParseAllAttempt(trackerName, &list[i], cycle, ok) && ok {
						list[i].MarkToday(p.loc)
					}
				}
			}
			argMap[arg] = list
			p.tasks[cat] = argMap
		}
	}
	_ = p.saveTasksLocked()
}

func (p *Parser) ParseAllTask(ctx context.Context, force bool) (string, error) {
	if err := p.requireLogin(ctx); err != nil {
		return "", err
	}
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
		log.Printf("kinozal: parsealltask — tasks empty, running updatetasksparse first")
		if _, err := p.UpdateTasksParse(ctx); err != nil {
			return "", err
		}
		p.mu.Lock()
		snapshot = cloneTasks(p.tasks)
		p.mu.Unlock()
	}

	// Opened against p.tasks rather than the snapshot: the first run stamps
	// the day's existing progress into the cycle, and that must be persisted
	// before the snapshot is taken.
	p.mu.Lock()
	cycle, pendingAtStart, cycleTotal := p.beginCycleLocked()
	snapshot = cloneTasks(p.tasks)
	p.mu.Unlock()

	totalPages := 0
	for _, byArg := range snapshot {
		for arg, list := range byArg {
			if arg == "" {
				continue
			}
			totalPages += len(list)
		}
	}
	processed, fetched, added, updated, skipped, failed, errs := 0, 0, 0, 0, 0, 0, 0
	for cat, byArg := range snapshot {
		for arg, list := range byArg {
			if arg == "" {
				continue
			}
			for _, task := range list {
				if !force && !core.PendingInCycle(&task, cycle) {
					continue
				}
				if p.Config.Kinozal.ParseDelay > 0 {
					select {
					case <-ctx.Done():
						return "", ctx.Err()
					case <-time.After(time.Duration(p.Config.Kinozal.ParseDelay) * time.Millisecond):
					}
				}
				items, err := p.parsePage(ctx, cat, task.Page, arg)
				if err != nil {
					log.Printf("kinozal: parsealltask cat=%s arg=%s page=%d error: %v", cat, arg, task.Page, err)
					p.settle(cycle, cat, arg, task.Page, false)
					errs++
					continue
				}
				processed++
				if len(items) == 0 {
					log.Printf("kinozal: parsealltask cat=%s arg=%s page=%d empty (marking today)", cat, arg, task.Page)
					p.settle(cycle, cat, arg, task.Page, true)
					continue
				}
				a, u, s, f, err := p.saveTorrents(ctx, items)
				if err != nil {
					log.Printf("kinozal: parsealltask cat=%s arg=%s page=%d save error: %v", cat, arg, task.Page, err)
					errs++
					continue
				}
				fetched += len(items)
				added += a
				updated += u
				skipped += s
				failed += f
				log.Printf("kinozal: parsealltask cat=%s arg=%s page=%d fetched=%d added=%d skipped=%d failed=%d", cat, arg, task.Page, len(items), a, s, f)
				p.settle(cycle, cat, arg, task.Page, true)
			}
		}
	}
	p.mu.Lock()
	cycleSlots, _ := core.CycleSlotsFromNestedMap[Task](p.tasks, func(t Task) int { return t.Page })
	pendingLeft := core.CountPendingInCycle(cycleSlots, cycle)
	p.mu.Unlock()
	log.Printf("kinozal: parsealltask cycle=%s pending=%d->%d/%d", cycle.CycleID, pendingAtStart, pendingLeft, cycleTotal)
	log.Printf("kinozal: parsealltask done processed=%d/%d fetched=%d added=%d updated=%d skipped=%d failed=%d errors=%d", processed, totalPages, fetched, added, updated, skipped, failed, errs)
	return "ok", nil
}

func (p *Parser) ParseLatest(ctx context.Context, pages int) (string, error) {
	if !p.latestMu.TryLock() {
		return "work", nil
	}
	defer p.latestMu.Unlock()
	if err := p.requireLogin(ctx); err != nil {
		return "", err
	}
	if pages <= 0 {
		pages = 100
	}
	if pages > 100 {
		pages = 100
	}
	var lines []string
	processed, fetched, added, updated, skipped, failed, errs := 0, 0, 0, 0, 0, 0, 0
	for _, cat := range parseCats {
		for page := 0; page < pages; page++ {
			if p.Config.Kinozal.ParseDelay > 0 {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(time.Duration(p.Config.Kinozal.ParseDelay) * time.Millisecond):
				}
			}
			items, err := p.parsePage(ctx, cat, page, "")
			if err != nil {
				log.Printf("kinozal: parselatest cat=%s page=%d error: %v", cat, page, err)
				errs++
				continue
			}
			processed++
			if len(items) == 0 {
				log.Printf("kinozal: parselatest cat=%s page=%d empty (marking today)", cat, page)
				p.markLatestPageToday(cat, page)
				continue
			}
			a, u, s, f, err := p.saveTorrents(ctx, items)
			if err != nil {
				log.Printf("kinozal: parselatest cat=%s page=%d save error: %v", cat, page, err)
				errs++
				continue
			}
			fetched += len(items)
			added += a
			updated += u
			skipped += s
			failed += f
			log.Printf("kinozal: parselatest cat=%s page=%d fetched=%d added=%d skipped=%d failed=%d", cat, page, len(items), a, s, f)
			p.markLatestPageToday(cat, page)
			lines = append(lines, fmt.Sprintf("%s - %d", cat, page))
		}
	}
	log.Printf("kinozal: parselatest done processed=%d fetched=%d added=%d updated=%d skipped=%d failed=%d errors=%d", processed, fetched, added, updated, skipped, failed, errs)
	if len(lines) == 0 {
		return "ok", nil
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func (p *Parser) markLatestPageToday(cat string, page int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tasks == nil {
		p.tasks = map[string]map[string][]Task{}
	}
	if _, ok := p.tasks[cat]; !ok {
		p.tasks[cat] = map[string][]Task{}
	}
	list := p.tasks[cat][""]
	found := false
	for i := range list {
		if list[i].Page == page {
			list[i].MarkToday(p.loc)
			found = true
			break
		}
	}
	if !found {
		t := Task{Page: page}
		t.MarkToday(p.loc)
		list = append(list, t)
	}
	p.tasks[cat][""] = list
	_ = p.saveTasksLocked()
}

func (p *Parser) parsePage(ctx context.Context, cat string, page int, arg string) ([]filedb.TorrentDetails, error) {
	htmlBody, err := p.fetchBrowse(ctx, cat, page, arg)
	if err != nil {
		return nil, err
	}
	if htmlBody == "" || !kinozalTitleRe.MatchString(htmlBody) {
		return nil, nil
	}
	if p.getCookie() == "" || !strings.Contains(htmlBody, ">Выход</a>") {
		// This page was served to a guest and cannot be salvaged; re-login so the
		// next one is not. Discarding the error here used to hide a broken login
		// behind a run that reported fetched=0 for every category.
		if err := p.takeLogin(ctx); err != nil && !errors.Is(err, errLoginCooldown) {
			log.Printf("kinozal: re-login failed mid-run: %v", err)
		}
	}
	rows := rowSplitRe.Split(replaceBadNames(htmlBody), -1)
	out := make([]filedb.TorrentDetails, 0, len(rows))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	host := strings.TrimRight(p.Config.Kinozal.Host, "/")
	for _, row := range rows[1:] {
		if strings.TrimSpace(row) == "" {
			continue
		}
		reFind := func(re *regexp.Regexp, idx ...int) string {
			group := 1
			if len(idx) > 0 {
				group = idx[0]
			}
			m := re.FindStringSubmatch(row)
			if len(m) <= group {
				return ""
			}
			s := html.UnescapeString(strings.TrimSpace(m[group]))
			s = cleanSpaceRe.ReplaceAllString(s, " ")
			return strings.TrimSpace(s)
		}
		createTime := time.Time{}
		if strings.Contains(row, "<td class='s'>сегодня") {
			createTime = time.Now().UTC()
		} else if strings.Contains(row, "<td class='s'>вчера") {
			createTime = time.Now().UTC().AddDate(0, 0, -1)
		} else if m := dateOnlyRe.FindStringSubmatch(row); len(m) > 1 {
			createTime = parseCreateTime(m[1], "02.01.2006")
		}
		if createTime.IsZero() {
			continue
		}
		urlPath := reFind(mp1Re)
		title := reFind(mp2Re)
		sidRaw := reFind(mp3Re)
		pirRaw := reFind(mp4Re)
		sizeName := reFind(mp5Re)
		if urlPath == "" || title == "" || sidRaw == "" || pirRaw == "" || sizeName == "" {
			continue
		}
		name, original, relased := parseTitle(cat, title, row)
		if strings.TrimSpace(name) == "" {
			name = fallbackName(title)
		}
		types := categoryTypes(cat)
		if name == "" || len(types) == 0 {
			continue
		}
		sid, _ := strconv.Atoi(sidRaw)
		pir, _ := strconv.Atoi(pirRaw)
		out = append(out, filedb.TorrentRecord{
			TrackerName:  trackerName,
			Types:        types,
			URL:          host + "/" + strings.TrimLeft(urlPath, "/"),
			Title:        title,
			Sid:          sid,
			Pir:          pir,
			SizeName:     sizeName,
			CreateTime:   createTime.UTC().Format(time.RFC3339Nano),
			UpdateTime:   now,
			Name:         name,
			OriginalName: original,
			Relased:      relased,
			SearchName:   core.SearchName(name),
			SearchOrig:   core.SearchName(firstNonEmpty(original, name)),
		}.ToMap())
	}
	return out, nil
}

func (p *Parser) saveTorrents(ctx context.Context, torrents []filedb.TorrentDetails) (int, int, int, int, error) {
	added, updated, skipped, failed := 0, 0, 0, 0
	plog := core.NewParserLog(trackerName, filepath.Join(p.DB.DataDir, "log"), p.Config.LogParsers && p.Config.Kinozal.Log)
	bucketCache := make(map[string]map[string]filedb.TorrentDetails, len(torrents))
	changed := make(map[string]time.Time, len(torrents))
	for _, incoming := range torrents {
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
		urlv := strings.TrimSpace(asString(incoming["url"]))
		if urlv == "" {
			skipped++
			continue
		}
		existing, exists := bucket[urlv]
		// Only resolve magnet if title changed or existing has no magnet (C# predicate pattern)
		needMagnet := !exists || asString(existing["title"]) != asString(incoming["title"]) || strings.TrimSpace(asString(existing["magnet"])) == ""
		if needMagnet {
			magnet, err := p.resolveMagnet(ctx, urlv)
			if err != nil {
				plog.WriteFailed(urlv, asString(incoming["title"]))
				failed++
				continue
			}
			if magnet == "" {
				plog.WriteFailed(urlv, asString(incoming["title"]))
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
		if !result.IsNew {
			plog.WriteUpdated(urlv, asString(incoming["title"]))
			updated++
		} else {
			plog.WriteAdded(urlv, asString(incoming["title"]))
			added++
		}
		changed[key] = fileTime(result.Torrent)
	}
	for key, bucket := range bucketCache {
		if _, ok := changed[key]; !ok {
			continue
		}
		if err := p.DB.SaveBucket(key, bucket, changed[key]); err != nil {
			return added, updated, skipped, failed, err
		}
	}
	return added, updated, skipped, failed, nil
}

func (p *Parser) resolveMagnet(ctx context.Context, detailURL string) (string, error) {
	idm := idURLRe.FindStringSubmatch(detailURL)
	if len(idm) < 2 {
		return "", nil
	}
	id := idm[1]
	cookie := p.getCookie()
	form := url.Values{}
	form.Set("id", id)
	form.Set("action", "2")
	host := strings.TrimRight(requestHost(p.Config.Kinozal), "/")
	reqURL := host + "/get_srv_details.php?id=" + id + "&action=2"
	if err := ctx.Err(); err != nil {
		return "", err
	}
	res, err := p.Fetcher.Do(reqURL, p.Config.Kinozal, core.FetchOptions{
		Method:       http.MethodPost,
		Body:         []byte(form.Encode()),
		ContentType:  "application/x-www-form-urlencoded",
		ExtraCookie:  cookie,
		UserAgent:    kinozalUA(p.Config.Kinozal),
		ExtraHeaders: kinozalHeaders(host),
	})
	if err != nil {
		log.Printf("kinozal: resolveMagnet id=%s error: %v", id, err)
		return "", err
	}
	// Server may return UTF-8 or CP1251 — try raw first
	text := string(res.Body)
	if !strings.Contains(text, "Инфо хеш") {
		text = core.DecodeCP1251(res.Body)
	}
	if m := hashRe.FindStringSubmatch(text); len(m) > 1 {
		h := strings.TrimSpace(m[1])
		if h != "" {
			return "magnet:?xt=urn:btih:" + h, nil
		}
	}
	// Log first failures for debugging
	if len(text) < 500 {
		log.Printf("kinozal: resolveMagnet id=%s FAILED status=%d cookie=[%s] body=%q", id, res.StatusCode, core.CookieNames(cookie), text)
	} else {
		log.Printf("kinozal: resolveMagnet id=%s FAILED status=%d cookie=[%s] bodyLen=%d", id, res.StatusCode, core.CookieNames(cookie), len(text))
	}
	return "", nil
}

func (p *Parser) fetchBrowse(ctx context.Context, cat string, page int, arg string) (string, error) {
	rawURL := fmt.Sprintf("%s/browse.php?c=%s&page=%d%s", strings.TrimRight(requestHost(p.Config.Kinozal), "/"), cat, page, arg)
	ts := p.Config.Kinozal
	if c := p.getCookie(); c != "" {
		ts.Cookie = c
	}
	data, status, err := p.Fetcher.Download(rawURL, ts)
	if err != nil {
		return "", err
	}
	text := core.DecodeCP1251(data)
	if !strings.Contains(text, "Кинозал") {
		text = string(data)
	}
	if status < 200 || status >= 300 {
		return "", nil
	}
	return text, nil
}

// errLoginCooldown marks the "we tried recently, wait" branch so a caller doing
// a best-effort re-login can stay quiet about it. Without it the mid-run
// retry in parsePage logs once per page for the whole cooldown.
var errLoginCooldown = fmt.Errorf("kinozal: login on cooldown: %w", core.ErrNotAuthorized)

const (
	// loginCooldown throttles ordinary retries; loginCFCooldown is longer
	// because a challenged POST means the credentials were never checked, so
	// hammering it only feeds Cloudflare more samples.
	loginCooldown   = 2 * time.Minute
	loginCFCooldown = 15 * time.Minute
)

func (p *Parser) noteLoginCooldown(d time.Duration, reason string) {
	p.cookieMu.Lock()
	p.loginBlockUntil = time.Now().Add(d)
	p.loginBlockReason = reason
	p.cookieMu.Unlock()
}

func (p *Parser) takeLogin(ctx context.Context) error {
	host := strings.TrimRight(requestHost(p.Config.Kinozal), "/")
	if host == "" || strings.TrimSpace(p.Config.Kinozal.Login.U) == "" {
		return fmt.Errorf("kinozal: login is not configured: %w", core.ErrNotAuthorized)
	}

	p.cookieMu.Lock()
	if until := p.loginBlockUntil; time.Now().Before(until) {
		reason := p.loginBlockReason
		p.cookieMu.Unlock()
		return fmt.Errorf("kinozal: not retrying login for %s — %s: %w",
			time.Until(until).Round(time.Second), reason, core.ErrNotAuthorized)
	}
	if since := time.Since(p.lastLoginAttempt); since < loginCooldown {
		p.cookieMu.Unlock()
		// Returning nil here made a cooldown indistinguishable from a
		// successful login, after which requireLogin blamed "login produced no
		// session cookie" — a different failure with a different fix.
		return fmt.Errorf("%w for %s", errLoginCooldown, (loginCooldown - since).Round(time.Second))
	}
	p.lastLoginAttempt = time.Now()
	p.cookieMu.Unlock()

	loginURL := host + "/takelogin.php"
	log.Printf("kinozal: attempting login to %s as %s", host, p.Config.Kinozal.Login.U)

	// takelogin.php sits behind the same Cloudflare managed challenge as the
	// rest of the site. Measured 2026-09-26 with this parser's own User-Agent:
	// takelogin.php, login.php and browse.php all answer 403 carrying
	// cf_chl_opt. This POST used to go out on a raw net/http client, which can
	// hold no cf_clearance and does not impersonate Chrome's ClientHello — so
	// it was answered by the interstitial every time and the credentials never
	// reached kinozal. Login was not failing; it was structurally impossible,
	// and because browse.php then 302s a guest to login.php, whose body carries
	// no brand title, parsePage returned (nil, nil) and the run reported
	// fetched=0 failed=0 — a silent zero for every category.
	//
	// Route it through Fetcher, which carries both the clearance and the Chrome
	// TLS fingerprint. Set-Cookie off the 302 arrives in FetchResult.Header and
	// FetchOptions.NoRedirect keeps the redirect unfollowed, which were the two
	// things that used to require the raw client.
	ua := kinozalUA(p.Config.Kinozal)
	postCookie := strings.TrimSpace(p.Config.Kinozal.Cookie)
	if flareCookie, flareUA := p.Fetcher.GetFlareCookies(loginURL); flareCookie != "" {
		postCookie = core.MergeCookieStrings(postCookie, flareCookie)
		if ua == "" {
			ua = flareUA
		}
		log.Printf("kinozal: login using cf_clearance from flaresolverr")
	}

	form := url.Values{}
	form.Set("username", p.Config.Kinozal.Login.U)
	form.Set("password", p.Config.Kinozal.Login.P)
	form.Set("returnto", "")

	tracker := p.Config.Kinozal
	tracker.Cookie = postCookie
	// Ask for standard mode, but do not rely on getting it: Do re-promotes any
	// domain in the CF auto-detect registry, and kinozal.guru is in it. Both
	// routes end in the same place for a non-GET — Do's flare branch merges the
	// cached clearance and then issues a plain POST over the impersonating
	// client — so the request is correct either way. What matters is that it is
	// never the raw net/http client this function used to build.
	tracker.FetchMode = "standard"
	res, err := p.Fetcher.Do(loginURL, tracker, core.FetchOptions{
		Method:       http.MethodPost,
		Body:         []byte(form.Encode()),
		ContentType:  "application/x-www-form-urlencoded",
		UserAgent:    ua,
		NoRedirect:   true,
		ExtraHeaders: kinozalHeaders(host),
	})
	if err != nil {
		return fmt.Errorf("kinozal: login HTTP error: %w", err)
	}
	log.Printf("kinozal: login response status=%d cf-mitigated=%q",
		res.StatusCode, res.Header.Get("Cf-Mitigated"))
	if p.acceptLoginCookies(res.Header.Values("Set-Cookie")) {
		return nil
	}

	// Tell "Cloudflare never let us reach kinozal" apart from "kinozal rejected
	// these credentials" — they need completely different fixes.
	if res.Header.Get("Cf-Mitigated") == "challenge" || looksLikeCFChallenge(decodeKinozalBody(res.Body)) {
		log.Printf("kinozal: login POST was challenged by cloudflare — retrying through the browser")
		cookies, berr := p.loginViaBrowser(ctx, loginURL, form.Encode(), postCookie)
		if berr != nil {
			log.Printf("kinozal: browser login failed: %v", berr)
		} else if p.acceptLoginCookies(cookies) {
			return nil
		}
		p.noteLoginCooldown(loginCFCooldown, "cloudflare challenge on the login form")
		return fmt.Errorf("kinozal: login blocked by a cloudflare challenge, credentials were never checked: %w",
			core.ErrNotAuthorized)
	}
	// Names only: `pass` is the session credential itself.
	return fmt.Errorf("kinozal: takelogin.php set [%s], need uid+pass: %w",
		core.CookieNames(res.Header.Values("Set-Cookie")...), core.ErrNotAuthorized)
}

// acceptLoginCookies stores a session if the response actually carried one.
// Shared by both login routes — the HTTP POST and the browser form — so they
// cannot drift on what counts as success or on what gets persisted.
func (p *Parser) acceptLoginCookies(setCookies []string) bool {
	uid, pass := "", ""
	for _, line := range setCookies {
		if uid == "" && strings.Contains(line, "uid=") {
			if m := inlineReC4d16cRe.FindStringSubmatch(line); len(m) > 1 {
				uid = m[1]
			}
		}
		if pass == "" && strings.Contains(line, "pass=") {
			if m := inlineReF31405Re.FindStringSubmatch(line); len(m) > 1 {
				pass = m[1]
			}
		}
	}
	if uid == "" || pass == "" {
		return false
	}
	cookie := fmt.Sprintf("uid=%s; pass=%s;", uid, pass)
	p.cookieMu.Lock()
	p.cookie = cookie
	p.loginBlockUntil = time.Time{}
	p.loginBlockReason = ""
	p.cookieMu.Unlock()
	_ = core.DefaultSessionStore().SaveAuth(p.domainKey(), cookie)
	log.Printf("kinozal: login OK — got uid+pass cookies")
	return true
}

// loginViaBrowser submits the credentials in the browser that earned the
// clearance, which sidesteps a challenged POST by construction.
func (p *Parser) loginViaBrowser(ctx context.Context, loginURL, postData, cookie string) ([]string, error) {
	cookieStr, _, err := p.Fetcher.PostViaBrowser(ctx, loginURL, postData, cookie)
	if err != nil {
		return nil, err
	}
	// PostViaBrowser hands back the jar as one "a=1; b=2" string, while
	// acceptLoginCookies reads Set-Cookie lines. Splitting keeps one parser for
	// both routes.
	parts := strings.Split(cookieStr, ";")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out, nil
}

// decodeKinozalBody picks the right charset. Origin pages are CP1251, but a CF
// interstitial is UTF-8 and a body that came back through flaresolverr was
// already decoded by the browser.
func decodeKinozalBody(body []byte) string {
	text := core.DecodeCP1251(body)
	if !strings.Contains(text, "Кинозал") && !kinozalTitleRe.MatchString(text) {
		return string(body)
	}
	return text
}

// requireLogin resolves a session and names the reason when it cannot, so every
// entrypoint fails the same way. takeLogin now wraps core.ErrNotAuthorized with
// its own cause — a cooldown, a Cloudflare challenge or refused credentials are
// three different problems — so it is propagated rather than re-wrapped, which
// used to flatten all of them into "login failed".
func (p *Parser) requireLogin(ctx context.Context) error {
	if p.getCookie() != "" {
		return nil
	}
	if err := p.takeLogin(ctx); err != nil {
		return err
	}
	if p.getCookie() == "" {
		return fmt.Errorf("kinozal: login produced no session cookie: %w", core.ErrNotAuthorized)
	}
	return nil
}

func (p *Parser) getCookie() string {
	p.cookieMu.Lock()
	defer p.cookieMu.Unlock()
	if strings.TrimSpace(p.cookie) != "" {
		return p.cookie
	}
	if strings.TrimSpace(p.Config.Kinozal.Cookie) != "" {
		return strings.TrimSpace(p.Config.Kinozal.Cookie)
	}
	return ""
}

// kinozalUA is the User-Agent for standard-mode requests. Empty means "let
// Fetcher pick", which is the right default: defaultUserAgent is pinned to the
// tls-client's Chrome impersonation profile, and the two have to agree or CF
// sees a Go handshake claiming to be Chrome. What used to sit here instead was
// a hardcoded Chrome/99.0.4844.51 — 47 majors behind the profile, and a
// standing contradiction on every request that carried it.
func kinozalUA(cfg app.TrackerSettings) string {
	return strings.TrimSpace(cfg.UserAgent)
}

// kinozalHeaders is the browser-ish header set kinozal expects, shared by the
// login POST and the magnet lookup so the two cannot drift.
func kinozalHeaders(host string) map[string]string {
	host = strings.TrimRight(host, "/")
	return map[string]string{
		"Cache-Control":             "no-cache",
		"Pragma":                    "no-cache",
		"DNT":                       "1",
		"Origin":                    host,
		"Referer":                   host + "/",
		"Upgrade-Insecure-Requests": "1",
	}
}

// looksLikeCFChallenge reports a Cloudflare interstitial. The brand guard comes
// first for the same reason rutracker's does: a cleared kinozal page still
// loads CF's JS-detection beacon, so a marker-only check would call every
// successful fetch a challenge.
func looksLikeCFChallenge(body string) bool {
	if kinozalTitleRe.MatchString(body) {
		return false
	}
	return strings.Contains(body, "cf_chl_opt") ||
		strings.Contains(body, "orchestrate/chl_page") ||
		strings.Contains(body, "<title>Just a moment")
}

func (p *Parser) tasksPath() string {
	return filepath.Join(p.DataDir, "temp", "kinozal_taskParse.json")
}
func (p *Parser) loadTasks() error {
	data, err := os.ReadFile(p.tasksPath())
	if err != nil {
		if os.IsNotExist(err) {
			p.tasks = map[string]map[string][]Task{}
			return nil
		}
		return err
	}
	var tasks map[string]map[string][]Task
	if err := json.Unmarshal(data, &tasks); err != nil {
		return err
	}
	if tasks == nil {
		tasks = map[string]map[string][]Task{}
	}
	p.tasks = tasks
	return nil
}
func (p *Parser) saveTasksLocked() error {
	if p.tasks == nil {
		p.tasks = map[string]map[string][]Task{}
	}
	if err := os.MkdirAll(filepath.Dir(p.tasksPath()), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(p.tasks)
	if err != nil {
		return err
	}
	return os.WriteFile(p.tasksPath(), data, 0o644)
}

func cloneTasks(in map[string]map[string][]Task) map[string]map[string][]Task {
	out := make(map[string]map[string][]Task, len(in))
	for cat, byArg := range in {
		out[cat] = map[string][]Task{}
		for arg, list := range byArg {
			vv := make([]Task, len(list))
			copy(vv, list)
			out[cat][arg] = vv
		}
	}
	return out
}
func (t Task) UpdatedToday(loc *time.Location) bool {
	tm := parseTaskTime(t.UpdateTime, loc)
	if tm.IsZero() {
		return false
	}
	now := time.Now().In(loc)
	y1, m1, d1 := tm.Date()
	y2, m2, d2 := now.Date()
	return y1 == y2 && m1 == m2 && d1 == d2
}
func (t *Task) MarkToday(loc *time.Location) {
	if loc == nil {
		loc = time.Local
	}
	now := time.Now().In(loc)
	t.UpdateTime = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).Format(time.RFC3339)
}
func parseTaskTime(s string, loc *time.Location) time.Time {
	if strings.TrimSpace(s) == "" {
		return time.Time{}
	}
	layouts := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"}
	for _, layout := range layouts {
		if tm, err := time.Parse(layout, s); err == nil {
			if loc != nil {
				return tm.In(loc)
			}
			return tm
		}
		if loc != nil {
			if tm, err := time.ParseInLocation(layout, s, loc); err == nil {
				return tm
			}
		}
	}
	return time.Time{}
}

func parseTitle(cat, title, row string) (string, string, int) {
	var name, original string
	relased := 0
	if movieCategory(cat) {
		if g := movieMainRe.FindStringSubmatch(title); len(g) > 5 {
			name = strings.TrimSpace(g[1])
			original = strings.TrimSpace(g[3])
			relased, _ = strconv.Atoi(strings.TrimSpace(g[5]))
		} else if g := movieShortRe.FindStringSubmatch(title); len(g) > 2 {
			name = strings.TrimSpace(g[1])
			relased, _ = strconv.Atoi(strings.TrimSpace(g[2]))
		}
	} else if cat == "45" || cat == "22" {
		if strings.Contains(row, "сезон") {
			if g := rusSeasonRe.FindStringSubmatch(title); len(g) > 4 {
				name = strings.TrimSpace(g[1])
				relased, _ = strconv.Atoi(strings.TrimSpace(g[4]))
			}
		} else if g := rusSeriesRe.FindStringSubmatch(title); len(g) > 4 {
			name = strings.TrimSpace(g[1])
			relased, _ = strconv.Atoi(strings.TrimSpace(g[4]))
		}
	} else if cat == "46" || cat == "21" || cat == "20" {
		if strings.Contains(row, "сезон") {
			if g := forSeasonRe.FindStringSubmatch(title); len(g) > 5 {
				name = strings.TrimSpace(g[1])
				original = strings.TrimSpace(g[4])
				relased, _ = strconv.Atoi(strings.TrimSpace(g[5]))
			}
		} else if g := forSeriesRe.FindStringSubmatch(title); len(g) > 5 {
			name = strings.TrimSpace(g[1])
			original = strings.TrimSpace(g[4])
			relased, _ = strconv.Atoi(strings.TrimSpace(g[5]))
		} else if g := forShortRe.FindStringSubmatch(title); len(g) > 3 {
			name = strings.TrimSpace(g[1])
			original = strings.TrimSpace(g[2])
			relased, _ = strconv.Atoi(strings.TrimSpace(g[3]))
		}
	} else if cat == "49" || cat == "50" {
		if g := tvMainRe.FindStringSubmatch(title); len(g) > 4 {
			name = strings.TrimSpace(g[1])
			original = strings.TrimSpace(g[3])
			relased, _ = strconv.Atoi(strings.TrimSpace(g[4]))
		} else if g := tvShortRe.FindStringSubmatch(title); len(g) > 3 {
			name = strings.TrimSpace(g[1])
			relased, _ = strconv.Atoi(strings.TrimSpace(g[3]))
		}
	}
	return strings.TrimSpace(name), strings.TrimSpace(original), relased
}
func movieCategory(cat string) bool {
	switch cat {
	case "8", "6", "15", "17", "35", "39", "13", "14", "24", "11", "9", "47", "18", "37", "12", "10", "7", "16":
		return true
	default:
		return false
	}
}
func categoryTypes(cat string) []string {
	switch cat {
	case "8", "6", "15", "17", "35", "39", "13", "14", "24", "11", "9", "47", "18", "37", "12", "10", "7", "16":
		return []string{"movie"}
	case "45", "46":
		return []string{"serial"}
	case "49", "50":
		return []string{"tvshow"}
	case "21", "22":
		return []string{"multfilm", "multserial"}
	case "20":
		return []string{"anime"}
	default:
		return nil
	}
}
func fallbackName(title string) string {
	parts := fallbackSplitRe.Split(title, -1)
	if len(parts) == 0 {
		return strings.TrimSpace(title)
	}
	return strings.TrimSpace(parts[0])
}
func requestHost(cfg app.TrackerSettings) string {
	if strings.TrimSpace(cfg.Alias) != "" {
		return strings.TrimSpace(cfg.Alias)
	}
	return strings.TrimSpace(cfg.Host)
}
func replaceBadNames(s string) string {
	return strings.NewReplacer("Ванда/Вижн ", "ВандаВижн ", "Ё", "Е", "ё", "е").Replace(s)
}
func parseCreateTime(line, format string) time.Time {
	line = strings.ToLower(strings.TrimSpace(line))
	for _, p := range monthSubsts {
		line = p.re.ReplaceAllString(line, p.to)
	}
	if dateLeadingZero.MatchString(line) {
		line = "0" + line
	}
	layouts := []string{format, "02.01.2006 15:04:05", "02.01.2006 15:04", "02.01.2006"}
	for _, layout := range layouts {
		if tm, err := time.ParseInLocation(layout, line, time.Local); err == nil {
			return tm
		}
	}
	return time.Time{}
}
func fileTime(t filedb.TorrentDetails) time.Time {
	if tm, ok := t["updateTime"].(time.Time); ok {
		return tm
	}
	return time.Now()
}
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
func isDisabled(list []string, name string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), name) {
			return true
		}
	}
	return false
}
func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case fmt.Stringer:
		return x.String()
	case nil:
		return ""
	default:
		if v == nil {
			return ""
		}
		return fmt.Sprint(v)
	}
}
func asInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int32:
		return int(x)
	case int64:
		return int(x)
	case float64:
		return int(x)
	case json.Number:
		i, _ := x.Int64()
		return int(i)
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(x))
		return i
	default:
		i, _ := strconv.Atoi(strings.TrimSpace(fmt.Sprint(v)))
		return i
	}
}
