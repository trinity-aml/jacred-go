package core

import (
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// Plain HTTP requests are made with a Chrome TLS fingerprint rather than Go's.
//
// Why: a cf_clearance cookie is minted by a real Chrome and then replayed by
// this process on every subsequent fetch. Cloudflare fingerprints the TLS
// ClientHello (JA3/JA4) and can compare it against the browser that earned the
// cookie, so a Go handshake carrying a Chrome cookie is a visible mismatch.
// Go's net/http cannot express this: the ClientHello is built by crypto/tls.
//
// The profile is pinned to Chrome 146 to match the CloakBrowser build that
// solves the challenges (see core/cloakbrowser_pin.go) — the cookie and the
// handshake should come from the same Chrome version. Bump both together.
var impersonateProfile = profiles.Chrome_146

const impersonateTimeout = 30 * time.Second

var (
	tlsClientMu    sync.RWMutex
	tlsClientCache = map[FetchProfile]tlsclient.HttpClient{}
)

// clientFor returns a cached impersonating client for the given network
// profile. Clients are cached process-wide so each (proxy, tls) combination
// keeps its keep-alive connections warm across requests.
func clientFor(p FetchProfile) (tlsclient.HttpClient, error) {
	tlsClientMu.RLock()
	c, ok := tlsClientCache[p]
	tlsClientMu.RUnlock()
	if ok {
		return c, nil
	}

	tlsClientMu.Lock()
	defer tlsClientMu.Unlock()
	if c, ok := tlsClientCache[p]; ok {
		return c, nil
	}

	opts := []tlsclient.HttpClientOption{
		tlsclient.WithClientProfile(impersonateProfile),
		tlsclient.WithTimeoutSeconds(int(impersonateTimeout / time.Second)),
		// No cookie jar: every caller passes its own Cookie header and the
		// session store owns cookie lifetime. A jar here would silently share
		// cookies between trackers on the same client.
	}
	if p.insecure {
		opts = append(opts, tlsclient.WithInsecureSkipVerify())
	}
	proxyURL, err := p.proxyURLWithAuth()
	if err != nil {
		return nil, fmt.Errorf("tlsclient: bad proxy url: %w", err)
	}
	if proxyURL != "" {
		opts = append(opts, tlsclient.WithProxyUrl(proxyURL))
	}

	c, err = tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), opts...)
	if err != nil {
		return nil, fmt.Errorf("tlsclient: %w", err)
	}
	tlsClientCache[p] = c
	return c, nil
}

// defaultUserAgent is what we send when no cookie/session supplies one. It
// must stay consistent with impersonateProfile: a bare "Mozilla/5.0" paired
// with a Chrome ClientHello is a contradiction any bot check can read, and
// CF auto-detect means any tracker can become CF-gated without warning.
const defaultUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"

// samePage reports whether the browser ended up on the page we asked for,
// comparing scheme+host+path+query but ignoring fragments and a trailing
// slash. Used to detect a site bouncing the browser somewhere else.
func samePage(got, want string) bool {
	if strings.TrimSpace(got) == "" {
		// Older library versions don't report a final URL; assume no redirect
		// rather than throwing away a good body.
		return true
	}
	norm := func(raw string) string {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil {
			return strings.TrimSpace(raw)
		}
		p := strings.TrimSuffix(u.EscapedPath(), "/")
		if p == "" {
			p = "/"
		}
		return strings.ToLower(u.Hostname()) + p + "?" + u.RawQuery
	}
	return norm(got) == norm(want)
}

// chromeHeaderOrder is the order Chrome emits these headers in. HTTP/2 header
// order is itself fingerprinted, so sending our own headers in map-iteration
// order would give away the client just as the ClientHello would.
var chromeHeaderOrder = []string{
	"host",
	"connection",
	"cache-control",
	"sec-ch-ua",
	"sec-ch-ua-mobile",
	"sec-ch-ua-platform",
	"upgrade-insecure-requests",
	"user-agent",
	"content-type",
	"accept",
	"origin",
	"referer",
	"sec-fetch-site",
	"sec-fetch-mode",
	"sec-fetch-user",
	"sec-fetch-dest",
	"accept-encoding",
	"accept-language",
	"cookie",
}

// applyChromeHeaderOrder tags the request with the header order fhttp should
// serialize. Names absent from the request are skipped by fhttp.
func applyChromeHeaderOrder(h fhttp.Header) {
	h[fhttp.HeaderOrderKey] = chromeHeaderOrder
	h[fhttp.PHeaderOrderKey] = []string{":method", ":authority", ":scheme", ":path"}
}
