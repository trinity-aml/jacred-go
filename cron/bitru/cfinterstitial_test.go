package bitru

import (
	"os"
	"strings"
	"testing"

	"jacred/core"
)

// russianInterstitial is the distinguishing markup of the body bitru answered
// with for 22 hours from 2026-11-14 22:00: HTTP 403, ~45 KB, Cloudflare's
// interstitial in the browser's own locale. The challenge token is truncated.
const russianInterstitial = `<!DOCTYPE html><html lang="ru-RU"><head><title>Один момент…</title>
<script>window._cf_chl_opt = {cFPWv: 'b', cH: 'TRUNCATED'};</script></head>
<body><span id="challenge-error-text">Выполнение проверки безопасности</span>
<input type="hidden" name="cf-turnstile-response" id="cf-chl-widget-cybag_response">
</body></html>`

// TestACloudflareInterstitialIsRecognisedInAnyLanguage is the behavioural half:
// jacred's own detector must not depend on the English title either, because
// everything downstream of it — keep the session or drop it, render or replay —
// is decided from this answer.
func TestACloudflareInterstitialIsRecognisedInAnyLanguage(t *testing.T) {
	if !core.LooksLikeCFChallenge([]byte(russianInterstitial)) {
		t.Error("a Russian-language Cloudflare interstitial is not recognised as a challenge")
	}
	if core.LooksLikeCFChallenge([]byte(`<html><head><title>Торренты | Фильмы</title></head>
<body><div id="logo"></div><a href="details.php?id=1">x</a>
<script src="/cdn-cgi/challenge-platform/scripts/jsd/main.js"></script></body></html>`)) {
		t.Error("a real listing is called a challenge; cf-managed pages all carry the jsd script")
	}
}

// TestAnInterstitialDoesNotBuyAColdSolve guards the amplifier. fetchBrowse used
// to invalidate the flare session on any non-2xx, which forces a cold solve —
// ~9 s in the browser plus the launch — for a page that will come back as the
// same interstitial. Measured on production over those 22 hours: ~180 cold
// solves an hour, 2 641 in all, every one reporting clearance=true and every
// page still 403.
//
// Fetcher owns that decision and has more to decide it with: it knows how old
// the clearance is, so it can tell "this cookie is fine, render the page" from
// "this cookie is stale, drop it". The parser must not override it.
func TestAnInterstitialDoesNotBuyAColdSolve(t *testing.T) {
	body := funcBody(t, "fetchBrowse")
	invalidate := strings.Index(body, "InvalidateSession")
	if invalidate < 0 {
		return // the call is gone entirely, which satisfies this guard
	}
	guard := strings.Index(body, "LooksLikeCFChallenge")
	if guard < 0 {
		t.Fatal("fetchBrowse invalidates the session without ever asking whether the body is a Cloudflare interstitial")
	}
	if guard > invalidate {
		t.Error("fetchBrowse invalidates the session before checking for an interstitial, so a challenge still costs a cold solve per page")
	}
}

// funcBody returns the named function's body with comment lines stripped. The
// comments above these branches name both helpers, so a scan that kept them
// would pass with the code deleted.
func funcBody(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("bitru.go")
	if err != nil {
		t.Fatalf("read bitru.go: %v", err)
	}
	src := string(raw)
	start := strings.Index(src, ") "+name+"(")
	if start < 0 {
		t.Fatalf("%s not found in bitru.go", name)
	}
	rest := src[start:]
	if end := strings.Index(rest, "\n}\n"); end >= 0 {
		rest = rest[:end]
	}
	var kept []string
	for _, line := range strings.Split(rest, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
