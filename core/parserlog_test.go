package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParserLogBufferingAndFlush(t *testing.T) {
	dir := t.TempDir()
	l := NewParserLog("smoketest", dir, true)
	l.WriteAdded("http://example.com/1", "first")
	l.WriteUpdated("http://example.com/1", "first-rev")

	path := filepath.Join(dir, "smoketest.log")
	// Before Flush, debounce timer hasn't fired — but file may exist (open
	// triggers create). Content must not yet be visible.
	if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), "first") {
		t.Fatalf("buffer flushed too early: %q", data)
	}

	l.Flush()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log after flush: %v", err)
	}
	if !strings.Contains(string(data), "added") || !strings.Contains(string(data), "updated") {
		t.Fatalf("missing expected actions: %q", data)
	}
	if !strings.Contains(string(data), "http://example.com/1") {
		t.Fatalf("missing url: %q", data)
	}

	// Close should flush + release fd.
	l.WriteFailed("http://example.com/2", "second")
	l.Close()
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log after close: %v", err)
	}
	if !strings.Contains(string(data), "second") {
		t.Fatalf("missing post-close write: %q", data)
	}
}

func TestParserLogDisabled(t *testing.T) {
	dir := t.TempDir()
	l := NewParserLog("disabled", dir, false)
	l.WriteAdded("u", "t")
	l.Flush()
	if _, err := os.Stat(filepath.Join(dir, "disabled.log")); !os.IsNotExist(err) {
		t.Fatalf("disabled logger created file: %v", err)
	}
}
