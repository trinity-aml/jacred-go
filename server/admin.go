package server

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"regexp"

	"jacred/app"
	"jacred/core"
	"jacred/cron/bitruapi"
	"jacred/cron/knaben"
	"jacred/filedb"
	"jacred/tracks"
)

var jsonDBSaveWork atomic.Bool

func writeBareNotFound(w http.ResponseWriter) {
	h := w.Header()
	h.Del("Content-Type")
	h.Del("Content-Length")
	w.WriteHeader(http.StatusNotFound)
}

func (s *Server) handleStatsTrackers(w http.ResponseWriter, r *http.Request) {
	if !s.GetConfig().OpenStats {
		writeJSONArrayString(w, http.StatusOK, "[]")
		return
	}

	path := normalizeRoutePath(r.URL.Path)

	if path == "/stats/trackers" || path == "/stats/trackers/" {
		q := r.URL.Query()
		list := s.collectStatsTorrents(
			"",
			atoi(firstQuery(q, "newtoday", "newToday")),
			atoi(firstQuery(q, "updatedtoday", "updatedToday")),
			defaultInt(firstQuery(q, "limit", "take"), 200),
		)
		writeCanonicalJSON(w, http.StatusOK, list)
		return
	}

	tail, ok := trimPathPrefixFold(path, "/stats/trackers/")
	if ok {
		parts := strings.Split(strings.Trim(tail, "/"), "/")
		if len(parts) == 2 {
			limit := defaultInt(firstQuery(r.URL.Query(), "limit", "take"), 200)
			mode := strings.ToLower(strings.TrimSpace(parts[1]))
			if mode == "new" {
				writeCanonicalJSON(w, http.StatusOK, s.collectStatsTorrents(parts[0], 1, 0, limit))
				return
			}
			if mode == "updated" {
				writeCanonicalJSON(w, http.StatusOK, s.collectStatsTorrents(parts[0], 0, 1, limit))
				return
			}
		}
	}

	http.NotFound(w, r)
}

func (s *Server) handleStatsTorrentsEx(w http.ResponseWriter, r *http.Request) {
	if !s.GetConfig().OpenStats {
		writeJSONArrayString(w, http.StatusOK, "[]")
		return
	}
	q := r.URL.Query()
	trackerName := strings.TrimSpace(q.Get("trackerName"))
	if trackerName == "" {
		statsPath := filepath.Join(s.DB.DataDir, "temp", "stats.json")
		b, err := os.ReadFile(statsPath)
		if err != nil {
			writeJSONArrayString(w, http.StatusOK, "[]")
			return
		}
		writeJSONBytes(w, http.StatusOK, b)
		return
	}
	list := s.collectStatsTorrents(trackerName, atoi(firstQuery(q, "newtoday", "newToday")), atoi(firstQuery(q, "updatedtoday", "updatedToday")), defaultInt(firstQuery(q, "limit", "take"), 200))
	writeCanonicalJSON(w, http.StatusOK, list)
}

func (s *Server) collectStatsTorrents(trackerName string, newtoday, updatedtoday, limit int) []map[string]any {
	today := todayLocalMidnightUTC()
	collected := make([]filedb.TorrentDetails, 0, limit)
	for _, item := range s.DB.UnorderedMasterEntries() {
		bucket, err := s.DB.OpenReadNoCache(item.Key)
		if err != nil {
			continue
		}
		for _, t := range bucket {
			if t == nil || strings.TrimSpace(asString(t["trackerName"])) == "" {
				continue
			}
			if trackerName != "" && !strings.EqualFold(asString(t["trackerName"]), trackerName) {
				continue
			}
			if newtoday == 1 && filedbTime(t, "createTime").Before(today) {
				continue
			}
			if updatedtoday == 1 && filedbTime(t, "updateTime").Before(today) {
				continue
			}
			collected = append(collected, t)
		}
	}
	sort.Slice(collected, func(i, j int) bool {
		return filedbTime(collected[i], "createTime").After(filedbTime(collected[j], "createTime"))
	})
	if limit > 0 && len(collected) > limit {
		collected = collected[:limit]
	}
	out := make([]map[string]any, 0, len(collected))
	for _, t := range collected {
		out = append(out, map[string]any{
			"trackerName":  asString(t["trackerName"]),
			"types":        t["types"],
			"url":          t["url"],
			"title":        t["title"],
			"sid":          t["sid"],
			"pir":          t["pir"],
			"sizeName":     t["sizeName"],
			"createTime":   formatLocalDateTime(filedbTime(t, "createTime")),
			"updateTime":   formatLocalDateTime(filedbTime(t, "updateTime")),
			"hasMagnet":    strings.TrimSpace(asString(t["magnet"])) != "",
			"name":         t["name"],
			"originalname": t["originalname"],
			"relased":      t["relased"],
		})
	}
	return out
}

func (s *Server) handleSyncConf(w http.ResponseWriter, r *http.Request) {
	writeJSONOrdered(w, http.StatusOK, [][2]any{
		{"fbd", true},
		{"spidr", true},
		{"version", 2},
	})
}

func (s *Server) handleSyncFdb(w http.ResponseWriter, r *http.Request) {
	if !s.GetConfig().OpenSync {
		writeJSONArrayString(w, http.StatusOK, "[]")
		return
	}

	q := r.URL.Query()

	if strings.TrimSpace(q.Get("q")) != "" ||
		strings.TrimSpace(q.Get("Key")) != "" ||
		(strings.TrimSpace(q.Get("take")) != "" && strings.TrimSpace(q.Get("limit")) == "") {
		writeJSONArrayString(w, http.StatusOK, "[]")
		return
	}

	keyq := strings.TrimSpace(q.Get("key"))
	limit := defaultInt(firstQuery(q, "limit", "take"), 20)
	if limit <= 0 {
		limit = 20
	}

	out := []map[string]any{}
	for _, item := range s.DB.OrderedMasterEntries() {
		if keyq != "" && !strings.Contains(item.Key, keyq) {
			continue
		}
		bucket, err := s.DB.OpenRead(item.Key)
		if err != nil {
			continue
		}
		out = append(out, map[string]any{
			"Key":        item.Key,
			"updateTime": item.Value.UpdateTime,
			"fileTime":   item.Value.FileTime,
			"path":       "Data/fdb/" + filedbKeyPath(item.Key),
			"value":      bucket,
		})
		if len(out) >= limit {
			break
		}
	}

	writeCanonicalJSON(w, http.StatusOK, out)
}

