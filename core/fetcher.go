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
)

// InitFlareService initializes the shared Xvfb display and flaresolverr-go config. Call once at startup.
func InitFlareService(cfg app.FlareSolverrGoConfig) {
	flareSvcCfg = cfg
	// Sweep leftover browser profiles from prior crashes (SIGKILL, panic).
	cleanupStaleProfiles()
	// Start persistent Xvfb if no DISPLAY and not on a desktop
	if os.Getenv("DISPLAY") == "" {
		startXvfb()
	}
}

// CloseFlareService shuts down the shared flaresolverr-go service and Xvfb. Call on shutdown.
func CloseFlareService() {
	flareShutdown.Store(true)

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
		if flareSvcCfg.BrowserPath != "" {
			flareCfg.BrowserPath = flareSvcCfg.BrowserPath
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
	cfClient  *CFClient // tls-client for CF-protected sites (Chrome TLS fingerprint)

	// Cookie cache: domain -> cached session
	flareMu    sync.RWMutex
	flareCache map[string]*flareSession
}

// flareSession holds cookies obtained from flaresolverr for a domain.
type flareSession struct {
	cookies   string
	userAgent string
	obtained  time.Time
}

// flareSolveResult holds the full result from a solve: cookies + page body.
type flareSolveResult struct {
	session *flareSession
	body    []byte // page HTML returned by the browser
	status  int
}

const flareSessionTTL = 30 * time.Minute

// NewFetcher creates a Fetcher with standard HTTP and tls-client backends.
func NewFetcher(cfg app.Config) *Fetcher {
	cf, err := NewCFClientWithConfig(cfg.CFClient.Profile, cfg.CFClient.UserAgent)
	if err != nil {
		log.Printf("fetcher: cfclient init failed: %v (will use standard HTTP)", err)
	}
	return &Fetcher{
		cfg:        cfg,
		stdClient:  &http.Client{Timeout: 30 * time.Second},
		cfClient:   cf,
		flareCache: make(map[string]*flareSession),
	}
}

// UpdateConfig updates the fetcher's config (for hot-reload).
func (f *Fetcher) UpdateConfig(cfg app.Config) {
	f.cfg = cfg
}

// GetCFClient returns the tls-client instance for direct use by parsers (login, etc).
func (f *Fetcher) GetCFClient() *CFClient {
	return f.cfClient
}

// GetFlareCookies returns cached or freshly solved flaresolverr cookies and user-agent for a URL's domain.
func (f *Fetcher) GetFlareCookies(rawURL string) (cookie, userAgent string) {
	domain := extractDomain(rawURL)
	if sess := f.getFlareSession(domain); sess != nil {
		return sess.cookies, sess.userAgent
	}
	result, err := f.solveFlare(rawURL, domain)
	if err != nil {
		return "", ""
	}
	return result.session.cookies, result.session.userAgent
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
//   1. Try cached cookies via cfclient (fast path, Chrome TLS fingerprint)
//   2. If no cache or 403: solve via flaresolverr-go browser
//   3. Use browser's response body if available
//   4. Otherwise use cookies via cfclient (Chrome TLS fingerprint)
//   5. Cache cookies for subsequent requests
func (f *Fetcher) fetchViaFlare(rawURL, cookie string, transport *http.Transport) (*FetchResult, error) {
	domain := extractDomain(rawURL)

	// Try with cached flare cookies via cfclient (Chrome TLS fingerprint)
	if sess := f.getFlareSession(domain); sess != nil {
		res, err := f.fetchWithCookies(rawURL, cookie, sess, transport)
		if err == nil && res.StatusCode != 403 {
			return res, nil
		}
		f.clearFlareSession(domain)
	}

	// Solve via flaresolverr-go browser — returns cookies + page body
	result, err := f.solveFlare(rawURL, domain)
	if err != nil {
		log.Printf("flaresolverr: solve failed for %s: %v", domain, err)
		return f.doHTTP(rawURL, cookie, "Mozilla/5.0", transport)
	}

	// Use browser's response body directly if available
	if len(result.body) > 0 {
		return &FetchResult{Body: result.body, StatusCode: result.status}, nil
	}

	// Browser body empty — fetch via cfclient with solved cookies (Chrome TLS fingerprint)
	return f.fetchWithCookies(rawURL, cookie, result.session, transport)
}

// fetchWithCookies tries cfclient first (Chrome TLS fingerprint), falls back to standard HTTP.
func (f *Fetcher) fetchWithCookies(rawURL, cookie string, sess *flareSession, transport *http.Transport) (*FetchResult, error) {
	merged := mergeCookies(cookie, sess.cookies)
	ua := sess.userAgent
	if ua == "" {
		ua = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
	}

	// Try cfclient (tls-client with Chrome TLS fingerprint)
	if f.cfClient != nil {
		body, status, err := f.cfClient.Download(rawURL, merged, "")
		if err == nil && status != 403 {
			return &FetchResult{Body: body, StatusCode: status}, nil
		}
	}

	// Fallback: standard HTTP
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
}

// solveFlare calls the shared flaresolverr-go service to solve CF challenge.
// Returns cookies (cached for future requests) + the page body from the browser.
//
// Coordination:
//   - Rejects new solves once CloseFlareService has begun.
//   - Serializes per domain: concurrent callers for the same domain wait for
//     the first solve, then reuse its cached cookies (see the second cache
//     check below). Prevents N parallel Chrome instances for one CF site.
//   - Caps global concurrency via flareSolveSem to prevent a Chrome swarm
//     when many parsers run simultaneously.
func (f *Fetcher) solveFlare(rawURL, domain string) (*flareSolveResult, error) {
	if flareShutdown.Load() {
		return nil, fmt.Errorf("flaresolverr: service is shutting down")
	}

	svc := getFlareService()
	if svc == nil {
		return nil, fmt.Errorf("flaresolverr service not initialized")
	}

	dm := getDomainLock(domain)
	dm.Lock()
	defer dm.Unlock()

	// Re-check cache after acquiring domain lock: a prior concurrent caller
	// may have already solved this domain. Body is empty — caller falls through
	// to fetchWithCookies which uses the cached cookies via CFClient.
	if sess := f.getFlareSession(domain); sess != nil {
		return &flareSolveResult{session: sess, status: 200}, nil
	}

	// Re-check shutdown: flag may have been raised while we waited for the lock.
	if flareShutdown.Load() {
		return nil, fmt.Errorf("flaresolverr: service is shutting down")
	}

	flareSolveSem <- struct{}{}
	defer func() { <-flareSolveSem }()

	flareSolveWG.Add(1)
	flareInflight.Add(1)
	defer flareSolveWG.Done()
	defer flareInflight.Add(-1)

	log.Printf("flaresolverr: solving challenge for %s", domain)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	resp, _ := svc.ControllerV1(ctx, &flaresolverr.V1Request{
		Cmd:           "request.get",
		URL:           rawURL,
		MaxTimeout:    60000,
		WaitInSeconds: 2,
	})

	if resp.Status != "ok" {
		return nil, fmt.Errorf("flaresolverr status=%s message=%s", resp.Status, resp.Message)
	}
	if resp.Solution == nil {
		return nil, fmt.Errorf("flaresolverr: no solution returned")
	}

	// Build cookie string from solution cookies
	var cookieParts []string
	for _, c := range resp.Solution.Cookies {
		cookieParts = append(cookieParts, c.Name+"="+c.Value)
	}
	cookieStr := strings.Join(cookieParts, "; ")
	ua := resp.Solution.UserAgent
	log.Printf("flaresolverr: solved %s cookies=%d bodyLen=%d", domain, len(resp.Solution.Cookies), len(resp.Solution.Response))
	if ua == "" {
		ua = "Mozilla/5.0"
	}

	sess := &flareSession{
		cookies:   cookieStr,
		userAgent: ua,
		obtained:  time.Now(),
	}

	f.flareMu.Lock()
	f.flareCache[domain] = sess
	f.flareMu.Unlock()

	status := resp.Solution.Status
	if status == 0 {
		status = 200
	}

	return &flareSolveResult{
		session: sess,
		body:    []byte(resp.Solution.Response),
		status:  status,
	}, nil
}

func extractDomain(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Hostname()
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
