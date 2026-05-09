package filedb

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFdbLoggerBufferAndFlush(t *testing.T) {
	dir := t.TempDir()
	l := NewFdbLogger(dir, 0, 0, 0)

	l.LogBucketChanges("bucketA", nil, map[string]TorrentDetails{
		"http://x/1": {"title": "one"},
	})
	// Before flush, no bytes on disk yet (they sit in the bufio buffer).
	today := time.Now().Format("2006-01-02")
	logPath := filepath.Join(dir, "fdb."+today+".log")
	if data, err := os.ReadFile(logPath); err == nil && len(data) > 0 {
		t.Fatalf("flushed too early, got %d bytes", len(data))
	}

	l.Flush()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read after flush: %v", err)
	}
	if !strings.Contains(string(data), "bucketA") || !strings.Contains(string(data), "http://x/1") {
		t.Fatalf("missing entry payload: %q", data)
	}
	// Must be a single JSON object per line.
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	count := 0
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line not JSON: %q: %v", line, err)
		}
		count++
	}
	if count != 1 {
		t.Fatalf("want 1 entry, got %d", count)
	}

	// Close should flush + release fd; subsequent write reopens.
	l.LogBucketChanges("bucketB", map[string]TorrentDetails{"http://x/2": {"title": "two"}}, nil)
	l.Close()
	data, _ = os.ReadFile(logPath)
	if !strings.Contains(string(data), "bucketB") {
		t.Fatalf("close did not flush: %q", data)
	}
}

func TestFdbLoggerCleanupHandlesCurrentFile(t *testing.T) {
	dir := t.TempDir()
	l := NewFdbLogger(dir, 0, 0, 1) // keep only 1 file
	// Pre-create a stale file so the rotation will keep ours and delete it.
	stale := filepath.Join(dir, "fdb.2000-01-01.log")
	if err := os.WriteFile(stale, []byte("old\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	l.LogBucketChanges("k", nil, map[string]TorrentDetails{"u": {"t": "v"}})
	l.Flush()
	l.CleanupLogs()

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale file should have been deleted")
	}
	// Current file must still exist and be writable; trigger another write.
	l.LogBucketChanges("k", nil, map[string]TorrentDetails{"u2": {"t": "v"}})
	l.Flush()

	today := time.Now().Format("2006-01-02")
	curr := filepath.Join(dir, "fdb."+today+".log")
	data, err := os.ReadFile(curr)
	if err != nil {
		t.Fatalf("current file gone after cleanup: %v", err)
	}
	if !strings.Contains(string(data), "u2") {
		t.Fatalf("post-cleanup write missing: %q", data)
	}
	l.Close()
}
