package server

import (
	"encoding/xml"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"jacred/app"
)

// pages are the documents served to a browser.
var pages = []string{"index.html", "stats.html", "settings.html", "trackers.html", "schedule.html"}

func readWWW(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(staticFS(), name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// externalResourceRe matches a resource the browser would fetch from another
// host — not a plain hyperlink, which is what <a href> is.
var externalResourceRe = regexp.MustCompile(`(?i)<(?:script|link|img|iframe)\b[^>]*\b(?:src|href)\s*=\s*["']https?://[^"']+`)

// The admin UI of a self-hosted service has to render with no outbound network.
// The pages used to load Tailwind from cdn.tailwindcss.com (a compiler that
// rebuilt the stylesheet in the browser on every open), Inter from Google Fonts
// and Font Awesome from cdnjs — so on a box with no internet, or one where those
// hosts are blocked, the settings page came up as unstyled HTML while the rest of
// the binary worked fine.
func TestPagesLoadNoExternalResources(t *testing.T) {
	for _, p := range pages {
		body := readWWW(t, p)
		for _, m := range externalResourceRe.FindAllString(body, -1) {
			t.Errorf("%s loads an external resource: %s", p, strings.TrimSpace(m))
		}
	}
}

// The generated stylesheet and the shared script are what replaced them.
func TestPagesUseSharedAssets(t *testing.T) {
	for _, p := range pages {
		body := readWWW(t, p)
		for _, want := range []string{`href="./assets/app.css"`, `src="./assets/app.js"`} {
			if strings.Count(body, want) != 1 {
				t.Errorf("%s: expected exactly one %s", p, want)
			}
		}
	}
	for _, asset := range []string{
		"assets/app.css",
		"assets/app.js",
		"assets/fonts/inter-cyrillic.woff2",
		"assets/fonts/inter-latin.woff2",
	} {
		if _, err := fs.ReadFile(staticFS(), asset); err != nil {
			t.Errorf("%s is not embedded: %v", asset, err)
		}
	}
}

// Every page mounts the shared header, and each names itself so the right link
// is marked current. index used to link to /stats and /settings while both of
// those linked only back to "/", so there was no way from statistics to
// settings without passing through the search page.
func TestPagesMountSharedHeader(t *testing.T) {
	want := map[string]string{
		"index.html":    `id="appHeader" data-page="search"`,
		"stats.html":    `id="appHeader" data-page="stats"`,
		"settings.html": `id="appHeader" data-page="settings"`,
		"trackers.html": `id="appHeader" data-page="trackers"`,
		"schedule.html": `id="appHeader" data-page="schedule"`,
	}
	for p, marker := range want {
		if !strings.Contains(readWWW(t, p), marker) {
			t.Errorf("%s does not mount the shared header (%s)", p, marker)
		}
	}
}

// The snippet has to run before the first paint, so it cannot move into
// assets/app.js and is duplicated by necessity. index.html shipped without it
// and hardcoded class="dark" on <html>, so a theme chosen on the settings page
// was ignored there — this pins all three to the same behaviour.
func TestThemeBootstrapIsIdenticalOnEveryPage(t *testing.T) {
	const marker = `document.documentElement.classList.toggle('dark', storedTheme === 'dark' || (storedTheme === null && prefersDark));`
	for _, p := range pages {
		body := readWWW(t, p)
		if strings.Count(body, marker) != 1 {
			t.Errorf("%s: theme bootstrap missing or duplicated", p)
		}
		if strings.Contains(body, `<html lang="ru" class="dark">`) {
			t.Errorf("%s hardcodes the dark class, overriding the stored choice", p)
		}
	}
}

// A Tailwind class with no rule fails silently in the browser, and app.css is a
// build artefact that a markup change can leave stale. This is the cheap half of
// that check: the classes the shared header itself needs must be present.
func TestGeneratedStylesheetCoversTheSharedHeader(t *testing.T) {
	css := readWWW(t, "assets/app.css")
	for _, sel := range []string{
		".app-shell", ".app-brand-mark", ".app-nav", ".app-nav-label",
		".app-nav-title", ".app-badge", ".app-foot", ".app-icon-btn",
		"#appHeader", "--surface-border", "@font-face",
		// settings.html defined these inline until trackers.html needed them too
		".field-input", ".field-label", ".checkbox-row",
	} {
		if !strings.Contains(css, sel) {
			t.Errorf("app.css is missing %s — rerun ./build_css.sh", sel)
		}
	}
}

// Every entry in the shared navigation must resolve to a page that exists,
// otherwise a link in the header 404s.
func TestNavigationTargetsExist(t *testing.T) {
	js := readWWW(t, "assets/app.js")
	routes := map[string]string{
		"'/'":         "index.html",
		"'/trackers'": "trackers.html",
		"'/schedule'": "schedule.html",
		"'/stats'":    "stats.html",
		"'/settings'": "settings.html",
	}
	for href, page := range routes {
		if !strings.Contains(js, "href: "+href) {
			t.Errorf("app.js navigation is missing %s", href)
		}
		if _, err := fs.ReadFile(staticFS(), page); err != nil {
			t.Errorf("%s is linked from the navigation but not embedded: %v", page, err)
		}
	}
}

// index.html has advertised <link rel="search" href="/opensearch.xml"> since the
// beginning while nothing served that path, so browsers silently found no search
// description. Anything the pages advertise must be routed.
func TestAdvertisedSearchDescriptionIsServed(t *testing.T) {
	body := readWWW(t, "index.html")
	m := regexp.MustCompile(`<link rel="search"[^>]*href="([^"]+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Skip("index.html no longer advertises a search description")
	}
	srv := &Server{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, m[1], nil)
	req.Host = "jacred.local:9117"
	srv.handleOpenSearch(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("%s answered %d, not 200", m[1], rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "opensearchdescription") {
		t.Errorf("content type = %q", ct)
	}
	var doc struct {
		ShortName string `xml:"ShortName"`
		URL       struct {
			Template string `xml:"template,attr"`
		} `xml:"Url"`
	}
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("document is not well-formed XML: %v", err)
	}
	if doc.ShortName == "" {
		t.Error("document has no ShortName")
	}
	// The template must be absolute and built from the host the client used —
	// a self-hosted instance has no canonical address.
	want := "http://jacred.local:9117/?s={searchTerms}"
	if doc.URL.Template != want {
		t.Errorf("template = %q, want %q", doc.URL.Template, want)
	}
}

// Behind a reverse proxy r.Host and the missing TLS state describe the back end,
// so an unforwarded template would point somewhere the browser cannot reach.
func TestOpenSearchHonoursForwardedHeaders(t *testing.T) {
	srv := &Server{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/opensearch.xml", nil)
	req.Host = "127.0.0.1:9117"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "jacred.example.org")
	srv.handleOpenSearch(rec, req)

	if !strings.Contains(rec.Body.String(), `template="https://jacred.example.org/?s={searchTerms}"`) {
		t.Errorf("forwarded host ignored:\n%s", rec.Body.String())
	}
}

// The Host header is attacker-controlled, so it must not be able to close the
// attribute it lands in.
func TestOpenSearchEscapesTheHost(t *testing.T) {
	srv := &Server{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/opensearch.xml", nil)
	req.Header.Set("X-Forwarded-Host", `evil"/><script>x</script>`)
	srv.handleOpenSearch(rec, req)

	if strings.Contains(rec.Body.String(), "<script>") {
		t.Errorf("host injected markup:\n%s", rec.Body.String())
	}
	var v any
	if err := xml.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Errorf("hostile host produced malformed XML: %v", err)
	}
}

// The description points at /?s=…; the page has to act on that parameter, or the
// browser's search box lands on an idle form. The form searched in place and
// never wrote the query to the address bar either, so no URL described a result.
func TestSearchPageHonoursTheQueryParameter(t *testing.T) {
	body := readWWW(t, "index.html")
	for _, want := range []string{
		`searchParams.get('s')`,  // a URL query runs the search
		`u.searchParams.set('s'`, // and a search produces such a URL
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index.html is missing %s — /?s=… would not work", want)
		}
	}
}

// The schedule page tells the reader to turn the scheduler on "в настройках"
// and links there. That promise was made before the control existed, so the
// only way to enable it was editing init.yaml by hand — the page pointed at a
// page that could not do what it said.
func TestSettingsExposesTheSchedulerControls(t *testing.T) {
	settings := readWWW(t, "settings.html")
	for _, want := range []string{`data-path="scheduler"`, `data-path="schedulerfile"`} {
		if strings.Count(settings, want) != 1 {
			t.Errorf("settings.html is missing exactly one %s", want)
		}
	}

	// And the link that makes the promise still resolves.
	schedule := readWWW(t, "schedule.html")
	if strings.Contains(schedule, `href="/settings"`) && !strings.Contains(settings, `data-path="scheduler"`) {
		t.Error("schedule.html sends the reader to /settings, which has no scheduler control")
	}

	// The settings page must also point back, or a reader who turns the
	// scheduler on has nowhere to go to write the rules.
	if !strings.Contains(settings, `href="/schedule"`) {
		t.Error("settings.html does not link to the page where the rules are edited")
	}
}

// The settings page keeps its own list of trackers in a JS literal, which is a
// hand-maintained duplicate of the roster in app.Config. It drifted: rudub and
// subsplease were wired into the config, the routes and /trackers, but never
// added here, so neither could be configured from the web UI at all.
//
// Nothing loses data when that happens — the form posts back a copy of the
// config it fetched, so an unlisted tracker's settings survive a save — which
// is exactly why the gap is quiet. This pins the list to the Go side instead.
func TestSettingsListsEveryConfigurableTracker(t *testing.T) {
	// Every TrackerSettings field of app.Config is a tracker with its own
	// config section, and its field name is the section name.
	var want []string
	cfg := reflect.TypeOf(app.Config{})
	for i := 0; i < cfg.NumField(); i++ {
		if cfg.Field(i).Type == reflect.TypeOf(app.TrackerSettings{}) {
			want = append(want, cfg.Field(i).Name)
		}
	}
	if len(want) < 20 {
		t.Fatalf("only %d tracker sections found in app.Config — the reflection is wrong, not the page", len(want))
	}

	body := readWWW(t, "settings.html")
	m := regexp.MustCompile(`(?s)const TRACKERS = \[(.*?)\]`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("settings.html no longer declares a TRACKERS list")
	}
	listed := map[string]bool{}
	for _, q := range regexp.MustCompile(`'([A-Za-z]+)'`).FindAllStringSubmatch(m[1], -1) {
		listed[q[1]] = true
	}

	for _, name := range want {
		if !listed[name] {
			t.Errorf("%s has a config section but is not on the settings page", name)
		}
		delete(listed, name)
	}
	for name := range listed {
		t.Errorf("settings page lists %q, which has no config section", name)
	}
}
