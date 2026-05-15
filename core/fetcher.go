package core

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"jacred/app"

	flaresolverr "github.com/trinity-aml/flaresolverr-go/server"
)

// Shared flaresolverr-go service singleton (one Chrome for all parsers).
var (
	flareSvcOnce sync.Once
	flareSvc     *flaresolverr.Service
	flareSvcCfg  app.FlareSolverrGoConfig
	xvfbCmd      *exec.Cmd

	// Pinned Chrome for Testing paths, populated by InitFlareService when
	// cfg.ChromeVersion is set. Empty means "use whatever the library finds".
	pinnedBrowserPath string
	pinnedDriverPath  string

	// Concurrency coordination for Chrome solves (flaresolverr-go spawns an
	// ephemeral Chrome per ControllerV1 call without Session).
	// flareSolveSem caps total concurrent Chrome instances across all domains.
	flareSolveSem = make(chan struct{}, 2)

	// flareDomainLocks serializes solves per domain: concurrent callers for the
	// same domain wait for the first solve and then reuse the cached cookies
	// instead of each spawning Chrome. lastUsed tracks recency; entries idle
	// longer than flareDomainLockTTL are swept opportunistically.
	// Sized to fit all configured trackers without rehash at startup.
	flareDomainMu       sync.Mutex
	flareDomainLocks    = make(map[string]*domainLockEntry, 32)
	flareDomainSweepCnt int

	// flareSolveWG tracks in-flight solves so CloseFlareService can wait for them.
	flareSolveWG  sync.WaitGroup
	flareInflight atomic.Int32
	flareShutdown atomic.Bool

	// Circuit breaker: timestamps of last failed solve per domain. Callers that
	// retry immediately after a 90s timeout get a fast 503 instead of spawning
	// another Chrome that almost certainly times out again.
	flareFailMu   sync.RWMutex
	flareLastFail = make(map[string]time.Time, 32)

	// flareLastChallenge records when fetchViaFlare's standard-HTTP probe last
	// saw a CF challenge for a domain. While the entry is within probeTTL we
	// skip the probe and go straight to solveFlare — saving one wasted HTTP
	// roundtrip per cached-session rollover (~60 min) on still-protected
	// sites. Cleared when a probe returns a non-challenge body, which
	// signals CF has been lifted server-side.
	flareChallengeMu  sync.RWMutex
	flareLastChallenge = make(map[string]time.Time, 32)

	// Idle-session reaper: destroy flaresolverr-go browser sessions that
	// haven't been used for flareSessionIdleTTL so Camoufox doesn't sit in
	// RAM between cron runs. One session ≈ 800–1000 MB resident; without
	// reaping, every domain we solve once pins a browser until jacred exits.
	flareLastUsedMu sync.Mutex
	flareLastUsed   = make(map[string]time.Time, 8)

	flareReaperStop chan struct{}
	flareReaperDone chan struct{}
)

const (
	flareFailCooldown   = 3 * time.Minute
	flareSessionIdleTTL = 5 * time.Minute
	flareReaperInterval = 1 * time.Minute
	// flareProbeTTL bounds how long we trust a "domain showed a CF challenge
	// recently" mark. 6h is long enough to skip probes during a typical
	// parse cycle yet short enough that a CF removal is picked up within a
	// few hours of the next cron run.
	flareProbeTTL = 6 * time.Hour

	// Single flaresolverr-go session shared by all CF-gated parsers. One
	// Camoufox process hosts navigations for every domain (Firefox isolates
	// cookies per origin in the shared jar, so no cross-contamination).
	// Trades parallelism — parsers serialize through the session's mutex —
	// for ~80% less RAM during parsing peaks. The cookie-cache fast path in
	// fetchViaFlare keeps the session out of the critical path for
	// HTTP-friendly sites (1 solve per domain, then plain HTTP in parallel).
	flareSharedSessionID = "jacred-shared"
)

// InitFlareService initializes the shared Xvfb display and flaresolverr-go config. Call once at startup.
func InitFlareService(cfg app.FlareSolverrGoConfig) {
	flareSvcCfg = cfg
	// Sweep leftover browser profiles from prior crashes (SIGKILL, panic).
	cleanupStaleProfiles()
	// Pin Chrome for Testing to a specific version if requested. Avoids
	// driver/browser major-version mismatches.
	if v := strings.TrimSpace(cfg.ChromeVersion); v != "" {
		log.Printf("chrome-pin: ensuring Chrome for Testing %s", v)
		bp, dp, err := EnsureChromeVersion(v)
		if err != nil {
			log.Printf("chrome-pin: %v (falling back to system Chrome)", err)
		} else {
			pinnedBrowserPath = bp
			pinnedDriverPath = dp
			log.Printf("chrome-pin: chrome=%s driver=%s", bp, dp)
		}
	}
	// Auto-download Camoufox when geckodriver backend is selected without an
	// explicit browser_path. Avoids a manual ~680 MB install step and keeps
	// CF-gated parsers working out of the box on first run.
	if strings.TrimSpace(flareSvcCfg.BrowserPath) == "" &&
		strings.EqualFold(strings.TrimSpace(flareSvcCfg.BrowserBackend), "geckodriver") {
		if p, err := EnsureCamoufox(); err != nil {
			log.Printf("camoufox: %v (flaresolverr-go will try its own discovery)", err)
		} else {
			flareSvcCfg.BrowserPath = p
			log.Printf("camoufox: using %s", p)
		}
	}
	// Start persistent Xvfb if no DISPLAY and not on a desktop
	if os.Getenv("DISPLAY") == "" {
		startXvfb()
	}

	// Start idle-session reaper.
	flareReaperStop = make(chan struct{})
	flareReaperDone = make(chan struct{})
	go reapIdleSessions()
}

// markSessionUsed records when a session was last accessed for the idle reaper.
// Keyed by flaresolverr-go session ID (not by domain) because multiple domains
// may share one browser session under flareSharedSessionID.
func markSessionUsed(sessionID string) {
	flareLastUsedMu.Lock()
	flareLastUsed[sessionID] = time.Now()
	flareLastUsedMu.Unlock()
}

// reapIdleSessions destroys flaresolverr-go sessions that have been idle for
// longer than flareSessionIdleTTL. The browser is recreated on the next solve.
// Cookies in flareCache / on-disk persist stay untouched: they remain valid
// for their own TTL and let HTTP+cookies fast-path keep working.
func reapIdleSessions() {
	defer close(flareReaperDone)
	ticker := time.NewTicker(flareReaperInterval)
	defer ticker.Stop()
	for {
		select {
		case <-flareReaperStop:
			return
		case <-ticker.C:
			if flareShutdown.Load() {
				return
			}
			svc := flareSvc
			if svc == nil {
				continue
			}
			cutoff := time.Now().Add(-flareSessionIdleTTL)
			var idle []string
			flareLastUsedMu.Lock()
			for sessionID, last := range flareLastUsed {
				if last.Before(cutoff) {
					idle = append(idle, sessionID)
					delete(flareLastUsed, sessionID)
				}
			}
			flareLastUsedMu.Unlock()
			for _, sessionID := range idle {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				resp, _ := svc.ControllerV1(ctx, &flaresolverr.V1Request{
					Cmd:     "sessions.destroy",
					Session: sessionID,
				})
				cancel()
				if resp.Status == "ok" {
					log.Printf("flaresolverr: reaped idle session %s", sessionID)
				}
			}
		}
	}
}

