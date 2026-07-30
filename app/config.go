package app

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
)

// LoadConfig reads init.yaml and merges it onto DefaultConfig. When the file
// is missing, a default config is written to that path and returned — first
// run shouldn't crash the binary, the user can edit init.yaml afterwards.
// Other I/O or parse errors are surfaced as-is.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			text := MarshalYAML(cfg)
			if werr := os.WriteFile(path, []byte(text), 0o644); werr != nil {
				return cfg, fmt.Errorf("create default %s: %w", path, werr)
			}
			log.Printf("config: %s not found — wrote default config", path)
			return cfg, nil
		}
		return cfg, err
	}
	parseYAMLIntoConfig(string(b), &cfg)
	return cfg, nil
}

func parseYAMLIntoConfig(text string, cfg *Config) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var currentTracker *TrackerSettings
	var currentProxy *ProxySettings
	section := ""
	inTrackerLogin := false
	inEvercache := false
	inFlareSolverrGo := false
	inTracksInterval := false
	currentListTarget := ""

	for _, rawLine := range lines {
		raw := strings.TrimRight(rawLine, "\ufeff \t")
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || trimmed == "---" {
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " "))

		// Section headers and nested-block keys can carry a trailing comment
		// too, but they are matched on the whole line rather than on a value.
		// Only consult the stripped form when the line holds no quoted scalar,
		// so a value such as `p: "a: #b:"` can never be mistaken for a header.
		structural := trimmed
		if !strings.Contains(trimmed, `"`) {
			if structural = stripInlineComment(trimmed); structural == "" {
				continue
			}
		}

		if indent == 0 && strings.HasSuffix(structural, ":") {
			section = strings.TrimSuffix(structural, ":")
			currentTracker = trackerByName(cfg, section)
			currentProxy = nil
			inTrackerLogin = false
			inEvercache = false
			inFlareSolverrGo = false
			inTracksInterval = false
			currentListTarget = ""
			if isListKey(section) {
				// `synctrackers:` on its own line opens a list block whose
				// items follow as `- item`. Without this it would be taken for
				// a section header and every item silently dropped — only the
				// `synctrackers: []` spelling used to collect them.
				setConfigKV(cfg, section, "")
				currentListTarget = section
			}
			if section == "globalproxy" {
				cfg.GlobalProxy = nil
			}
			continue
		}

		if section == "globalproxy" {
			if indent == 2 && strings.HasPrefix(trimmed, "- ") {
				cfg.GlobalProxy = append(cfg.GlobalProxy, ProxySettings{})
				currentProxy = &cfg.GlobalProxy[len(cfg.GlobalProxy)-1]
				currentListTarget = ""
				rest := strings.TrimPrefix(structural, "- ")
				if strings.Contains(rest, ":") {
					k, v := splitKV(rest)
					setProxyKV(currentProxy, "globalproxy", k, v)
					if k == "list" {
						currentListTarget = "proxy.list"
					}
				}
				continue
			}
			if currentProxy != nil && indent == 4 && strings.Contains(trimmed, ":") {
				k, v := splitKV(trimmed)
				setProxyKV(currentProxy, "globalproxy", k, v)
				if k == "list" {
					currentListTarget = "proxy.list"
				} else {
					currentListTarget = ""
				}
				continue
			}
			if currentProxy != nil && indent >= 6 && strings.HasPrefix(trimmed, "- ") && currentListTarget == "proxy.list" {
				currentProxy.List = append(currentProxy.List, unquote(stripInlineComment(strings.TrimPrefix(trimmed, "- "))))
				continue
			}
		}

		if currentTracker != nil {
			if indent == 2 && strings.HasSuffix(structural, ":") {
				name := strings.TrimSuffix(structural, ":")
				inTrackerLogin = name == "login"
				continue
			}
			if indent == 2 && strings.Contains(trimmed, ":") {
				k, v := splitKV(trimmed)
				setTrackerKV(currentTracker, section, k, v)
				continue
			}
			if inTrackerLogin && indent == 4 && strings.Contains(trimmed, ":") {
				k, v := splitKV(trimmed)
				if k == "u" {
					currentTracker.Login.U = unquote(v)
				}
				if k == "p" {
					currentTracker.Login.P = unquote(v)
				}
				continue
			}
		}

		if section == "evercache" && indent == 2 && strings.Contains(trimmed, ":") {
			inEvercache = true
			k, v := splitKV(trimmed)
			switch k {
			case "enable":
				cfg.Evercache.Enable = parseBoolAt("evercache", k, v)
			case "validHour":
				cfg.Evercache.ValidHour = parseIntAt("evercache", k, v)
			case "maxOpenWriteTask":
				cfg.Evercache.MaxOpenWriteTask = parseIntAt("evercache", k, v)
			case "dropCacheTake":
				cfg.Evercache.DropCacheTake = parseIntAt("evercache", k, v)
			}
			continue
		}
		if section == "tracksinterval" && indent == 2 && strings.Contains(trimmed, ":") {
			inTracksInterval = true
			k, v := splitKV(trimmed)
			switch k {
			case "task0":
				cfg.TracksInterval.Task0 = parseIntAt("tracksinterval", k, v)
			case "task1":
				cfg.TracksInterval.Task1 = parseIntAt("tracksinterval", k, v)
			}
			continue
		}
		if section == "flaresolverr_go" && indent == 2 && strings.Contains(trimmed, ":") {
			inFlareSolverrGo = true
			k, v := splitKV(trimmed)
			switch k {
			case "browser_backend":
				cfg.FlareSolverrGo.BrowserBackend = unquote(v)
			case "browser_path":
				cfg.FlareSolverrGo.BrowserPath = unquote(v)
			case "driver_path":
				cfg.FlareSolverrGo.DriverPath = unquote(v)
			case "headless":
				val := parseBoolAt("flaresolverr_go", k, v)
				cfg.FlareSolverrGo.Headless = &val
			case "chrome_version":
				cfg.FlareSolverrGo.ChromeVersion = unquote(v)
			}
			continue
		}

		if indent == 0 && strings.Contains(trimmed, ":") {
			k, v := splitKV(trimmed)
			setConfigKV(cfg, k, v)
			if isListKey(k) {
				currentListTarget = k
			} else if v != "" {
				currentListTarget = ""
			}
			if k == "evercache" {
				inEvercache = true
				continue
			}
			if k == "flaresolverr_go" {
				inFlareSolverrGo = true
				continue
			}
			if k == "tracksinterval" {
				inTracksInterval = true
				continue
			}
		}

		if indent == 2 && strings.HasPrefix(trimmed, "- ") {
			val := unquote(stripInlineComment(strings.TrimPrefix(trimmed, "- ")))
			switch currentListTarget {
			case "synctrackers":
				cfg.SyncTrackers = append(cfg.SyncTrackers, val)
			case "disable_trackers":
				cfg.DisableTrackers = append(cfg.DisableTrackers, val)
			case "tsuri":
				cfg.TSURI = append(cfg.TSURI, val)
			}
		}
		_ = inEvercache
		_ = inFlareSolverrGo
		_ = inTracksInterval
	}
}

