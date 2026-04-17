package app

import (
	"fmt"
	"strings"
)

// MarshalYAML renders Config in the init.yaml shape read by parseYAMLIntoConfig.
// Stable order, no comments. Values go through yamlScalar for safe quoting.
func MarshalYAML(cfg Config) string {
	var b strings.Builder
	b.WriteString("---\n")

	// Server
	writeScalar(&b, "listenip", cfg.ListenIP)
	writeScalar(&b, "listenport", cfg.ListenPort)
	writeScalar(&b, "apikey", cfg.APIKey)
	writeScalar(&b, "devkey", cfg.DevKey)
	b.WriteString("\n")

	// DB / merge
	writeScalar(&b, "fdbPathLevels", cfg.FDBPathLevels)
	writeScalar(&b, "mergeduplicates", cfg.MergeDuplicates)
	writeScalar(&b, "mergenumduplicates", cfg.MergeNumDuplicates)
	b.WriteString("\n")

	// Logging
	writeScalar(&b, "log", cfg.Log)
	writeScalar(&b, "logParsers", cfg.LogParsers)
	writeScalar(&b, "logFdb", cfg.LogFdb)
	writeScalar(&b, "logFdbRetentionDays", cfg.LogFdbRetentionDays)
	writeScalar(&b, "logFdbMaxSizeMb", cfg.LogFdbMaxSizeMb)
	writeScalar(&b, "logFdbMaxFiles", cfg.LogFdbMaxFiles)
	b.WriteString("\n")

	// API access
	writeScalar(&b, "openstats", cfg.OpenStats)
	writeScalar(&b, "opensync", cfg.OpenSync)
	writeScalar(&b, "opensync_v1", cfg.OpenSyncV1)
	writeScalar(&b, "web", cfg.Web)
	b.WriteString("\n")

	// Stats/sync timing
	writeScalar(&b, "timeStatsUpdate", cfg.TimeStatsUpdate)
	writeScalar(&b, "timeSync", cfg.TimeSync)
	writeScalar(&b, "timeSyncSpidr", cfg.TimeSyncSpidr)
	b.WriteString("\n")

	// Sync
	writeScalar(&b, "syncapi", cfg.SyncAPI)
	writeScalar(&b, "syncsport", cfg.SyncSport)
	writeScalar(&b, "syncspidr", cfg.SyncSpidr)
	writeList(&b, "synctrackers", cfg.SyncTrackers)
	writeList(&b, "disable_trackers", cfg.DisableTrackers)
	b.WriteString("\n")

	// Tracks
	writeScalar(&b, "tracks", cfg.Tracks)
	writeScalar(&b, "tracksmod", cfg.TracksMod)
	writeScalar(&b, "tracksdelay", cfg.TracksDelay)
	writeScalar(&b, "trackslog", cfg.TracksLog)
	writeScalar(&b, "tracksatempt", cfg.TracksAttempt)
	writeScalar(&b, "trackscategory", cfg.TracksCategory)
	b.WriteString("tracksinterval:\n")
	writeIndented(&b, 2, "task0", cfg.TracksInterval.Task0)
	writeIndented(&b, 2, "task1", cfg.TracksInterval.Task1)
	writeList(&b, "tsuri", cfg.TSURI)
	b.WriteString("\n")

	// Runtime
	writeScalar(&b, "maxreadfile", cfg.MaxReadFile)
	writeScalar(&b, "memlimit", cfg.MemLimitMB)
	writeScalar(&b, "gcpercent", cfg.GCPercent)
	b.WriteString("\n")

	// Evercache
	b.WriteString("evercache:\n")
	writeIndented(&b, 2, "enable", cfg.Evercache.Enable)
	writeIndented(&b, 2, "validHour", cfg.Evercache.ValidHour)
	writeIndented(&b, 2, "maxOpenWriteTask", cfg.Evercache.MaxOpenWriteTask)
	writeIndented(&b, 2, "dropCacheTake", cfg.Evercache.DropCacheTake)
	b.WriteString("\n")

	// FlareSolverr-go
	b.WriteString("flaresolverr_go:\n")
	if cfg.FlareSolverrGo.Headless != nil {
		writeIndented(&b, 2, "headless", *cfg.FlareSolverrGo.Headless)
	} else {
		writeIndented(&b, 2, "headless", true)
	}
	writeIndented(&b, 2, "browser_path", cfg.FlareSolverrGo.BrowserPath)
	b.WriteString("\n")

	// CFClient
	b.WriteString("cfclient:\n")
	writeIndented(&b, 2, "profile", cfg.CFClient.Profile)
	writeIndented(&b, 2, "useragent", cfg.CFClient.UserAgent)
	b.WriteString("\n")

	// Trackers (same order as types.go)
	writeTracker(&b, "Rutor", cfg.Rutor)
	writeTracker(&b, "Megapeer", cfg.Megapeer)
	writeTracker(&b, "TorrentBy", cfg.TorrentBy)
	writeTracker(&b, "Kinozal", cfg.Kinozal)
	writeTracker(&b, "NNMClub", cfg.NNMClub)
	writeTracker(&b, "Bitru", cfg.Bitru)
	writeTracker(&b, "Toloka", cfg.Toloka)
	writeTracker(&b, "Mazepa", cfg.Mazepa)
	writeTracker(&b, "Rutracker", cfg.Rutracker)
	writeTracker(&b, "Selezen", cfg.Selezen)
	writeTracker(&b, "Lostfilm", cfg.Lostfilm)
	writeTracker(&b, "Animelayer", cfg.Animelayer)
	writeTracker(&b, "Anidub", cfg.Anidub)
	writeTracker(&b, "Aniliberty", cfg.Aniliberty)
	writeTracker(&b, "Knaben", cfg.Knaben)
	writeTracker(&b, "Anistar", cfg.Anistar)
	writeTracker(&b, "Anifilm", cfg.Anifilm)
	writeTracker(&b, "Leproduction", cfg.Leproduction)
	writeTracker(&b, "Baibako", cfg.Baibako)

	// Proxies
	b.WriteString("globalproxy:\n")
	if len(cfg.GlobalProxy) == 0 {
		b.WriteString("  []\n")
	} else {
		for _, p := range cfg.GlobalProxy {
			b.WriteString(fmt.Sprintf("  - pattern: %s\n", yamlScalar(p.Pattern)))
			b.WriteString(fmt.Sprintf("    useAuth: %s\n", yamlScalar(p.UseAuth)))
			b.WriteString(fmt.Sprintf("    BypassOnLocal: %s\n", yamlScalar(p.BypassOnLocal)))
			b.WriteString(fmt.Sprintf("    username: %s\n", yamlScalar(p.Username)))
			b.WriteString(fmt.Sprintf("    password: %s\n", yamlScalar(p.Password)))
			b.WriteString("    list:\n")
			if len(p.List) == 0 {
				b.WriteString("      []\n")
			} else {
				for _, v := range p.List {
					b.WriteString(fmt.Sprintf("      - %s\n", yamlScalar(v)))
				}
			}
		}
	}

	return b.String()
}

