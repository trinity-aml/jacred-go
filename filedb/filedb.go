package filedb

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"jacred/app"
	"jacred/core"
)

// pathCache memoizes key → bucket file path so we don't recompute MD5 on
// every PathDb call. Bounded to pathCacheMax entries; once full, ~10% of
// entries are evicted in random map-iteration order. With 50k entries the
// cache holds a ~10 MiB working set covering most active keys; rarer keys
// get re-MD5'd on next access (cheap, ~1 µs).
const (
	pathCacheMax  = 50000
	pathCacheDrop = 5000
)

var (
	pathCacheMu sync.RWMutex
	pathCache   = make(map[string]string, 1024)
)

type TorrentDetails map[string]any

type TorrentInfo struct {
	UpdateTime time.Time `json:"updateTime"`
	FileTime   int64     `json:"fileTime"`
}

type DB struct {
	Config   app.Config
	cfgMu    sync.RWMutex
	DataDir  string
	mu       sync.RWMutex
	saveMu   sync.Mutex // serializes concurrent SaveChangesToFile calls
	masterDb map[string]TorrentInfo
	fastdb   map[string][]string
	// keyShards holds a fixed pool of mutexes. We hash bucket keys into a
	// slot to serialize writes, instead of allocating a fresh *sync.Mutex
	// per key. The old sync.Map design leaked one mutex per distinct key
	// ever encountered — over a month of sync the map grew unbounded as
	// new titles streamed in. 256 shards keeps the false-sharing rate
	// negligible (a Linux box with 100k unique active keys ends up with
	// ~400 keys per slot; contention is only seen when two writers hit
	// the same slot, which is the same probability as a 1-in-256 hash
	// collision).
	keyShards [256]sync.Mutex
	dirty    atomic.Bool  // true when masterDb has unsaved changes
	lastSaved atomic.Int64 // unix nanoseconds of last successful save
	fdbLog   *FdbLogger   // audit logger for bucket changes (nil if disabled)

	// Dirty bucket cache: modified buckets held in memory, flushed to disk periodically.
	dirtyMu      sync.RWMutex
	dirtyBuckets map[string]*dirtyEntry

	// Sorted masterDb cache: rebuilt every 10 minutes in background.
	orderedMu    sync.RWMutex
	orderedCache []MasterEntry
}

// dirtyEntry holds a bucket that has been modified but not yet flushed to disk.
type dirtyEntry struct {
	bucket    map[string]TorrentDetails
	updatedAt time.Time
}

func New(cfg app.Config, dataDir string) *DB {
	db := &DB{
		Config:       cfg,
		DataDir:      dataDir,
		masterDb:     map[string]TorrentInfo{},
		fastdb:       map[string][]string{},
		dirtyBuckets: map[string]*dirtyEntry{},
	}
	if cfg.LogFdb {
		db.fdbLog = NewFdbLogger(
			filepath.Join(dataDir, "log"),
			cfg.LogFdbRetentionDays,
			cfg.LogFdbMaxSizeMb,
			cfg.LogFdbMaxFiles,
		)
	}
	return db
}

// SetConfig atomically replaces the config.
func (db *DB) SetConfig(cfg app.Config) {
	db.cfgMu.Lock()
	db.Config = cfg
	db.cfgMu.Unlock()
}

// GetConfig returns a thread-safe copy of the current config.
func (db *DB) GetConfig() app.Config {
	db.cfgMu.RLock()
	c := db.Config
	db.cfgMu.RUnlock()
	return c
}

// lockKey returns the shared mutex that serializes writes for the slot the
// key hashes into. Two different keys may share a slot — that's intentional;
// the shard pool is fixed-size so the lock table never grows.
func (db *DB) lockKey(key string) *sync.Mutex {
	// FNV-1a over the key bytes. Cheap and well-distributed for our string
	// shape (md5-ish hex + title fragments).
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return &db.keyShards[h%uint32(len(db.keyShards))]
}