func (s *Server) handleSyncFdbTorrents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	if strings.TrimSpace(q.Get("fileTime")) != "" ||
		strings.TrimSpace(q.Get("startTime")) != "" ||
		strings.TrimSpace(q.Get("spider")) != "" ||
		(strings.TrimSpace(q.Get("take")) != "" && strings.TrimSpace(q.Get("limit")) == "") {
		writeCanonicalJSON(w, http.StatusOK, map[string]any{
			"collections": []any{},
			"nextread":    false,
		})
		return
	}

	ft := filedb.NormalizeFileTime(func() int64 {
		v, _ := strconv.ParseInt(firstQuery(q, "time", "fileTime"), 10, 64)
		return v
	}())
	startRaw := firstQuery(q, "start", "startTime")
	start, _ := strconv.ParseInt(startRaw, 10, 64)
	if start == 0 && strings.TrimSpace(startRaw) == "" {
		start = -1
	}
	spidr := parseBool(firstQuery(q, "spidr", "spider"))

	if !s.GetConfig().OpenSync || ft == 0 {
		writeCanonicalJSON(w, http.StatusOK, map[string]any{
			"collections": []any{},
			"nextread":    false,
		})
		return
	}

	take := defaultInt(firstQuery(q, "take", "limit"), 2000)
	if take <= 0 {
		take = 2000
	}

	nextread := false
	countread := 0
	collections := []map[string]any{}

	for _, item := range s.DB.OrderedMasterEntries() {
		if item.Value.FileTime <= ft {
			continue
		}
		// If we already exceeded take in a previous collection, signal nextread
		// but don't process any more — the client will resume from the last fileTime.
		if countread > take {
			nextread = true
			break
		}

		bucket, err := s.DB.OpenReadNoCache(item.Key)
		if err != nil {
			continue
		}

		keys := make([]string, 0, len(bucket))
		for k := range bucket {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		torrent := map[string]filedb.TorrentDetails{}
		for _, k := range keys {
			t := bucket[k]
			if t == nil {
				continue
			}
			if !trackerAllowedByConfig(s.GetConfig().DisableTrackers, asString(t["trackerName"])) {
				continue
			}

			if spidr || (start != -1 && start > filedb.ToFileTimeUTC(filedbTime(t, "updateTime"))) {
				torrent[k] = filedb.TorrentDetails{
					"sid": t["sid"],
					"pir": t["pir"],
					"url": t["url"],
				}
			} else {
				torrent[k] = normalizeSyncTorrent(s.enrichTorrentWithTracks(cloneMap(t)))
			}

			countread++
		}

		if len(torrent) == 0 {
			continue
		}

		collections = append(collections, map[string]any{
			"Key": item.Key,
			"Value": map[string]any{
				"time":     item.Value.UpdateTime,
				"fileTime": filedb.SyncFileTime(filedb.NormalizeFileTime(item.Value.FileTime)),
				"torrents": torrent,
			},
		})
	}

	writeCanonicalJSON(w, http.StatusOK, map[string]any{
		"nextread":   nextread,
		"countread":  countread,
		"take":       take,
		"collections": collections,
	})
}

func (s *Server) handleSyncTorrents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ft, _ := strconv.ParseInt(firstQuery(q, "time", "fileTime"), 10, 64)
	ft = filedb.NormalizeFileTime(ft)
	if !s.GetConfig().OpenSyncV1 || ft == 0 {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	take := defaultInt(firstQuery(q, "take", "limit"), 2000)
	if take <= 0 {
		take = 2000
	}
	trackerName := firstQuery(q, "trackerName", "tracker")
	torrents := []map[string]any{}
	for _, item := range s.DB.OrderedMasterEntries() {
		if item.Value.FileTime <= ft {
			continue
		}
		bucket, err := s.DB.OpenReadNoCache(item.Key)
		if err != nil {
			continue
		}
		keys := make([]string, 0, len(bucket))
		for k := range bucket {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			t := bucket[k]
			if t == nil || !trackerAllowedByConfig(s.GetConfig().DisableTrackers, asString(t["trackerName"])) {
				continue
			}
			if trackerName != "" && !strings.EqualFold(asString(t["trackerName"]), trackerName) {
				continue
			}
			cp := normalizeSyncTorrent(s.enrichTorrentWithTracks(cloneMap(t)))
			cp["updateTime"] = item.Value.UpdateTime
			torrents = append(torrents, map[string]any{"key": k, "value": cp})
			if len(torrents) >= take {
				break
			}
		}
		if len(torrents) >= take {
			break
		}
	}
	sort.SliceStable(torrents, func(i, j int) bool {
		iv, _ := torrents[i]["value"].(filedb.TorrentDetails)
		jv, _ := torrents[j]["value"].(filedb.TorrentDetails)
		ik := firstNonEmpty(asString(iv["trackerName"]), asString(iv["title"]), asString(torrents[i]["key"]))
		jk := firstNonEmpty(asString(jv["trackerName"]), asString(jv["title"]), asString(torrents[j]["key"]))
		if ik == jk {
			return asString(torrents[i]["key"]) < asString(torrents[j]["key"])
		}
		return ik < jk
	})
	writeCanonicalJSON(w, http.StatusOK, map[string]any{
		"take": take,
		"torrents": torrents,
	})
}

func (s *Server) handleJSONDBSave(w http.ResponseWriter, r *http.Request) {
	if jsonDBSaveWork.Load() {
		writePlainUTF8(w, http.StatusOK, "work")
		return
	}
	if strings.TrimSpace(s.GetConfig().SyncAPI) != "" {
		writePlainUTF8(w, http.StatusOK, "syncapi")
		return
	}
	jsonDBSaveWork.Store(true)
	defer jsonDBSaveWork.Store(false)
	if err := s.DB.SaveChangesToFileNow(); err != nil {
		writePlainUTF8(w, http.StatusInternalServerError, "error")
		return
	}
	writePlainUTF8(w, http.StatusOK, "ok")
}

func (s *Server) handleDevFindCorrupt(w http.ResponseWriter, r *http.Request) {
	writeCanonicalJSON(w, http.StatusOK, s.DB.FindCorrupt(defaultInt(firstQuery(r.URL.Query(), "sampleSize", "sample", "limit"), 20)))
}

func (s *Server) handleDevRemoveNullValues(w http.ResponseWriter, r *http.Request) {
	removed, affected, err := s.DB.RemoveNullValues()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed, "affectedFiles": affected})
}

func (s *Server) handleDevFindDuplicateKeys(w http.ResponseWriter, r *http.Request) {
	excludeNumeric := true
	if raw := strings.TrimSpace(r.URL.Query().Get("excludeNumeric")); raw != "" {
		excludeNumeric = parseBool(raw)
	}
	writeCanonicalJSON(w, http.StatusOK, s.DB.FindDuplicateKeys(firstQuery(r.URL.Query(), "tracker", "trackerName"), excludeNumeric))
}

func (s *Server) handleDevFindEmptySearchFields(w http.ResponseWriter, r *http.Request) {
	writeCanonicalJSON(w, http.StatusOK, s.DB.FindEmptySearchFields(defaultInt(firstQuery(r.URL.Query(), "sampleSize", "sample", "limit"), 20)))
}


