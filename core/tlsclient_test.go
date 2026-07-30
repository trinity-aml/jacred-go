package core

import (
	"strings"
	"testing"
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

// The UA we send when nothing else supplies one has to agree with the TLS
// fingerprint we present, or the pair is self-contradicting.
func TestDefaultUserAgentMatchesImpersonationProfile(t *testing.T) {
	if want := "Chrome/146"; !strings.Contains(defaultUserAgent, want) {
		t.Errorf("defaultUserAgent %q does not advertise %q — impersonateProfile is Chrome_146", defaultUserAgent, want)
	}
	if name := impersonateProfile.GetClientHelloStr(); !strings.Contains(name, "146") {
		t.Errorf("impersonateProfile is %q, expected a Chrome 146 profile to match defaultUserAgent", name)
	}
}
