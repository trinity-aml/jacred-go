package filedb

import (
	"compress/gzip"
	"crypto/md5"
	"encoding/json"
	"fmt"
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
	const ticksBetweenEpochs = int64(621355968000000000)
	return t.UTC().Unix()*ticksPerSecond + int64(t.UTC().Nanosecond())/100 + ticksBetweenEpochs
}

func (db *DB) OrderedMasterEntries() []MasterEntry {
	db.mu.RLock()
	defer db.mu.RUnlock()
	out := make([]MasterEntry, 0, len(db.masterDb))
	for k, v := range db.masterDb {
		out = append(out, MasterEntry{Key: k, Value: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Value.FileTime == out[j].Value.FileTime {
			return out[i].Key < out[j].Key
		}
		return out[i].Value.FileTime < out[j].Value.FileTime
	})
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
}

func (db *DB) SaveChangesToFile() error {
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
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	enc := json.NewEncoder(gz)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(master); err != nil {
		_ = gz.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	db.dailyBackup(path)
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
	for _, item := range db.OrderedMasterEntries() {
		bucket, err := db.OpenRead(item.Key)
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
	for _, item := range db.OrderedMasterEntries() {
		path := db.PathDb(item.Key)
		bucket, err := db.OpenRead(item.Key)
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
		if err := db.SaveChangesToFile(); err != nil {
			return totalRemoved, affectedFiles, err
		}
	}
	return totalRemoved, affectedFiles, nil
}

func (db *DB) FindDuplicateKeys(tracker string, excludeNumeric bool) map[string]any {
	duplicateKeys := []map[string]any{}
	for _, item := range db.OrderedMasterEntries() {
		key := item.Key
		parts := strings.SplitN(key, ":", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], parts[1]) {
			continue
		}
		if excludeNumeric && parts[0] != "" && allDigits(parts[0]) {
			continue
		}
		bucket, err := db.OpenRead(key)
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
	for _, item := range db.OrderedMasterEntries() {
		bucket, err := db.OpenRead(item.Key)
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