func sortedStrings(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func normalizeSyncTorrent(t filedb.TorrentDetails) filedb.TorrentDetails {
	if t == nil {
		return t
	}
	if v, ok := t["languages"]; ok {
		t["languages"] = sortedStrings(toStringSliceAny(v))
	}
	if v, ok := t["size"]; ok {
		switch n := v.(type) {
		case float64:
			t["size"] = int64(n)
		case json.Number:
			i, _ := n.Int64()
			t["size"] = i
		}
	}
	return t
}

func normalizeRoutePath(path string) string {
	if strings.TrimSpace(path) == "" {
		return "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return path
}

func trimPathPrefixFold(path, prefix string) (string, bool) {
	path = normalizeRoutePath(path)
	prefix = normalizeRoutePath(prefix)
	if len(path) < len(prefix) {
		return "", false
	}
	if strings.EqualFold(path[:len(prefix)], prefix) {
		return path[len(prefix):], true
	}
	return "", false
}

func firstQuery(v url.Values, keys ...string) string {
	for _, key := range keys {
		if s := strings.TrimSpace(v.Get(key)); s != "" {
			return s
		}
	}
	return ""
}

func writeCanonicalJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(canonicalJSONString(v)))
}

// writeCanonicalJSONCached writes JSON response and returns the bytes for caching.
func writeCanonicalJSONCached(w http.ResponseWriter, code int, v any) []byte {
	data := []byte(canonicalJSONString(v))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(data)
	return data
}

func canonicalJSONString(v any) string {
	var b strings.Builder
	writeCanonicalJSONValue(&b, v)
	return b.String()
}

func writeCanonicalJSONValue(b *strings.Builder, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		enc, _ := json.Marshal(x)
		b.Write(enc)
	case json.Number:
		b.WriteString(x.String())
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		enc, _ := json.Marshal(x)
		b.Write(enc)
	case []string:
		b.WriteByte('[')
		for i, it := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonicalJSONValue(b, it)
		}
		b.WriteByte(']')
	case []int:
		b.WriteByte('[')
		for i, it := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonicalJSONValue(b, it)
		}
		b.WriteByte(']')
	case []any:
		b.WriteByte('[')
		for i, it := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonicalJSONValue(b, it)
		}
		b.WriteByte(']')
	case map[string]any:
		writeCanonicalJSONMap(b, x)
	case filedb.TorrentDetails:
		writeCanonicalJSONMap(b, map[string]any(x))
	case map[string]filedb.TorrentDetails:
		m := make(map[string]any, len(x))
		for k, v := range x {
			m[k] = v
		}
		writeCanonicalJSONMap(b, m)
	case map[string]string:
		m := make(map[string]any, len(x))
		for k, v := range x {
			m[k] = v
		}
		writeCanonicalJSONMap(b, m)
	default:
		enc, _ := json.Marshal(x)
		if len(enc) == 0 {
			b.WriteString("null")
			return
		}
		var decoded any
		if err := json.Unmarshal(enc, &decoded); err == nil {
			writeCanonicalJSONValue(b, decoded)
			return
		}
		b.Write(enc)
	}
}

func writeCanonicalJSONMap(b *strings.Builder, m map[string]any) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		name, _ := json.Marshal(k)
		b.Write(name)
		b.WriteByte(':')
		writeCanonicalJSONValue(b, m[k])
	}
	b.WriteByte('}')
}

func writeJSONOrdered(w http.ResponseWriter, code int, pairs [][2]any) {
	m := make(map[string]any, len(pairs))
	keys := make([]string, 0, len(pairs))
	for _, p := range pairs {
		k := strings.TrimSpace(asString(p[0]))
		if k == "" {
			continue
		}
		if _, ok := m[k]; !ok {
			keys = append(keys, k)
		}
		m[k] = p[1]
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		name, _ := json.Marshal(k)
		b.Write(name)
		b.WriteByte(':')
		val, _ := json.Marshal(m[k])
		b.Write(val)
	}
	b.WriteByte('}')
	writeJSONBytes(w, code, []byte(b.String()))
}

func writeJSONBytes(w http.ResponseWriter, code int, b []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

func writePlainUTF8(w http.ResponseWriter, code int, s string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(s))
}

func writeJSONArrayString(w http.ResponseWriter, code int, v string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(v))
}

func formatLocalDateTime(t time.Time) string {
	if t.IsZero() {
		return "0001-01-01 00:00:00"
	}
	return t.In(time.FixedZone("+0200", 2*3600)).Format("2006-01-02 15:04:05")
}

func todayLocalMidnightUTC() time.Time {
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return midnight.UTC()
}

func defaultInt(s string, def int) int {
	if strings.TrimSpace(s) == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

func filedbTime(t filedb.TorrentDetails, key string) time.Time {
	raw := t[key]
	switch v := raw.(type) {
	case string:
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.9999999Z07:00", "2006-01-02T15:04:05Z07:00", "2006-01-02T15:04:05"} {
			if tm, err := time.Parse(layout, v); err == nil {
				return tm
			}
		}
	case time.Time:
		return v
	}
	return time.Time{}
}

func cloneMap(src filedb.TorrentDetails) filedb.TorrentDetails {
	dst := filedb.TorrentDetails{}
	for k, v := range src {
		switch vv := v.(type) {
		case []any:
			cp := make([]any, len(vv))
			copy(cp, vv)
			dst[k] = cp
		case []string:
			cp := make([]string, len(vv))
			copy(cp, vv)
			dst[k] = cp
		case []int:
			cp := make([]int, len(vv))
			copy(cp, vv)
			dst[k] = cp
		default:
			dst[k] = v
		}
	}
	return dst
}

func trackerAllowedByConfig(disabled []string, tracker string) bool {
	for _, item := range disabled {
		if strings.EqualFold(strings.TrimSpace(item), strings.TrimSpace(tracker)) {
			return false
		}
	}
	return true
}

func filedbKeyPath(key string) string {
	md5key := fmt.Sprintf("%x", md5.Sum([]byte(key)))
	return filepath.ToSlash(filepath.Join(md5key[:2], md5key[2:]))
}


func toStringSliceAny(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case []string:
		out := make([]string, 0, len(x))
		for _, s := range x {
			s = strings.TrimSpace(s)
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(x))
		for _, it := range x {
			s := strings.TrimSpace(asString(it))
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		s := strings.TrimSpace(asString(v))
		if s == "" {
			return nil
		}
		return []string{s}
	}
}

func isLocalRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	// Unwrap IPv4-mapped IPv6 (e.g. ::ffff:127.0.0.1)
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() {
		return true
	}
	// fe80::/10 link-local
	if ip.IsLinkLocalUnicast() {
		return true
	}
	// RFC1918 (10/8, 172.16/12, 192.168/16) + RFC4193 (fc00::/7)
	if ip.IsPrivate() {
		return true
	}
	return false
}

