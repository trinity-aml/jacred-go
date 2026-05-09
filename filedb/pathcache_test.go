package filedb

import (
	"strconv"
	"testing"

	"jacred/app"
)

// TestPathCacheBounded floods the cache past its cap and verifies the size
// stays at or below the limit.
func TestPathCacheBounded(t *testing.T) {
	pathCacheMu.Lock()
	pathCache = make(map[string]string, 1024)
	pathCacheMu.Unlock()

	tmp := t.TempDir()
	cfg := app.DefaultConfig()
	db := New(cfg, tmp)

	// Drive a small amount over the cap to confirm eviction kicks in.
	const n = pathCacheMax + pathCacheDrop + 100
	for i := 0; i < n; i++ {
		_ = db.PathDb("key-" + strconv.Itoa(i))
	}

	pathCacheMu.RLock()
	size := len(pathCache)
	pathCacheMu.RUnlock()
	if size > pathCacheMax {
		t.Fatalf("pathCache exceeded cap=%d after %d inserts: have %d", pathCacheMax, n, size)
	}
	if size == 0 {
		t.Fatal("pathCache unexpectedly empty")
	}
}

// TestPathCacheStable confirms the same key returns the same path even
// across cache evictions.
func TestPathCacheStable(t *testing.T) {
	pathCacheMu.Lock()
	pathCache = make(map[string]string, 1024)
	pathCacheMu.Unlock()

	tmp := t.TempDir()
	cfg := app.DefaultConfig()
	db := New(cfg, tmp)

	a := db.PathDb("stable-key")
	// Push many other keys through to potentially evict ours.
	for i := 0; i < pathCacheMax*2; i++ {
		_ = db.PathDb("scratch-" + strconv.Itoa(i))
	}
	b := db.PathDb("stable-key")
	if a != b {
		t.Fatalf("path mismatch after eviction: %q vs %q", a, b)
	}
}