func writeTracker(b *strings.Builder, name string, t TrackerSettings) {
	b.WriteString(name + ":\n")
	writeIndented(b, 2, "host", t.Host)
	if t.Alias != "" {
		writeIndented(b, 2, "alias", t.Alias)
	}
	if t.Cookie != "" {
		writeIndented(b, 2, "cookie", t.Cookie)
	}
	if t.FetchMode != "" {
		writeIndented(b, 2, "fetchmode", t.FetchMode)
	}
	if t.InsecureSkipVerify {
		writeIndented(b, 2, "insecureSkipVerify", true)
	}
	writeIndented(b, 2, "useproxy", t.UseProxy)
	writeIndented(b, 2, "reqMinute", t.ReqMinute)
	writeIndented(b, 2, "parseDelay", t.ParseDelay)
	writeIndented(b, 2, "log", t.Log)
	b.WriteString("  login:\n")
	writeIndented(b, 4, "u", t.Login.U)
	writeIndented(b, 4, "p", t.Login.P)
	b.WriteString("\n")
}

func writeScalar(b *strings.Builder, key string, v any) {
	b.WriteString(fmt.Sprintf("%s: %s\n", key, yamlScalar(v)))
}

func writeIndented(b *strings.Builder, indent int, key string, v any) {
	b.WriteString(strings.Repeat(" ", indent))
	b.WriteString(fmt.Sprintf("%s: %s\n", key, yamlScalar(v)))
}

func writeList(b *strings.Builder, key string, items []string) {
	if len(items) == 0 {
		b.WriteString(fmt.Sprintf("%s: []\n", key))
		return
	}
	b.WriteString(key + ":\n")
	for _, v := range items {
		b.WriteString(fmt.Sprintf("  - %s\n", yamlScalar(v)))
	}
}

// yamlScalar returns a safely-quoted YAML scalar for the value.
// Strings containing special characters are quoted with double quotes.
func yamlScalar(v any) string {
	switch x := v.(type) {
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int:
		return fmt.Sprintf("%d", x)
	case int64:
		return fmt.Sprintf("%d", x)
	case float64:
		return fmt.Sprintf("%g", x)
	case string:
		return yamlQuoteString(x)
	default:
		return yamlQuoteString(fmt.Sprintf("%v", v))
	}
}

func yamlQuoteString(s string) string {
	if s == "" {
		return `""`
	}
	// Quote if contains special chars, leading/trailing space, or looks like a reserved word
	if strings.ContainsAny(s, ":#\"'\n\t{}[],&*!|>%@`") || strings.HasPrefix(s, " ") || strings.HasSuffix(s, " ") {
		return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
	}
	lower := strings.ToLower(s)
	switch lower {
	case "true", "false", "null", "yes", "no", "on", "off", "~":
		return `"` + s + `"`
	}
	// Numeric-looking strings should be quoted to avoid type coercion
	if looksNumeric(s) {
		return `"` + s + `"`
	}
	return s
}

func looksNumeric(s string) bool {
	if s == "" {
		return false
	}
	hasDigit := false
	for i, r := range s {
		if r >= '0' && r <= '9' {
			hasDigit = true
			continue
		}
		if (r == '-' || r == '+') && i == 0 {
			continue
		}
		if r == '.' || r == 'e' || r == 'E' {
			continue
		}
		return false
	}
	return hasDigit
}