// CloseFlareService shuts down the shared flaresolverr-go service and Xvfb. Call on shutdown.
func CloseFlareService() {
	flareShutdown.Store(true)

	// Stop idle reaper first so it doesn't race with Close().
	if flareReaperStop != nil {
		close(flareReaperStop)
		select {
		case <-flareReaperDone:
		case <-time.After(2 * time.Second):
		}
		flareReaperStop = nil
	}

	// Wait for in-flight solves to finish their defer client.Close() so Chrome
	// processes are terminated before we proceed with service shutdown.
	done := make(chan struct{})
	go func() {
		flareSolveWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		log.Printf("flaresolverr: shutdown timeout, %d solves still in flight", flareInflight.Load())
	}

	if flareSvc != nil {
		flareSvc.Close()
	}
	stopXvfb()
	cleanupStaleProfiles()
}

// domainLockEntry pairs the per-domain mutex with a lastUsed timestamp so
// the registry can shed long-idle domains. Without this, the map grows
// monotonically with every distinct domain ever encountered and never
// shrinks for the lifetime of the process.
type domainLockEntry struct {
	mu       *sync.Mutex
	lastUsed time.Time
}

// flareDomainLockTTL is how long an idle domain entry is kept around. Set
// generously past flareSessionTTL so a sweeper running between solves never
// evicts a domain whose cached session is still alive.
const flareDomainLockTTL = 2 * flareSessionTTL

// getDomainLock returns the mutex that serializes solves for a given
// domain. The just-touched entry's lastUsed bump happens before the
// opportunistic sweep, so a fresh-acquired entry can never be evicted by
// the same call — preventing the "caller A holds pointer, sweep evicts,
// caller C creates new pointer, mutual exclusion lost" race.
func getDomainLock(domain string) *sync.Mutex {
	flareDomainMu.Lock()
	defer flareDomainMu.Unlock()
	e, ok := flareDomainLocks[domain]
	if !ok {
		e = &domainLockEntry{mu: &sync.Mutex{}}
		flareDomainLocks[domain] = e
	}
	e.lastUsed = time.Now()

	flareDomainSweepCnt++
	if flareDomainSweepCnt%64 == 0 {
		cutoff := time.Now().Add(-flareDomainLockTTL)
		for k, ent := range flareDomainLocks {
			if k == domain || ent.lastUsed.After(cutoff) {
				continue
			}
			// Defensive: never evict an entry whose mutex is currently
			// held — TryLock failing means a goroutine is mid-solve.
			if ent.mu.TryLock() {
				ent.mu.Unlock()
				delete(flareDomainLocks, k)
			}
		}
	}
	return e.mu
}

// flareCooldownRemaining reports whether a recent solve for this domain failed
// and how much cooldown time is left.
func flareCooldownRemaining(domain string) (time.Duration, bool) {
	flareFailMu.RLock()
	last, ok := flareLastFail[domain]
	flareFailMu.RUnlock()
	if !ok {
		return 0, false
	}
	elapsed := time.Since(last)
	if elapsed >= flareFailCooldown {
		return 0, false
	}
	return flareFailCooldown - elapsed, true
}

func markFlareFailure(domain string) {
	flareFailMu.Lock()
	flareLastFail[domain] = time.Now()
	flareFailMu.Unlock()
}

func clearFlareFailure(domain string) {
	flareFailMu.Lock()
	delete(flareLastFail, domain)
	flareFailMu.Unlock()
}

// recentChallengeSeen reports whether the standard-HTTP probe last saw a CF
// challenge for this domain within flareProbeTTL.
func recentChallengeSeen(domain string) bool {
	flareChallengeMu.RLock()
	t, ok := flareLastChallenge[domain]
	flareChallengeMu.RUnlock()
	return ok && time.Since(t) < flareProbeTTL
}

func markChallengeSeen(domain string) {
	flareChallengeMu.Lock()
	flareLastChallenge[domain] = time.Now()
	flareChallengeMu.Unlock()
}

// clearChallengeSeen drops the mark when the probe stopped seeing challenges,
// signalling CF was removed (or never gated this URL).
func clearChallengeSeen(domain string) {
	flareChallengeMu.Lock()
	_, existed := flareLastChallenge[domain]
	delete(flareLastChallenge, domain)
	flareChallengeMu.Unlock()
	if existed {
		log.Printf("flaresolverr: %s served direct HTTP without CF challenge — consider fetchmode: standard in config", domain)
	}
}

// cleanupStaleProfiles removes /tmp/flaresolverr-go-profile-* directories left
// behind by crashed Chrome processes. Safe to call at startup and shutdown.
func cleanupStaleProfiles() {
	pattern := filepath.Join(os.TempDir(), "flaresolverr-go-profile-*")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return
	}
	removed := 0
	for _, p := range matches {
		if err := os.RemoveAll(p); err == nil {
			removed++
		}
	}
	if removed > 0 {
		log.Printf("flaresolverr: removed %d stale profile dirs", removed)
	}
}

