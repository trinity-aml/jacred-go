package filedb

import (
	"jacred/app"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalCrawlerIndexPersistsWithoutSync(t *testing.T) {
	cfg := app.DefaultConfig()
	cfg.SyncAPI = ""
	dir := t.TempDir()
	db := New(cfg, dir)
	key := db.KeyDb("local movie", "local movie")
	row := TorrentDetails{"name": "local movie", "originalname": "local movie", "url": "https://example.test/1"}
	if err := db.SaveBucket(key, map[string]TorrentDetails{"https://example.test/1": row}, time.Now()); err != nil {
		t.Fatal(err)
	}
	db.persistPendingChanges()
	restored := New(cfg, dir)
	if err := restored.RebuildIndexes(); err != nil {
		t.Fatal(err)
	}
	if _, ok := restored.MasterEntries()[key]; !ok {
		t.Fatal("new local key was lost")
	}
	before, err := os.Stat(filepath.Join(dir, "masterDb.bz"))
	if err != nil {
		t.Fatal(err)
	}
	db.persistPendingChanges()
	after, _ := os.Stat(filepath.Join(dir, "masterDb.bz"))
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("clean index rewritten")
	}
	if err := db.SaveBucket(key, map[string]TorrentDetails{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	db.lastSaved.Store(0)
	db.persistPendingChanges()
	restored = New(cfg, dir)
	if err := restored.RebuildIndexes(); err != nil {
		t.Fatal(err)
	}
	if _, ok := restored.MasterEntries()[key]; ok {
		t.Fatal("deleted index key restored")
	}
}

func TestOrderedCacheReusesUnchangedIndex(t *testing.T) {
	db := New(app.DefaultConfig(), t.TempDir())
	key := db.KeyDb("movie", "movie")
	row := TorrentDetails{"name": "movie", "url": "https://example.test/1"}
	if err := db.SaveBucket(key, map[string]TorrentDetails{"https://example.test/1": row}, time.Now()); err != nil {
		t.Fatal(err)
	}
	first := db.rebuildOrderedCache()
	second := db.rebuildOrderedCache()
	if &first[0] != &second[0] {
		t.Fatal("unchanged index sorted again")
	}
	db.RemoveKeyFromMasterDb(key)
	if got := db.rebuildOrderedCache(); len(got) != 0 {
		t.Fatal("deletion did not invalidate ordered cache")
	}
}