func isListKey(k string) bool {
	switch k {
	case "synctrackers", "disable_trackers", "tsuri":
		return true
	default:
		return false
	}
}

func trackerByName(cfg *Config, name string) *TrackerSettings {
	switch name {
	case "Rutor":
		return &cfg.Rutor
	case "Megapeer":
		return &cfg.Megapeer
	case "TorrentBy":
		return &cfg.TorrentBy
	case "Kinozal":
		return &cfg.Kinozal
	case "NNMClub":
		return &cfg.NNMClub
	case "Bitru":
		return &cfg.Bitru
	case "Toloka":
		return &cfg.Toloka
	case "Mazepa":
		return &cfg.Mazepa
	case "Rutracker":
		return &cfg.Rutracker
	case "Selezen":
		return &cfg.Selezen
	case "Lostfilm":
		return &cfg.Lostfilm
	case "Animelayer":
		return &cfg.Animelayer
	case "Anidub":
		return &cfg.Anidub
	case "Anilibria", "Aniliberty":
		return &cfg.Aniliberty
	case "Knaben":
		return &cfg.Knaben
	case "Anistar":
		return &cfg.Anistar
	case "Anifilm":
		return &cfg.Anifilm
	case "Leproduction":
		return &cfg.Leproduction
	case "Korsars":
		return &cfg.Korsars
	case "Ultradox":
		return &cfg.Ultradox
	case "Viruseproject":
		return &cfg.Viruseproject
	case "Anibelka":
		return &cfg.Anibelka
	default:
		return nil
	}
}

