package core

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ParserLog writes structured per-tracker log entries to Data/log/{tracker}.log.
// Each line: "2006-01-02 15:04:05 | action  | url | title"
type ParserLog struct {
	path string
	mu   sync.Mutex
}

var parserLogMu sync.Mutex
var parserLogs = map[string]*ParserLog{}

// NewParserLog returns a ParserLog for the given tracker, writing to logDir/{tracker}.log.
func NewParserLog(tracker, logDir string) *ParserLog {
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
	ts := time.Now().Format("2006-01-02 15:04:05")
	line := fmt.Sprintf("%s | %s | %s | %s\n", ts, action, url, title)
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = os.MkdirAll(filepath.Dir(l.path), 0o755)
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line)
	f.Close()
}
