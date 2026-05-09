package core

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ParserLog writes structured per-tracker log entries to Data/log/{tracker}.log.
// Each line: "2006-01-02 15:04:05 | action  | url | title"
//
// The file is opened on first write and kept open with a buffered writer.
// Writes coalesce in the buffer until either the buffer fills or a debounce
// timer fires (parserLogFlushDelay later). Earlier each Write* call did its
// own open/append/close — on a full parser run that meant thousands of
// syscalls per minute and per-line fsync waits. Flushing on a timer keeps the
// hot path cheap while still bounding the data-loss window if the process is
// killed without graceful shutdown (call FlushParserLogs from cmd/main.go on
// SIGTERM to make that window zero).
type ParserLog struct {
	path     string
	disabled bool
	mu       sync.Mutex
	f        *os.File
	bw       *bufio.Writer
	timer    *time.Timer
}

const (
	parserLogFlushDelay = 2 * time.Second
	parserLogBufSize    = 16 * 1024
)

var (
	parserLogMu sync.Mutex
	parserLogs  = map[string]*ParserLog{}
)

// NewParserLog returns a ParserLog for the given tracker.
// If enabled is false, all write operations are no-ops.
func NewParserLog(tracker, logDir string, enabled bool) *ParserLog {
	if !enabled {
		return &ParserLog{disabled: true}
	}
	key := filepath.Join(logDir, tracker+".log")
	parserLogMu.Lock()
	defer parserLogMu.Unlock()
	if l, ok := parserLogs[key]; ok {
		return l
	}
	l := &ParserLog{path: key}
	parserLogs[key] = l
	return l
}

func (l *ParserLog) WriteAdded(url, title string) {
	l.write("added  ", url, title)
}

func (l *ParserLog) WriteUpdated(url, title string) {
	l.write("updated", url, title)
}

func (l *ParserLog) WriteSkipped(url, title string) {
	l.write("skipped", url, title)
}

func (l *ParserLog) WriteFailed(url, title string) {
	l.write("failed ", url, title)
}

func (l *ParserLog) write(action, url, title string) {
	if l.disabled {
		return
	}
	ts := time.Now().Format("2006-01-02 15:04:05")
	line := fmt.Sprintf("%s | %s | %s | %s\n", ts, action, url, title)
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.openLocked(); err != nil {
		return
	}
	_, _ = l.bw.WriteString(line)
	l.scheduleFlushLocked()
}

func (l *ParserLog) openLocked() error {
	if l.bw != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	l.f = f
	l.bw = bufio.NewWriterSize(f, parserLogBufSize)
	return nil
}

func (l *ParserLog) scheduleFlushLocked() {
	if l.timer != nil {
		return
	}
	l.timer = time.AfterFunc(parserLogFlushDelay, func() {
		l.mu.Lock()
		l.flushLocked()
		l.mu.Unlock()
	})
}

func (l *ParserLog) flushLocked() {
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
	if l.bw != nil {
		_ = l.bw.Flush()
	}
}

// Flush forces pending writes to disk without releasing the file handle.
func (l *ParserLog) Flush() {
	if l == nil || l.disabled {
		return
	}
	l.mu.Lock()
	l.flushLocked()
	l.mu.Unlock()
}

// Close flushes pending writes and releases the file handle. Subsequent
// writes will lazily reopen the file.
func (l *ParserLog) Close() {
	if l == nil || l.disabled {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flushLocked()
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
		l.bw = nil
	}
}

// FlushParserLogs flushes every registered ParserLog. Safe to call from
// shutdown handlers.
func FlushParserLogs() {
	parserLogMu.Lock()
	logs := make([]*ParserLog, 0, len(parserLogs))
	for _, l := range parserLogs {
		logs = append(logs, l)
	}
	parserLogMu.Unlock()
	for _, l := range logs {
		l.Flush()
	}
}

// CloseParserLogs closes every registered ParserLog (flushing first). Use on
// final shutdown to release file descriptors cleanly.
func CloseParserLogs() {
	parserLogMu.Lock()
	logs := make([]*ParserLog, 0, len(parserLogs))
	for _, l := range parserLogs {
		logs = append(logs, l)
	}
	parserLogMu.Unlock()
	for _, l := range logs {
		l.Close()
	}
}