func (db *DB) KeyDb(name, original string) string { return core.NameToHash(name, original) }
func (db *DB) PathDb(key string) string {
	pathCacheMu.RLock()
	if v, ok := pathCache[key]; ok {
		pathCacheMu.RUnlock()
		return v
	}
	pathCacheMu.RUnlock()

	md5key := core.MD5(key)
	var path string
	if db.GetConfig().FDBPathLevels == 2 || db.GetConfig().FDBPathLevels == 0 {
		path = filepath.Join(db.DataDir, "fdb", md5key[:2], md5key[2:])
	} else {
		path = filepath.Join(db.DataDir, "fdb", md5key[:1], md5key)
	}

	pathCacheMu.Lock()
	if _, exists := pathCache[key]; !exists {
		if len(pathCache) >= pathCacheMax {
			i := 0
			for k := range pathCache {
				delete(pathCache, k)
				i++
				if i >= pathCacheDrop {
					break
				}
			}
		}
		pathCache[key] = path
	}
	pathCacheMu.Unlock()
	return path
}
func (db *DB) OpenRead(key string) (map[string]TorrentDetails, error) {
	// Check dirty cache first (latest in-memory version)
	db.dirtyMu.RLock()
	if de, ok := db.dirtyBuckets[key]; ok {
		cp := deepCopyBucket(de.bucket)
		db.dirtyMu.RUnlock()
		return cp, nil
	}
	db.dirtyMu.RUnlock()

	path := db.PathDb(key)
	if cached := db.ecGet(path); cached != nil {
		return cached, nil
	}
	bucket, err := db.openReadPath(path)
	if err == nil {
		db.ecPut(path, bucket)
	}
	return bucket, err
}

