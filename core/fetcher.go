package core

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"jacred/app"
)

// Fetcher provides unified HTTP fetching with configurable mode per tracker.
type Fetcher struct {
	cfg       app.Config
	stdClient *http.Client
	cfClient  *CFClient // tls-client for CF-protected sites (Chrome TLS fingerprint)

	// FlareSolverr cookie cache: domain -> cached session
	flareMu    sync.RWMutex
	flareCache map[string]*flareSession
}

// flareSession holds cookies obtained from FlareSolverr for a domain.
type flareSession struct {
	cookies    string
	userAgent  string
	obtained   time.Time
	cookieFail bool // true if cookies don't work via standard HTTP — use cfclient instead
}

const flareSessionTTL = 30 * time.Minute

// NewFetcher creates a Fetcher with standard HTTP, tls-client and FlareSolverr backends.
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

// GetFlareCookies returns cached or freshly solved FlareSolverr cookies and user-agent for a URL's domain.
// Returns empty strings if flaresolverr is not configured or solve fails.
func (f *Fetcher) GetFlareCookies(rawURL string) (cookie, userAgent string) {
	domain := extractDomain(rawURL)
	if sess := f.getFlareSession(domain); sess != nil {
		return sess.cookies, sess.userAgent
	}
	solveURL := rawURL
	if idx := strings.IndexByte(solveURL, '?'); idx > 0 {
		solveURL = solveURL[:idx]
	}
	sess, err := f.solveFlareSolverr(solveURL, domain)
	if err != nil {
		return "", ""
	}
	return sess.cookies, sess.userAgent
}

// InvalidateSession clears cached FlareSolverr cookies for a URL's domain.
// Call this when the response body indicates a CF challenge despite 200 status.
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

	// Apply InsecureSkipVerify if configured
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

// fetchViaFlare uses FlareSolverr to obtain CF cookies, then fetches via standard HTTP.
// If cookies don't work with standard HTTP (TLS fingerprint check), uses CFClient (tls-client with Chrome fingerprint).
func (f *Fetcher) fetchViaFlare(rawURL, cookie string, transport *http.Transport) (*FetchResult, error) {
	domain := extractDomain(rawURL)

	sess := f.getFlareSession(domain)

	// If domain needs cfclient (cookies failed with standard HTTP before)
	if sess != nil && sess.cookieFail {
		return f.fetchViaCFClient(rawURL, cookie, sess)
	}

	// Try with cached flare cookies via standard HTTP first
	if sess != nil {
		merged := mergeCookies(cookie, sess.cookies)
		res, err := f.doHTTP(rawURL, merged, sess.userAgent, transport)
		if err == nil {
			if res.StatusCode != 403 {
				return res, nil
			}
			log.Printf("flaresolverr: cached cookies expired for %s (got 403)", domain)
		}
		f.clearFlareSession(domain)
	}

	// Solve CF challenge via FlareSolverr
	solveURL := rawURL
	if idx := strings.IndexByte(solveURL, '?'); idx > 0 {
		solveURL = solveURL[:idx]
	}
	newSess, err := f.solveFlareSolverr(solveURL, domain)
	if err != nil {
		log.Printf("flaresolverr: solve failed for %s: %v", domain, err)
		return f.doHTTP(rawURL, cookie, "Mozilla/5.0", transport)
	}

	// Try fetching with new cookies via standard HTTP
	merged := mergeCookies(cookie, newSess.cookies)
	res, err := f.doHTTP(rawURL, merged, newSess.userAgent, transport)
	if err == nil && res.StatusCode != 403 {
		return res, nil
	}

	// Standard HTTP got 403 — CF checks TLS fingerprint. Switch to cfclient mode.
	if f.cfClient == nil {
		log.Printf("flaresolverr: cookies don't work for %s and cfclient not available", domain)
		f.clearFlareSession(domain)
		return res, err
	}

	log.Printf("flaresolverr: cookies don't work with standard HTTP for %s, switching to cfclient", domain)
	newSess.cookieFail = true
	f.flareMu.Lock()
	f.flareCache[domain] = newSess
	f.flareMu.Unlock()

	return f.fetchViaCFClient(rawURL, cookie, newSess)
}

