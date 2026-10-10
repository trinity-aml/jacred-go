package filedb

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SnapshotCandidate is one day's snapshot as it looks for one bucket.
type SnapshotCandidate struct {
	Date     string `json:"date"`
	Path     string `json:"path"`
	Readable bool   `json:"readable"`
	Records  int    `json:"records"`
	Error    string `json:"error,omitempty"`
}

// SnapshotsForKey lists every snapshot that holds this bucket, newest first,
// saying for each whether the copy is readable and how many records it has.
//
// Reading each candidate before offering it is the point: restoring an
// unreadable copy over a damaged live file would turn one problem into two with
// nothing to go back to.
func (db *DB) SnapshotsForKey(key string) []SnapshotCandidate {
	out := []SnapshotCandidate{}
	rel, err := filepath.Rel(filepath.Join(db.DataDir, "fdb"), db.PathDb(key))
	if err != nil {
		return out
	}
	backupRoot := filepath.Join(db.DataDir, "backup")
	entries, err := os.ReadDir(backupRoot)
	if err != nil {
		return out
	}
	type dated struct {
		name string
		t    time.Time
	}
	var days []dated
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "fdb-") || strings.HasSuffix(e.Name(), ".partial") {
			continue
		}
		t, perr := time.Parse("02-01-2006", strings.TrimPrefix(e.Name(), "fdb-"))
		if perr != nil {
			continue
		}
		days = append(days, dated{e.Name(), t})
	}
	sort.Slice(days, func(i, j int) bool { return days[i].t.After(days[j].t) })
	for _, d := range days {
		p := filepath.Join(backupRoot, d.name, rel)
		c := SnapshotCandidate{Date: strings.TrimPrefix(d.name, "fdb-"), Path: p}
		if b, rerr := db.openReadPath(p); rerr != nil {
			c.Error = rerr.Error()
		} else {
			c.Readable = true
			c.Records = len(b)
		}
		out = append(out, c)
	}
	return out
}

// RestoreBucketFromSnapshot puts one bucket back from a snapshot.
//
// It exists because the procedure is short, easy to get wrong, and wrong in a
// way that destroys the backup: `cp` onto the live path opens the destination
// with O_TRUNC and writes into the existing inode, which the snapshot still
// shares whenever that bucket has not been rewritten since the snapshot was
// taken. Measured on a model — live and snapshot at link count 2 — a plain `cp`
// left *both* holding the new bytes. A hand-written runbook had exactly that
// command in it until this was noticed, which is reason enough to encode the
// correct one here instead.
//
// `date` empty means the newest snapshot holding a readable copy.
func (db *DB) RestoreBucketFromSnapshot(key, date string) (SnapshotCandidate, error) {
	var zero SnapshotCandidate
	if strings.TrimSpace(key) == "" || !strings.Contains(key, ":") {
		return zero, fmt.Errorf("filedb: key required, format name:originalname")
	}
	// A pending in-memory version would be flushed straight over the restored
	// file, so refuse rather than appear to work.
	db.dirtyMu.RLock()
	_, pending := db.dirtyBuckets[key]
	db.dirtyMu.RUnlock()
	if pending {
		return zero, fmt.Errorf("filedb: %s has unflushed changes — run /jsondb/save first, or they would overwrite the restore", key)
	}

	var pick SnapshotCandidate
	for _, c := range db.SnapshotsForKey(key) {
		if !c.Readable {
			continue
		}
		if date == "" || c.Date == date {
			pick = c
			break
		}
	}
	if pick.Path == "" {
		if date != "" {
			return zero, fmt.Errorf("filedb: no readable copy of %s in snapshot %s", key, date)
		}
		return zero, fmt.Errorf("filedb: no snapshot holds a readable copy of %s", key)
	}

	live := db.PathDb(key)
	if err := os.MkdirAll(filepath.Dir(live), 0o755); err != nil {
		return zero, err
	}
	// Temp + rename, never a write onto the live path. The suffix is the one
	// RebuildIndexes skips, so an interrupted restore is not read as a bucket.
	tmp := live + bucketTmpSuffix
	if err := copyFileAtomic(pick.Path, tmp); err != nil {
		_ = os.Remove(tmp)
		return zero, err
	}
	// Prove the copy survived the trip before it becomes the live bucket.
	if _, err := db.openReadPath(tmp); err != nil {
		_ = os.Remove(tmp)
		return zero, fmt.Errorf("filedb: restored copy did not read back, live bucket left alone: %w", err)
	}
	if err := os.Rename(tmp, live); err != nil {
		_ = os.Remove(tmp)
		return zero, err
	}
	// Drop the evercache copy so readers see the restored file.
	ecDelete(live)
	return pick, nil
}

// UnreadableKeys returns the keys whose bucket file cannot be read, which is
// the same set /dev/findcorrupt reports under corrupt.unreadableBucket.
//
// It exists so a restore does not require a human to copy keys out of one
// endpoint's JSON and into another's query string: the list is already known,
// and retyping it is both tedious and a chance to restore the wrong bucket.
// Missing files are not corruption and are left out, exactly as in FindCorrupt.
func (db *DB) UnreadableKeys() []string {
	out := []string{}
	for _, item := range db.UnorderedMasterEntries() {
		if _, err := db.OpenReadNoCache(item.Key); err != nil && !errors.Is(err, os.ErrNotExist) {
			out = append(out, item.Key)
		}
	}
	sort.Strings(out)
	return out
}

// BucketRestoreOutcome is what happened to one key in a bulk restore.
type BucketRestoreOutcome struct {
	Key          string `json:"key"`
	Restored     bool   `json:"restored"`
	RestoredFrom string `json:"restoredFrom,omitempty"`
	Records      int    `json:"records,omitempty"`
	Error        string `json:"error,omitempty"`
}

// RestoreAllUnreadable restores every bucket that cannot be read, reporting each
// one separately. One key that has no readable snapshot must not stop the rest,
// so nothing here aborts the loop — the caller gets the whole picture and can
// see which keys still need a human.
func (db *DB) RestoreAllUnreadable(date string) []BucketRestoreOutcome {
	keys := db.UnreadableKeys()
	out := make([]BucketRestoreOutcome, 0, len(keys))
	for _, k := range keys {
		res := BucketRestoreOutcome{Key: k}
		picked, err := db.RestoreBucketFromSnapshot(k, date)
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Restored = true
			res.RestoredFrom = picked.Date
			res.Records = picked.Records
		}
		out = append(out, res)
	}
	return out
}
