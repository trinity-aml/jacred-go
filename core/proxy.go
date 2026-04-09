package core

import (
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"jacred/app"
)

// ProxyForURL returns an http.Transport with proxy configured, or nil if no proxy matches.
// Checks tracker-level useproxy first, then globalproxy patterns.
func ProxyForURL(rawURL string, useProxy bool, cfg app.Config) *http.Transport {
	// 1. Tracker-level proxy (uses first globalproxy with non-empty list as fallback)
	if useProxy {
		for _, gp := range cfg.GlobalProxy {
			if len(gp.List) > 0 {
				proxyURL := pickRandom(gp.List)
				return makeTransport(proxyURL, gp)
			}
		}
	}

	// 2. GlobalProxy pattern matching
	for _, gp := range cfg.GlobalProxy {
		if gp.Pattern == "" || len(gp.List) == 0 {
			continue
		}
		re, err := regexp.Compile(gp.Pattern)
		if err != nil {
			continue
		}
		if re.MatchString(rawURL) {
			proxyURL := pickRandom(gp.List)
			return makeTransport(proxyURL, gp)
		}
	}

	return nil
}

func pickRandom(list []string) string {
	if len(list) == 1 {
		return list[0]
	}
	return list[rand.Intn(len(list))]
}

func makeTransport(proxyAddr string, gp app.ProxySettings) *http.Transport {
	proxyAddr = strings.TrimSpace(proxyAddr)
	pURL, err := url.Parse(proxyAddr)
	if err != nil {
		return nil
	}
	if gp.UseAuth && gp.Username != "" {
		pURL.User = url.UserPassword(gp.Username, gp.Password)
	}
	return &http.Transport{
		Proxy: http.ProxyURL(pURL),
	}
}
