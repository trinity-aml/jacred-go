package filedb

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// FdbLogger writes audit log entries for bucket changes in JSON Lines format.
// Each line is [incoming, existing] per changed torrent.
// Files: Data/log/fdb.YYYY-MM-dd.log
// Retention: by days, max total size, max files.
type FdbLogger struct {
	logDir        string
	retentionDays int
	maxSizeMb     int
	maxFiles      int
	mu            sync.Mutex
}

func NewFdbLogger(logDir string, retentionDays, maxSizeMb, maxFiles int) *FdbLogger {
	return &FdbLogger{
		logDir:        logDir,
		retentionDays: retentionDays,
		maxSizeMb:     maxSizeMb,
		maxFiles:      maxFiles,
	}
}

// LogBucketChanges compares old and new bucket, logs any differences.
func (l *FdbLogger) LogBucketChanges(bucketKey string, oldBucket, newBucket map[string]TorrentDetails) {
	if l == nil {
		return
	}
	for urlv, newT := range newBucket {
		oldT, existed := oldBucket[urlv]
		if !existed {
			l.writeEntry(bucketKey, urlv, nil, newT)
			continue
		}
		if !torrentDetailsEqual(oldT, newT) {
			l.writeEntry(bucketKey, urlv, oldT, newT)
		}
	}
	for urlv, oldT := range oldBucket {
		if _, exists := newBucket[urlv]; !exists {
			l.writeEntry(bucketKey, urlv, oldT, nil)
		}
	}
}

func (l *FdbLogger) writeEntry(bucketKey, urlv string, existing, incoming TorrentDetails) {
	entry := map[string]any{
		"ts":     time.Now().UTC().Format(time.RFC3339),
		"bucket": bucketKey,
		"url":    urlv,
	}
	if existing != nil {
		entry["existing"] = existing
	}
	if incoming != nil {
		entry["incoming"] = incoming
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = os.MkdirAll(l.logDir, 0o755)
	name := fmt.Sprintf("fdb.%s.log", time.Now().Format("2006-01-02"))
	fp := filepath.Join(l.logDir, name)
	f, err := os.OpenFile(fp, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	_, _ = f.Write(b)
	_, _ = f.WriteString("\n")
	f.Close()
}

// CleanupLogs removes old fdb log files based on retention settings.
func (l *FdbLogger) CleanupLogs() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	entries, err := os.ReadDir(l.logDir)
	if err != nil {
		return
	}
	var fdbFiles []os.DirEntry
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "fdb.") && strings.HasSuffix(e.Name(), ".log") {
			fdbFiles = append(fdbFiles, e)
		}
	}
	sort.Slice(fdbFiles, func(i, j int) bool {
		return fdbFiles[i].Name() < fdbFiles[j].Name()
	})

	// By days
	if l.retentionDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -l.retentionDays)
		for _, e := range fdbFiles {
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				_ = os.Remove(filepath.Join(l.logDir, e.Name()))
			}
		}
	}

	// Re-read after deletions
	entries, err = os.ReadDir(l.logDir)
	if err != nil {
		return
	}
	fdbFiles = fdbFiles[:0]
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "fdb.") && strings.HasSuffix(e.Name(), ".log") {
			fdbFiles = append(fdbFiles, e)
		}
	}
	sort.Slice(fdbFiles, func(i, j int) bool {
		return fdbFiles[i].Name() < fdbFiles[j].Name()
	})

	// By max files (remove oldest first)
	if l.maxFiles > 0 && len(fdbFiles) > l.maxFiles {
		for _, e := range fdbFiles[:len(fdbFiles)-l.maxFiles] {
			_ = os.Remove(filepath.Join(l.logDir, e.Name()))
		}
		fdbFiles = fdbFiles[len(fdbFiles)-l.maxFiles:]
	}

	// By total size
	if l.maxSizeMb > 0 {
		maxBytes := int64(l.maxSizeMb) * 1024 * 1024
		var totalSize int64
		for _, e := range fdbFiles {
			info, err := e.Info()
			if err != nil {
				continue
			}
			totalSize += info.Size()
		}
		for totalSize > maxBytes && len(fdbFiles) > 0 {
			info, err := fdbFiles[0].Info()
			if err == nil {
				totalSize -= info.Size()
			}
			_ = os.Remove(filepath.Join(l.logDir, fdbFiles[0].Name()))
			fdbFiles = fdbFiles[1:]
		}
	}
}

// torrentDetailsEqual does a shallow comparison of two TorrentDetails maps.
func torrentDetailsEqual(a, b TorrentDetails) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok {
			return false
		}
		if fmt.Sprintf("%v", va) != fmt.Sprintf("%v", vb) {
			return false
		}
	}
	return true
}
