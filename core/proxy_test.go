package core

import (
	"testing"

	"jacred/app"
)

// A scoped proxy entry must not become the default for any tracker that sets
// useproxy: true. This is the production failure of 2026-11-13 in miniature:
// DefaultConfig scopes Tor to \.onion, Kinozal and Rutracker carried
// useproxy: true, and every request for them — the cf_clearance replay and the
// credentials POST included — left through a Tor exit. Cloudflare challenged
// all of it, which surfaced as "login blocked by a cloudflare challenge" and
// looked like a login bug.
func TestUseProxyDoesNotHijackAScopedEntry(t *testing.T) {
	cfg := app.DefaultConfig()
	// Guard the premise: if the shipped default stops scoping its proxy this
	// test silently stops testing anything.
	if len(cfg.GlobalProxy) == 0 || cfg.GlobalProxy[0].Pattern == "" {
		t.Fatalf("DefaultConfig no longer ships a scoped proxy entry: %+v", cfg.GlobalProxy)
	}

	got, _, _, _ := pickProxy("https://rutracker.org/forum/login.php", true, cfg)
	if got != "" {
		t.Errorf("useproxy: true reached the %q-scoped entry and would send rutracker through %s",
			cfg.GlobalProxy[0].Pattern, got)
	}
}

// The scope itself still has to work, and it applies whether or not the
// tracker opted in — that is the whole point of a pattern.
func TestAPatternAppliesWithoutUseProxy(t *testing.T) {
	cfg := app.DefaultConfig()
	want := cfg.GlobalProxy[0].List[0]
	for _, useProxy := range []bool{false, true} {
		got, _, _, _ := pickProxy("http://abcdef.onion/page", useProxy, cfg)
		if got != want {
			t.Errorf("useProxy=%v: .onion URL got %q, want %q", useProxy, got, want)
		}
	}
}

// An entry with no pattern is the default proxy, and that is what useproxy: true
// selects. Without this half the flag would mean nothing at all.
func TestUseProxySelectsTheUnscopedEntry(t *testing.T) {
	cfg := app.Config{GlobalProxy: []app.ProxySettings{
		{Pattern: `\.onion`, List: []string{"socks5://127.0.0.1:9050"}},
		{Pattern: "", List: []string{"http://proxy.example:8080"}, UseAuth: true, Username: "u", Password: "p"},
	}}

	got, useAuth, user, pass := pickProxy("https://rutracker.org/", true, cfg)
	if got != "http://proxy.example:8080" {
		t.Errorf("useproxy: true got %q, want the unscoped entry", got)
	}
	if !useAuth || user != "u" || pass != "p" {
		t.Errorf("credentials lost: useAuth=%v user=%q pass set=%v", useAuth, user, pass != "")
	}

	if got, _, _, _ := pickProxy("https://rutracker.org/", false, cfg); got != "" {
		t.Errorf("useproxy: false still got %q", got)
	}

	// A matching pattern is the more specific statement and keeps precedence.
	if got, _, _, _ := pickProxy("http://abcdef.onion/", true, cfg); got != "socks5://127.0.0.1:9050" {
		t.Errorf(".onion URL got %q, want the scoped entry", got)
	}
}
