package filedb

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"jacred/core"
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
	db.orderedBuildMu.Lock()
	defer db.orderedBuildMu.Unlock()
	db.mu.RLock()
	version := db.masterVersion
	db.orderedMu.RLock()
	if db.orderedReady && db.orderedVersion == version {
		cached := db.orderedCache
		db.orderedMu.RUnlock()
		db.mu.RUnlock()
		return cached
	}
	db.orderedMu.RUnlock()
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
	db.orderedVersion = version
	db.orderedReady = true
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
	db.masterVersion++
	db.dirty.Store(true)
	db.mu.Unlock()
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
	version := db.masterVersion
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
	gz := core.AcquireGzipWriter(f)
	enc := json.NewEncoder(gz)
	enc.SetEscapeHTML(false)
	encErr := enc.Encode(master)
	gzErr := gz.Close()
	core.ReleaseGzipWriter(gz)
	// fsync before the rename. temp+rename alone orders nothing against the
	// disk, so a power loss can land the rename ahead of the bytes and leave a
	// short or empty masterDb.bz — which costs the slow full .gz walk at the
	// next start, the very thing this temp+rename was added to avoid.
	syncErr := f.Sync()
	fErr := f.Close()
	if syncErr != nil {
		_ = os.Remove(tmp)
		return syncErr
	}
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
	// A writer may have changed the live index while this snapshot was encoded.
	db.mu.Lock()
	db.dirty.Store(db.masterVersion != version)
	db.mu.Unlock()
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
		// The copy is itself written temp→rename. A backup truncated by a
		// crash mid-copy is worse than no backup: it looks like one until the
		// day you need it.
		if err := copyFileAtomic(masterPath, backupPath); err != nil {
			log.Printf("filedb: masterDb backup failed: %v", err)
		}
	}

	db.dailyDataSnapshot()

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
			db.persistPendingChanges()
		case <-cacheTicker.C:
			db.rebuildOrderedCache()
		}
	}
}