// OpenReadNoCache reads a bucket directly from disk, bypassing the evercache.
// Use for bulk scans (stats, admin) where caching every bucket wastes memory.
// Still checks the dirty cache since it holds the latest version.
func (db *DB) OpenReadNoCache(key string) (map[string]TorrentDetails, error) {
	db.dirtyMu.RLock()
	if de, ok := db.dirtyBuckets[key]; ok {
		cp := deepCopyBucket(de.bucket)
		db.dirtyMu.RUnlock()
		return cp, nil
	}
	db.dirtyMu.RUnlock()
	return db.openReadPath(db.PathDb(key))
}
func (db *DB) openReadPath(path string) (map[string]TorrentDetails, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := core.AcquireGzipReader(f)
	if err != nil {
		return nil, err
	}
	defer core.ReleaseGzipReader(gz)
	var out map[string]TorrentDetails
	err = json.NewDecoder(gz).Decode(&out)
	return out, err
}
func (db *DB) RebuildIndexes() error {
	master := map[string]TorrentInfo{}
	masterPath := filepath.Join(db.DataDir, "masterDb.bz")
	if _, err := os.Stat(masterPath); err == nil {
		if loaded, err := readMasterDb(masterPath); err == nil && len(loaded) > 0 {
			// Migrate old C# DateTime ticks (since year 0001) to Windows FILETIME (since 1601).
			// Old values are ~6.39e17 for 2026; correct Windows FILETIME is ~1.34e17.
			const dotNetToWindowsDiff = int64(504911232000000000)
			const threshold = int64(200000000000000000) // 2e17: above this indicates old C# ticks
			for key, ti := range loaded {
				if ti.FileTime > threshold {
					ti.FileTime -= dotNetToWindowsDiff
					loaded[key] = ti
				}
			}
			master = loaded
		}
	}
	if len(master) == 0 {
		// masterDb.bz is missing or corrupt — falling back to slow full scan of all .gz files.
		// This typically happens after an OOM kill during SaveChangesToFile.
		// Expected duration: ~1-3 min for large databases. Will auto-fix on next save.
		log.Printf("filedb: masterDb.bz missing or corrupt, rebuilding from .gz files (may take minutes)...")
		err := filepath.Walk(filepath.Join(db.DataDir, "fdb"), func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return err
			}
			bucket, err := db.openReadPath(path)
			if err != nil {
				return nil
			}
			for _, t := range bucket {
				key := db.torrentKey(t)
				if key == ":" || key == "" {
					continue
				}
				ut := torrentTime(t, "updateTime")
				ti := TorrentInfo{UpdateTime: ut, FileTime: ToFileTimeUTC(ut)}
				if prev, ok := master[key]; !ok || ti.UpdateTime.After(prev.UpdateTime) {
					master[key] = ti
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	// Sized to len(master) as a conservative upper bound — each key
	// contributes 1-2 distinct parts, so the actual final size is typically
	// 0.5×–1.0× len(master). Pre-sizing avoids the rehash cascade as the
	// map grows past 8/16/32/… buckets during the initial fill.
	fast := make(map[string][]string, len(master))
	for key := range master {
		for _, part := range strings.Split(key, ":") {
			if part == "" {
				continue
			}
			fast[part] = append(fast[part], key)
		}
	}
	for _, keys := range fast {
		sort.Strings(keys)
	}
	db.mu.Lock()
	db.masterDb = master
	db.fastdb = fast
	db.mu.Unlock()
	return nil
}
func readMasterDb(path string) (map[string]TorrentInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r, err := core.AcquireGzipReader(f)
	if err != nil {
		if _, err2 := f.Seek(0, io.SeekStart); err2 == nil {
			var out map[string]TorrentInfo
			if err3 := json.NewDecoder(f).Decode(&out); err3 == nil {
				return out, nil
			}
		}
		return nil, err
	}
	defer core.ReleaseGzipReader(r)
	var out map[string]TorrentInfo
	if err := json.NewDecoder(r).Decode(&out); err == nil {
		return out, nil
	}
	return nil, errors.New("failed to decode masterDb")
}
func (db *DB) torrentKey(t TorrentDetails) string {
	name := asString(t["name"])
	original := asString(t["originalname"])
	if original == "" {
		original = name
	}
	if name == "" {
		name = original
	}
	return db.KeyDb(name, original)
}
func (db *DB) FastDB() map[string][]string {
	db.mu.RLock()
	defer db.mu.RUnlock()
	out := make(map[string][]string, len(db.fastdb))
	for k, v := range db.fastdb {
		vv := make([]string, len(v))
		copy(vv, v)
		out[k] = vv
	}
	return out
}
func (db *DB) LastUpdateDB() string {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if len(db.masterDb) == 0 {
		return "01.01.2000 01:01"
	}
	var max time.Time
	for _, v := range db.masterDb {
		if v.UpdateTime.After(max) {
			max = v.UpdateTime
		}
	}
	if max.IsZero() {
		return "01.01.2000 01:01"
	}
	// UTC: stats.html parseUTCDate interprets this as UTC and toLocaleString
	// renders it in the browser's actual local timezone. Sending a fixed
	// +0200 here produced a 2-hour skew for users outside that zone.
	return max.UTC().Format("02.01.2006 15:04")
}
func (db *DB) Search(query, title, titleOriginal string, year, isSerial int) ([]TorrentDetails, error) {
	fastdb := db.FastDB()
	torrents := make(map[string]TorrentDetails, 64)
	add := func(t TorrentDetails) {
		url := asString(t["url"])
		if url == "" {
			return
		}
		if prev, ok := torrents[url]; ok {
			if torrentTime(t, "updateTime").After(torrentTime(prev, "updateTime")) {
				torrents[url] = t
			}
			return
		}
		torrents[url] = t
	}
	if title != "" || titleOriginal != "" {
		n := core.SearchName(title)
		o := core.SearchName(titleOriginal)
		keys := map[string]struct{}{}
		if n != "" {
			for _, k := range fastdb[n] {
				keys[k] = struct{}{}
			}
		}
		if o != "" {
			for _, k := range fastdb[o] {
				keys[k] = struct{}{}
			}
		}
		for key := range keys {
			bucket, err := db.OpenRead(key)
			if err != nil {
				continue
			}
			for _, t := range bucket {
				sn := asString(t["_sn"])
				if sn == "" {
					sn = core.SearchName(asString(t["name"]))
				}
				so := asString(t["_so"])
				if so == "" {
					so = core.SearchName(asString(t["originalname"]))
				}
				if (n != "" && sn == n) || (o != "" && so == o) {
					if matchSerialAndYear(t, isSerial, year) {
						add(t)
					}
				}
			}
		}
	} else if strings.TrimSpace(query) != "" && len(strings.TrimSpace(query)) > 1 {
		s := core.SearchName(query)
		keys := map[string]struct{}{}
		if exact, ok := fastdb[s]; ok && len(exact) > 0 {
			for _, k := range exact {
				keys[k] = struct{}{}
			}
		} else {
			for fk, fv := range fastdb {
				if strings.Contains(fk, s) {
					for _, k := range fv {
						keys[k] = struct{}{}
					}
				}
			}
		}
		for key := range keys {
			bucket, err := db.OpenRead(key)
			if err != nil {
				continue
			}
			for _, t := range bucket {
				if strings.Contains(asString(t["title"]), " КПК") {
					continue
				}
				if matchSerialAndYear(t, isSerial, year) {
					add(t)
				}
			}
		}
	}
	out := make([]TorrentDetails, 0, len(torrents))
	for _, t := range torrents {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		ti := torrentTime(out[i], "createTime")
		tj := torrentTime(out[j], "createTime")
		if ti.Equal(tj) {
			return asString(out[i]["trackerName"]) < asString(out[j]["trackerName"])
		}
		return ti.After(tj)
	})
	return out, nil
}
func matchSerialAndYear(t TorrentDetails, isSerial, year int) bool {
	types := asStringSlice(t["types"])
	if len(types) == 0 {
		return false
	}
	released := asInt(t["relased"])
	has := func(want ...string) bool {
		set := map[string]struct{}{}
		for _, v := range types {
			set[v] = struct{}{}
		}
		for _, v := range want {
			if _, ok := set[v]; ok {
				return true
			}
		}
		return false
	}
	switch isSerial {
	case 1:
		if !has("movie", "multfilm", "anime", "documovie") {
			return false
		}
		if year > 0 && !(released == year || released == year-1 || released == year+1) {
			return false
		}
	case 2:
		if !has("serial", "multserial", "anime", "docuserial", "tvshow") {
			return false
		}
		if year > 0 && !(released >= year-1) {
			return false
		}
	case 3:
		if !has("tvshow") {
			return false
		}
		if year > 0 && !(released >= year-1) {
			return false
		}
	case 4:
		if !has("docuserial", "documovie") {
			return false
		}
		if year > 0 && !(released >= year-1) {
			return false
		}
	case 5:
		if !has("anime") {
			return false
		}
		if year > 0 && !(released >= year-1) {
			return false
		}
	default:
		if year > 0 {
			if has("movie", "multfilm", "documovie") {
				if !(released == year || released == year-1 || released == year+1) {
					return false
				}
			} else if !(released >= year-1) {
				return false
			}
		}
	}
	return true
}
// TorrentTime is the exported version of torrentTime.
func TorrentTime(t TorrentDetails, key string) time.Time { return torrentTime(t, key) }

func torrentTime(t TorrentDetails, key string) time.Time {
	raw, ok := t[key]
	if !ok || raw == nil {
		return time.Time{}
	}
	switch v := raw.(type) {
	case string:
		return parseDotNetTime(v)
	case time.Time:
		return v
	default:
		return time.Time{}
	}
}
func parseDotNetTime(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.9999999Z07:00", "2006-01-02T15:04:05Z07:00", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t
		}
	}
	return time.Time{}
}
func asString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}
func asInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case string:
		// strconv.Atoi avoids the heap escape of &i that Sscanf forces,
		// and runs ~10× faster on string→int conversion.
		i, _ := strconv.Atoi(strings.TrimSpace(n))
		return i
	default:
		return 0
	}
}
func asStringSlice(v any) []string {
	if arr, ok := v.([]any); ok {
		out := make([]string, 0, len(arr))
		for _, it := range arr {
			out = append(out, asString(it))
		}
		return out
	}
	if arr2, ok := v.([]string); ok {
		return arr2
	}
	return nil
}
