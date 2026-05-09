package filedb

import (
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type MasterEntry struct {
	Key   string
	Value TorrentInfo
}

func ToFileTimeUTC(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	const ticksPerSecond = int64(10000000)
	const ticksBetweenEpochs = int64(116444736000000000) // 100-ns ticks from 1601-01-01 to 1970-01-01 (Windows FILETIME epoch)
	return t.UTC().Unix()*ticksPerSecond + int64(t.UTC().Nanosecond())/100 + ticksBetweenEpochs
}

// NormalizeFileTime converts a FileTime that may have been generated with the
// old C# DateTime epoch (ticks since 0001-01-01) to the correct Windows FILETIME
// epoch (ticks since 1601-01-01). Values below 5e17 are assumed already correct.
// The difference between the two epochs is 504911232000000000 ticks.
func NormalizeFileTime(ft int64) int64 {
	const csharpEpochDiff = int64(504911232000000000) // 621355968000000000 - 116444736000000000
	if ft > 500_000_000_000_000_000 {
		return ft - csharpEpochDiff
	}
	return ft
}

// SyncFileTime returns the next float64-distinct value above ft.
// Torrs and similar clients compare FileTime values as float64; at magnitudes
// around 1.34e17, float64 ULP = 16 ticks (1.6 µs), so values within the same
// 16-tick bucket compare equal. Returning math.Nextafter(float64(ft)) guarantees
// col.FileTime > filetime is true in the client, advancing the sync cursor.
func SyncFileTime(ft int64) int64 {
	return int64(math.Nextafter(float64(ft), math.MaxFloat64))
}

// OrderedMasterEntries returns a cached sorted snapshot of masterDb.
// The cache is rebuilt every 10 minutes by RunBackgroundJobs.
// Falls back to live sort if cache is empty (before first rebuild).
func (db *DB) OrderedMasterEntries() []MasterEntry {
	db.orderedMu.RLock()
	cached := db.orderedCache
	db.orderedMu.RUnlock()
	if len(cached) > 0 {
		return cached
	}
	// Fallback: build on demand (first call before background rebuild runs)
	return db.rebuildOrderedCache()
}

// rebuildOrderedCache sorts masterDb and stores in orderedCache. Returns the new slice.
func (db *DB) rebuildOrderedCache() []MasterEntry {
	db.mu.RLock()
	out := make([]MasterEntry, 0, len(db.masterDb))
	for k, v := range db.masterDb {
		out = append(out, MasterEntry{Key: k, Value: v})
	}
	db.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Value.FileTime == out[j].Value.FileTime {
			return out[i].Key < out[j].Key
		}
		return out[i].Value.FileTime < out[j].Value.FileTime
	})
	db.orderedMu.Lock()
	db.orderedCache = out
	db.orderedMu.Unlock()
	return out
}

// UnorderedMasterEntries returns all master DB entries in arbitrary (map) order.
// Use instead of OrderedMasterEntries for full scans where sort order does not
// matter (stats generation, dev/maintenance handlers). Avoids O(n log n) sort
// over 100k+ entries.
func (db *DB) UnorderedMasterEntries() []MasterEntry {
	db.mu.RLock()
	defer db.mu.RUnlock()
	out := make([]MasterEntry, 0, len(db.masterDb))
	for k, v := range db.masterDb {
		out = append(out, MasterEntry{Key: k, Value: v})
	}
	return out
}

// MigrateTorrentToNewKey adds a torrent entry to a different bucket identified by newKey.
// The caller is responsible for removing the entry from the original bucket.
func (db *DB) MigrateTorrentToNewKey(t TorrentDetails, newKey string) error {
	if newKey == "" || !strings.Contains(newKey, ":") {
		return nil
	}
	bucket, err := db.OpenReadOrEmpty(newKey)
	if err != nil {
		return err
	}
	url := asString(t["url"])
	if url == "" {
		return nil
	}
	bucket[url] = t
	return db.SaveBucket(newKey, bucket, torrentTime(t, "updateTime"))
}

// RemoveKeyFromMasterDb removes a key from masterDb and fastdb indexes without touching the bucket file.
func (db *DB) RemoveKeyFromMasterDb(key string) {
	db.mu.Lock()
	delete(db.masterDb, key)
	for part, keys := range db.fastdb {
		filtered := keys[:0]
		for _, k := range keys {
			if k != key {
				filtered = append(filtered, k)
			}
		}
		if len(filtered) == 0 {
			delete(db.fastdb, part)
		} else {
			db.fastdb[part] = filtered
		}
	}
	db.mu.Unlock()
	db.dirty.Store(true)
}