// Local crawlers need index persistence even when external sync is disabled.
func (db *DB) persistPendingChanges() {
	if n := db.FlushDirtyBuckets(); n > 0 {
		log.Printf("filedb: flushed %d dirty buckets", n)
	}
	if err := db.SaveChangesToFile(); err != nil {
		log.Printf("filedb: periodic index save failed: %v", err)
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
	unreadableCount := 0
	unreadableSample := []map[string]any{}
	for _, item := range db.UnorderedMasterEntries() {
		bucket, err := db.OpenReadNoCache(item.Key)
		if err != nil {
			// A bucket that cannot be read **is** the corruption this endpoint is
			// named after, and it used to be the one thing skipped in silence — so
			// /dev/findcorrupt reported a clean database while every save against
			// that bucket failed. Production 2026-10-02: `save error: invalid
			// character ':' after object key:value pair` once per page for a whole
			// rutracker category, with nothing naming the file. A *missing* file is
			// not corruption, so that case stays skipped.
			if !errors.Is(err, os.ErrNotExist) {
				unreadableCount++
				if len(unreadableSample) < sampleSize {
					unreadableSample = append(unreadableSample, map[string]any{
						"fdbKey": item.Key,
						"path":   db.PathDb(item.Key),
						"error":  err.Error(),
					})
				}
			}
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
			// First, because it is the only entry that means a file on disk is
			// damaged rather than a record being incomplete — and the only one
			// that blocks every save against that bucket.
			"unreadableBucket":    map[string]any{"count": unreadableCount, "sample": unreadableSample},
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

// bucketTmpSuffix marks a half-written bucket. It stays in the same directory
// as the target, because rename is only atomic within one filesystem, and
// RebuildIndexes skips files carrying it.
const bucketTmpSuffix = ".tmp"

// writeBucket replaces the bucket file atomically: temp file → fsync → rename.
//
// It used to `os.Create(path)` straight onto the live file, which truncates it
// before a single byte of the new content exists. A process killed in that
// window — the OOM killer is the suspect named in doSave's own comment — leaves
// an intact gzip header over a truncated JSON body, which is exactly the
// corruption found in production 2026-10-02 (`Data/fdb/2f/15f214…`: one bad
// bucket blocking the save of every page of a whole rutracker category).
//
// doSave already wrote masterDb.bz this way, with a comment spelling out the
// reason. The rule was simply never applied to the 551k bucket files beside it.
// With temp+rename a crash leaves either the complete old file or the complete
// new one and never half of either; the fsync is what stops a power loss from
// landing the rename ahead of the data.
//
// A crash between fsync and rename leaves one `<bucket>.tmp` behind. That is
// harmless and self-limiting: the next write of the same key reuses the same
// temp name, so the litter cannot grow past one file per bucket, and the
// index rebuild skips the suffix.
func writeBucket(path string, bucket map[string]TorrentDetails) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + bucketTmpSuffix
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	fail := func(e error) error {
		_ = f.Close()
		_ = os.Remove(tmp)
		return e
	}
	gz := core.AcquireGzipWriter(f)
	enc := json.NewEncoder(gz)
	enc.SetEscapeHTML(false)
	encErr := enc.Encode(bucket)
	gzErr := gz.Close()
	core.ReleaseGzipWriter(gz)
	if encErr != nil {
		return fail(encErr)
	}
	if gzErr != nil {
		return fail(gzErr)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
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

// copyFileAtomic copies src over dst through a temp file in dst's directory.
func copyFileAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".partial"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// dataSnapshotKeepDays matches the masterDb retention. Hard links make extra
// days nearly free — a snapshot only costs space for buckets that changed
// since — so this can be raised without much thought if a longer window is
// wanted.
const dataSnapshotKeepDays = 3

// dailyDataSnapshot mirrors Data/fdb into Data/backup/fdb-DD-MM-YYYY using
// **hard links**, so the 2.58M records finally have a backup at all. Until
// 2026-10-10 only masterDb.bz was backed up — the index, not the data — which
// meant losing Data/fdb left nothing to restore but a full re-parse of every
// tracker.
//
// **Hard links are a true point-in-time snapshot here only because writeBucket
// is atomic.** It never modifies a bucket in place; it writes a temp file and
// renames over the target, which replaces the directory entry and leaves the
// old inode alive wherever another link to it exists. Before the atomicity fix
// landed the same day, an in-place write would have mutated the snapshot's
// bytes too, and this design would have quietly backed up nothing. That
// invariant is load-bearing: anything that later opens a bucket for writing
// instead of replacing it breaks every snapshot ever taken, which is what
// TestSnapshotSurvivesABucketRewrite exists to catch.
//
// What this protects against is *logical* damage — a corrupt bucket, a bad
// migration, a parser that wrote nonsense — which is what has actually bitten
// here. It shares a filesystem with the live data, so it is **not** protection
// against disk loss; that needs a copy off the box and is a separate job.
func (db *DB) dailyDataSnapshot() {
	fdbDir := filepath.Join(db.DataDir, "fdb")
	if st, err := os.Stat(fdbDir); err != nil || !st.IsDir() {
		return
	}
	backupRoot := filepath.Join(db.DataDir, "backup")
	today := time.Now().Format("02-01-2006")
	final := filepath.Join(backupRoot, "fdb-"+today)
	if _, err := os.Stat(final); err == nil {
		db.pruneDataSnapshots(backupRoot)
		return
	}
	// Build under .partial and rename the directory into place, so a snapshot
	// interrupted half-way is never mistaken for a complete one.
	staging := final + ".partial"
	_ = os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		log.Printf("filedb: data snapshot failed: %v", err)
		return
	}

	linked, failed := 0, 0
	err := filepath.Walk(fdbDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, bucketTmpSuffix) {
			return nil
		}
		rel, rerr := filepath.Rel(fdbDir, path)
		if rerr != nil {
			return nil
		}
		dst := filepath.Join(staging, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			failed++
			return nil
		}
		if err := os.Link(path, dst); err != nil {
			failed++
			return nil
		}
		linked++
		return nil
	})
	if err != nil || linked == 0 {
		_ = os.RemoveAll(staging)
		// An empty Data/fdb is the normal state of a fresh install and of the
		// first save after one, so it is not worth a line that reads like a
		// failure. Only say something when there was data and it could not be
		// linked.
		if err != nil || failed > 0 {
			log.Printf("filedb: data snapshot produced nothing (linked=%d failed=%d err=%v)", linked, failed, err)
		}
		return
	}
	// Keep the index beside the data so a restore is self-contained.
	masterPath := filepath.Join(db.DataDir, "masterDb.bz")
	if _, serr := os.Stat(masterPath); serr == nil {
		if lerr := os.Link(masterPath, filepath.Join(staging, "masterDb.bz")); lerr != nil {
			_ = copyFileAtomic(masterPath, filepath.Join(staging, "masterDb.bz"))
		}
	}
	if err := os.Rename(staging, final); err != nil {
		_ = os.RemoveAll(staging)
		log.Printf("filedb: data snapshot could not be published: %v", err)
		return
	}
	if failed > 0 {
		log.Printf("filedb: data snapshot %s — %d buckets linked, %d could not be (is Data/backup on another filesystem?)", today, linked, failed)
	} else {
		log.Printf("filedb: data snapshot %s — %d buckets", today, linked)
	}
	db.pruneDataSnapshots(backupRoot)
}

// pruneDataSnapshots drops snapshots older than dataSnapshotKeepDays, and any
// .partial left by an interrupted run.
func (db *DB) pruneDataSnapshots(backupRoot string) {
	entries, err := os.ReadDir(backupRoot)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -dataSnapshotKeepDays)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "fdb-") {
			continue
		}
		if strings.HasSuffix(name, ".partial") {
			_ = os.RemoveAll(filepath.Join(backupRoot, name))
			continue
		}
		t, perr := time.Parse("02-01-2006", strings.TrimPrefix(name, "fdb-"))
		if perr != nil {
			continue
		}
		if t.Before(cutoff) {
			_ = os.RemoveAll(filepath.Join(backupRoot, name))
		}
	}
}
