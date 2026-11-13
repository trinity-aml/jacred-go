package core

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"jacred/app"

	tls "github.com/bogdanfinn/utls"
)

// samePage decides whether the browser's rendered body may be handed back as
// the answer to the URL we asked for. Getting it wrong is silent: a "no" that
// should be "yes" costs one HTTP roundtrip, but a "yes" that should be "no"
// feeds the parser somebody else's page and it reads as an empty listing.
func TestSamePage(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
		same bool
	}{
		{"identical", "https://rutracker.org/forum/viewforum.php?f=2090", "https://rutracker.org/forum/viewforum.php?f=2090", true},
		{"trailing slash", "https://bitru.org/browse/", "https://bitru.org/browse", true},
		{"host case", "https://RuTracker.org/forum/index.php", "https://rutracker.org/forum/index.php", true},
		{"fragment ignored", "https://rutor.is/browse/0#top", "https://rutor.is/browse/0", true},
		// The case this exists for: rutracker bounces a cold profile to the
		// forum index, which has no topic rows at all.
		{"bounced to index", "https://rutracker.org/forum/index.php", "https://rutracker.org/forum/viewforum.php?f=2090", false},
		{"different query", "https://rutracker.org/forum/viewforum.php?f=1", "https://rutracker.org/forum/viewforum.php?f=2090", false},
		{"different host", "https://mirror.rutor.is/browse/0", "https://rutor.is/browse/0", false},
		// No final URL reported (older library): keep the body rather than
		// throwing away a good response.
		{"unknown final url", "", "https://rutracker.org/forum/viewforum.php?f=2090", true},
	}
	for _, tc := range tests {
		if got := samePage(tc.got, tc.want); got != tc.same {
			t.Errorf("%s: samePage(%q, %q) = %v, want %v", tc.name, tc.got, tc.want, got, tc.same)
		}
	}
}

// The UA we send when nothing else supplies one has to advertise the same
// Chrome major as the browser that mints the clearance, or the pair is
// self-contradicting. Derived from cloakBrowserFreeTag rather than written out
// again, so bumping the browser cannot leave a stale UA behind.
func TestDefaultUserAgentMatchesTheBrowserBuild(t *testing.T) {
	major := strings.TrimSuffix(strings.TrimPrefix(cloakBrowserFreeTag, "chromium-v"), ".")
	if major == "" {
		t.Fatalf("cannot read a Chrome major out of cloakBrowserFreeTag %q", cloakBrowserFreeTag)
	}
	if want := "Chrome/" + major; !strings.Contains(defaultUserAgent, want) {
		t.Errorf("defaultUserAgent %q does not advertise %q, the CloakBrowser build it is paired with", defaultUserAgent, want)
	}
}

// browserHelloExtensions is the extension set CloakBrowser actually presents,
// measured against tls.peet.ws on 2026-11-13 (Chromium 146.0.7680.177):
//
//	0-5-10-11-13-16-18-23-27-35-43-45-51-17613-65037-65281
//
// GREASE ids are excluded because they are random per handshake.
//
// This is the thing impersonateProfile has to reproduce, and it is NOT implied
// by the profile's version number: tls-client's Chrome_146 adds `trust_anchors`
// (0xca34 = 51764), which this Chromium 146 does not send, and that single
// extra extension is a visible mismatch on every clearance replay. Chrome_144
// is the same profile without it.
var browserHelloExtensions = []uint16{0, 5, 10, 11, 13, 16, 18, 23, 27, 35, 43, 45, 51, 17613, 65037, 65281}

// greaseExtension reports the 16 reserved GREASE ids (0x?A?A with both bytes
// equal), which a handshake picks at random and a fingerprint comparison must
// therefore ignore.
func greaseExtension(id uint16) bool {
	return byte(id>>8) == byte(id) && byte(id)&0x0f == 0x0a
}