const masterDbSaveInterval = 5 * time.Minute

// SaveChangesToFile writes masterDb to disk only if there are unsaved changes
// and at least masterDbSaveInterval has passed since the last save.
// Use SaveChangesToFileNow for forced saves (shutdown, dev ops).
func (db *DB) SaveChangesToFile() error {
	if !db.dirty.Load() {
		return nil
	}
	if time.Since(time.Unix(0, db.lastSaved.Load())) < masterDbSaveInterval {
		return nil
	}
	return db.doSave()
}

// SaveChangesToFileNow writes masterDb to disk unconditionally.
// Use for shutdown handlers and dev/maintenance operations where immediate
// persistence is required regardless of the throttle interval.
func (db *DB) SaveChangesToFileNow() error {
	return db.doSave()
}

func (db *DB) doSave() error {
	db.saveMu.Lock()
	defer db.saveMu.Unlock()
	// Flush dirty buckets to disk before saving masterDb
	db.FlushDirtyBuckets()
	db.mu.RLock()
	master := make(map[string]TorrentInfo, len(db.masterDb))
	for k, v := range db.masterDb {
		master[k] = v
	}
	db.mu.RUnlock()
	path := filepath.Join(db.DataDir, "masterDb.bz")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Write to a temp file first, then rename atomically.
	// os.Create would truncate the file immediately — if the process is killed
	// mid-write (e.g. by OOM killer), the file is left corrupt and the next
	// startup falls back to the slow full .gz walk (2-3 min for 123k entries).
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	enc := json.NewEncoder(gz)
	enc.SetEscapeHTML(false)
	encErr := enc.Encode(master)
	gzErr := gz.Close()
	fErr := f.Close()
	if encErr != nil {
		_ = os.Remove(tmp)
		return encErr
	}
	if gzErr != nil {
		_ = os.Remove(tmp)
		return gzErr
	}
	if fErr != nil {
		_ = os.Remove(tmp)
		return fErr
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	db.dirty.Store(false)
	db.lastSaved.Store(time.Now().UnixNano())
	db.dailyBackup(path)
	if db.fdbLog != nil {
		go db.fdbLog.CleanupLogs()
	}
	return nil
}

// dailyBackup creates masterDb_DD-MM-YYYY.bz if it doesn't exist today,
// and removes backups older than 3 days.
func (db *DB) dailyBackup(masterPath string) {
	today := time.Now().Format("02-01-2006")
	backupName := "masterDb_" + today + ".bz"
	backupPath := filepath.Join(filepath.Dir(masterPath), backupName)

	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		src, err := os.Open(masterPath)
		if err == nil {
			dst, err := os.Create(backupPath)
			if err == nil {
				_, _ = dst.ReadFrom(src)
				dst.Close()
			}
			src.Close()
		}
	}

	// Remove backups older than 3 days
	cutoff := time.Now().AddDate(0, 0, -3)
	entries, err := os.ReadDir(filepath.Dir(masterPath))
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "masterDb_") || !strings.HasSuffix(name, ".bz") || name == "masterDb.bz" {
			continue
		}
		dateStr := strings.TrimSuffix(strings.TrimPrefix(name, "masterDb_"), ".bz")
		t, err := time.Parse("02-01-2006", dateStr)
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			_ = os.Remove(filepath.Join(filepath.Dir(masterPath), name))
		}
	}
}

// FlushDirtyBuckets writes all dirty buckets to disk and clears the dirty cache.
func (db *DB) FlushDirtyBuckets() int {
	db.dirtyMu.Lock()
	toFlush := db.dirtyBuckets
	db.dirtyBuckets = make(map[string]*dirtyEntry, len(toFlush))
	db.dirtyMu.Unlock()

	if len(toFlush) == 0 {
		return 0
	}

	flushed := 0
	for key, de := range toFlush {
		path := db.PathDb(key)
		if err := writeBucket(path, de.bucket); err != nil {
			log.Printf("filedb: flush error key=%s: %v", key, err)
			// Put back on failure
			db.dirtyMu.Lock()
			if _, exists := db.dirtyBuckets[key]; !exists {
				db.dirtyBuckets[key] = de
			}
			db.dirtyMu.Unlock()
		} else {
			flushed++
		}
	}
	return flushed
}

