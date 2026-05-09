package filedb

import (
	"bufio"
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
// Each line is a JSON object with {ts, bucket, url, existing?, incoming?}.
// Files: Data/log/fdb.YYYY-MM-dd.log (rolled daily on first write past midnight).
// Retention: by days, max total size, max files (see CleanupLogs).
//
// The current day's file is held open with a buffered writer; entries land in
// the buffer and a debounce timer flushes after fdbLogFlushDelay. This trades
// the previous open/write/close-per-entry pattern (one fsync-shaped syscall
// per torrent change) for batched writes that keep up with the bucket save
// rate. Call Flush on shutdown to drain the buffer.
type FdbLogger struct {
	logDir        string
	retentionDays int
	maxSizeMb     int
	maxFiles      int
	mu            sync.Mutex

	f       *os.File
	bw      *bufio.Writer
	curDate string
	timer   *time.Timer
}

const (
	fdbLogFlushDelay = 2 * time.Second
	fdbLogBufSize    = 64 * 1024
)

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
	if err := l.ensureOpenLocked(); err != nil {
		return
	}
	_, _ = l.bw.Write(b)
	_, _ = l.bw.WriteString("\n")
	l.scheduleFlushLocked()
}

// ensureOpenLocked opens (or rotates to) today's file. Caller holds l.mu.
func (l *FdbLogger) ensureOpenLocked() error {
	today := time.Now().Format("2006-01-02")
	if l.bw != nil && l.curDate == today {
		return nil
	}
	// Date rollover or first-time open.
	l.closeFileLocked()
	if err := os.MkdirAll(l.logDir, 0o755); err != nil {
		return err
	}
	name := fmt.Sprintf("fdb.%s.log", today)
	fp := filepath.Join(l.logDir, name)
	f, err := os.OpenFile(fp, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	l.f = f
	l.bw = bufio.NewWriterSize(f, fdbLogBufSize)
	l.curDate = today
	return nil
}

func (l *FdbLogger) closeFileLocked() {
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
	if l.bw != nil {
		_ = l.bw.Flush()
		l.bw = nil
	}
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
}

func (l *FdbLogger) scheduleFlushLocked() {
	if l.timer != nil {
		return
	}
	l.timer = time.AfterFunc(fdbLogFlushDelay, func() {
		l.mu.Lock()
		l.flushLocked()
		l.mu.Unlock()
	})
}

func (l *FdbLogger) flushLocked() {
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
	if l.bw != nil {
		_ = l.bw.Flush()
	}
}

// Flush forces pending writes to disk without releasing the file handle.
func (l *FdbLogger) Flush() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.flushLocked()
	l.mu.Unlock()
}

// Close flushes and closes the underlying file. Subsequent writes will
// lazily reopen the day's file via ensureOpenLocked.
func (l *FdbLogger) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.closeFileLocked()
	l.curDate = ""
	l.mu.Unlock()
}

// CleanupLogs removes old fdb log files based on retention settings.
func (l *FdbLogger) CleanupLogs() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	// Drain the buffer first; if cleanup deletes our current file the
	// in-flight bytes still land on disk before we drop the handle.
	l.flushLocked()
	prevDate := l.curDate
	currentName := ""
	if prevDate != "" {
		currentName = fmt.Sprintf("fdb.%s.log", prevDate)
	}

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

	currentDeleted := false
	deleteFile := func(name string) {
		_ = os.Remove(filepath.Join(l.logDir, name))
		if name == currentName {
			currentDeleted = true
		}
	}

	// By days
	if l.retentionDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -l.retentionDays)
		for _, e := range fdbFiles {
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				deleteFile(e.Name())
			}
		}
	}

	// Re-read after deletions
	entries, err = os.ReadDir(l.logDir)
	if err != nil {
		if currentDeleted {
			l.closeFileLocked()
			l.curDate = ""
		}
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
			deleteFile(e.Name())
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
			deleteFile(fdbFiles[0].Name())
			fdbFiles = fdbFiles[1:]
		}
	}

	// If retention nuked our currently-open file, drop the stale handle so
	// the next write reopens against the newly-created path. Without this
	// we'd keep appending to an unlinked inode and lose the data.
	if currentDeleted {
		l.closeFileLocked()
		l.curDate = ""
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