// TestImpersonationMatchesTheBrowserHello reads the extension ids out of the
// chosen profile's own ClientHelloSpec, so it checks the handshake that will
// actually go on the wire rather than a name. Verified by setting
// impersonateProfile back to profiles.Chrome_146 and watching it fail on
// extension 51764.
func TestImpersonationMatchesTheBrowserHello(t *testing.T) {
	spec, err := impersonateProfile.GetClientHelloSpec()
	if err != nil {
		t.Fatalf("GetClientHelloSpec: %v", err)
	}
	got := map[uint16]bool{}
	for _, ext := range spec.Extensions {
		// SNI serialises to nothing until a hostname is set — the handshake
		// fills it from the request — so give it one, or extension 0 would
		// read as absent.
		if sni, ok := ext.(*tls.SNIExtension); ok {
			sni.ServerName = "example.org"
		}
		// Read writes the extension starting with its 2-byte type, which is
		// the only id this interface exposes. It reports io.EOF once it has
		// written everything, which is success, not a failure.
		buf := make([]byte, ext.Len())
		n, err := ext.Read(buf)
		if n < 2 {
			t.Fatalf("extension %T serialised to %d bytes: %v", ext, n, err)
		}
		id := uint16(buf[0])<<8 | uint16(buf[1])
		if greaseExtension(id) {
			continue
		}
		got[id] = true
	}
	want := map[uint16]bool{}
	for _, id := range browserHelloExtensions {
		want[id] = true
	}
	for id := range got {
		if !want[id] {
			t.Errorf("impersonateProfile (%s) sends extension %d (0x%04x), which the CloakBrowser build does not — the clearance replay is distinguishable from the client that earned it",
				impersonateProfile.GetClientHelloStr(), id, id)
		}
	}
	for id := range want {
		if !got[id] {
			t.Errorf("impersonateProfile (%s) omits extension %d (0x%04x), which the CloakBrowser build sends",
				impersonateProfile.GetClientHelloStr(), id, id)
		}
	}
}

// A plain fetch has to look like the browser that minted the clearance all the
// way up the stack, not only in the ClientHello. Before this, chromeHeaderOrder
// listed a dozen header names and nothing set any of them, so every request
// went out with three headers in a carefully ordered list of three.
func TestPlainFetchCarriesTheChromeHeaderSet(t *testing.T) {
	// Keys are compared lowercase: http.Header.Get canonicalises, and the
	// point here is the exact name on the wire.
	got := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clear(got)
		for k, v := range r.Header {
			if len(v) > 0 {
				got[strings.ToLower(k)] = v[0]
			}
		}
		_, _ = w.Write([]byte("<html>ok</html>"))
	}))
	defer srv.Close()

	f := NewFetcher(app.DefaultConfig())
	if _, err := f.doHTTP("GET", srv.URL, "sid=1", defaultUserAgent, "", nil, nil, FetchProfile{}); err != nil {
		t.Fatalf("doHTTP: %v", err)
	}
	for _, kv := range chromeNavigationHeaders {
		if got[kv[0]] != kv[1] {
			t.Errorf("header %s = %q, want %q", kv[0], got[kv[0]], kv[1])
		}
	}
	// Derived, not fixed: no Referer means nothing came before this request.
	if v := got["sec-fetch-site"]; v != "none" {
		t.Errorf("sec-fetch-site = %q, want none for a request with no Referer", v)
	}
	// The caller still owns what it sets.
	if _, err := f.doHTTP("GET", srv.URL, "", defaultUserAgent, "", nil,
		map[string]string{"Accept": "application/x-bittorrent", "Referer": "https://www.google.com/"}, FetchProfile{}); err != nil {
		t.Fatalf("doHTTP: %v", err)
	}
	if v := got["accept"]; v != "application/x-bittorrent" {
		t.Errorf("caller's Accept was overwritten with %q", v)
	}
	if v := got["sec-fetch-site"]; v != "cross-site" {
		t.Errorf("sec-fetch-site = %q, want cross-site for a google Referer", v)
	}
}

// Every name the default set sends has to appear in the order list, or fhttp
// serialises it wherever it likes and the h2 header order gives the client
// away just as the missing headers did.
func TestEveryDefaultHeaderIsOrdered(t *testing.T) {
	ordered := map[string]bool{}
	for _, n := range chromeHeaderOrder {
		ordered[n] = true
	}
	names := append([]string{"sec-fetch-site", "user-agent", "cookie"}, nil...)
	for _, kv := range chromeNavigationHeaders {
		names = append(names, kv[0])
	}
	for _, n := range names {
		if !ordered[n] {
			t.Errorf("%q is sent but missing from chromeHeaderOrder", n)
		}
	}
}