// DirtyCount returns the number of buckets waiting to be flushed to disk.
func (db *DB) DirtyCount() int {
	db.dirtyMu.RLock()
	n := len(db.dirtyBuckets)
	db.dirtyMu.RUnlock()
	return n
}

// FlushFdbLog drains the buffered fdb-audit log to disk. Safe to call when
// no logger is configured. Use on shutdown.
func (db *DB) FlushFdbLog() {
	db.fdbLog.Flush()
}

// CloseFdbLog flushes and closes the audit log file handle.
func (db *DB) CloseFdbLog() {
	db.fdbLog.Close()
}

// RunBackgroundJobs starts periodic tasks:
//   - Flush dirty buckets to disk every 30 seconds
//   - Rebuild sorted masterDb cache every 10 minutes
//
// Call as: go db.RunBackgroundJobs(ctx)
func (db *DB) RunBackgroundJobs(ctx context.Context) {
	// Build initial ordered cache
	db.rebuildOrderedCache()
	log.Printf("filedb: ordered cache built (%d entries)", len(db.orderedCache))

	flushTicker := time.NewTicker(30 * time.Second)
	cacheTicker := time.NewTicker(10 * time.Minute)
	defer flushTicker.Stop()
	defer cacheTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Final flush on shutdown
			if n := db.FlushDirtyBuckets(); n > 0 {
				log.Printf("filedb: shutdown flush: %d buckets", n)
			}
			return
		case <-flushTicker.C:
			if n := db.FlushDirtyBuckets(); n > 0 {
				log.Printf("filedb: flushed %d dirty buckets", n)
			}
		case <-cacheTicker.C:
			db.rebuildOrderedCache()
		}
	}
}

func (db *DB) FindCorrupt(sampleSize int) map[string]any {
	if sampleSize <= 0 {
		sampleSize = 20
	}
	totalTorrents := 0
	nullValueCount := 0
	missingNameCount := 0
	missingOriginalnameCount := 0
	missingTrackerNameCount := 0
	nullValueSample := []map[string]any{}
	missingNameSample := []map[string]any{}
	missingOriginalnameSample := []map[string]any{}
	missingTrackerNameSample := []map[string]any{}
	for _, item := range db.UnorderedMasterEntries() {
		bucket, err := db.OpenReadNoCache(item.Key)
		if err != nil {
			continue
		}
		for url, t := range bucket {
			totalTorrents++
			if t == nil {
				nullValueCount++
				if len(nullValueSample) < sampleSize {
					nullValueSample = append(nullValueSample, map[string]any{"fdbKey": item.Key, "url": url})
				}
				continue
			}
			if strings.TrimSpace(asString(t["trackerName"])) == "" {
				missingTrackerNameCount++
				if len(missingTrackerNameSample) < sampleSize {
					missingTrackerNameSample = append(missingTrackerNameSample, map[string]any{"fdbKey": item.Key, "url": url, "title": t["title"]})
				}
			}
			if strings.TrimSpace(asString(t["name"])) == "" {
				missingNameCount++
				if len(missingNameSample) < sampleSize {
					missingNameSample = append(missingNameSample, map[string]any{"fdbKey": item.Key, "url": url, "title": t["title"]})
				}
			}
			if strings.TrimSpace(asString(t["originalname"])) == "" {
				missingOriginalnameCount++
				if len(missingOriginalnameSample) < sampleSize {
					missingOriginalnameSample = append(missingOriginalnameSample, map[string]any{"fdbKey": item.Key, "url": url, "title": t["title"]})
				}
			}
		}
	}
	return map[string]any{
		"ok":            true,
		"totalFdbKeys":  len(db.MasterEntries()),
		"totalTorrents": totalTorrents,
		"corrupt": map[string]any{
			"nullValue":           map[string]any{"count": nullValueCount, "sample": nullValueSample},
			"missingName":         map[string]any{"count": missingNameCount, "sample": missingNameSample},
			"missingOriginalname": map[string]any{"count": missingOriginalnameCount, "sample": missingOriginalnameSample},
			"missingTrackerName":  map[string]any{"count": missingTrackerNameCount, "sample": missingTrackerNameSample},
		},
	}
}