func splitKV(s string) (string, string) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return strings.TrimSpace(s), ""
	}
	return strings.TrimSpace(parts[0]), stripInlineComment(parts[1])
}

// stripInlineComment drops a trailing YAML-style comment from a raw value and
// trims the remainder. Following YAML, a '#' opens a comment only when it
// starts the value or is preceded by whitespace, so hashes inside tokens —
// passwords, URL fragments, regexes — survive untouched. Inside a
// double-quoted scalar '#' is always literal; anything past the closing quote
// is dropped. An unterminated quote is returned as-is for unquote to handle.
func stripInlineComment(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return v
	}
	if v[0] == '"' {
		for i := 1; i < len(v); i++ {
			switch v[i] {
			case '\\':
				i++
			case '"':
				return v[:i+1]
			}
		}
		return v
	}
	for i := 0; i < len(v); i++ {
		if v[i] == '#' && (i == 0 || v[i-1] == ' ' || v[i-1] == '\t') {
			return strings.TrimRight(v[:i], " \t")
		}
	}
	return v
}

func unquote(v string) string {
	v = strings.TrimSpace(v)
	if v == "null" {
		return ""
	}
	return strings.Trim(strings.TrimSpace(v), `"`)
}

// isBlankValue reports values that carry no information, so they are left to
// the caller's zero value without a warning.
func isBlankValue(s string) bool {
	return s == "" || s == "null" || s == "~"
}

// keyPath renders the position of a key for log messages: "Rutor.parseDelay"
// for a tracker key, plain "listenport" for a root-level one.
func keyPath(where, k string) string {
	if where == "" {
		return k
	}
	return where + "." + k
}

// parseIntAt converts a config value to an int and warns when it cannot.
// Config parsing is otherwise silent — an unreadable line just leaves a zero
// behind — so without this a typo like `parseDelay: 700O` disables the delay
// with no trace in the log.
func parseIntAt(where, k, v string) int {
	s := strings.Trim(strings.TrimSpace(v), `"`)
	if isBlankValue(s) {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		log.Printf("config: %s: %q is not a number — using 0", keyPath(where, k), s)
		return 0
	}
	return n
}

// parseBoolAt accepts only true/false (any case) and warns on anything else,
// including near-misses like yes/on/1 that other YAML parsers would accept.
func parseBoolAt(where, k, v string) bool {
	s := strings.Trim(strings.TrimSpace(v), `"`)
	if isBlankValue(s) {
		return false
	}
	if strings.EqualFold(s, "true") {
		return true
	}
	if !strings.EqualFold(s, "false") {
		log.Printf("config: %s: %q is not true/false — using false", keyPath(where, k), s)
	}
	return false
}

func setTrackerKV(t *TrackerSettings, where, k, v string) {
	num := func() int { return parseIntAt(where, k, v) }
	flag := func() bool { return parseBoolAt(where, k, v) }
	switch k {
	case "host":
		t.Host = unquote(v)
	case "alias":
		t.Alias = unquote(v)
	case "cookie":
		t.Cookie = unquote(v)
	case "useproxy":
		t.UseProxy = flag()
	case "fetchmode":
		t.FetchMode = unquote(v)
	case "useragent":
		t.UserAgent = unquote(v)
	case "insecureSkipVerify":
		t.InsecureSkipVerify = flag()
	case "reqMinute":
		t.ReqMinute = num()
	case "parseDelay":
		t.ParseDelay = num()
	case "log":
		t.Log = flag()
	}
}