func recomputeSizeBytes(sizeName string) int64 {
	sizeName = strings.TrimSpace(strings.ReplaceAll(sizeName, ",", "."))
	if sizeName == "" {
		return 0
	}
	parts := strings.Fields(sizeName)
	if len(parts) < 2 {
		return 0
	}
	value, _ := strconv.ParseFloat(parts[0], 64)
	if value == 0 {
		return 0
	}
	switch strings.ToLower(parts[1]) {
	case "gb", "гб":
		value *= 1024
	case "tb", "тб":
		value *= 1048576
	case "mb", "мб":
	default:
		return 0
	}
	return int64(value * 1048576)
}

func (s *Server) handleDevUpdateSize(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"badip": true})
		return
	}
	updated := 0
	for _, item := range s.DB.UnorderedMasterEntries() {
		bucket, err := s.DB.OpenReadOrEmpty(item.Key)
		if err != nil {
			continue
		}
		changed := false
		for url, t := range bucket {
			if t == nil {
				delete(bucket, url)
				changed = true
				continue
			}
			newSize := recomputeSizeBytes(asString(t["sizeName"]))
			if toInt64Any(t["size"]) != newSize {
				t["size"] = newSize
				t["updateTime"] = time.Now().UTC().Format(time.RFC3339Nano)
				bucket[url] = t
				changed = true
				updated++
			}
		}
		if changed {
			_ = s.DB.SaveBucket(item.Key, bucket, time.Now().UTC())
		}
	}
	_ = s.DB.SaveChangesToFileNow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "updated": updated})
}

func (s *Server) handleDevUpdateSearchName(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"badip": true})
		return
	}
	updated := 0
	for _, item := range s.DB.UnorderedMasterEntries() {
		bucket, err := s.DB.OpenReadOrEmpty(item.Key)
		if err != nil {
			continue
		}
		changed := false
		for url, t := range bucket {
			if t == nil {
				delete(bucket, url)
				changed = true
				continue
			}
			name := strings.TrimSpace(asString(t["name"]))
			original := strings.TrimSpace(asString(t["originalname"]))
			if name == "" {
				name = firstNonEmpty(strings.TrimSpace(asString(t["title"])), original)
				t["name"] = name
			}
			if original == "" {
				original = firstNonEmpty(strings.TrimSpace(asString(t["title"])), name)
				t["originalname"] = original
			}
			sn := core.SearchName(name)
			so := core.SearchName(original)
			if asString(t["_sn"]) != sn || asString(t["_so"]) != so {
				t["_sn"] = sn
				t["_so"] = so
				t["updateTime"] = time.Now().UTC().Format(time.RFC3339Nano)
				bucket[url] = t
				changed = true
				updated++
			}
		}
		if changed {
			_ = s.DB.SaveBucket(item.Key, bucket, time.Now().UTC())
		}
	}
	_ = s.DB.SaveChangesToFileNow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "updated": updated})
}

func toInt64Any(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n
	default:
		n, _ := strconv.ParseInt(strings.TrimSpace(asString(v)), 10, 64)
		return n
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func (s *Server) handleDevResetCheckTime(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"badip": true})
		return
	}
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format(time.RFC3339Nano)
	for _, item := range s.DB.UnorderedMasterEntries() {
		bucket, err := s.DB.OpenReadOrEmpty(item.Key)
		if err != nil {
			continue
		}
		changed := false
		for url, t := range bucket {
			if t == nil {
				delete(bucket, url)
				changed = true
				continue
			}
			t["checkTime"] = yesterday
			bucket[url] = t
			changed = true
		}
		if changed {
			_ = s.DB.SaveBucket(item.Key, bucket, time.Now().UTC())
		}
	}
	_ = s.DB.SaveChangesToFileNow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDevUpdateDetails(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"badip": true})
		return
	}
	updated := 0
	for _, item := range s.DB.UnorderedMasterEntries() {
		bucket, err := s.DB.OpenReadOrEmpty(item.Key)
		if err != nil {
			continue
		}
		changed := false
		for url, t := range bucket {
			if t == nil {
				delete(bucket, url)
				changed = true
				continue
			}
			filedb.UpdateFullDetails(t)
			t["updateTime"] = time.Now().UTC().Format(time.RFC3339Nano)
			bucket[url] = t
			changed = true
			updated++
		}
		if changed {
			_ = s.DB.SaveBucket(item.Key, bucket, time.Now().UTC())
		}
	}
	_ = s.DB.SaveChangesToFileNow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "updated": updated})
}

func (s *Server) handleDevFixKnabenNames(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"badip": true})
		return
	}
	processed, updated, migrated := 0, 0, 0
	for _, item := range s.DB.UnorderedMasterEntries() {
		bucket, unlock, err := s.DB.OpenReadOrEmptyLocked(item.Key)
		if err != nil {
			continue
		}
		type migration struct {
			url    string
			t      filedb.TorrentDetails
			newKey string
		}
		var toMigrate []migration
		bucketChanged := false
		for url, t := range bucket {
			if t == nil {
				delete(bucket, url)
				bucketChanged = true
				continue
			}
			if asString(t["trackerName"]) != "knaben" {
				continue
			}
			processed++
			source := firstNonEmpty(asString(t["title"]), asString(t["name"]))
			if source == "" {
				continue
			}
			newName, newRelased := knaben.ParseNameAndYear(source)
			if newName == "" {
				continue
			}
			suffix := ""
			if m := knabenSuffixRe.FindString(source); m != "" {
				suffix = m
			}
			newTitle := knaben.BuildTitleForFileDB(strings.TrimRight(source, " ")) + suffix
			nameChanged := newName != asString(t["name"]) || newName != asString(t["originalname"])
			relasedChanged := newRelased != asInt(t["relased"])
			titleChanged := newTitle != asString(t["title"])
			if !nameChanged && !relasedChanged && !titleChanged {
				continue
			}
			t["name"] = newName
			t["originalname"] = newName
			t["relased"] = newRelased
			t["title"] = newTitle
			t["_sn"] = core.SearchName(newName)
			t["_so"] = core.SearchName(newName)
			bucket[url] = t
			bucketChanged = true
			updated++
			newKey := s.DB.KeyDb(newName, newName)
			if newKey != "" && newKey != item.Key && strings.Contains(newKey, ":") {
				toMigrate = append(toMigrate, migration{url, t, newKey})
			}
		}
		for _, m := range toMigrate {
			delete(bucket, m.url)
			_ = s.DB.MigrateTorrentToNewKey(m.t, m.newKey)
			migrated++
		}
		if bucketChanged {
			_ = s.DB.SaveBucketUnlocked(item.Key, bucket, time.Now().UTC())
		}
		unlock()
	}
	_ = s.DB.SaveChangesToFileNow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "processed": processed, "updated": updated, "migrated": migrated})
}

