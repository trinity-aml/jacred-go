package server

import (
	"sync"
	"time"
)

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
	// Evict expired entries if over limit
	if len(c.entries) >= c.maxSize {
		now := time.Now()
		for k, e := range c.entries {
			if now.Sub(e.created) > c.ttl {
				delete(c.entries, k)
			}
		}
	}
	c.entries[key] = &cacheEntry{data: data, created: time.Now()}
	c.mu.Unlock()
}

func (c *searchCache) Invalidate() {
	c.mu.Lock()
	c.entries = make(map[string]*cacheEntry)
	c.mu.Unlock()
}