// startXvfb starts a persistent Xvfb on the first free display :99-:119.
// Sets DISPLAY env so flaresolverr-go uses it instead of spawning its own.
func startXvfb() {
	xvfbPath, err := exec.LookPath("Xvfb")
	if err != nil {
		log.Printf("xvfb: not found, flaresolverr-go will use headless Chrome")
		return
	}

	for displayNum := 99; displayNum < 120; displayNum++ {
		socketPath := fmt.Sprintf("/tmp/.X11-unix/X%d", displayNum)

		// Clean up stale socket if no process owns it
		if _, err := os.Stat(socketPath); err == nil {
			os.Remove(socketPath)
		}

		display := fmt.Sprintf(":%d", displayNum)
		cmd := exec.Command(xvfbPath, display, "-screen", "0", "1920x1080x24", "-nolisten", "tcp")
		cmd.Stdout = nil
		cmd.Stderr = nil
		if err := cmd.Start(); err != nil {
			continue
		}

		// Wait for socket to appear
		ok := false
		for i := 0; i < 50; i++ {
			if _, err := os.Stat(socketPath); err == nil {
				ok = true
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !ok {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			continue
		}

		xvfbCmd = cmd
		os.Setenv("DISPLAY", display)
		log.Printf("xvfb: started on %s (pid %d)", display, cmd.Process.Pid)
		return
	}

	log.Printf("xvfb: failed to start on any display :99-:119")
}

func stopXvfb() {
	if xvfbCmd != nil && xvfbCmd.Process != nil {
		_ = xvfbCmd.Process.Kill()
		_, _ = xvfbCmd.Process.Wait()
		log.Printf("xvfb: stopped")
		xvfbCmd = nil
	}
}

func getFlareService() *flaresolverr.Service {
	flareSvcOnce.Do(func() {
		flareCfg := flaresolverr.Config{
			Headless: true,
			// Let the library fetch a matching chromedriver on first use and
			// cache it. Without this the library's withDefaults() leaves it
			// false and logs `chromedriver not found; falling back to chromedp`
			// on every solve.
			DriverAutoDownload: true,
		}
		// Pinned Chrome for Testing wins over config BrowserPath.
		if pinnedBrowserPath != "" {
			flareCfg.BrowserPath = pinnedBrowserPath
			flareCfg.DriverPath = pinnedDriverPath
			// With a pinned driver we don't want the library to auto-download
			// a mismatched one.
			flareCfg.DriverAutoDownload = false
		} else if flareSvcCfg.BrowserPath != "" {
			flareCfg.BrowserPath = flareSvcCfg.BrowserPath
		}
		if flareSvcCfg.DriverPath != "" {
			flareCfg.DriverPath = flareSvcCfg.DriverPath
			flareCfg.DriverAutoDownload = false
		}
		if flareSvcCfg.BrowserBackend != "" {
			flareCfg.BrowserBackend = flareSvcCfg.BrowserBackend
		}
		if flareSvcCfg.Headless != nil {
			flareCfg.Headless = *flareSvcCfg.Headless
		}
		flareSvc = flaresolverr.NewService(flareCfg)
		log.Printf("fetcher: flaresolverr-go service initialized")
	})
	return flareSvc
}

// Fetcher provides unified HTTP fetching with configurable mode per tracker.
type Fetcher struct {
	cfg       app.Config
	stdClient *http.Client

	// Cookie cache: domain -> cached session
	flareMu    sync.RWMutex
	flareCache map[string]*flareSession

	// Reusable http.Client cache keyed by *http.Transport. Transports are
	// produced by core.TransportForURL which caches them by (proxy, insecure)
	// shape — so the pool here ends up with one client per distinct
	// transport, sharing keep-alive connections across all requests.
	clientMu   sync.RWMutex
	clientPool map[*http.Transport]*http.Client
}

// flareSession holds cookies obtained from flaresolverr for a domain.
// browserID is the flaresolverr-go session ID that keeps a Chrome instance
// alive between solves for this domain — reusing it avoids the ~10s Chrome
// startup on every URL. Not persisted to disk: session IDs belong to running
// Chrome processes and don't survive a restart.
type flareSession struct {
	cookies   string
	userAgent string
	browserID string
	obtained  time.Time
}

// cf_clearance cookies from managed challenges typically live 30–120 minutes.
// We conservatively reuse them for up to an hour before forcing a re-solve.
const flareSessionTTL = 60 * time.Minute

// NewFetcher creates a Fetcher with standard HTTP and embedded flaresolverr-go backends.
// Previously-solved sessions written to SetFlarePersistDir are rehydrated so
// quick restarts don't trigger a fresh Chrome solve for every tracker.
func NewFetcher(cfg app.Config) *Fetcher {
	return &Fetcher{
		cfg:        cfg,
		stdClient:  &http.Client{Timeout: 30 * time.Second},
		flareCache: loadFlareSessions(flareSessionTTL),
		clientPool: make(map[*http.Transport]*http.Client),
	}
}

// clientForTransport returns a reusable *http.Client wrapping the supplied
// transport. The default (transport==nil) uses the shared stdClient which
// rides http.DefaultTransport's pool. Custom transports get their own cached
// client so each (proxy, tls) combination keeps its keep-alive connections
// across requests instead of throwing them away per-call.
func (f *Fetcher) clientForTransport(t *http.Transport) *http.Client {
	if t == nil {
		return f.stdClient
	}
	f.clientMu.RLock()
	c, ok := f.clientPool[t]
	f.clientMu.RUnlock()
	if ok {
		return c
	}
	f.clientMu.Lock()
	defer f.clientMu.Unlock()
	if c, ok := f.clientPool[t]; ok {
		return c
	}
	c = &http.Client{Timeout: 30 * time.Second, Transport: t}
	f.clientPool[t] = c
	return c
}

// UpdateConfig updates the fetcher's config (for hot-reload).
func (f *Fetcher) UpdateConfig(cfg app.Config) {
	f.cfg = cfg
}

// GetFlareCookies returns cached or freshly solved flaresolverr cookies and user-agent for a URL's domain.
func (f *Fetcher) GetFlareCookies(rawURL string) (cookie, userAgent string) {
	domain := extractDomain(rawURL)
	if sess := f.getFlareSession(domain); sess != nil {
		return sess.cookies, sess.userAgent
	}
	sess, _, err := f.solveFlare(rawURL, domain, false)
	if err != nil {
		return "", ""
	}
	return sess.cookies, sess.userAgent
}

// PeekFlareCookies returns cached flaresolverr cookies + UA for a URL's domain
// without triggering a solve. Use this when a parser wants to piggyback on
// another parser's solved session (e.g. bitruapi reusing bitru's cf_clearance)
// without initiating its own CF-bypass flow. Returns ok=false when there's no
// valid cached session.
func (f *Fetcher) PeekFlareCookies(rawURL string) (cookie, userAgent string, ok bool) {
	sess := f.getFlareSession(extractDomain(rawURL))
	if sess == nil {
		return "", "", false
	}
	return sess.cookies, sess.userAgent, true
}

// InvalidateSession clears cached flaresolverr cookies for a URL's domain.
func (f *Fetcher) InvalidateSession(rawURL string) {
	domain := extractDomain(rawURL)
	f.clearFlareSession(domain)
	log.Printf("flaresolverr: session invalidated for %s", domain)
}

// FetchResult holds the response from a fetch operation.
type FetchResult struct {
	Body       []byte
	StatusCode int
}

// Get fetches a URL using the mode specified in tracker settings.
// Modes: "standard" (default), "flaresolverr".
func (f *Fetcher) Get(rawURL string, tracker app.TrackerSettings) (*FetchResult, error) {
	return f.GetExt(rawURL, tracker, "", "")
}

// GetExt is like Get but lets callers merge an extra runtime cookie
// (e.g. from a parser's own login flow) and override the User-Agent used for
// standard-mode requests. Empty strings keep defaults. Flare-mode always uses
// the browser's User-Agent regardless of the userAgent argument, since the
// cookie is bound to the UA that solved the challenge.
func (f *Fetcher) GetExt(rawURL string, tracker app.TrackerSettings, extraCookie, userAgent string) (*FetchResult, error) {
	if strings.TrimSpace(extraCookie) != "" {
		tracker.Cookie = MergeCookieStrings(tracker.Cookie, extraCookie)
	}

	mode := strings.ToLower(strings.TrimSpace(tracker.FetchMode))
	if mode == "" {
		mode = "standard"
	}

	// Auto-CF: a previous standard fetch on this domain returned a CF
	// challenge. Skip the wasted request and go straight to flare.
	domain := extractDomain(rawURL)
	if mode != "flaresolverr" && isDomainCFAuto(domain) {
		mode = "flaresolverr"
	}

	cookie := strings.TrimSpace(tracker.Cookie)
	transport := TransportForURL(rawURL, tracker.UseProxy, tracker.InsecureSkipVerify, f.cfg)

	switch mode {
	case "flaresolverr":
		return f.fetchViaFlare(rawURL, cookie, nil, transport)
	default:
		ua := userAgent
		if strings.TrimSpace(ua) == "" {
			ua = "Mozilla/5.0"
		}
		res, err := f.doHTTP(http.MethodGet, rawURL, cookie, ua, "", nil, nil, transport)
		if err != nil {
			return res, err
		}
		// Auto-detect: if the standard response is a CF interstitial, flag the
		// domain and transparently retry via flare. The retry result is what
		// the caller actually wanted; subsequent calls skip the doomed standard
		// roundtrip via the isDomainCFAuto check above.
		if isCloudflareChallenge(res.Body) {
			markDomainCF(domain)
			return f.fetchViaFlare(rawURL, cookie, nil, transport)
		}
		return res, nil
	}
}

// GetString is a convenience wrapper that returns body as string.
func (f *Fetcher) GetString(rawURL string, tracker app.TrackerSettings) (string, int, error) {
	return f.GetStringExt(rawURL, tracker, "", "")
}

// GetStringExt is the string-result variant of GetExt.
func (f *Fetcher) GetStringExt(rawURL string, tracker app.TrackerSettings, extraCookie, userAgent string) (string, int, error) {
	res, err := f.GetExt(rawURL, tracker, extraCookie, userAgent)
	if err != nil {
		return "", 0, err
	}
	return string(res.Body), res.StatusCode, nil
}

// Download fetches raw bytes (for torrent files, etc).
func (f *Fetcher) Download(rawURL string, tracker app.TrackerSettings) ([]byte, int, error) {
	return f.DownloadExt(rawURL, tracker, "", "")
}

// DownloadExt is the byte-result variant of GetExt.
func (f *Fetcher) DownloadExt(rawURL string, tracker app.TrackerSettings, extraCookie, userAgent string) ([]byte, int, error) {
	res, err := f.GetExt(rawURL, tracker, extraCookie, userAgent)
	if err != nil {
		return nil, 0, err
	}
	return res.Body, res.StatusCode, nil
}

// FetchOptions configures Fetcher.Do. Empty fields use sensible defaults.
type FetchOptions struct {
	Method       string            // "GET" (default), "POST", etc.
	Body         []byte            // request body (for POST/PUT)
	ContentType  string            // Content-Type for the body
	ExtraCookie  string            // merged with tracker.Cookie
	UserAgent    string            // overrides the default UA (standard mode)
	ExtraHeaders map[string]string // additional request headers
}

// Do sends a request honoring opts. Routes through standard HTTP or
// flaresolverr per tracker.FetchMode.
//
//   - GET in flare mode delegates to the browser-rendered fast path
//     (cached cf_clearance → standard HTTP, fallback to full browser fetch).
//     ExtraHeaders are honored on the cached-cookies HTTP roundtrip (the
//     fast path), but NOT during the browser solve itself — the headless
//     browser controls its own headers when it actually navigates.
//   - POST in flare mode obtains cf_clearance via cached or freshly-solved
//     browser session, then sends the actual POST via standard HTTP with
//     those cookies + the browser's User-Agent. The body itself never goes
//     through the headless browser.
//   - POST in standard mode is a plain net/http POST with the supplied body,
//     content-type, cookies, UA, and extra headers.
func (f *Fetcher) Do(rawURL string, tracker app.TrackerSettings, opts FetchOptions) (*FetchResult, error) {
	method := strings.ToUpper(strings.TrimSpace(opts.Method))
	if method == "" {
		method = http.MethodGet
	}

	if strings.TrimSpace(opts.ExtraCookie) != "" {
		tracker.Cookie = MergeCookieStrings(tracker.Cookie, opts.ExtraCookie)
	}

	mode := strings.ToLower(strings.TrimSpace(tracker.FetchMode))
	if mode == "" {
		mode = "standard"
	}

	// Auto-CF: domain previously observed serving a challenge — go through
	// flare from the start instead of wasting a request.
	domain := extractDomain(rawURL)
	if mode != "flaresolverr" && isDomainCFAuto(domain) {
		mode = "flaresolverr"
	}

	cookie := strings.TrimSpace(tracker.Cookie)
	transport := TransportForURL(rawURL, tracker.UseProxy, tracker.InsecureSkipVerify, f.cfg)

	// GET in flare mode goes through the full browser-aware path so it can
	// re-solve on stale cookies or fall back to a browser-rendered body.
	// Non-GET can't use that path because the browser only navigates.
	if mode == "flaresolverr" && method == http.MethodGet {
		return f.fetchViaFlare(rawURL, cookie, opts.ExtraHeaders, transport)
	}

	ua := opts.UserAgent
	if mode == "flaresolverr" {
		// Non-GET flare path: piggyback on cached cf_clearance, solving on the
		// origin if needed. The actual request goes via standard HTTP.
		flareCookie, flareUA := f.GetFlareCookies(rawURL)
		if flareCookie != "" {
			// Login PHPSESSID in `cookie` must win over flare's guest PHPSESSID.
			// cf_clearance from flare has no name conflict and is still added.
			cookie = mergeCookies(flareCookie, cookie)
		}
		if strings.TrimSpace(ua) == "" && flareUA != "" {
			ua = flareUA
		}
	}
	if strings.TrimSpace(ua) == "" {
		ua = "Mozilla/5.0"
	}

	res, err := f.doHTTP(method, rawURL, cookie, ua, opts.ContentType, opts.Body, opts.ExtraHeaders, transport)
	if err != nil {
		return res, err
	}
	// Auto-detect: standard-mode response is a CF interstitial. Flag the
	// domain and retry through flare. Safe to retry POST because CF rejects
	// before forwarding to the upstream API — the original request had no
	// effect. Future calls take the fast path via isDomainCFAuto above.
	if mode != "flaresolverr" && isCloudflareChallenge(res.Body) {
		markDomainCF(domain)
		if method == http.MethodGet {
			return f.fetchViaFlare(rawURL, cookie, opts.ExtraHeaders, transport)
		}
		flareCookie, flareUA := f.GetFlareCookies(rawURL)
		if flareCookie != "" {
			cookie = mergeCookies(flareCookie, cookie)
		}
		retryUA := opts.UserAgent
		if strings.TrimSpace(retryUA) == "" {
			retryUA = flareUA
		}
		if strings.TrimSpace(retryUA) == "" {
			retryUA = "Mozilla/5.0"
		}
		return f.doHTTP(method, rawURL, cookie, retryUA, opts.ContentType, opts.Body, opts.ExtraHeaders, transport)
	}
	return res, nil
}

// doHTTP performs an HTTP request honoring the caller's method, body,
// content-type, cookie, UA, and any extra headers. Body is capped at 5 MiB.
func (f *Fetcher) doHTTP(method, rawURL, cookie, userAgent, contentType string, body []byte, extraHeaders map[string]string, transport *http.Transport) (*FetchResult, error) {
	if strings.TrimSpace(method) == "" {
		method = http.MethodGet
	}
	client := f.clientForTransport(transport)
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, bodyReader)
	if err != nil {
		return nil, err
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, err
	}
	return &FetchResult{Body: data, StatusCode: resp.StatusCode}, nil
}

// fetchViaFlare uses embedded flaresolverr-go to solve CF and fetch pages.
// Strategy:
//   1. Try cached cookies with standard HTTP (fast path)
//   2. No cached cookies → probe with plain HTTP first. Some trackers
//      configured as fetchmode: flaresolverr have since dropped CF (e.g.
//      megapeer); the probe returns valid HTML, we skip a 10s+ Chrome
//      solve. If the probe is a challenge / 403, fall through to step 3.
//   3. Solve CF challenge on the site's origin via flaresolverr-go browser
//      (not the deep URL — some sites serve a managed challenge on deep
//      URLs that automation can't pass)
//   4. Use the resulting cookies to fetch the actual deep URL
//   5. Cache cookies for subsequent requests
func (f *Fetcher) fetchViaFlare(rawURL, cookie string, extraHeaders map[string]string, transport *http.Transport) (*FetchResult, error) {
	domain := extractDomain(rawURL)
	httpCookiesFailed := false

	if sess := f.getFlareSession(domain); sess != nil {
		res, err := f.fetchWithCookies(rawURL, cookie, sess, extraHeaders, transport)
		// CF re-challenge returns 200 with challenge HTML — status-only check is
		// not enough, inspect the body too.
		if err == nil && res.StatusCode != 403 && !isCloudflareChallenge(res.Body) {
			return res, nil
		}
		// Only drop the cached session when cookies are actually stale
		// (challenge markers in body). A plain 403 typically means CF WAF is
		// blocking our raw HTTP client's IP/fingerprint — cookies are still
		// valid, we just can't use them without the browser.
		if err == nil && isCloudflareChallenge(res.Body) {
			f.clearFlareSession(domain)
		}
		// Force the browser to actually navigate this URL; reusing the cached
		// session we just tried would send us straight back here.
		httpCookiesFailed = true
	} else if !recentChallengeSeen(domain) {
		// No cached flare session, and the last probe within flareProbeTTL
		// did NOT see a challenge — probe again. Skips the wasted probe on
		// still-protected sites where we already know CF is up.
		if probe, err := f.doHTTP(http.MethodGet, rawURL, cookie, "Mozilla/5.0", "", nil, extraHeaders, transport); err == nil {
			if probe.StatusCode != 403 && !isCloudflareChallenge(probe.Body) {
				// CF appears inactive — skip the Chrome solve, use direct body.
				clearChallengeSeen(domain)
				return probe, nil
			}
			// Challenge confirmed: remember so the next caller within TTL
			// skips this probe and dives straight to solveFlare.
			markChallengeSeen(domain)
		}
		// On HTTP error fall through to solveFlare without marking — a
		// transient network issue shouldn't lock us out of the probe path.
	}

	// Solve via flaresolverr-go browser. For non-binary URLs the browser
	// navigates to rawURL itself, so its rendered Response is the page we
	// actually want — use it directly when available.
	sess, direct, err := f.solveFlare(rawURL, domain, httpCookiesFailed)
	if err != nil {
		log.Printf("flaresolverr: solve failed for %s: %v", domain, err)
		// No doHTTP fallback: fetchmode=flaresolverr means the site is CF-gated,
		// a plain HTTP request is guaranteed to return 403 + challenge page and
		// just wastes a request. Return 503 so parsers' retry logic can kick in.
		return &FetchResult{Body: nil, StatusCode: 503}, nil
	}

	if direct != nil && !isCloudflareChallenge(direct.Body) {
		return direct, nil
	}

	// Retry via cookies. If that still returns challenge, fall back to the
	// browser-rendered body (even if direct == nil above, because e.g. the
	// binary-download redirect stripped it).
	res, err := f.fetchWithCookies(rawURL, cookie, sess, extraHeaders, transport)
	if err == nil && res.StatusCode != 403 && !isCloudflareChallenge(res.Body) {
		return res, nil
	}
	if direct != nil {
		return direct, nil
	}
	return res, err
}

// isCloudflareChallenge returns true if the response body is a CF interstitial
// (managed challenge / turnstile / "Just a moment...") rather than the real page.
// Used to invalidate stale cf_clearance and trigger a fresh solve.
//
// Note: `/cdn-cgi/challenge-platform/` alone is NOT a challenge marker — CF
// injects its JSD/RUM script on every proxied page. We match only on markers
// that appear exclusively on interstitials.
func isCloudflareChallenge(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	// Cap the scan to keep things fast on large real pages.
	scan := body
	if len(scan) > 64*1024 {
		scan = scan[:64*1024]
	}
	s := string(scan)
	return strings.Contains(s, "<title>Just a moment") ||
		strings.Contains(s, "window._cf_chl_opt") ||
		strings.Contains(s, "cf-browser-verification") ||
		strings.Contains(s, "Checking your browser before accessing")
}

// fetchWithCookies fetches via standard HTTP with the browser's UA and solved cookies.
// Pass the UA that was used by the browser during solve so CF sees a consistent
// (cookie, UA) pair. Mismatch invalidates cf_clearance and triggers a re-challenge.
func (f *Fetcher) fetchWithCookies(rawURL, cookie string, sess *flareSession, extraHeaders map[string]string, transport *http.Transport) (*FetchResult, error) {
	// Caller's cookie carries the login PHPSESSID and must win over the
	// browser's guest session captured during solve. Unique flare cookies
	// (cf_clearance) still get added — only same-name conflicts flip.
	merged := mergeCookies(sess.cookies, cookie)
	ua := sess.userAgent
	if ua == "" {
		ua = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
	}
	return f.doHTTP(http.MethodGet, rawURL, merged, ua, "", nil, extraHeaders, transport)
}

func (f *Fetcher) getFlareSession(domain string) *flareSession {
	f.flareMu.RLock()
	defer f.flareMu.RUnlock()
	sess, ok := f.flareCache[domain]
	if !ok || time.Since(sess.obtained) > flareSessionTTL {
		return nil
	}
	// A session with no cookies isn't useful — it's a leftover from a solve
	// that returned 0 cookies (CF challenge resolved but no clearance issued,
	// or the geckodriver currentCookies-before-wait bug). Treating it as
	// valid wedges callers into using empty Cookie: headers, which CF then
	// blocks. Pretend the session doesn't exist so solveFlare gets called
	// again and actually populates the jar.
	if strings.TrimSpace(sess.cookies) == "" {
		return nil
	}
	return sess
}

func (f *Fetcher) clearFlareSession(domain string) {
	f.flareMu.Lock()
	delete(f.flareCache, domain)
	f.flareMu.Unlock()
	deleteFlareSession(domain)
}

// solveFlare calls the shared flaresolverr-go service to solve CF challenge.
// Returns cookies cached for future requests on this domain, and — when the
// browser was pointed at rawURL itself (not redirected to the origin for a
// binary download) — a *FetchResult containing the browser-rendered body.
// That direct result lets callers bypass the subsequent HTTP roundtrip on
// CF-aggressive pages where cookies alone trigger a re-challenge.
//
// Coordination:
//   - Rejects new solves once CloseFlareService has begun.
//   - Serializes per domain: concurrent callers for the same domain wait for
//     the first solve, then reuse its cached cookies (see the second cache
//     check below). Prevents N parallel Chrome instances for one CF site.
//   - Caps global concurrency via flareSolveSem to prevent a Chrome swarm
//     when many parsers run simultaneously.
//
// forceRender=true skips the "cached session" short-circuit after acquiring
// the domain lock and always navigates the browser. Callers that have already
// determined cached cookies don't work for this URL (e.g. HTTP+cookies just
// returned a WAF 403) pass true to avoid looping back through the same stale
// fast path.
func (f *Fetcher) solveFlare(rawURL, domain string, forceRender bool) (*flareSession, *FetchResult, error) {
	if flareShutdown.Load() {
		return nil, nil, fmt.Errorf("flaresolverr: service is shutting down")
	}

	svc := getFlareService()
	if svc == nil {
		return nil, nil, fmt.Errorf("flaresolverr service not initialized")
	}

	// Circuit breaker: skip if a recent solve for this domain failed. Retries
	// from parsers within the cooldown window get a fast error instead of
	// spawning Chrome that will almost certainly time out again.
	if remaining, blocked := flareCooldownRemaining(domain); blocked {
		return nil, nil, fmt.Errorf("flaresolverr: %s in cooldown for %s after recent failure", domain, remaining.Round(time.Second))
	}

	dm := getDomainLock(domain)
	dm.Lock()
	defer dm.Unlock()

	// Re-check cache after acquiring domain lock: a prior concurrent caller
	// may have already solved this domain. Skip when forceRender because the
	// caller wants this specific URL rendered by the browser, not just cookies.
	if !forceRender {
		if sess := f.getFlareSession(domain); sess != nil {
			return sess, nil, nil
		}
	}

	// Re-check cooldown under the lock: waiters behind us may have seen the
	// first solver fail, and we'd immediately spawn another Chrome otherwise.
	if remaining, blocked := flareCooldownRemaining(domain); blocked {
		return nil, nil, fmt.Errorf("flaresolverr: %s in cooldown for %s after recent failure", domain, remaining.Round(time.Second))
	}

	// Re-check shutdown: flag may have been raised while we waited for the lock.
	if flareShutdown.Load() {
		return nil, nil, fmt.Errorf("flaresolverr: service is shutting down")
	}

	flareSolveSem <- struct{}{}
	defer func() { <-flareSolveSem }()

	flareSolveWG.Add(1)
	flareInflight.Add(1)
	defer flareSolveWG.Done()
	defer flareInflight.Add(-1)

	// All domains share one persistent browser session; see flareSharedSessionID.
	// Firefox isolates cookies per origin inside the session's jar, so domains
	// don't cross-pollinate. Downside: navigations serialize at the library's
	// per-session mutex. Mitigation: the HTTP+cookies fast path in
	// fetchViaFlare skips the session for sites where cookies alone work.
	browserID := flareSharedSessionID

	log.Printf("flaresolverr: solving challenge for %s", domain)

	// MaxTimeout caps Chrome's solve time inside the library.
	// Bitru + Turnstile sometimes takes >60s on cold webdriver start; 90s is a
	// safer margin. Outer ctx must exceed it.
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	defer cancel()

	// If the URL that triggered us is a binary attachment (.torrent, /download.php
	// etc.), point the browser at the site origin instead. cf_clearance is
	// domain-scoped so it still covers the deep URL, and we avoid Firefox
	// stalling on a save-file flow that never fires a load event.
	solveURL := rawURL
	redirected := false
	if looksLikeBinaryDownload(rawURL) {
		solveURL = originURL(rawURL)
		redirected = true
	}

	resp, _ := svc.ControllerV1(ctx, &flaresolverr.V1Request{
		Cmd:               "request.get",
		URL:               solveURL,
		MaxTimeout:        90000,
		// 8s wait covers slow custom anti-bot JS challenges (e.g. nnmclub's
		// eb927f21fc_* cookies set a ~2s delay + verification). At 2s the
		// browser snapshot caught only Yandex.Metrica cookies, missing both
		// the anti-bot tokens and phpBB anonymous session.
		WaitInSeconds:     8,
		Session:           browserID,
		SessionTTLMinutes: int(flareSessionTTL / time.Minute),
	})
	markSessionUsed(browserID)

	if resp.Status != "ok" {
		markFlareFailure(domain)
		return nil, nil, fmt.Errorf("flaresolverr status=%s message=%s", resp.Status, resp.Message)
	}
	if resp.Solution == nil {
		markFlareFailure(domain)
		return nil, nil, fmt.Errorf("flaresolverr: no solution returned")
	}
	clearFlareFailure(domain)

	// Build cookie string from solution cookies
	cookieParts := make([]string, 0, len(resp.Solution.Cookies))
	for _, c := range resp.Solution.Cookies {
		cookieParts = append(cookieParts, c.Name+"="+c.Value)
	}
	cookieStr := strings.Join(cookieParts, "; ")
	ua := resp.Solution.UserAgent
	log.Printf("flaresolverr: solved %s cookies=%d", domain, len(resp.Solution.Cookies))
	if ua == "" {
		ua = "Mozilla/5.0"
	}

	// Empty cookie set means the browser navigated but never received CF
	// clearance (challenge not actually solved, or solver returned before
	// cookies were persisted). Caching this as a valid session creates a
	// permanent stall: every subsequent GetFlareCookies returns ("", ua)
	// and parsers loop on re-login. Treat it as a transient failure so
	// the next caller re-attempts the solve.
	if cookieStr == "" {
		markFlareFailure(domain)
		return nil, nil, fmt.Errorf("flaresolverr: %s solved but returned 0 cookies", domain)
	}

	sess := &flareSession{
		cookies:   cookieStr,
		userAgent: ua,
		browserID: browserID,
		obtained:  time.Now(),
	}

	f.flareMu.Lock()
	f.flareCache[domain] = sess
	f.flareMu.Unlock()
	saveFlareSession(domain, sess)

	// When the browser actually navigated to rawURL, hand the rendered body
	// back so the caller can skip an HTTP roundtrip that would likely hit a
	// fresh CF challenge anyway.
	var direct *FetchResult
	if !redirected && resp.Solution.Response != "" {
		status := resp.Solution.Status
		if status == 0 {
			status = 200
		}
		direct = &FetchResult{
			Body:       []byte(resp.Solution.Response),
			StatusCode: status,
		}
	}

	return sess, direct, nil
}

// SolveForLogin runs a fresh flaresolverr GET on rawURL and returns the full
// browser state needed by parsers that want to drive their login flow
// locally: complete cookie jar (anti-bot tokens + phpBB session + any
// cf cookies), final User-Agent, response body, and the Turnstile widget
// token if the page hosts one. Use this instead of GetFlareCookies when
// the calling parser needs the rendered HTML or a Turnstile token in
// addition to cookies.
func (f *Fetcher) SolveForLogin(ctx context.Context, rawURL string) (cookies, userAgent, body, turnstileToken string, err error) {
	if flareShutdown.Load() {
		return "", "", "", "", fmt.Errorf("flaresolverr: service is shutting down")
	}
	svc := getFlareService()
	if svc == nil {
		return "", "", "", "", fmt.Errorf("flaresolverr service not initialized")
	}
	domain := extractDomain(rawURL)
	if remaining, blocked := flareCooldownRemaining(domain); blocked {
		return "", "", "", "", fmt.Errorf("flaresolverr: %s in cooldown for %s", domain, remaining.Round(time.Second))
	}
	dm := getDomainLock(domain)
	dm.Lock()
	defer dm.Unlock()
	if flareShutdown.Load() {
		return "", "", "", "", fmt.Errorf("flaresolverr: service is shutting down")
	}
	flareSolveSem <- struct{}{}
	defer func() { <-flareSolveSem }()
	flareSolveWG.Add(1)
	flareInflight.Add(1)
	defer flareSolveWG.Done()
	defer flareInflight.Add(-1)

	log.Printf("flaresolverr: solving login flow for %s", domain)
	browserID := flareSharedSessionID
	solveCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	// TabsTillVerify hints the library to do extra interactive verification
	// passes when a challenge widget is on the page. Geckodriver backend
	// ignores the field today; webdriver/chromedp backends use it.
	tabs := 1
	resp, _ := svc.ControllerV1(solveCtx, &flaresolverr.V1Request{
		Cmd:               "request.get",
		URL:               rawURL,
		MaxTimeout:        90000,
		WaitInSeconds:     12,
		Session:           browserID,
		SessionTTLMinutes: int(flareSessionTTL / time.Minute),
		TabsTillVerify:    &tabs,
	})
	markSessionUsed(browserID)

	if resp.Status != "ok" {
		markFlareFailure(domain)
		return "", "", "", "", fmt.Errorf("flaresolverr status=%s message=%s", resp.Status, resp.Message)
	}
	if resp.Solution == nil {
		markFlareFailure(domain)
		return "", "", "", "", fmt.Errorf("flaresolverr: no solution returned")
	}

	parts := make([]string, 0, len(resp.Solution.Cookies))
	for _, c := range resp.Solution.Cookies {
		parts = append(parts, c.Name+"="+c.Value)
	}
	cookies = strings.Join(parts, "; ")
	userAgent = resp.Solution.UserAgent
	if userAgent == "" {
		userAgent = "Mozilla/5.0"
	}
	body = resp.Solution.Response
	turnstileToken = resp.Solution.TurnstileToken
	log.Printf("flaresolverr: login solve %s cookies=%d body=%d turnstile=%v",
		domain, len(resp.Solution.Cookies), len(body), turnstileToken != "")

	if cookies != "" {
		clearFlareFailure(domain)
		sess := &flareSession{
			cookies:   cookies,
			userAgent: userAgent,
			browserID: browserID,
			obtained:  time.Now(),
		}
		f.flareMu.Lock()
		f.flareCache[domain] = sess
		f.flareMu.Unlock()
		saveFlareSession(domain, sess)
	}
	return cookies, userAgent, body, turnstileToken, nil
}

// LoginViaFlare submits a login form through the flaresolverr browser. The
// browser opens loginURL, transparently solves any Cloudflare challenge,
// then POSTs postData. Returns the full set of cookies set on the response
// (cf_clearance + the site's session/auth cookies all in one string), the
// browser User-Agent, and the rendered post-submit body. The CF half is
// also stored in the flare cache so subsequent listing fetches via Fetcher
// reuse the same cf_clearance.
//
// This is the right primitive for sites that gate login.php behind CF —
// doing the POST locally (with cf_clearance copied from a separate GET)
// fails when the site's session cookies are set as HttpOnly/SameSite=Strict
// or when CF rotates clearance between the GET and the POST. Going through
// the browser sidesteps both.
func (f *Fetcher) LoginViaFlare(ctx context.Context, rawURL, postData string) (cookies, userAgent, body string, err error) {
	if flareShutdown.Load() {
		return "", "", "", fmt.Errorf("flaresolverr: service is shutting down")
	}
	svc := getFlareService()
	if svc == nil {
		return "", "", "", fmt.Errorf("flaresolverr service not initialized")
	}
	domain := extractDomain(rawURL)
	if remaining, blocked := flareCooldownRemaining(domain); blocked {
		return "", "", "", fmt.Errorf("flaresolverr: %s in cooldown for %s after recent failure", domain, remaining.Round(time.Second))
	}

	dm := getDomainLock(domain)
	dm.Lock()
	defer dm.Unlock()

	if flareShutdown.Load() {
		return "", "", "", fmt.Errorf("flaresolverr: service is shutting down")
	}

	flareSolveSem <- struct{}{}
	defer func() { <-flareSolveSem }()
	flareSolveWG.Add(1)
	flareInflight.Add(1)
	defer flareSolveWG.Done()
	defer flareInflight.Add(-1)

	browserID := flareSharedSessionID
	log.Printf("flaresolverr: submitting login POST for %s", domain)

	postCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	resp, _ := svc.ControllerV1(postCtx, &flaresolverr.V1Request{
		Cmd:               "request.post",
		URL:               rawURL,
		PostData:          postData,
		MaxTimeout:        90000,
		WaitInSeconds:     3,
		Session:           browserID,
		SessionTTLMinutes: int(flareSessionTTL / time.Minute),
	})
	markSessionUsed(browserID)

	if resp.Status != "ok" {
		markFlareFailure(domain)
		return "", "", "", fmt.Errorf("flaresolverr status=%s message=%s", resp.Status, resp.Message)
	}
	if resp.Solution == nil {
		markFlareFailure(domain)
		return "", "", "", fmt.Errorf("flaresolverr: no solution returned")
	}

	cookieParts := make([]string, 0, len(resp.Solution.Cookies))
	for _, c := range resp.Solution.Cookies {
		cookieParts = append(cookieParts, c.Name+"="+c.Value)
	}
	cookies = strings.Join(cookieParts, "; ")
	userAgent = resp.Solution.UserAgent
	if userAgent == "" {
		userAgent = "Mozilla/5.0"
	}
	body = resp.Solution.Response
	log.Printf("flaresolverr: login POST %s returned cookies=%d body=%d", domain, len(resp.Solution.Cookies), len(body))

	// flaresolverr-go bug workaround: in the geckodriver backend the resolve
	// flow reads currentCookies BEFORE WaitInSeconds elapses, so for a
	// request.post the form-submit navigation often hasn't settled when the
	// cookie snapshot is taken. The jar inside the persistent session IS up
	// to date — we just can't see it in this response. Drive a follow-up
	// request.get on the same session to the site origin so the WebDriver
	// re-reads cookies on a stable, fully-loaded same-origin page.
	if cookies == "" {
		origin := originURL(rawURL)
		if origin == "" {
			origin = rawURL
		}
		log.Printf("flaresolverr: post returned 0 cookies, probing %s for auth jar", origin)
		probeCtx, cancel2 := context.WithTimeout(ctx, 90*time.Second)
		resp2, _ := svc.ControllerV1(probeCtx, &flaresolverr.V1Request{
			Cmd:               "request.get",
			URL:               origin,
			MaxTimeout:        60000,
			WaitInSeconds:     2,
			Session:           browserID,
			SessionTTLMinutes: int(flareSessionTTL / time.Minute),
		})
		cancel2()
		markSessionUsed(browserID)
		if resp2.Status == "ok" && resp2.Solution != nil {
			parts2 := make([]string, 0, len(resp2.Solution.Cookies))
			for _, c := range resp2.Solution.Cookies {
				parts2 = append(parts2, c.Name+"="+c.Value)
			}
			if len(parts2) > 0 {
				cookies = strings.Join(parts2, "; ")
				if resp2.Solution.UserAgent != "" {
					userAgent = resp2.Solution.UserAgent
				}
				if resp2.Solution.Response != "" {
					body = resp2.Solution.Response
				}
				log.Printf("flaresolverr: post follow-up probe %s returned cookies=%d", domain, len(parts2))
			}
		}
	}

	if cookies == "" {
		markFlareFailure(domain)
		return "", userAgent, body, fmt.Errorf("flaresolverr: %s login POST returned 0 cookies (even after follow-up probe)", domain)
	}
	clearFlareFailure(domain)

	// Cache the CF half so subsequent listing fetches don't trigger another
	// solve. We store the full cookie string — cf_clearance is the part that
	// matters for CF, the extra session cookies are harmless to include.
	sess := &flareSession{
		cookies:   cookies,
		userAgent: userAgent,
		browserID: browserID,
		obtained:  time.Now(),
	}
	f.flareMu.Lock()
	f.flareCache[domain] = sess
	f.flareMu.Unlock()
	saveFlareSession(domain, sess)

	return cookies, userAgent, body, nil
}

func extractDomain(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	// Lowercased so it matches DomainFromHost(cfg.<X>.Host) used by parsers
	// when keying SessionStore. Hostnames are case-insensitive per RFC, but
	// url.Hostname() preserves whatever case was in the input.
	return strings.ToLower(u.Hostname())
}

// originURL returns scheme://host (no path/query). Falls back to rawURL if
// parsing fails or the input has no scheme/host.
func originURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return rawURL
	}
	return u.Scheme + "://" + u.Host + "/"
}