func (db *DB) RemoveNullValues() (int, int, error) {
	totalRemoved := 0
	affectedFiles := 0
	for _, item := range db.UnorderedMasterEntries() {
		path := db.PathDb(item.Key)
		bucket, err := db.OpenReadNoCache(item.Key)
		if err != nil {
			continue
		}
		removedHere := 0
		for url, t := range bucket {
			if t == nil {
				delete(bucket, url)
				removedHere++
			}
		}
		if removedHere == 0 {
			continue
		}
		if err := writeBucket(path, bucket); err != nil {
			return totalRemoved, affectedFiles, err
		}
		totalRemoved += removedHere
		affectedFiles++
	}
	if affectedFiles > 0 {
		if err := db.RebuildIndexes(); err != nil {
			return totalRemoved, affectedFiles, err
		}
		if err := db.SaveChangesToFileNow(); err != nil {
			return totalRemoved, affectedFiles, err
		}
	}
	return totalRemoved, affectedFiles, nil
}

func (db *DB) FindDuplicateKeys(tracker string, excludeNumeric bool) map[string]any {
	duplicateKeys := []map[string]any{}
	for _, item := range db.UnorderedMasterEntries() {
		key := item.Key
		parts := strings.SplitN(key, ":", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], parts[1]) {
			continue
		}
		if excludeNumeric && parts[0] != "" && allDigits(parts[0]) {
			continue
		}
		bucket, err := db.OpenReadNoCache(key)
		if err != nil {
			continue
		}
		if strings.TrimSpace(tracker) != "" {
			hasTracker := false
			for _, t := range bucket {
				if strings.EqualFold(asString(t["trackerName"]), strings.TrimSpace(tracker)) {
					hasTracker = true
					break
				}
			}
			if !hasTracker {
				continue
			}
		}
		duplicateKeys = append(duplicateKeys, map[string]any{"key": key, "count": len(bucket)})
	}
	return map[string]any{"ok": true, "count": len(duplicateKeys), "keys": duplicateKeys}
}

func (db *DB) FindEmptySearchFields(sampleSize int) map[string]any {
	if sampleSize <= 0 {
		sampleSize = 20
	}
	totalTorrents := 0
	emptySnCount := 0
	emptySoCount := 0
	emptyBothCount := 0
	emptySnSample := []map[string]any{}
	emptySoSample := []map[string]any{}
	emptyBothSample := []map[string]any{}
	for _, item := range db.UnorderedMasterEntries() {
		bucket, err := db.OpenReadNoCache(item.Key)
		if err != nil {
			continue
		}
		for url, t := range bucket {
			totalTorrents++
			if t == nil {
				continue
			}
			hasEmptySn := strings.TrimSpace(asString(t["_sn"])) == ""
			hasEmptySo := strings.TrimSpace(asString(t["_so"])) == ""
			sample := map[string]any{"fdbKey": item.Key, "url": url, "title": t["title"], "name": t["name"], "originalname": t["originalname"]}
			switch {
			case hasEmptySn && hasEmptySo:
				emptyBothCount++
				if len(emptyBothSample) < sampleSize {
					emptyBothSample = append(emptyBothSample, sample)
				}
			case hasEmptySn:
				emptySnCount++
				if len(emptySnSample) < sampleSize {
					emptySnSample = append(emptySnSample, sample)
				}
			case hasEmptySo:
				emptySoCount++
				if len(emptySoSample) < sampleSize {
					emptySoSample = append(emptySoSample, sample)
				}
			}
		}
	}
	return map[string]any{
		"ok":            true,
		"totalFdbKeys":  len(db.MasterEntries()),
		"totalTorrents": totalTorrents,
		"emptySearchFields": map[string]any{
			"emptySn":   map[string]any{"count": emptySnCount, "sample": emptySnSample},
			"emptySo":   map[string]any{"count": emptySoCount, "sample": emptySoSample},
			"emptyBoth": map[string]any{"count": emptyBothCount, "sample": emptyBothSample},
			"total":     emptySnCount + emptySoCount + emptyBothCount,
		},
	}
}

func writeBucket(path string, bucket map[string]TorrentDetails) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	enc := json.NewEncoder(gz)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(bucket); err != nil {
		_ = gz.Close()
		return err
	}
	return gz.Close()
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func filedbKeyPath(key string) string {
	md5key := fmt.Sprintf("%x", md5.Sum([]byte(key)))
	return filepath.ToSlash(filepath.Join(md5key[:2], md5key[2:]))
}
