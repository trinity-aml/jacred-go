package background

import (
	"context"
	"log"
	"time"

	"jacred/filedb"
)

// RunEvercacheCron periodically evicts stale entries from the in-memory bucket cache.
// Runs every 10 minutes regardless of validHour (validHour controls staleness cutoff).
// Does nothing when evercache is disabled or validHour <= 0.
func RunEvercacheCron(ctx context.Context, db *filedb.DB) {
	ecCfg := db.GetConfig().Evercache
	if !ecCfg.Enable || ecCfg.ValidHour <= 0 {
		return
	}
	take := ecCfg.DropCacheTake
	if take <= 0 {
		take = 100
	}
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		removed := db.EvictCache(take)
		if removed > 0 {
			log.Printf("evercache: evicted %d stale entries (cache size=%d)", removed, filedb.CacheSize())
		}
	}
}
