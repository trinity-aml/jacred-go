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
		tls_client.WithInsecureSkipVerify(),
	}
	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		return nil, fmt.Errorf("tls-client init: %w", err)
	}
	return &CFClient{client: client}, nil
}

// Get performs an HTTP GET with browser-like headers.
func (c *CFClient) Get(rawURL, cookie, referer string) (string, int, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "", 0, err
	}
	setBrowserHeaders(req, cookie, referer)
	resp, err := c.client.Do(req)
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

// Download performs an HTTP GET and returns raw bytes.
func (c *CFClient) Download(rawURL, cookie, referer string) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	setBrowserHeaders(req, cookie, referer)
	resp, err := c.client.Do(req)
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
			"cookie", "referer",
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
