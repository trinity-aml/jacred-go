package filedb

import (
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// bucketCacheEntry holds a cached bucket and the last time it was accessed.
type bucketCacheEntry struct {
	mu         sync.RWMutex
	bucket     map[string]TorrentDetails
	accessedAt time.Time
}

// ecStore is the global in-memory bucket cache (keyed by bucket path on disk).
var (
	ecMu    sync.RWMutex
	ecStore = map[string]*bucketCacheEntry{}
)

// ecEnabled reports whether evercache is active for the given DB config.
func (db *DB) ecEnabled() bool {
	return db.Config.Evercache.Enable && db.Config.Evercache.ValidHour > 0
}

// ecGet returns a deep copy of the cached bucket for path, or nil if not cached / expired.
func (db *DB) ecGet(path string) map[string]TorrentDetails {
	if !db.ecEnabled() {
		return nil
	}
	ecMu.RLock()
	e, ok := ecStore[path]
	ecMu.RUnlock()
	if !ok {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	cutoff := time.Now().Add(-time.Duration(db.Config.Evercache.ValidHour) * time.Hour)
	if e.accessedAt.Before(cutoff) {
		return nil
	}
	e.accessedAt = time.Now()
	return deepCopyBucket(e.bucket)
}

// ecPut stores (or replaces) a deep copy of bucket for path.
// If the cache exceeds MaxOpenWriteTask entries, the oldest DropCacheTake entries are evicted.
func (db *DB) ecPut(path string, bucket map[string]TorrentDetails) {
	if !db.ecEnabled() {
		return
	}
	cp := deepCopyBucket(bucket)
	entry := &bucketCacheEntry{bucket: cp, accessedAt: time.Now()}
	ecMu.Lock()
	ecStore[path] = entry
	maxSize := db.Config.Evercache.MaxOpenWriteTask
	overflow := maxSize > 0 && len(ecStore) > maxSize
	ecMu.Unlock()
	if overflow {
		take := db.Config.Evercache.DropCacheTake
		if take <= 0 {
			take = 200
		}
		db.evictOldest(take)
	}
}

// evictOldest removes the `take` least-recently-used entries from the cache.
func (db *DB) evictOldest(take int) {
	type kv struct {
		path       string
		accessedAt time.Time
	}
	ecMu.RLock()
	entries := make([]kv, 0, len(ecStore))
	for path, e := range ecStore {
		e.mu.Lock()
		at := e.accessedAt
		e.mu.Unlock()
		entries = append(entries, kv{path, at})
	}
	ecMu.RUnlock()

	// sort oldest first
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].accessedAt.Before(entries[j].accessedAt)
	})
	if take > len(entries) {
		take = len(entries)
	}
	ecMu.Lock()
	for i := 0; i < take; i++ {
		delete(ecStore, entries[i].path)
	}
	ecMu.Unlock()
}

// ecDelete removes a path from the cache (used when a bucket is deleted).
func ecDelete(path string) {
	ecMu.Lock()
	delete(ecStore, path)
	ecMu.Unlock()
}

// EvictCache removes up to take entries whose last access is older than validHour.
// Returns the number of entries removed. If take <= 0, all stale entries are removed.
func (db *DB) EvictCache(take int) int {
	if !db.ecEnabled() {
		return 0
	}
	cutoff := time.Now().Add(-time.Duration(db.Config.Evercache.ValidHour) * time.Hour)

	ecMu.Lock()
	defer ecMu.Unlock()

	removed := 0
	for path, e := range ecStore {
		if take > 0 && removed >= take {
			break
		}
		e.mu.RLock()
		stale := e.accessedAt.Before(cutoff)
		e.mu.RUnlock()
		if stale {
			delete(ecStore, path)
			removed++
		}
	}
	return removed
}

// CacheSize returns the current number of entries in the evercache.
func CacheSize() int {
	ecMu.RLock()
	n := len(ecStore)
	ecMu.RUnlock()
	return n
}

// deepCopyBucket returns a full deep copy of a bucket via JSON round-trip.
func deepCopyBucket(src map[string]TorrentDetails) map[string]TorrentDetails {
	if src == nil {
		return nil
	}
	b, err := json.Marshal(src)
	if err != nil {
		return nil
	}
	var dst map[string]TorrentDetails
	if err := json.Unmarshal(b, &dst); err != nil {
		return nil
	}
	return dst
}