func (s *Server) handleDevFixBitruNames(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"badip": true})
		return
	}
	processed, updated, migrated := 0, 0, 0
	for _, item := range s.DB.UnorderedMasterEntries() {
		bucket, unlock, err := s.DB.OpenReadOrEmptyLocked(item.Key)
		if err != nil {
			continue
		}
		type migration struct {
			url    string
			t      filedb.TorrentDetails
			newKey string
		}
		var toMigrate []migration
		bucketChanged := false
		for url, t := range bucket {
			if t == nil {
				delete(bucket, url)
				bucketChanged = true
				continue
			}
			if asString(t["trackerName"]) != "bitru" {
				continue
			}
			processed++
			name := strings.TrimSpace(asString(t["name"]))
			orig := strings.TrimSpace(asString(t["originalname"]))
			newName := strings.TrimSpace(bitruapi.CleanTitleForSearch(name))
			newOrig := strings.TrimSpace(bitruapi.CleanTitleForSearch(orig))
			if newName == "" {
				newName = name
			}
			if newOrig == "" {
				newOrig = orig
			}
			if newOrig == "" {
				newOrig = newName
			}
			if newName == name && newOrig == orig {
				continue
			}
			t["name"] = newName
			t["originalname"] = newOrig
			t["_sn"] = core.SearchName(newName)
			t["_so"] = core.SearchName(newOrig)
			bucket[url] = t
			bucketChanged = true
			updated++
			newKey := s.DB.KeyDb(newName, newOrig)
			if newKey != "" && newKey != item.Key && strings.Contains(newKey, ":") {
				toMigrate = append(toMigrate, migration{url, t, newKey})
			}
		}
		for _, m := range toMigrate {
			delete(bucket, m.url)
			_ = s.DB.MigrateTorrentToNewKey(m.t, m.newKey)
			migrated++
		}
		if bucketChanged {
			_ = s.DB.SaveBucketUnlocked(item.Key, bucket, time.Now().UTC())
		}
		unlock()
	}
	_ = s.DB.SaveChangesToFileNow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "processed": processed, "updated": updated, "migrated": migrated})
}

func (s *Server) handleDevRemoveBucket(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"badip": true})
		return
	}
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" || !strings.Contains(key, ":") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "key required, format: name:originalname"})
		return
	}
	migrateName := strings.TrimSpace(r.URL.Query().Get("migrateName"))
	migrateOrig := strings.TrimSpace(r.URL.Query().Get("migrateOriginalname"))
	doMigrate := migrateName != "" && migrateOrig != ""
	newKey := ""
	if doMigrate {
		newKey = s.DB.KeyDb(migrateName, migrateOrig)
	}

	bucket, unlock, err := s.DB.OpenReadOrEmptyLocked(key)
	if err != nil {
		unlock()
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	removedCount, migratedCount := 0, 0
	for url, t := range bucket {
		if t == nil || !doMigrate {
			delete(bucket, url)
			removedCount++
			continue
		}
		t["name"] = migrateName
		t["originalname"] = migrateOrig
		t["_sn"] = core.SearchName(migrateName)
		t["_so"] = core.SearchName(migrateOrig)
		delete(bucket, url)
		_ = s.DB.MigrateTorrentToNewKey(t, newKey)
		migratedCount++
	}
	_ = s.DB.SaveBucketUnlocked(key, bucket, time.Now().UTC())
	unlock()
	_ = s.DB.SaveChangesToFileNow()
	resp := map[string]any{"ok": true, "key": key, "removed": removedCount, "migrated": migratedCount}
	if doMigrate {
		resp["newKey"] = newKey
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDevFixEmptySearchFields(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"badip": true})
		return
	}
	snFixed, soFixed, migratedCount := 0, 0, 0
	for _, item := range s.DB.UnorderedMasterEntries() {
		bucket, unlock, err := s.DB.OpenReadOrEmptyLocked(item.Key)
		if err != nil {
			continue
		}
		type migration struct {
			url    string
			t      filedb.TorrentDetails
			newKey string
		}
		var toMigrate []migration
		bucketChanged := false
		for url, t := range bucket {
			if t == nil {
				delete(bucket, url)
				bucketChanged = true
				continue
			}
			sn := asString(t["_sn"])
			so := asString(t["_so"])
			if strings.TrimSpace(sn) == "" {
				name := firstNonEmpty(asString(t["name"]), asString(t["title"]))
				if name != "" {
					t["_sn"] = core.SearchName(name)
					snFixed++
					bucketChanged = true
				}
			}
			if strings.TrimSpace(so) == "" {
				orig := firstNonEmpty(asString(t["originalname"]), asString(t["name"]), asString(t["title"]))
				if orig != "" {
					t["_so"] = core.SearchName(orig)
					soFixed++
					bucketChanged = true
				}
			}
			bucket[url] = t
			newKey := s.DB.KeyDb(asString(t["name"]), asString(t["originalname"]))
			if newKey != "" && newKey != item.Key && strings.Contains(newKey, ":") {
				toMigrate = append(toMigrate, migration{url, t, newKey})
			}
		}
		for _, m := range toMigrate {
			delete(bucket, m.url)
			_ = s.DB.MigrateTorrentToNewKey(m.t, m.newKey)
			migratedCount++
			bucketChanged = true
		}
		if bucketChanged {
			_ = s.DB.SaveBucketUnlocked(item.Key, bucket, time.Now().UTC())
		}
		unlock()
	}
	_ = s.DB.SaveChangesToFileNow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "snFixed": snFixed, "soFixed": soFixed, "migrated": migratedCount})
}

var reBtih = regexp.MustCompile(`(?i)urn:btih:([a-fA-F0-9]{40})`)

