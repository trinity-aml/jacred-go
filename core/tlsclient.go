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
// What has to match is the browser's actual ClientHello, not its version
// number — and those are two different things here.
//
// Measured 2026-11-13 against tls.peet.ws from this box, the CloakBrowser
// build that mints the clearance (Chromium 146.0.7680.177) against each
// candidate tls-client profile:
//
//	CloakBrowser    ja4=t13d1516h2_8daaf6152771_d8a2da3f94cd
//	profiles.Chrome_146  ja4=t13d1517h2_8daaf6152771_dcad5a053991   differs
//	profiles.Chrome_144  ja4=t13d1516h2_8daaf6152771_d8a2da3f94cd   identical
//
// Ciphers, curves, ALPN and the whole HTTP/2 fingerprint
// (`1:65536;2:0;4:6291456;6:262144|15663105|0|m,a,s,p`) were already identical
// on both. The entire difference is **one TLS extension**: tls-client's
// Chrome_146 sends `trust_anchors`, id 0xca34 = 51764, which this Chromium 146
// does not — it is behind a Chromium rollout flag, so "Chrome 146" is two
// different ClientHellos depending on the build. Chrome_144 is Chrome_146
// minus exactly that extension, and 0xca34 appears in no other profile in the
// library.
//
// So the replay used to announce itself as a client the clearance was never
// issued to, by one extension, on every request — which is the mismatch
// `cf_clearance` is bound to and the reason a freshly minted clearance could
// be challenged a second later. The version in the profile name is now
// deliberately *not* the browser's: `defaultUserAgent` tracks the browser
// build (146), `impersonateProfile` tracks its handshake. Do not "fix" the two
// into agreement — re-measure instead. TestImpersonationMatchesTheBrowserHello
// pins the extension set so a bump back to Chrome_146 fails the build.
var impersonateProfile = profiles.Chrome_144

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
	if p.noRedirect {
		opts = append(opts, tlsclient.WithNotFollowRedirects())
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
// must advertise the same Chrome major as the CloakBrowser build
// (cloakBrowserFreeTag): a bare "Mozilla/5.0" paired with a Chrome ClientHello
// is a contradiction any bot check can read, and CF auto-detect means any
// tracker can become CF-gated without warning. Note this is the *browser's*
// version, which is not impersonateProfile's name — see above.
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
	"priority",
}

// chromeNavigationHeaders is what CloakBrowser 146 actually sent for a
// top-level navigation, measured against tls.peet.ws on 2026-11-13 and
// reproduced here in that order.
//
// This list used to exist only in chromeHeaderOrder above, and nothing ever
// set the headers — so the ordering machinery was ordering a list of two:
// every plain fetch went out with `user-agent`, `cookie` and whatever
// `accept-encoding` fhttp adds, and nothing else. A request carrying a
// pixel-perfect Chrome ClientHello and a Chrome UA, with no `accept`, no
// `accept-language` and none of the `sec-*` headers Chrome has sent on every
// navigation since 2020, identifies itself as not-Chrome on the first line of
// the request. The ClientHello work was being undone one layer up.
//
// `sec-fetch-site` is not here because it depends on the Referer; see
// secFetchSite.
//
// Two residuals, both known rather than overlooked:
//   - `accept-language`. The browser on this box sends its *locale*, verbatim
//     and malformed (`en_US.UTF-8,en_US.UTF;q=0.9`), which no real Chrome
//     does and which would differ on every host. This sends the value a
//     default Chrome sends instead. Measured byte-identical output to sending
//     nothing on rutracker, rutor and nnmclub — while `ru-RU,…` changed
//     rutracker's page, so the risk is in asserting Russian, not English.
//   - `sec-ch-ua` brand list. Chrome randomises the brand order and the
//     "Not-A.Brand" version per build; this is one sample of it.
var chromeNavigationHeaders = [][2]string{
	{"sec-ch-ua", `"Chromium";v="146", "Not-A.Brand";v="24", "Google Chrome";v="146"`},
	{"sec-ch-ua-mobile", "?0"},
	{"sec-ch-ua-platform", `"Linux"`},
	{"upgrade-insecure-requests", "1"},
	{"accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"},
	{"sec-fetch-mode", "navigate"},
	{"sec-fetch-user", "?1"},
	{"sec-fetch-dest", "document"},
	{"accept-language", "en-US,en;q=0.9"},
	{"priority", "u=0, i"},
}

// secFetchSite reports what Chrome would put in Sec-Fetch-Site for this
// request: nothing came before it, the same origin brought us here, or another
// site did. Chrome also distinguishes `same-site` for a sibling host under one
// registrable domain; that needs a public-suffix list, and `cross-site` is the
// honest answer without one.
//
// It has to be derived rather than fixed, because a request that carries a
// Referer and claims `none` contradicts itself — and ultradox's whole fetch
// path depends on sending a search engine as the Referer.
func secFetchSite(rawURL, referer string) string {
	if referer == "" {
		return "none"
	}
	target, err := url.Parse(rawURL)
	if err != nil {
		return "cross-site"
	}
	from, err := url.Parse(referer)
	if err != nil || from.Host == "" {
		return "cross-site"
	}
	if strings.EqualFold(from.Host, target.Host) {
		return "same-origin"
	}
	return "cross-site"
}

// applyChromeNavigationHeaders fills in the headers a real Chrome navigation
// carries, for names the caller has not set itself. The caller always wins: a
// parser that sends its own Accept (ultradox asks for
// application/x-bittorrent) or its own Referer means it.
func applyChromeNavigationHeaders(h fhttp.Header, rawURL string) {
	for _, kv := range chromeNavigationHeaders {
		if h.Get(kv[0]) == "" {
			h.Set(kv[0], kv[1])
		}
	}
	if h.Get("sec-fetch-site") == "" {
		h.Set("sec-fetch-site", secFetchSite(rawURL, h.Get("referer")))
	}
}

// applyChromeHeaderOrder tags the request with the header order fhttp should
// serialize. Names absent from the request are skipped by fhttp.
func applyChromeHeaderOrder(h fhttp.Header) {
	h[fhttp.HeaderOrderKey] = chromeHeaderOrder
	h[fhttp.PHeaderOrderKey] = []string{":method", ":authority", ":scheme", ":path"}
}
