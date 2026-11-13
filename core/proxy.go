package core

import (
	"math/rand"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"jacred/app"
)

// FetchProfile identifies the network path a request takes: which proxy (if
// any) and whether TLS verification is skipped. It is a comparable value used
// directly as the cache key for the impersonating HTTP clients in
// tlsclient.go, so every distinct (proxy, tls) combination keeps its own warm
// keep-alive pool instead of allocating a client per fetch.
//
// This replaced a *http.Transport: TLS fingerprint impersonation happens below
// the transport, so the shape of the connection is now a property of the
// client, not something a stdlib transport can express.
type FetchProfile struct {
	proxyURL string
	useAuth  bool
	user     string
	pass     string
	insecure bool
	// noRedirect keeps the client from following redirects. It is part of the
	// key because it is a property of the cached client, not of the request:
	// a login POST needs the 302 itself, since that is where Set-Cookie
	// arrives.
	noRedirect bool
}

// profileForURL resolves the proxy rules in cfg for rawURL. The zero profile
// means "direct, verified TLS" — still a valid key, unlike the old
// transport-based scheme where that case was represented by nil.
func profileForURL(rawURL string, useProxy, insecureSkipVerify bool, cfg app.Config) FetchProfile {
	proxyURL, useAuth, user, pass := pickProxy(rawURL, useProxy, cfg)
	return FetchProfile{
		proxyURL: proxyURL,
		useAuth:  useAuth,
		user:     user,
		pass:     pass,
		insecure: insecureSkipVerify,
	}
}

// proxyURLWithAuth renders the profile's proxy as a URL string, folding in
// credentials when the config asked for authentication. Empty when direct.
func (p FetchProfile) proxyURLWithAuth() (string, error) {
	if p.proxyURL == "" {
		return "", nil
	}
	u, err := url.Parse(strings.TrimSpace(p.proxyURL))
	if err != nil {
		return "", err
	}
	if p.useAuth && p.user != "" {
		u.User = url.UserPassword(p.user, p.pass)
	}
	return u.String(), nil
}

// pickProxy resolves which proxy, if any, carries a request for rawURL.
//
// The two mechanisms are deliberately separate, and conflating them is what
// sent two trackers through Tor in production (2026-11-13):
//
//   - A `pattern` scopes its entry to the URLs it matches. That scope holds
//     for every tracker, opted in or not, and it is checked first because it
//     is the more specific statement.
//   - `useproxy: true` on a tracker opts it into the *default* proxy — an
//     entry carrying no pattern. It must never reach a scoped entry.
//
// This used to take the first entry with a non-empty list the moment
// useProxy was set, ignoring `pattern` entirely. DefaultConfig ships
// `{pattern: \.onion, list: [socks5://127.0.0.1:9050]}`, so `useproxy: true`
// silently meant "send this tracker through Tor" — on a config whose only
// proxy statement was about .onion addresses. Measured consequence: the
// cf_clearance minted by the browser on the box's own IP was replayed from a
// rotating Tor exit, so Cloudflare challenged every replay and every login
// POST, while the same code from the same LAN logged in first try when it
// went out direct.
//
// Upstream keeps them apart for the same reason: `ResolveProxies`
// (Infrastructure/Networking/HttpClient.cs) matches globalproxy by pattern
// only, and `useproxy` selects a different setting entirely.
func pickProxy(rawURL string, useProxy bool, cfg app.Config) (proxyURL string, useAuth bool, user, pass string) {
	for _, gp := range cfg.GlobalProxy {
		if gp.Pattern == "" || len(gp.List) == 0 {
			continue
		}
		re := getProxyRegex(gp.Pattern)
		if re == nil {
			continue
		}
		if re.MatchString(rawURL) {
			return pickRandom(gp.List), gp.UseAuth, gp.Username, gp.Password
		}
	}
	if useProxy {
		for _, gp := range cfg.GlobalProxy {
			if gp.Pattern != "" || len(gp.List) == 0 {
				continue
			}
			return pickRandom(gp.List), gp.UseAuth, gp.Username, gp.Password
		}
	}
	return "", false, "", ""
}

func pickRandom(list []string) string {
	if len(list) == 1 {
		return list[0]
	}
	return list[rand.Intn(len(list))]
}

// --- regex pattern cache -----------------------------------------------------

var (
	proxyRegexMu    sync.RWMutex
	proxyRegexCache = map[string]*regexp.Regexp{}
	// Sentinel for patterns that failed to compile so we don't retry forever.
	proxyRegexBad = (*regexp.Regexp)(nil)
)

func getProxyRegex(pattern string) *regexp.Regexp {
	proxyRegexMu.RLock()
	re, ok := proxyRegexCache[pattern]
	proxyRegexMu.RUnlock()
	if ok {
		return re
	}
	proxyRegexMu.Lock()
	defer proxyRegexMu.Unlock()
	if re, ok := proxyRegexCache[pattern]; ok {
		return re
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		proxyRegexCache[pattern] = proxyRegexBad
		return nil
	}
	proxyRegexCache[pattern] = compiled
	return compiled
}

// Client caching now lives in tlsclient.go, keyed by FetchProfile.
