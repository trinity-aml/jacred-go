package filedb

import (
	"fmt"
	"sync"
	"testing"

	"jacred/app"
)

func TestMatchingKeysSnapshot(t *testing.T) {
	db := New(app.Config{}, t.TempDir())
	db.fastdb = map[string][]string{"f1": {"f1:f1themovie"}, "f1themovie": {"f1:f1themovie", "formula1:f1themovie"}, "other": {"other:other"}}
	got := db.matchingShardKeys([]string{"f1", "f1themovie"}, false, 0)
	if len(got) != 2 {
		t.Fatalf("expected deduplicated union, got %v", got)
	}
	delete(got, "f1:f1themovie")
	if len(db.fastdb["f1"]) != 1 || len(db.matchingShardKeys([]string{"f1"}, false, 0)) != 1 {
		t.Fatal("snapshot mutation changed the shared index")
	}
	if len(db.matchingShardKeys([]string{""}, true, 0)) != 0 {
		t.Fatal("empty normalized query must not match the full catalogue")
	}
}

func TestMissingSearchDoesNotAllocateFullIndex(t *testing.T) {
	db := New(app.Config{}, t.TempDir())
	for i := 0; i < 20000; i++ {
		k := fmt.Sprintf("film%d", i)
		db.fastdb[k] = []string{k + ":original"}
	}
	allocs := testing.AllocsPerRun(5, func() {
		if len(db.matchingShardKeys([]string{"missing"}, true, 0)) != 0 {
			t.Fatal("unexpected matches")
		}
	})
	if allocs > 2 {
		t.Fatalf("missing search allocated %.0f objects for unrelated keys", allocs)
	}
}

func TestMatchingKeysConcurrentWriters(t *testing.T) {
	db := New(app.Config{}, t.TempDir())
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				db.mu.Lock()
				db.fastdb["film"] = []string{fmt.Sprintf("film:%d", i)}
				db.mu.Unlock()
				_ = db.matchingShardKeys([]string{"film"}, true, 0)
			}
		}()
	}
	wg.Wait()
}
