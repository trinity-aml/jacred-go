package filedb

import (
	"os"
	"sort"
	"strings"
	"time"
)

func (db *DB) OpenReadOrEmpty(key string) (map[string]TorrentDetails, error) {
	bucket, err := db.OpenRead(key)
	if err == nil {
		return bucket, nil
	}
	if os.IsNotExist(err) {
		return map[string]TorrentDetails{}, nil
	}
	return nil, err
}

// OpenReadOrEmptyLocked reads the bucket while holding the per-key write lock.
// The caller MUST call the returned unlock function when done with modifications
// (typically after SaveBucketUnlocked). Usage pattern:
//
//	bucket, unlock, err := db.OpenReadOrEmptyLocked(key)
//	defer unlock()
//	// ... modify bucket ...
//	db.SaveBucketUnlocked(key, bucket, time.Now())
func (db *DB) OpenReadOrEmptyLocked(key string) (map[string]TorrentDetails, func(), error) {
	mu := db.lockKey(key)
	mu.Lock()
	bucket, err := db.OpenReadOrEmpty(key)
	if err != nil {
		mu.Unlock()
		return nil, func() {}, err
	}
	return bucket, mu.Unlock, nil
}

// SaveBucketUnlocked writes the bucket without acquiring the per-key lock.
// Use only when the caller already holds the lock via OpenReadOrEmptyLocked.
func (db *DB) SaveBucketUnlocked(key string, bucket map[string]TorrentDetails, updatedAt time.Time) error {
	return db.saveBucketInternal(key, bucket, updatedAt)
}

func (db *DB) SaveBucket(key string, bucket map[string]TorrentDetails, updatedAt time.Time) error {
	mu := db.lockKey(key)
	mu.Lock()
	defer mu.Unlock()
	return db.saveBucketInternal(key, bucket, updatedAt)
}

func (db *DB) saveBucketInternal(key string, bucket map[string]TorrentDetails, updatedAt time.Time) error {
	if strings.TrimSpace(key) == "" {
		return nil
	}
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	for _, t := range bucket {
		UpdateFullDetails(t)
	}
	// FDB audit log: compare old bucket with new before writing
	if db.fdbLog != nil {
		oldBucket, _ := db.OpenRead(key)
		if oldBucket == nil {
			oldBucket = map[string]TorrentDetails{}
		}
		db.fdbLog.LogBucketChanges(key, oldBucket, bucket)
	}
	path := db.PathDb(key)
	if err := writeBucket(path, bucket); err != nil {
		return err
	}
	if len(bucket) == 0 {
		ecDelete(path)
		db.mu.Lock()
		delete(db.masterDb, key)
		for part, keys := range db.fastdb {
			filtered := keys[:0]
			for _, existing := range keys {
				if existing != key {
					filtered = append(filtered, existing)
				}
			}
			if len(filtered) == 0 {
				delete(db.fastdb, part)
			} else {
				db.fastdb[part] = filtered
			}
		}
		db.mu.Unlock()
		return nil
	}
	db.ecPut(path, bucket)
	db.mu.Lock()
	db.masterDb[key] = TorrentInfo{UpdateTime: updatedAt.UTC(), FileTime: ToFileTimeUTC(updatedAt.UTC())}
	db.dirty.Store(true)
	for _, part := range strings.Split(key, ":") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		keys := db.fastdb[part]
		found := false
		for _, existing := range keys {
			if existing == key {
				found = true
				break
			}
		}
		if !found {
			keys = append(keys, key)
			sort.Strings(keys)
			db.fastdb[part] = keys
		}
	}
	db.mu.Unlock()
	return nil
}
