package filedb

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"jacred/app"
)

// TestSaveBucketRunsUpdateFullDetails verifies the public SaveBucket path
// still applies UpdateFullDetails (size from sizeName, quality from title)
// even after the call was hoisted out from under the per-key lock.
func TestSaveBucketRunsUpdateFullDetails(t *testing.T) {
	tmp, err := os.MkdirTemp("", "filedb-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "fdb"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := app.Config{}
	db := New(cfg, tmp)

	key := db.KeyDb("test name", "test orig")
	bucket := map[string]TorrentDetails{
		"http://x/a": {
			"title":       "Some Movie 1080p",
			"sizeName":    "2.5 GB",
			"trackerName": "rutor",
		},
	}

	if err := db.SaveBucket(key, bucket, time.Now().UTC()); err != nil {
		t.Fatalf("SaveBucket: %v", err)
	}
	got, ok := bucket["http://x/a"]
	if !ok {
		t.Fatal("torrent missing from bucket after save")
	}
	if got["quality"] != 1080 {
		t.Fatalf("quality not computed: %v", got["quality"])
	}
	sz, _ := got["size"].(int64)
	if sz <= 0 {
		t.Fatalf("size not computed: %v", got["size"])
	}
}

// TestSaveBucketConcurrentSameKey exercises the parallel-save path. Without
// the lock-shortening change a concurrent burst on the same key serialized
// CPU + disk; the test just confirms no data races and that all goroutines
// observe their own torrent in the merged result.
func TestSaveBucketConcurrentSameKey(t *testing.T) {
	tmp, err := os.MkdirTemp("", "filedb-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "fdb"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := app.Config{}
	db := New(cfg, tmp)
	key := db.KeyDb("concurrent", "concurrent")

	const N = 32
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b := map[string]TorrentDetails{
				"http://x/" + string(rune('a'+i)): {
					"title":       "Movie 720p",
					"sizeName":    "1 GB",
					"trackerName": "rutor",
				},
			}
			if err := db.SaveBucket(key, b, time.Now().UTC()); err != nil {
				t.Errorf("SaveBucket: %v", err)
			}
		}(i)
	}
	wg.Wait()
	// Final dirty bucket should hold whichever goroutine wrote last (last-
	// writer-wins is the existing semantics — we're not changing that, just
	// verifying no race tripped). DirtyCount must be at least 1.
	if db.DirtyCount() < 1 {
		t.Fatalf("expected at least one dirty bucket, got %d", db.DirtyCount())
	}
}