func setProxyKV(p *ProxySettings, where, k, v string) {
	flag := func() bool { return parseBoolAt(where, k, v) }
	switch k {
	case "pattern":
		p.Pattern = unquote(v)
	case "useAuth":
		p.UseAuth = flag()
	case "BypassOnLocal":
		p.BypassOnLocal = flag()
	case "username":
		p.Username = unquote(v)
	case "password":
		p.Password = unquote(v)
	}
}

func setConfigKV(cfg *Config, k, v string) {
	num := func() int { return parseIntAt("", k, v) }
	flag := func() bool { return parseBoolAt("", k, v) }
	switch k {
	case "listenip":
		cfg.ListenIP = unquote(v)
	case "listenport":
		cfg.ListenPort = num()
	case "apikey":
		cfg.APIKey = unquote(v)
	case "devkey":
		cfg.DevKey = unquote(v)
	case "mergeduplicates":
		cfg.MergeDuplicates = flag()
	case "mergenumduplicates":
		cfg.MergeNumDuplicates = flag()
	case "log":
		cfg.Log = flag()
	case "logParsers":
		cfg.LogParsers = flag()
	case "logFdb":
		cfg.LogFdb = flag()
	case "logFdbRetentionDays":
		cfg.LogFdbRetentionDays = num()
	case "logFdbMaxSizeMb":
		cfg.LogFdbMaxSizeMb = num()
	case "logFdbMaxFiles":
		cfg.LogFdbMaxFiles = num()
	case "fdbPathLevels":
		cfg.FDBPathLevels = num()
	case "openstats":
		cfg.OpenStats = flag()
	case "opensync":
		cfg.OpenSync = flag()
	case "opensync_v1":
		cfg.OpenSyncV1 = flag()
	case "web":
		cfg.Web = flag()
	case "syncapi":
		cfg.SyncAPI = unquote(v)
	case "syncsport":
		cfg.SyncSport = flag()
	case "syncspidr":
		cfg.SyncSpidr = flag()
	case "maxreadfile":
		cfg.MaxReadFile = num()
	case "memlimit":
		cfg.MemLimitMB = num()
	case "gcpercent":
		cfg.GCPercent = num()
	case "timeStatsUpdate":
		cfg.TimeStatsUpdate = num()
	case "timeSync":
		cfg.TimeSync = num()
	case "timeSyncSpidr":
		cfg.TimeSyncSpidr = num()
	case "flaresolverr":
		cfg.FlareSolverr = unquote(v)
	case "synctrackers":
		cfg.SyncTrackers = []string{}
	case "disable_trackers":
		cfg.DisableTrackers = []string{}
	case "tracks":
		cfg.Tracks = flag()
	case "tracksmod":
		cfg.TracksMod = num()
	case "tracksdelay":
		cfg.TracksDelay = num()
	case "trackslog":
		cfg.TracksLog = flag()
	case "tracksatempt":
		cfg.TracksAttempt = num()
	case "trackscategory":
		cfg.TracksCategory = unquote(v)
	case "tsuri":
		cfg.TSURI = []string{}
	}
}

func SafeConfigJSON(cfg Config) string {
	var raw map[string]any
	b, _ := json.Marshal(cfg)
	_ = json.Unmarshal(b, &raw)
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, vv := range x {
				lk := strings.ToLower(k)
				if lk == "apikey" || lk == "devkey" || lk == "cookie" || lk == "u" || lk == "p" || lk == "username" || lk == "password" {
					if s, ok := vv.(string); ok && s != "" {
						x[k] = "***"
					}
					continue
				}
				walk(vv)
			}
		case []any:
			for _, it := range x {
				walk(it)
			}
		}
	}
	walk(raw)
	out, _ := json.MarshalIndent(raw, "", "  ")
	return string(out)
}