func (s *Server) handleDevMigrateAnilibertyUrls(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"badip": true})
		return
	}
	processed, totalUpdated, skipped, totalErrors := 0, 0, 0, 0
	var errors []string
	for _, item := range s.DB.UnorderedMasterEntries() {
		bucket, err := s.DB.OpenReadOrEmpty(item.Key)
		if err != nil {
			continue
		}
		type update struct{ oldURL, newURL string; t filedb.TorrentDetails }
		var toUpdate []update
		for url, t := range bucket {
			if t == nil || asString(t["trackerName"]) != "aniliberty" {
				continue
			}
			processed++
			if strings.Contains(url, "?hash=") {
				skipped++
				continue
			}
			m := reBtih.FindStringSubmatch(asString(t["magnet"]))
			if len(m) < 2 {
				totalErrors++
				errors = append(errors, "no hash for: "+url)
				continue
			}
			sep := "?"
			if strings.Contains(url, "?") {
				sep = "&"
			}
			toUpdate = append(toUpdate, update{url, url + sep + "hash=" + m[1], t})
		}
		if len(toUpdate) == 0 {
			continue
		}
		for _, u := range toUpdate {
			delete(bucket, u.oldURL)
			u.t["url"] = u.newURL
			bucket[u.newURL] = u.t
			totalUpdated++
		}
		_ = s.DB.SaveBucket(item.Key, bucket, time.Now().UTC())
	}
	_ = s.DB.SaveChangesToFileNow()
	resp := map[string]any{"ok": true, "totalProcessed": processed, "totalUpdated": totalUpdated, "totalSkipped": skipped, "totalErrors": totalErrors}
	if len(errors) > 0 {
		if len(errors) > 10 {
			errors = errors[:10]
		}
		resp["errors"] = errors
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDevRemoveDuplicateAniliberty(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"badip": true})
		return
	}
	type entry struct {
		bucketKey  string
		url        string
		t          filedb.TorrentDetails
		updateTime time.Time
	}
	hashMap := map[string][]entry{}
	for _, item := range s.DB.UnorderedMasterEntries() {
		bucket, err := s.DB.OpenReadOrEmpty(item.Key)
		if err != nil {
			continue
		}
		for url, t := range bucket {
			if t == nil || asString(t["trackerName"]) != "aniliberty" {
				continue
			}
			m := reBtih.FindStringSubmatch(asString(t["magnet"]))
			if len(m) < 2 {
				continue
			}
			hash := strings.ToLower(m[1])
			ut := filedb.TorrentTime(t, "updateTime")
			hashMap[hash] = append(hashMap[hash], entry{item.Key, url, t, ut})
		}
	}
	totalRemoved := 0
	for _, group := range hashMap {
		if len(group) <= 1 {
			continue
		}
		// sort newest first
		sort.Slice(group, func(i, j int) bool {
			if group[i].updateTime.Equal(group[j].updateTime) {
				return group[i].url < group[j].url
			}
			return group[i].updateTime.After(group[j].updateTime)
		})
		for _, e := range group[1:] {
			bucket, err := s.DB.OpenReadOrEmpty(e.bucketKey)
			if err != nil {
				continue
			}
			if _, ok := bucket[e.url]; ok {
				delete(bucket, e.url)
				_ = s.DB.SaveBucket(e.bucketKey, bucket, time.Now().UTC())
				totalRemoved++
			}
		}
	}
	_ = s.DB.SaveChangesToFileNow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "totalRemoved": totalRemoved})
}

var reAnimelayerID = regexp.MustCompile(`(?i)/([a-fA-F0-9]+)(?:[/?]|$)`)

func (s *Server) handleDevFixAnimelayerDuplicates(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"badip": true})
		return
	}
	totalFixed, totalRemoved := 0, 0
	for _, item := range s.DB.UnorderedMasterEntries() {
		bucket, err := s.DB.OpenReadOrEmpty(item.Key)
		if err != nil {
			continue
		}
		// Normalize http→https for animelayer, collect by hex ID
		type aEntry struct{ url string; t filedb.TorrentDetails }
		idMap := map[string][]aEntry{}
		bucketChanged := false
		for url, t := range bucket {
			if t == nil || asString(t["trackerName"]) != "animelayer" {
				continue
			}
			newURL := url
			if strings.HasPrefix(url, "http://") {
				newURL = "https://" + url[7:]
			}
			if newURL != url {
				delete(bucket, url)
				t["url"] = newURL
				bucket[newURL] = t
				totalFixed++
				bucketChanged = true
			}
			m := reAnimelayerID.FindStringSubmatch(newURL)
			if len(m) < 2 {
				continue
			}
			id := strings.ToLower(m[1])
			idMap[id] = append(idMap[id], aEntry{newURL, t})
		}
		for _, group := range idMap {
			if len(group) <= 1 {
				continue
			}
			sort.Slice(group, func(i, j int) bool {
				ti := filedb.TorrentTime(group[i].t, "updateTime")
				tj := filedb.TorrentTime(group[j].t, "updateTime")
				return ti.After(tj)
			})
			for _, e := range group[1:] {
				delete(bucket, e.url)
				totalRemoved++
				bucketChanged = true
			}
		}
		if bucketChanged {
			_ = s.DB.SaveBucket(item.Key, bucket, time.Now().UTC())
		}
	}
	_ = s.DB.SaveChangesToFileNow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "urlsFixed": totalFixed, "duplicatesRemoved": totalRemoved})
}

var (
	reKinozalID = regexp.MustCompile(`(?i)[?&]id=(\d+)`)
	reSelezenID = regexp.MustCompile(`(?i)/relizy-ot-selezen/(\d+)-`)
)

func (s *Server) handleDevFixKinozalUrls(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"badip": true})
		return
	}
	totalFixed, totalRemoved := 0, 0
	for _, item := range s.DB.UnorderedMasterEntries() {
		bucket, err := s.DB.OpenReadOrEmpty(item.Key)
		if err != nil {
			continue
		}
		type kEntry struct {
			url string
			t   filedb.TorrentDetails
		}
		idMap := map[string][]kEntry{}
		bucketChanged := false
		for url, t := range bucket {
			if t == nil || asString(t["trackerName"]) != "kinozal" {
				continue
			}
			newURL := url
			if strings.HasPrefix(url, "http://") {
				newURL = "https://" + url[7:]
			}
			if newURL != url {
				if _, clash := bucket[newURL]; !clash {
					delete(bucket, url)
					t["url"] = newURL
					bucket[newURL] = t
					totalFixed++
					bucketChanged = true
				} else {
					// normalized form already present — drop the http copy
					delete(bucket, url)
					totalRemoved++
					bucketChanged = true
					continue
				}
			}
			m := reKinozalID.FindStringSubmatch(newURL)
			if len(m) < 2 {
				continue
			}
			idMap[m[1]] = append(idMap[m[1]], kEntry{newURL, t})
		}
		for _, group := range idMap {
			if len(group) <= 1 {
				continue
			}
			sort.Slice(group, func(i, j int) bool {
				ti := filedb.TorrentTime(group[i].t, "updateTime")
				tj := filedb.TorrentTime(group[j].t, "updateTime")
				return ti.After(tj)
			})
			for _, e := range group[1:] {
				delete(bucket, e.url)
				totalRemoved++
				bucketChanged = true
			}
		}
		if bucketChanged {
			_ = s.DB.SaveBucket(item.Key, bucket, time.Now().UTC())
		}
	}
	_ = s.DB.SaveChangesToFileNow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "urlsFixed": totalFixed, "duplicatesRemoved": totalRemoved})
}

