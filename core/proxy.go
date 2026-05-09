package core

import (
	"crypto/tls"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"jacred/app"
)

// TransportForURL returns a cached *http.Transport reflecting the given
// (proxy, insecureTLS) combination, or nil when neither knob applies (in
// which case the caller should use the default-pool http.Client).
//
// Both the regex compilation for globalproxy patterns and the transport
// itself are cached process-wide. This keeps the keep-alive pool warm across
// thousands of requests instead of allocating a fresh transport (and a fresh
// connection pool) per fetch.
func TransportForURL(rawURL string, useProxy, insecureSkipVerify bool, cfg app.Config) *http.Transport {
	proxyURL, useAuth, user, pass := pickProxy(rawURL, useProxy, cfg)
	if proxyURL == "" && !insecureSkipVerify {
		return nil
	}
	return cachedTransport(transportKey{
		proxyURL: proxyURL,
		useAuth:  useAuth,
		user:     user,
		pass:     pass,
		insecure: insecureSkipVerify,
	})
}

// ProxyForURL is retained for backward compatibility (returns the raw
// transport if a proxy applies, ignoring insecureTLS). Prefer
// TransportForURL — it folds the TLS-skip decision into the cache key.
func ProxyForURL(rawURL string, useProxy bool, cfg app.Config) *http.Transport {
	return TransportForURL(rawURL, useProxy, false, cfg)
}

func pickProxy(rawURL string, useProxy bool, cfg app.Config) (proxyURL string, useAuth bool, user, pass string) {
	if useProxy {
		for _, gp := range cfg.GlobalProxy {
			if len(gp.List) > 0 {
				return pickRandom(gp.List), gp.UseAuth, gp.Username, gp.Password
			}
		}
	}
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

// --- transport cache ---------------------------------------------------------

type transportKey struct {
	proxyURL string
	useAuth  bool
	user     string
	pass     string
	insecure bool
}

var (
	transportMu    sync.RWMutex
	transportCache = map[transportKey]*http.Transport{}
)

func cachedTransport(key transportKey) *http.Transport {
	transportMu.RLock()
	t, ok := transportCache[key]
	transportMu.RUnlock()
	if ok {
		return t
	}
	transportMu.Lock()
	defer transportMu.Unlock()
	if t, ok := transportCache[key]; ok {
		return t
	}
	t = newPooledTransport()
	if key.proxyURL != "" {
		pURL, err := url.Parse(strings.TrimSpace(key.proxyURL))
		if err != nil {
			return nil
		}
		if key.useAuth && key.user != "" {
			pURL.User = url.UserPassword(key.user, key.pass)
		}
		t.Proxy = http.ProxyURL(pURL)
	}
	if key.insecure {
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	transportCache[key] = t
	return t
}

// newPooledTransport returns an http.Transport with the standard library's
// default tuning (keep-alive pool, HTTP/2, TLS handshake timeout). Cloning
// http.DefaultTransport mirrors net/http's behavior so we don't surprise the
// runtime with bespoke settings.
func newPooledTransport() *http.Transport {
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		return dt.Clone()
	}
	return &http.Transport{}
}
