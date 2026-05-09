package server

import (
	"fmt"
	"hash/fnv"
	"sync"
	"time"
)

// cacheKeyFor returns a fixed-size cache key from a prefix and the raw HTTP
// query string. Hashing the query (FNV-64) is enough — collisions degrade
// to a cache miss, never to wrong-data — and it caps key memory regardless
// of how long the attacker makes their query string. With raw concatenation
// an attacker can mint arbitrarily many distinct long keys to inflate the
// cache map; with a fixed-width hash the bound is just maxSize × ~32 bytes.
func cacheKeyFor(prefix, rawQuery string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(rawQuery))
	return fmt.Sprintf("%s:%016x", prefix, h.Sum64())
}

// searchCache is a simple TTL cache for search responses.
type searchCache struct {
	mu      sync.RWMutex
	entries map[string]*cacheEntry
	ttl     time.Duration
	maxSize int
}

type cacheEntry struct {
	data    []byte
	created time.Time
}

func newSearchCache(ttl time.Duration, maxSize int) *searchCache {
	return &searchCache{
		entries: make(map[string]*cacheEntry),
		ttl:     ttl,
		maxSize: maxSize,
	}
}

func (c *searchCache) Get(key string) ([]byte, bool) {
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Since(e.created) > c.ttl {
		c.mu.Lock()
		delete(c.entries, key)
		c.mu.Unlock()
		return nil, false
	}
	return e.data, true
}

func (c *searchCache) Set(key string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.maxSize {
		now := time.Now()
		// First pass: drop expired entries.
		for k, e := range c.entries {
			if now.Sub(e.created) > c.ttl {
				delete(c.entries, k)
			}
		}
		// Hard cap fallback: if everything is fresh and we're still at
		// the limit, evict ~10% in random map-iteration order so the cap
		// is actually enforced. Without this, a flood of unique queries
		// hitting the cache within TTL grows the map without bound.
		if len(c.entries) >= c.maxSize {
			target := c.maxSize - c.maxSize/10
			if target < 1 {
				target = 0
			}
			for k := range c.entries {
				if len(c.entries) <= target {
					break
				}
				delete(c.entries, k)
			}
		}
	}
	c.entries[key] = &cacheEntry{data: data, created: time.Now()}
}

func (c *searchCache) Invalidate() {
	c.mu.Lock()
	c.entries = make(map[string]*cacheEntry)
	c.mu.Unlock()
}