func (s *Server) handleDevFixSelezenUrls(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"badip": true})
		return
	}
	targetHost := "selezen.top"
	if host := s.GetConfig().Selezen.Host; host != "" {
		if u, err := url.Parse(host); err == nil && u.Host != "" {
			targetHost = u.Host
		}
	}
	totalFixed, totalRemoved := 0, 0
	for _, item := range s.DB.UnorderedMasterEntries() {
		bucket, err := s.DB.OpenReadOrEmpty(item.Key)
		if err != nil {
			continue
		}
		type sEntry struct {
			url string
			t   filedb.TorrentDetails
		}
		idMap := map[string][]sEntry{}
		bucketChanged := false
		for u, t := range bucket {
			if t == nil || asString(t["trackerName"]) != "selezen" {
				continue
			}
			newURL := u
			parsed, perr := url.Parse(u)
			if perr == nil && parsed.Host != "" && parsed.Host != targetHost {
				parsed.Scheme = "https"
				parsed.Host = targetHost
				newURL = parsed.String()
			} else if strings.HasPrefix(u, "http://") {
				newURL = "https://" + u[7:]
			}
			if newURL != u {
				if _, clash := bucket[newURL]; !clash {
					delete(bucket, u)
					t["url"] = newURL
					bucket[newURL] = t
					totalFixed++
					bucketChanged = true
				} else {
					delete(bucket, u)
					totalRemoved++
					bucketChanged = true
					continue
				}
			}
			m := reSelezenID.FindStringSubmatch(newURL)
			if len(m) < 2 {
				continue
			}
			idMap[m[1]] = append(idMap[m[1]], sEntry{newURL, t})
		}
		for _, group := range idMap {
			if len(group) <= 1 {
				continue
			}
			sort.Slice(group, func(i, j int) bool {
				ti := filedb.TorrentTime(group[i].t, "updateTime")
				tj := filedb.TorrentTime(group[j].t, "updateTime")
				return ti.After(tj)
			})
			for _, e := range group[1:] {
				delete(bucket, e.url)
				totalRemoved++
				bucketChanged = true
			}
		}
		if bucketChanged {
			_ = s.DB.SaveBucket(item.Key, bucket, time.Now().UTC())
		}
	}
	_ = s.DB.SaveChangesToFileNow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "urlsFixed": totalFixed, "duplicatesRemoved": totalRemoved})
}

// handleAdminConfigGet returns the full parsed config as JSON.
func (s *Server) handleAdminConfigGet(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"badip": true})
		return
	}
	cfg := s.GetConfig()
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// handleAdminConfigSave accepts a full config JSON, writes init.yaml atomically,
// then reloads it through the same parser used on startup so validation stays consistent.
func (s *Server) handleAdminConfigSave(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"badip": true})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "POST only"})
		return
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// Start from current config and overlay incoming JSON so missing fields keep their values
	cur := s.GetConfig()
	if err := json.Unmarshal(body, &cur); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON: " + err.Error()})
		return
	}
	yaml := app.MarshalYAML(cur)

	tmp, err := os.CreateTemp(".", "init.yaml.tmp-*")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	tmpPath := tmp.Name()
	if _, err := tmp.WriteString(yaml); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err := os.Rename(tmpPath, "init.yaml"); err != nil {
		os.Remove(tmpPath)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	reloaded, err := app.LoadConfig("init.yaml")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "saved but reload failed: " + err.Error()})
		return
	}
	s.UpdateConfig(reloaded)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

var knabenSuffixRe = regexp.MustCompile(`\s+\|\s+[^|]+$`)

func (s *Server) handleStatsRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// Track via Server.bgWG so graceful shutdown waits for the refresh to
	// finish writing stats.json instead of orphaning a half-written file.
	s.Background(s.generateStatsFile)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// RunStatsLoop periodically generates Data/temp/stats.json.
func (s *Server) RunStatsLoop(ctx context.Context) {
	interval := time.Duration(s.GetConfig().TimeStatsUpdate) * time.Minute
	if interval <= 0 {
		interval = 60 * time.Minute
	}
	// Generate once at startup (after 30s delay)
	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
	}
	s.generateStatsFile()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.generateStatsFile()
		}
	}
}

func (s *Server) generateStatsFile() {
	if !s.GetConfig().OpenStats {
		return
	}
	start := time.Now()
	today := todayLocalMidnightUTC()

	type trackerStat struct {
		LastNewTor  time.Time
		NewTor      int
		Update      int
		Check       int
		AllTorrents int
		TrkConfirm  int
		TrkWait     int
		TrkError    int
	}

	trackers := map[string]*trackerStat{}

	for _, item := range s.DB.UnorderedMasterEntries() {
		bucket, err := s.DB.OpenReadNoCache(item.Key)
		if err != nil {
			continue
		}
		for _, t := range bucket {
			if t == nil {
				continue
			}
			name := strings.TrimSpace(asString(t["trackerName"]))
			if name == "" {
				continue
			}
			st, ok := trackers[name]
			if !ok {
				st = &trackerStat{}
				trackers[name] = st
			}
			st.AllTorrents++

			ct := filedbTime(t, "createTime")
			ut := filedbTime(t, "updateTime")
			chk := filedbTime(t, "checkTime")

			if ct.After(st.LastNewTor) {
				st.LastNewTor = ct
			}
			if !ct.Before(today) {
				st.NewTor++
			}
			if !ut.Before(today) {
				st.Update++
			}
			if !chk.IsZero() && !chk.Before(today) {
				st.Check++
			}

			magnet := strings.TrimSpace(asString(t["magnet"]))
			types := toStringSliceAny(t["types"])
			if magnet != "" && !tracks.TheBad(types) {
				if asInt(t["ffprobe_tryingdata"]) >= 3 {
					st.TrkError++
				} else if t["ffprobe"] != nil || s.tracksHasFFProbe(magnet, types) {
					st.TrkConfirm++
				} else {
					st.TrkWait++
				}
			}
		}
	}

	// Build output array sorted by alltorrents desc
	type tracksEntry struct {
		Wait    int `json:"wait"`
		Confirm int `json:"confirm"`
		Skip    int `json:"skip"`
	}
	type statsEntry struct {
		TrackerName string      `json:"trackerName"`
		LastNewTor  string      `json:"lastnewtor"`
		NewTor      int         `json:"newtor"`
		Update      int         `json:"update"`
		Check       int         `json:"check"`
		AllTorrents int         `json:"alltorrents"`
		Tracks      tracksEntry `json:"tracks"`
	}

	entries := make([]statsEntry, 0, len(trackers))
	for name, st := range trackers {
		lastNew := ""
		if !st.LastNewTor.IsZero() {
			lastNew = st.LastNewTor.Format("02.01.2006")
		}
		entries = append(entries, statsEntry{
			TrackerName: name,
			LastNewTor:  lastNew,
			NewTor:      st.NewTor,
			Update:      st.Update,
			Check:       st.Check,
			AllTorrents: st.AllTorrents,
			Tracks: tracksEntry{
				Wait:    st.TrkWait,
				Confirm: st.TrkConfirm,
				Skip:    st.TrkError,
			},
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].AllTorrents > entries[j].AllTorrents
	})

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		log.Printf("stats: marshal error: %v", err)
		return
	}

	statsPath := filepath.Join(s.DB.DataDir, "temp", "stats.json")
	if err := os.WriteFile(statsPath, data, 0o644); err != nil {
		log.Printf("stats: write error: %v", err)
		return
	}
	log.Printf("stats: generated in %dms, trackers=%d", time.Since(start).Milliseconds(), len(trackers))
	// Manual runtime.GC() used to live here on the assumption that the
	// bucket maps just scanned needed prompt collection. In practice the
	// GC pacer reclaims them on its own within seconds, and forcing a
	// stop-the-world pause from a request handler hurts request latency
	// more than the early reclaim helps memory pressure.
}

