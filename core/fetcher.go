package core

import (
	"context"
	"crypto/tls"
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
	// instead of each spawning Chrome.
	flareDomainMu    sync.Mutex
	flareDomainLocks = make(map[string]*sync.Mutex)

	// flareSolveWG tracks in-flight solves so CloseFlareService can wait for them.
	flareSolveWG  sync.WaitGroup
	flareInflight atomic.Int32
	flareShutdown atomic.Bool

	// Circuit breaker: timestamps of last failed solve per domain. Callers that
	// retry immediately after a 90s timeout get a fast 503 instead of spawning
	// another Chrome that almost certainly times out again.
	flareFailMu   sync.RWMutex
	flareLastFail = make(map[string]time.Time)

	// Idle-session reaper: destroy flaresolverr-go browser sessions that
	// haven't been used for flareSessionIdleTTL so Camoufox doesn't sit in
	// RAM between cron runs. One session ≈ 800–1000 MB resident; without
	// reaping, every domain we solve once pins a browser until jacred exits.
	flareLastUsedMu sync.Mutex
	flareLastUsed   = make(map[string]time.Time)

	flareReaperStop chan struct{}
	flareReaperDone chan struct{}
)

const (
	flareFailCooldown   = 3 * time.Minute
	flareSessionIdleTTL = 5 * time.Minute
	flareReaperInterval = 1 * time.Minute

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

// getDomainLock returns the mutex that serializes solves for a given domain.
func getDomainLock(domain string) *sync.Mutex {
	flareDomainMu.Lock()
	defer flareDomainMu.Unlock()
	m, ok := flareDomainLocks[domain]
	if !ok {
		m = &sync.Mutex{}
		flareDomainLocks[domain] = m
	}
	return m
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
	}
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
	mode := strings.ToLower(strings.TrimSpace(tracker.FetchMode))
	if mode == "" {
		mode = "standard"
	}

	cookie := strings.TrimSpace(tracker.Cookie)
	proxy := ProxyForURL(rawURL, tracker.UseProxy, f.cfg)

	if tracker.InsecureSkipVerify {
		if proxy != nil {
			if proxy.TLSClientConfig == nil {
				proxy.TLSClientConfig = &tls.Config{}
			}
			proxy.TLSClientConfig.InsecureSkipVerify = true
		} else {
			proxy = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
		}
	}

	switch mode {
	case "flaresolverr":
		return f.fetchViaFlare(rawURL, cookie, proxy)
	default:
		return f.fetchStandard(rawURL, cookie, proxy)
	}
}

// GetString is a convenience wrapper that returns body as string.
func (f *Fetcher) GetString(rawURL string, tracker app.TrackerSettings) (string, int, error) {
	res, err := f.Get(rawURL, tracker)
	if err != nil {
		return "", 0, err
	}
	return string(res.Body), res.StatusCode, nil
}

// Download fetches raw bytes (for torrent files, etc).
func (f *Fetcher) Download(rawURL string, tracker app.TrackerSettings) ([]byte, int, error) {
	res, err := f.Get(rawURL, tracker)
	if err != nil {
		return nil, 0, err
	}
	return res.Body, res.StatusCode, nil
}

func (f *Fetcher) fetchStandard(rawURL, cookie string, transport *http.Transport) (*FetchResult, error) {
	return f.doHTTP(rawURL, cookie, "Mozilla/5.0", transport)
}

func (f *Fetcher) doHTTP(rawURL, cookie, userAgent string, transport *http.Transport) (*FetchResult, error) {
	client := f.stdClient
	if transport != nil {
		client = &http.Client{Timeout: 30 * time.Second, Transport: transport}
	}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
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
//   2. If no cache or 403: solve CF challenge on the site's origin via
//      flaresolverr-go browser (not the deep URL — some sites serve a
//      managed challenge on deep URLs that automation can't pass)
//   3. Use the resulting cookies to fetch the actual deep URL
//   4. Cache cookies for subsequent requests
func (f *Fetcher) fetchViaFlare(rawURL, cookie string, transport *http.Transport) (*FetchResult, error) {
	domain := extractDomain(rawURL)
	httpCookiesFailed := false

	if sess := f.getFlareSession(domain); sess != nil {
		res, err := f.fetchWithCookies(rawURL, cookie, sess, transport)
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
	res, err := f.fetchWithCookies(rawURL, cookie, sess, transport)
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
func (f *Fetcher) fetchWithCookies(rawURL, cookie string, sess *flareSession, transport *http.Transport) (*FetchResult, error) {
	merged := mergeCookies(cookie, sess.cookies)
	ua := sess.userAgent
	if ua == "" {
		ua = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
	}
	return f.doHTTP(rawURL, merged, ua, transport)
}

func (f *Fetcher) getFlareSession(domain string) *flareSession {
	f.flareMu.RLock()
	defer f.flareMu.RUnlock()
	sess, ok := f.flareCache[domain]
	if !ok || time.Since(sess.obtained) > flareSessionTTL {
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
		WaitInSeconds:     2,
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
	var cookieParts []string
	for _, c := range resp.Solution.Cookies {
		cookieParts = append(cookieParts, c.Name+"="+c.Value)
	}
	cookieStr := strings.Join(cookieParts, "; ")
	ua := resp.Solution.UserAgent
	log.Printf("flaresolverr: solved %s cookies=%d", domain, len(resp.Solution.Cookies))
	if ua == "" {
		ua = "Mozilla/5.0"
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

func extractDomain(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Hostname()
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

	flareMap := make(map[string]string)
	for _, part := range strings.Split(flareCookie, ";") {
		part = strings.TrimSpace(part)
		if eq := strings.IndexByte(part, '='); eq > 0 {
			flareMap[strings.TrimSpace(part[:eq])] = part
		}
	}

	var parts []string
	for _, part := range strings.Split(configCookie, ";") {
		part = strings.TrimSpace(part)
		if eq := strings.IndexByte(part, '='); eq > 0 {
			name := strings.TrimSpace(part[:eq])
			if _, overridden := flareMap[name]; !overridden {
				parts = append(parts, part)
			}
		}
	}

	for _, part := range strings.Split(flareCookie, ";") {
		part = strings.TrimSpace(part)
		if part != "" {
			parts = append(parts, part)
		}
	}

	return strings.Join(parts, "; ")
}
