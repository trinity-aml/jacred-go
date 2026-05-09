package server

import (
	"strings"
	"testing"
	"time"
)

func TestCacheKeyForBoundsKeyLength(t *testing.T) {
	short := cacheKeyFor("torrents", "q=foo")
	long := cacheKeyFor("torrents", strings.Repeat("x", 1<<16))
	if len(short) != len(long) {
		t.Fatalf("hashed keys must be equal length, got %d vs %d", len(short), len(long))
	}
	// Reasonable bound: prefix + ':' + 16 hex digits.
	if len(short) > 64 {
		t.Fatalf("key unexpectedly long: %s", short)
	}
}

func TestCacheKeyForDifferentQueriesDiffer(t *testing.T) {
	a := cacheKeyFor("torrents", "q=foo")
	b := cacheKeyFor("torrents", "q=bar")
	if a == b {
		t.Fatal("distinct queries must produce distinct keys")
	}
}

func TestSearchCacheHardCapEnforced(t *testing.T) {
	c := newSearchCache(1*time.Hour, 100)
	for i := 0; i < 250; i++ {
		c.Set(cacheKeyFor("torrents", "q="+string(rune('a'+(i%26)))+string(rune('a'+((i/26)%26)))+string(rune('a'+((i/676)%26)))), []byte("x"))
	}
	c.mu.RLock()
	size := len(c.entries)
	c.mu.RUnlock()
	if size > 100 {
		t.Fatalf("cache exceeds maxSize=100: have %d entries", size)
	}
}