// fetchViaCFClient fetches via tls-client (Chrome TLS fingerprint) with FlareSolverr cookies.
func (f *Fetcher) fetchViaCFClient(rawURL, cookie string, sess *flareSession) (*FetchResult, error) {
	if f.cfClient == nil {
		return nil, fmt.Errorf("cfclient not available")
	}
	merged := mergeCookies(cookie, sess.cookies)
	body, status, err := f.cfClient.Download(rawURL, merged, "")
	if err != nil {
		return nil, err
	}
	return &FetchResult{Body: body, StatusCode: status}, nil
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

// solveFlareSolverr calls FlareSolverr to solve CF challenge and caches the resulting cookies.
func (f *Fetcher) solveFlareSolverr(solveURL, domain string) (*flareSession, error) {
	endpoint := strings.TrimSpace(f.cfg.FlareSolverr)
	if endpoint == "" {
		return nil, fmt.Errorf("flaresolverr URL not configured")
	}
	endpoint = strings.TrimRight(endpoint, "/") + "/v1"
	freq := flareRequest{
		Cmd:        "request.get",
		URL:        solveURL,
		MaxTimeout: 60000,
	}

	body, err := json.Marshal(freq)
	if err != nil {
		return nil, err
	}

	log.Printf("flaresolverr: solving challenge for %s", domain)

	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("flaresolverr request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, err
	}

	var fresp flareResponse
	if err := json.Unmarshal(respBody, &fresp); err != nil {
		return nil, fmt.Errorf("flaresolverr response parse error: %w", err)
	}

	if fresp.Status != "ok" {
		return nil, fmt.Errorf("flaresolverr status=%s message=%s", fresp.Status, fresp.Message)
	}

	// Build cookie string from solution cookies
	var cookieParts []string
	for _, c := range fresp.Solution.Cookies {
		cookieParts = append(cookieParts, c.Name+"="+c.Value)
	}
	cookieStr := strings.Join(cookieParts, "; ")
	ua := fresp.Solution.UserAgent
	if ua == "" {
		ua = "Mozilla/5.0"
	}

	log.Printf("flaresolverr: solved %s cookies=%d ua=%s", domain, len(fresp.Solution.Cookies), ua)

	sess := &flareSession{
		cookies:   cookieStr,
		userAgent: ua,
		obtained:  time.Now(),
	}

	f.flareMu.Lock()
	f.flareCache[domain] = sess
	f.flareMu.Unlock()

	return sess, nil
}

// flaresolverr request/response types
type flareRequest struct {
	Cmd        string `json:"cmd"`
	URL        string `json:"url"`
	MaxTimeout int    `json:"maxTimeout"`
}

type flareResponse struct {
	Status   string `json:"status"`
	Message  string `json:"message"`
	Solution struct {
		URL       string        `json:"url"`
		Status    int           `json:"status"`
		Response  string        `json:"response"`
		Cookies   []flareCookie `json:"cookies"`
		UserAgent string        `json:"userAgent"`
	} `json:"solution"`
}

type flareCookie struct {
	Name  string `json:"name"`
	Value string `json:"value"`
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

	// Parse flare cookies into map for dedup
	flareMap := make(map[string]string)
	for _, part := range strings.Split(flareCookie, ";") {
		part = strings.TrimSpace(part)
		if eq := strings.IndexByte(part, '='); eq > 0 {
			flareMap[strings.TrimSpace(part[:eq])] = part
		}
	}

	// Start with config cookies, skip ones overridden by flare
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

	// Add all flare cookies
	for _, part := range strings.Split(flareCookie, ";") {
		part = strings.TrimSpace(part)
		if part != "" {
			parts = append(parts, part)
		}
	}

	return strings.Join(parts, "; ")
}