// handleDevTestFetch performs a diagnostic fetch of a tracker page and reports results.
// Usage: /dev/testfetch?tracker=megapeer
func (s *Server) handleDevTestFetch(w http.ResponseWriter, r *http.Request) {
	tracker := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("tracker")))
	if tracker == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tracker param required"})
		return
	}
	cfg := s.GetConfig()
	var host, cookie, alias string
	switch tracker {
	case "megapeer":
		host = cfg.Megapeer.Host
		cookie = cfg.Megapeer.Cookie
		alias = cfg.Megapeer.Alias
	case "bitru":
		host = cfg.Bitru.Host
		cookie = cfg.Bitru.Cookie
		alias = cfg.Bitru.Alias
	case "rutor":
		host = cfg.Rutor.Host
		cookie = cfg.Rutor.Cookie
		alias = cfg.Rutor.Alias
	case "torrentby":
		host = cfg.TorrentBy.Host
		cookie = cfg.TorrentBy.Cookie
		alias = cfg.TorrentBy.Alias
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown tracker: " + tracker})
		return
	}

	testURL := strings.TrimRight(host, "/") + "/"
	if alias != "" {
		testURL = strings.TrimRight(alias, "/") + "/"
	}

	hasCookie := cookie != ""
	cookiePreview := ""
	if hasCookie && len(cookie) > 30 {
		cookiePreview = cookie[:30] + "..."
	} else if hasCookie {
		cookiePreview = cookie
	}

	ua := "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
	if qUA := r.URL.Query().Get("ua"); qUA != "" {
		ua = qUA
	}

	// nocookie=true → test without cookie to check if CF blocks the IP itself
	if r.URL.Query().Get("nocookie") == "true" {
		cookie = ""
		hasCookie = false
	}

	result := map[string]any{
		"tracker":       tracker,
		"host":          host,
		"alias":         alias,
		"testURL":       testURL,
		"hasCookie":     hasCookie,
		"cookiePreview": cookiePreview,
		"userAgent":     ua,
	}

	// Test with standard http.Client + cookie
	if hasCookie {
		client := &http.Client{Timeout: 15 * time.Second}
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, testURL, nil)
		req.Header.Set("User-Agent", ua)
		req.Header.Set("Cookie", cookie)
		resp, err := client.Do(req)
		errStr := ""
		if err != nil {
			errStr = err.Error()
		}
		if resp != nil {
			defer resp.Body.Close()
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
			body := string(b)
			snippet := ""
			if len(body) > 300 {
				snippet = body[:300]
			} else {
				snippet = body
			}
			result["stdClient"] = map[string]any{
				"status":  resp.StatusCode,
				"bodyLen": len(body),
				"error":   errStr,
				"snippet": snippet,
			}
		} else if errStr != "" {
			result["stdClient"] = map[string]any{"error": errStr}
		}
	}

	writeJSON(w, http.StatusOK, result)
}

// tracksHasFFProbe reports whether the tracks DB holds an ffprobe entry for
// the given magnet (port of C# `TracksDB.Get(t.magnet) != null` used by stats).
// types is passed so TheBad-rejected categories (sport/tvshow/docuserial) are
// not counted as confirmed.
func (s *Server) tracksHasFFProbe(magnet string, types []string) bool {
	if s.TracksDB == nil || strings.TrimSpace(magnet) == "" {
		return false
	}
	streams, ok := s.TracksDB.GetByMagnet(magnet, types, true)
	return ok && len(streams) > 0
}

// enrichTorrentWithTracks augments a torrent map with ffprobe streams and
// derived languages looked up by magnet. Port of C# SyncController enrichment
// (SyncController.cs:93, 160). Only populates fields that are missing — never
// overwrites existing ffprobe/languages values.
func (s *Server) enrichTorrentWithTracks(t filedb.TorrentDetails) filedb.TorrentDetails {
	if t == nil {
		return t
	}
	if t["ffprobe"] != nil && t["languages"] != nil {
		return t
	}
	magnet := strings.TrimSpace(asString(t["magnet"]))
	if magnet == "" || s.TracksDB == nil {
		return t
	}
	streams, ok := s.TracksDB.GetByMagnet(magnet, toStringSliceAny(t["types"]), true)
	if !ok || len(streams) == 0 {
		return t
	}
	if t["ffprobe"] == nil {
		t["ffprobe"] = streams
	}
	if t["languages"] == nil {
		t["languages"] = tracks.Languages(toStringSliceAny(t["languages"]), streams)
	}
	return t
}

// handleSyncTracks serves the /sync/tracks endpoint for remote jacred instances
// to pull ffprobe data. Currently returns 404 — downstream consumers fall back
// to enrichment via the /sync/fdb/torrents payload which already includes
// ffprobe via enrichTorrentWithTracks.
func (s *Server) handleSyncTracks(w http.ResponseWriter, r *http.Request) {
	writeBareNotFound(w)
}

// handleSyncTracksCheck serves /sync/tracks/check. See handleSyncTracks.
func (s *Server) handleSyncTracksCheck(w http.ResponseWriter, r *http.Request) {
	writeBareNotFound(w)
}

// handleAdminCFDomains exposes the auto-detected CF-protected domain registry.
//
//	GET                       — list flagged domains and their last-seen timestamps
//	DELETE                    — clear all entries
//	DELETE ?domain=example.tld — clear a single entry
//
// LAN-only.
func (s *Server) handleAdminCFDomains(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"badip": true})
		return
	}
	switch r.Method {
	case http.MethodGet:
		snap := core.CFAutoSnapshot()
		out := make([]map[string]any, 0, len(snap))
		for domain, t := range snap {
			out = append(out, map[string]any{
				"domain":   domain,
				"detected": t.UTC().Format(time.RFC3339),
				"ageHours": int(time.Since(t).Hours()),
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(out), "domains": out})
	case http.MethodDelete:
		domain := strings.TrimSpace(r.URL.Query().Get("domain"))
		removed := core.ClearCFAuto(domain)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed})
	default:
		w.Header().Set("Allow", "GET, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "GET/DELETE only"})
	}
}
