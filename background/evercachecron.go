package background

import (
	"context"
	"log"
	"runtime"
	"runtime/debug"
	"time"

	"jacred/filedb"
)

// RunEvercacheCron periodically evicts stale entries from the in-memory bucket
// cache and asks the Go runtime to return unused heap memory to the OS.
// Without the FreeOSMemory hint, RSS lags far behind the live working set —
// after a heavy sync cycle parses 500MB of JSON into transient structures,
// the heap shrinks (GC reclaims), but the OS still sees the same ~6GB RSS
// because Go's scavenger releases pages slowly by default.
//
// Runs every 10 minutes. Does nothing when evercache is disabled.
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
		// Force the scavenger to give pages back to the OS. Cheap (a few ms)
		// on a steady-state heap; the actual GC cost was already amortized
		// when the live data shrank.
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		debug.FreeOSMemory()
		runtime.ReadMemStats(&after)
		released := int64(after.HeapReleased) - int64(before.HeapReleased)
		if released > 64<<20 { // log only meaningful (>64 MB) reclaims
			log.Printf("evercache: returned %d MB to OS (heap_inuse=%d MB)",
				released/(1<<20), after.HeapInuse/(1<<20))
		}
	}
}
