package core

import (
	"fmt"
	"io"
	"strings"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// CFClient wraps tls-client with Firefox TLS+HTTP/2 fingerprint.
// Bypasses Cloudflare JA3/JA4/HTTP2 detection.
type CFClient struct {
	client tls_client.HttpClient
}

// NewCFClient creates a new client with Firefox 117 fingerprint.
func NewCFClient() (*CFClient, error) {
	opts := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(30),
		tls_client.WithClientProfile(profiles.Firefox_117),
		tls_client.WithNotFollowRedirects(),
		tls_client.WithInsecureSkipVerify(),
	}
	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		return nil, fmt.Errorf("tls-client init: %w", err)
	}
	return &CFClient{client: client}, nil
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
		setBrowserHeaders(req, cookie, "")
	}
	return nil, fmt.Errorf("too many redirects")
}

// Get performs an HTTP GET with browser-like headers, following redirects.
func (c *CFClient) Get(rawURL, cookie, referer string) (string, int, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "", 0, err
	}
	setBrowserHeaders(req, cookie, referer)
	resp, err := c.doWithRedirects(req, cookie)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", resp.StatusCode, err
	}
	return string(b), resp.StatusCode, nil
}

// Download performs an HTTP GET and returns raw bytes, following redirects.
func (c *CFClient) Download(rawURL, cookie, referer string) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	setBrowserHeaders(req, cookie, referer)
	resp, err := c.doWithRedirects(req, cookie)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return b, resp.StatusCode, nil
}

func setBrowserHeaders(req *http.Request, cookie, referer string) {
	// Order matters for HTTP/2 fingerprinting
	req.Header = http.Header{
		"User-Agent":                {"Mozilla/5.0 (X11; Ubuntu; Linux x86_64; rv:148.0) Gecko/20100101 Firefox/148.0"},
		"Accept":                    {"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"},
		"Accept-Language":           {"ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7"},
		"Accept-Encoding":           {"gzip, deflate, br"},
		"Sec-Fetch-Dest":            {"document"},
		"Sec-Fetch-Mode":            {"navigate"},
		"Sec-Fetch-Site":            {"none"},
		"Upgrade-Insecure-Requests": {"1"},
		"Connection":                {"keep-alive"},
		http.HeaderOrderKey: {
			"user-agent", "accept", "accept-language", "accept-encoding",
			"cookie", "referer", "content-type",
			"sec-fetch-dest", "sec-fetch-mode", "sec-fetch-site",
			"upgrade-insecure-requests", "connection",
		},
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
	setBrowserHeaders(req, cookie, referer)
	// Direct Do (no redirect follow) to capture Set-Cookie
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
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
	setBrowserHeaders(req, cookie, referer)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, nil, err
	}
	return b, resp.StatusCode, resp.Header.Values("Set-Cookie"), nil
}
