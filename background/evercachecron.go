package background

import (
	"context"
	"log"
	"time"

	"jacred/filedb"
)

// RunEvercacheCron periodically evicts stale entries from the in-memory bucket cache.
// It runs every validHour hours, dropping up to dropCacheTake entries per cycle.
// Does nothing when evercache is disabled or validHour <= 0.
func RunEvercacheCron(ctx context.Context, db *filedb.DB) {
	cfg := db.Config.Evercache
	if !cfg.Enable || cfg.ValidHour <= 0 {
		return
	}
	interval := time.Duration(cfg.ValidHour) * time.Hour
	take := cfg.DropCacheTake
	if take <= 0 {
		take = 200
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		removed := db.EvictCache(take)
		if removed > 0 {
			log.Printf("evercache: evicted %d stale entries (cache size=%d)", removed, filedb.CacheSize())
		}
	}
}
