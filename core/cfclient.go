package core

import (
	"fmt"
	"io"
	"strings"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// CFClient wraps tls-client with configurable TLS fingerprint.
type CFClient struct {
	client    tls_client.HttpClient
	userAgent string
}

// profileMap maps config string to tls-client profile.
var profileMap = map[string]profiles.ClientProfile{
	"chrome_103":     profiles.Chrome_103,
	"chrome_104":     profiles.Chrome_104,
	"chrome_105":     profiles.Chrome_105,
	"chrome_106":     profiles.Chrome_106,
	"chrome_107":     profiles.Chrome_107,
	"chrome_108":     profiles.Chrome_108,
	"chrome_109":     profiles.Chrome_109,
	"chrome_110":     profiles.Chrome_110,
	"chrome_111":     profiles.Chrome_111,
	"chrome_112":     profiles.Chrome_112,
	"chrome_117":     profiles.Chrome_117,
	"chrome_120":     profiles.Chrome_120,
	"chrome_124":     profiles.Chrome_124,
	"chrome_131":     profiles.Chrome_131,
	"chrome_133":     profiles.Chrome_133,
	"chrome_144":     profiles.Chrome_144,
	"chrome_146":     profiles.Chrome_146,
	"firefox_102":    profiles.Firefox_102,
	"firefox_104":    profiles.Firefox_104,
	"firefox_105":    profiles.Firefox_105,
	"firefox_106":    profiles.Firefox_106,
	"firefox_108":    profiles.Firefox_108,
	"firefox_110":    profiles.Firefox_110,
	"firefox_117":    profiles.Firefox_117,
	"firefox_120":    profiles.Firefox_120,
}

const defaultUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"

// NewCFClient creates a client with default Chrome 144 profile.
func NewCFClient() (*CFClient, error) {
	return NewCFClientWithConfig("chrome_146", "")
}

// NewCFClientWithConfig creates a client with configurable profile and user-agent.
func NewCFClientWithConfig(profileName, userAgent string) (*CFClient, error) {
	profile, ok := profileMap[strings.ToLower(strings.TrimSpace(profileName))]
	if !ok {
		profile = profiles.Chrome_146
	}
	if strings.TrimSpace(userAgent) == "" {
		userAgent = defaultUserAgent
	}
	opts := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(30),
		tls_client.WithClientProfile(profile),
		tls_client.WithNotFollowRedirects(),
		tls_client.WithInsecureSkipVerify(),
	}
	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		return nil, fmt.Errorf("tls-client init: %w", err)
	}
	return &CFClient{client: client, userAgent: userAgent}, nil
}

const maxRedirects = 5

// doWithRedirects performs request and manually follows redirects up to maxRedirects.
func (c *CFClient) doWithRedirects(req *http.Request, cookie string) (*http.Response, error) {
	for i := 0; i < maxRedirects; i++ {
		resp, err := c.client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 300 || resp.StatusCode >= 400 {
			return resp, nil
		}
		loc := resp.Header.Get("Location")
		resp.Body.Close()
		if loc == "" {
			return resp, nil
		}
		// Resolve relative redirects
		if strings.HasPrefix(loc, "/") {
			// Extract scheme+host from original URL
			u := req.URL
			loc = u.Scheme + "://" + u.Host + loc
		}
		req, err = http.NewRequest(http.MethodGet, loc, nil)
		if err != nil {
			return nil, err
		}
		c.setBrowserHeaders(req, cookie, "")
	}
	return nil, fmt.Errorf("too many redirects")
}

// Get performs an HTTP GET with browser-like headers, following redirects.
func (c *CFClient) Get(rawURL, cookie, referer string) (string, int, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "", 0, err
	}
	c.setBrowserHeaders(req, cookie, referer)
	resp, err := c.doWithRedirects(req, cookie)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return "", resp.StatusCode, err
	}
	return string(b), resp.StatusCode, nil
}

// Download performs an HTTP GET and returns raw bytes, following redirects.
func (c *CFClient) Download(rawURL, cookie, referer string) ([]byte, int, error) {
	return c.DownloadWithUA(rawURL, cookie, referer, "")
}

// DownloadWithUA performs an HTTP GET with an optional User-Agent override.
// When ua is non-empty, it replaces the client's default UA for this request —
// used to match UA of a cf_clearance cookie that was issued by a different
// Chrome version than the one tls-client emulates.
func (c *CFClient) DownloadWithUA(rawURL, cookie, referer, ua string) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	c.setBrowserHeadersWithUA(req, cookie, referer, ua)
	resp, err := c.doWithRedirectsUA(req, cookie, ua)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return b, resp.StatusCode, nil
}

func (c *CFClient) doWithRedirectsUA(req *http.Request, cookie, ua string) (*http.Response, error) {
	for i := 0; i < maxRedirects; i++ {
		resp, err := c.client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 300 || resp.StatusCode >= 400 {
			return resp, nil
		}
		loc := resp.Header.Get("Location")
		resp.Body.Close()
		if loc == "" {
			return resp, nil
		}
		if strings.HasPrefix(loc, "/") {
			u := req.URL
			loc = u.Scheme + "://" + u.Host + loc
		}
		req, err = http.NewRequest(http.MethodGet, loc, nil)
		if err != nil {
			return nil, err
		}
		c.setBrowserHeadersWithUA(req, cookie, "", ua)
	}
	return nil, fmt.Errorf("too many redirects")
}

