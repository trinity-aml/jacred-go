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

		if indent == 0 && strings.HasSuffix(trimmed, ":") {
			section = strings.TrimSuffix(trimmed, ":")
			currentTracker = trackerByName(cfg, section)
			currentProxy = nil
			inTrackerLogin = false
			inEvercache = false
			inFlareSolverrGo = false
			inTracksInterval = false
			currentListTarget = ""
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
				rest := strings.TrimPrefix(trimmed, "- ")
				if strings.Contains(rest, ":") {
					k, v := splitKV(rest)
					setProxyKV(currentProxy, k, v)
					if k == "list" {
						currentListTarget = "proxy.list"
					}
				}
				continue
			}
			if currentProxy != nil && indent == 4 && strings.Contains(trimmed, ":") {
				k, v := splitKV(trimmed)
				setProxyKV(currentProxy, k, v)
				if k == "list" {
					currentListTarget = "proxy.list"
				} else {
					currentListTarget = ""
				}
				continue
			}
			if currentProxy != nil && indent >= 6 && strings.HasPrefix(trimmed, "- ") && currentListTarget == "proxy.list" {
				currentProxy.List = append(currentProxy.List, unquote(strings.TrimPrefix(trimmed, "- ")))
				continue
			}
		}

		if currentTracker != nil {
			if indent == 2 && strings.HasSuffix(trimmed, ":") {
				name := strings.TrimSuffix(trimmed, ":")
				inTrackerLogin = name == "login"
				continue
			}
			if indent == 2 && strings.Contains(trimmed, ":") {
				k, v := splitKV(trimmed)
				setTrackerKV(currentTracker, k, v)
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
				cfg.Evercache.Enable = parseBool(v)
			case "validHour":
				cfg.Evercache.ValidHour = parseInt(v)
			case "maxOpenWriteTask":
				cfg.Evercache.MaxOpenWriteTask = parseInt(v)
			case "dropCacheTake":
				cfg.Evercache.DropCacheTake = parseInt(v)
			}
			continue
		}
		if section == "tracksinterval" && indent == 2 && strings.Contains(trimmed, ":") {
			inTracksInterval = true
			k, v := splitKV(trimmed)
			switch k {
			case "task0":
				cfg.TracksInterval.Task0 = parseInt(v)
			case "task1":
				cfg.TracksInterval.Task1 = parseInt(v)
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
				val := parseBool(v)
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
			val := unquote(strings.TrimPrefix(trimmed, "- "))
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
	case "Baibako":
		return &cfg.Baibako
	case "Korsars":
		return &cfg.Korsars
	default:
		return nil
	}
}

func splitKV(s string) (string, string) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return strings.TrimSpace(s), ""
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
}

func unquote(v string) string {
	v = strings.TrimSpace(v)
	if v == "null" {
		return ""
	}
	return strings.Trim(strings.TrimSpace(v), `"`)
}

func parseBool(v string) bool { return strings.EqualFold(strings.TrimSpace(v), "true") }
func parseInt(v string) int {
	n, _ := strconv.Atoi(strings.Trim(strings.TrimSpace(v), `"`))
	return n
}

func setTrackerKV(t *TrackerSettings, k, v string) {
	switch k {
	case "host":
		t.Host = unquote(v)
	case "alias":
		t.Alias = unquote(v)
	case "cookie":
		t.Cookie = unquote(v)
	case "useproxy":
		t.UseProxy = parseBool(v)
	case "fetchmode":
		t.FetchMode = unquote(v)
	case "insecureSkipVerify":
		t.InsecureSkipVerify = parseBool(v)
	case "reqMinute":
		t.ReqMinute = parseInt(v)
	case "parseDelay":
		t.ParseDelay = parseInt(v)
	case "log":
		t.Log = parseBool(v)
	}
}

func setProxyKV(p *ProxySettings, k, v string) {
	switch k {
	case "pattern":
		p.Pattern = unquote(v)
	case "useAuth":
		p.UseAuth = parseBool(v)
	case "BypassOnLocal":
		p.BypassOnLocal = parseBool(v)
	case "username":
		p.Username = unquote(v)
	case "password":
		p.Password = unquote(v)
	}
}

func setConfigKV(cfg *Config, k, v string) {
	switch k {
	case "listenip":
		cfg.ListenIP = unquote(v)
	case "listenport":
		cfg.ListenPort = parseInt(v)
	case "apikey":
		cfg.APIKey = unquote(v)
	case "devkey":
		cfg.DevKey = unquote(v)
	case "mergeduplicates":
		cfg.MergeDuplicates = parseBool(v)
	case "mergenumduplicates":
		cfg.MergeNumDuplicates = parseBool(v)
	case "log":
		cfg.Log = parseBool(v)
	case "logParsers":
		cfg.LogParsers = parseBool(v)
	case "logFdb":
		cfg.LogFdb = parseBool(v)
	case "logFdbRetentionDays":
		cfg.LogFdbRetentionDays = parseInt(v)
	case "logFdbMaxSizeMb":
		cfg.LogFdbMaxSizeMb = parseInt(v)
	case "logFdbMaxFiles":
		cfg.LogFdbMaxFiles = parseInt(v)
	case "fdbPathLevels":
		cfg.FDBPathLevels = parseInt(v)
	case "openstats":
		cfg.OpenStats = parseBool(v)
	case "opensync":
		cfg.OpenSync = parseBool(v)
	case "opensync_v1":
		cfg.OpenSyncV1 = parseBool(v)
	case "web":
		cfg.Web = parseBool(v)
	case "syncapi":
		cfg.SyncAPI = unquote(v)
	case "syncsport":
		cfg.SyncSport = parseBool(v)
	case "syncspidr":
		cfg.SyncSpidr = parseBool(v)
	case "maxreadfile":
		cfg.MaxReadFile = parseInt(v)
	case "memlimit":
		cfg.MemLimitMB = parseInt(v)
	case "gcpercent":
		cfg.GCPercent = parseInt(v)
	case "timeStatsUpdate":
		cfg.TimeStatsUpdate = parseInt(v)
	case "timeSync":
		cfg.TimeSync = parseInt(v)
	case "timeSyncSpidr":
		cfg.TimeSyncSpidr = parseInt(v)
	case "flaresolverr":
		cfg.FlareSolverr = unquote(v)
	case "synctrackers":
		cfg.SyncTrackers = []string{}
	case "disable_trackers":
		cfg.DisableTrackers = []string{}
	case "tracks":
		cfg.Tracks = parseBool(v)
	case "tracksmod":
		cfg.TracksMod = parseInt(v)
	case "tracksdelay":
		cfg.TracksDelay = parseInt(v)
	case "trackslog":
		cfg.TracksLog = parseBool(v)
	case "tracksatempt":
		cfg.TracksAttempt = parseInt(v)
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