// looksLikeBinaryDownload returns true for URLs that typically serve a
// Content-Disposition: attachment response — i.e. the browser would try to
// save a file instead of rendering a page. Used to redirect CF-solving away
// from these URLs so Firefox doesn't stall on a save-file flow.
func looksLikeBinaryDownload(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	path := strings.ToLower(u.Path)
	switch {
	case strings.HasSuffix(path, ".torrent"),
		strings.HasSuffix(path, ".zip"),
		strings.HasSuffix(path, ".rar"),
		strings.HasSuffix(path, ".7z"),
		strings.Contains(path, "/download.php"),
		strings.Contains(path, "/dl.php"),
		strings.Contains(path, "/gettorrent"),
		strings.Contains(path, "/get_torrent"),
		strings.Contains(path, "/torrent/download"):
		return true
	}
	return false
}

// MergeCookieStrings combines two cookie strings, second takes precedence for duplicate names.
func MergeCookieStrings(base, override string) string {
	return mergeCookies(base, override)
}

// mergeCookies combines config cookies with flare cookies.
// Flare cookies take precedence for duplicate names.
func mergeCookies(configCookie, flareCookie string) string {
	if configCookie == "" {
		return flareCookie
	}
	if flareCookie == "" {
		return configCookie
	}

	flareParts := strings.Split(flareCookie, ";")
	flareMap := make(map[string]string, len(flareParts))
	for _, part := range flareParts {
		part = strings.TrimSpace(part)
		if eq := strings.IndexByte(part, '='); eq > 0 {
			flareMap[strings.TrimSpace(part[:eq])] = part
		}
	}

	configParts := strings.Split(configCookie, ";")
	parts := make([]string, 0, len(configParts)+len(flareParts))
	for _, part := range configParts {
		part = strings.TrimSpace(part)
		if eq := strings.IndexByte(part, '='); eq > 0 {
			name := strings.TrimSpace(part[:eq])
			if _, overridden := flareMap[name]; !overridden {
				parts = append(parts, part)
			}
		}
	}

	for _, part := range flareParts {
		part = strings.TrimSpace(part)
		if part != "" {
			parts = append(parts, part)
		}
	}

	return strings.Join(parts, "; ")
}
