package app

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// captureLog collects everything written to the standard logger while fn runs.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	out, flags, prefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(&buf)
	log.SetFlags(0)
	log.SetPrefix("")
	defer func() {
		log.SetOutput(out)
		log.SetFlags(flags)
		log.SetPrefix(prefix)
	}()
	fn()
	return buf.String()
}

func TestStripInlineComment(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain value", "7000", "7000"},
		{"comment after spaces", "7000    # ms between requests", "7000"},
		{"comment after tab", "true\t# enable", "true"},
		{"comment with no gap is not a comment", "pa#ss", "pa#ss"},
		{"url fragment survives", "https://a.tv/x#frag", "https://a.tv/x#frag"},
		{"value is only a comment", "# nothing here", ""},
		{"quoted value keeps hash", `"pa # ss"`, `"pa # ss"`},
		{"quoted value drops trailing comment", `"127.0.0.1"   # "any" for all`, `"127.0.0.1"`},
		{"quoted empty then comment", `""   # your username`, `""`},
		{"escaped quote inside", `"a\"b # c"  # real comment`, `"a\"b # c"`},
		{"unterminated quote left alone", `"oops`, `"oops`},
		{"empty", "", ""},
		{"only spaces", "   ", ""},
		{"hash at end of bare word", "abc#", "abc#"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripInlineComment(tc.in); got != tc.want {
				t.Errorf("stripInlineComment(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Every value below carries an inline comment. Before the fix each one was
// swallowed into the value: ints became 0, bools became false, and strings
// kept the comment text.
func TestParseYAMLIntoConfigStripsInlineComments(t *testing.T) {
	const src = `
listenip: "127.0.0.1"     # "any" listens everywhere
listenport: 9117          # default port
log: true                 # general log
gcpercent: 50             # GOGC knob
timeStatsUpdate: 90       # seconds
syncapi: ""               # remote instance

evercache:                # in-memory buckets
  enable: true            # on
  validHour: 1            # TTL hours
  maxOpenWriteTask: 500   # cap

tracksinterval:           # minutes
  task0: 180              # primary
  task1: 60               # secondary

flaresolverr_go:          # CF solver
  headless: true          # no window
  browser_backend: geckodriver   # Camoufox

Rutor:                    # 11 categories
  host: "https://rutor.is"     # base url
  parseDelay: 7000        # ms between categories
  reqMinute: 8            # soft cap
  login:                  # optional
    u: ""                 # your username
    p: ""                 # your password
`
	cfg := DefaultConfig()
	parseYAMLIntoConfig(src, &cfg)

	if cfg.ListenIP != "127.0.0.1" {
		t.Errorf("ListenIP = %q, want %q", cfg.ListenIP, "127.0.0.1")
	}
	if cfg.ListenPort != 9117 {
		t.Errorf("ListenPort = %d, want 9117", cfg.ListenPort)
	}
	if !cfg.Log {
		t.Error("Log = false, want true")
	}
	if cfg.GCPercent != 50 {
		t.Errorf("GCPercent = %d, want 50", cfg.GCPercent)
	}
	if cfg.TimeStatsUpdate != 90 {
		t.Errorf("TimeStatsUpdate = %d, want 90", cfg.TimeStatsUpdate)
	}
	if cfg.SyncAPI != "" {
		t.Errorf("SyncAPI = %q, want empty", cfg.SyncAPI)
	}
	if !cfg.Evercache.Enable || cfg.Evercache.ValidHour != 1 || cfg.Evercache.MaxOpenWriteTask != 500 {
		t.Errorf("Evercache = %+v, want {true 1 500 …}", cfg.Evercache)
	}
	if cfg.TracksInterval.Task0 != 180 || cfg.TracksInterval.Task1 != 60 {
		t.Errorf("TracksInterval = %+v, want {180 60}", cfg.TracksInterval)
	}
	if cfg.FlareSolverrGo.Headless == nil || !*cfg.FlareSolverrGo.Headless {
		t.Error("FlareSolverrGo.Headless not true")
	}
	if cfg.FlareSolverrGo.BrowserBackend != "geckodriver" {
		t.Errorf("BrowserBackend = %q, want %q", cfg.FlareSolverrGo.BrowserBackend, "geckodriver")
	}
	if cfg.Rutor.Host != "https://rutor.is" {
		t.Errorf("Rutor.Host = %q, want %q", cfg.Rutor.Host, "https://rutor.is")
	}
	if cfg.Rutor.ParseDelay != 7000 {
		t.Errorf("Rutor.ParseDelay = %d, want 7000", cfg.Rutor.ParseDelay)
	}
	if cfg.Rutor.ReqMinute != 8 {
		t.Errorf("Rutor.ReqMinute = %d, want 8", cfg.Rutor.ReqMinute)
	}
	// An empty credential must stay empty — a comment leaking in here makes
	// parsers believe they are configured and attempt a login.
	if cfg.Rutor.Login.U != "" || cfg.Rutor.Login.P != "" {
		t.Errorf("Rutor.Login = %+v, want empty", cfg.Rutor.Login)
	}
}

func TestParseYAMLIntoConfigPreservesHashesInValues(t *testing.T) {
	const src = `
Kinozal:
  host: "https://kinozal.tv"
  cookie: "uid=1#2; pass=a#b"
  login:
    u: user#1
    p: "p@ss # word"

globalproxy:
  - pattern: \.onion    # tor only
    username: us#er
    list:
      - "socks5://127.0.0.1:9050"   # local tor
`
	cfg := DefaultConfig()
	parseYAMLIntoConfig(src, &cfg)

	if cfg.Kinozal.Cookie != "uid=1#2; pass=a#b" {
		t.Errorf("Cookie = %q, want %q", cfg.Kinozal.Cookie, "uid=1#2; pass=a#b")
	}
	if cfg.Kinozal.Login.U != "user#1" {
		t.Errorf("Login.U = %q, want %q", cfg.Kinozal.Login.U, "user#1")
	}
	if cfg.Kinozal.Login.P != "p@ss # word" {
		t.Errorf("Login.P = %q, want %q", cfg.Kinozal.Login.P, "p@ss # word")
	}
	if len(cfg.GlobalProxy) != 1 {
		t.Fatalf("GlobalProxy len = %d, want 1", len(cfg.GlobalProxy))
	}
	p := cfg.GlobalProxy[0]
	if p.Pattern != `\.onion` {
		t.Errorf("Pattern = %q, want %q", p.Pattern, `\.onion`)
	}
	if p.Username != "us#er" {
		t.Errorf("Username = %q, want %q", p.Username, "us#er")
	}
	if len(p.List) != 1 || p.List[0] != "socks5://127.0.0.1:9050" {
		t.Errorf("List = %v, want [socks5://127.0.0.1:9050]", p.List)
	}
}

func TestParseYAMLIntoConfigListsWithComments(t *testing.T) {
	const src = `
synctrackers:        # only these
  - "Rutor"          # main
  - Kinozal          # secondary

disable_trackers:
  - "Mazepa"         # too slow
`
	cfg := DefaultConfig()
	parseYAMLIntoConfig(src, &cfg)

	want := []string{"Rutor", "Kinozal"}
	if len(cfg.SyncTrackers) != len(want) {
		t.Fatalf("SyncTrackers = %v, want %v", cfg.SyncTrackers, want)
	}
	for i := range want {
		if cfg.SyncTrackers[i] != want[i] {
			t.Errorf("SyncTrackers[%d] = %q, want %q", i, cfg.SyncTrackers[i], want[i])
		}
	}
	if len(cfg.DisableTrackers) != 1 || cfg.DisableTrackers[0] != "Mazepa" {
		t.Errorf("DisableTrackers = %v, want [Mazepa]", cfg.DisableTrackers)
	}
}

func TestParseIntAtWarnsOnGarbage(t *testing.T) {
	tests := []struct {
		in       string
		want     int
		wantWarn bool
	}{
		{"7000", 7000, false},
		{`"7000"`, 7000, false},
		{"-1", -1, false},
		{"", 0, false},
		{"null", 0, false},
		{"~", 0, false},
		{"700O", 0, true},
		{"7000ms", 0, true},
		{"true", 0, true},
	}
	for _, tc := range tests {
		var got int
		out := captureLog(t, func() { got = parseIntAt("Rutor", "parseDelay", tc.in) })
		if got != tc.want {
			t.Errorf("parseIntAt(%q) = %d, want %d", tc.in, got, tc.want)
		}
		if warned := out != ""; warned != tc.wantWarn {
			t.Errorf("parseIntAt(%q) warned = %v, want %v (log: %q)", tc.in, warned, tc.wantWarn, out)
		}
		if tc.wantWarn && !strings.Contains(out, "Rutor.parseDelay") {
			t.Errorf("parseIntAt(%q) warning %q does not name the key", tc.in, out)
		}
	}
}

func TestParseBoolAtWarnsOnGarbage(t *testing.T) {
	tests := []struct {
		in       string
		want     bool
		wantWarn bool
	}{
		{"true", true, false},
		{"TRUE", true, false},
		{`"true"`, true, false},
		{"false", false, false},
		{"False", false, false},
		{"", false, false},
		{"null", false, false},
		{"yes", false, true},
		{"1", false, true},
		{"flase", false, true},
	}
	for _, tc := range tests {
		var got bool
		out := captureLog(t, func() { got = parseBoolAt("", "log", tc.in) })
		if got != tc.want {
			t.Errorf("parseBoolAt(%q) = %v, want %v", tc.in, got, tc.want)
		}
		if warned := out != ""; warned != tc.wantWarn {
			t.Errorf("parseBoolAt(%q) warned = %v, want %v (log: %q)", tc.in, warned, tc.wantWarn, out)
		}
	}
}

// A warning has to say where the bad value is, otherwise it is useless in a
// file with 21 tracker sections that all share the same key names.
func TestParseYAMLIntoConfigWarningsNameTheSection(t *testing.T) {
	const src = `
listenport: 91l7
log: yes

evercache:
  validHour: two

tracksinterval:
  task0: ninety

flaresolverr_go:
  headless: 1

Rutor:
  parseDelay: 700O

Kinozal:
  reqMinute: 6
  insecureSkipVerify: on
`
	cfg := DefaultConfig()
	out := captureLog(t, func() { parseYAMLIntoConfig(src, &cfg) })

	for _, want := range []string{
		`listenport: "91l7"`,
		`log: "yes"`,
		`evercache.validHour: "two"`,
		`tracksinterval.task0: "ninety"`,
		`flaresolverr_go.headless: "1"`,
		`Rutor.parseDelay: "700O"`,
		`Kinozal.insecureSkipVerify: "on"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log is missing %s\ngot:\n%s", want, out)
		}
	}
	// The one valid value must not warn.
	if strings.Contains(out, "reqMinute") {
		t.Errorf("valid reqMinute warned:\n%s", out)
	}
	if cfg.Rutor.ParseDelay != 0 || cfg.Kinozal.ReqMinute != 6 {
		t.Errorf("Rutor.ParseDelay=%d Kinozal.ReqMinute=%d", cfg.Rutor.ParseDelay, cfg.Kinozal.ReqMinute)
	}
}

// A clean config must stay silent — warnings are only useful if they are rare.
func TestParseYAMLIntoConfigSilentOnValidInput(t *testing.T) {
	const src = `
listenport: 9117          # port
log: true                 # on
synctrackers: []
evercache:
  enable: true
  validHour: 1
Rutor:
  host: "https://rutor.is"
  parseDelay: 7000
  login:
    u: ""
    p: ""
globalproxy:
  - pattern: \.onion
    useAuth: false
    list:
      - "socks5://127.0.0.1:9050"
`
	cfg := DefaultConfig()
	out := captureLog(t, func() { parseYAMLIntoConfig(src, &cfg) })
	if out != "" {
		t.Errorf("valid config produced warnings:\n%s", out)
	}
}

// The `useragent` key exists so a cf_clearance cookie copied out of a real
// browser keeps working: Cloudflare binds it to the User-Agent that solved the
// challenge, so the pair has to travel together through parse and write-back.
func TestTrackerUserAgentRoundTrips(t *testing.T) {
	const ua = "Mozilla/5.0 (X11; Linux x86_64; rv:153.0) Gecko/20100101 Firefox/153.0"
	src := `
Rutracker:
  host: "https://rutracker.org"
  cookie: "cf_clearance=abc; bb_session=xyz"
  useragent: "` + ua + `"
  reqMinute: 8
`
	var cfg Config
	parseYAMLIntoConfig(src, &cfg)

	if cfg.Rutracker.UserAgent != ua {
		t.Fatalf("UserAgent = %q, want %q", cfg.Rutracker.UserAgent, ua)
	}
	if cfg.Rutracker.Cookie != "cf_clearance=abc; bb_session=xyz" {
		t.Errorf("Cookie = %q", cfg.Rutracker.Cookie)
	}

	// Saving settings from the web UI must not silently drop it.
	out := MarshalYAML(cfg)
	if !strings.Contains(out, "useragent: ") || !strings.Contains(out, ua) {
		t.Errorf("useragent lost on write-back; got:\n%s", out)
	}
	var back Config
	parseYAMLIntoConfig(out, &back)
	if back.Rutracker.UserAgent != ua {
		t.Errorf("after round-trip UserAgent = %q, want %q", back.Rutracker.UserAgent, ua)
	}
}