func (c *CFClient) setBrowserHeaders(req *http.Request, cookie, referer string) {
	c.setBrowserHeadersWithUA(req, cookie, referer, "")
}

func (c *CFClient) setBrowserHeadersWithUA(req *http.Request, cookie, referer, uaOverride string) {
	userAgent := c.userAgent
	if strings.TrimSpace(uaOverride) != "" {
		userAgent = uaOverride
	}
	isChrome := strings.Contains(userAgent, "Chrome")
	isFirefox := strings.Contains(userAgent, "Firefox")

	// Sec-Fetch-Site from referer: none (direct nav, no referer),
	// same-origin (referer host == req host), cross-site (otherwise).
	fetchSite := "none"
	if strings.TrimSpace(referer) != "" {
		if reqHost := req.URL.Host; reqHost != "" {
			refLower := strings.ToLower(referer)
			if strings.Contains(refLower, "://"+strings.ToLower(reqHost)) {
				fetchSite = "same-origin"
			} else {
				fetchSite = "cross-site"
			}
		}
	}

	if isChrome {
		chromeVer := "146"
		if idx := strings.Index(userAgent, "Chrome/"); idx >= 0 {
			rest := userAgent[idx+7:]
			if dot := strings.Index(rest, "."); dot > 0 {
				chromeVer = rest[:dot]
			}
		}
		req.Header = http.Header{
			"Sec-Ch-Ua":                 {fmt.Sprintf(`"Chromium";v="%s", "Google Chrome";v="%s", "Not?A_Brand";v="99"`, chromeVer, chromeVer)},
			"Sec-Ch-Ua-Mobile":          {"?0"},
			"Sec-Ch-Ua-Platform":        {`"Linux"`},
			"Upgrade-Insecure-Requests": {"1"},
			"User-Agent":                {userAgent},
			"Accept":                    {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"},
			"Sec-Fetch-Site":            {fetchSite},
			"Sec-Fetch-Mode":            {"navigate"},
			"Sec-Fetch-Dest":            {"document"},
			"Accept-Encoding":           {"gzip, deflate, br, zstd"},
			"Accept-Language":           {"ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7"},
			"Priority":                  {"u=0, i"},
		}
		// Chrome only sends Sec-Fetch-User on user-activated navigations.
		// Same-origin/cross-site nav (from referer) counts; direct URL entry
		// (fetchSite=none) does not.
		if fetchSite != "none" {
			req.Header.Set("Sec-Fetch-User", "?1")
		}
		req.Header[http.HeaderOrderKey] = []string{
			"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform",
			"upgrade-insecure-requests", "user-agent", "accept",
			"sec-fetch-site", "sec-fetch-mode", "sec-fetch-user", "sec-fetch-dest",
			"referer", "accept-encoding", "accept-language", "priority",
			"cookie", "content-type",
		}
	} else if isFirefox {
		req.Header = http.Header{
			"User-Agent":                {userAgent},
			"Accept":                    {"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"},
			"Accept-Language":           {"ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7"},
			"Accept-Encoding":           {"gzip, deflate, br, zstd"},
			"Upgrade-Insecure-Requests": {"1"},
			"Sec-Fetch-Dest":            {"document"},
			"Sec-Fetch-Mode":            {"navigate"},
			"Sec-Fetch-Site":            {fetchSite},
			"Priority":                  {"u=0, i"},
			"TE":                        {"trailers"},
		}
		req.Header[http.HeaderOrderKey] = []string{
			"user-agent", "accept", "accept-language", "accept-encoding",
			"referer", "cookie", "upgrade-insecure-requests",
			"sec-fetch-dest", "sec-fetch-mode", "sec-fetch-site",
			"priority", "te", "content-type",
		}
	} else {
		req.Header = http.Header{
			"User-Agent":      {userAgent},
			"Accept":          {"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"},
			"Accept-Language": {"ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7"},
			"Accept-Encoding": {"gzip, deflate, br"},
			"Connection":      {"keep-alive"},
		}
		req.Header[http.HeaderOrderKey] = []string{
			"user-agent", "accept", "accept-language", "accept-encoding",
			"cookie", "referer", "content-type", "connection",
		}
	}

	if strings.TrimSpace(cookie) != "" {
		req.Header.Set("Cookie", cookie)
	}
	if strings.TrimSpace(referer) != "" {
		req.Header.Set("Referer", referer)
	}
}

// GetWithHeaders performs GET and returns body + all Set-Cookie headers.
func (c *CFClient) GetWithHeaders(rawURL, cookie, referer string) (body []byte, status int, setCookies []string, err error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, nil, err
	}
	c.setBrowserHeaders(req, cookie, referer)
	// Direct Do (no redirect follow) to capture Set-Cookie
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, resp.StatusCode, nil, err
	}
	return b, resp.StatusCode, resp.Header.Values("Set-Cookie"), nil
}

// PostForm performs a POST with form data and returns body + Set-Cookie headers (no redirect follow).
func (c *CFClient) PostForm(rawURL, cookie, referer string, formData string) (body []byte, status int, setCookies []string, err error) {
	req, err := http.NewRequest(http.MethodPost, rawURL, strings.NewReader(formData))
	if err != nil {
		return nil, 0, nil, err
	}
	c.setBrowserHeaders(req, cookie, referer)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, resp.StatusCode, nil, err
	}
	return b, resp.StatusCode, resp.Header.Values("Set-Cookie"), nil
}
