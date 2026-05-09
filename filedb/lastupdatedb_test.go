package filedb

import (
	"testing"
	"time"

	"jacred/app"
)

func TestLastUpdateDBReturnsUTC(t *testing.T) {
	tmp := t.TempDir()
	db := New(app.DefaultConfig(), tmp)

	utcNow := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	db.mu.Lock()
	db.masterDb["test"] = TorrentInfo{UpdateTime: utcNow}
	db.mu.Unlock()

	got := db.LastUpdateDB()
	want := "09.05.2026 12:00"
	if got != want {
		t.Fatalf("got %q, want %q (UTC time, no FixedZone shift)", got, want)
	}
}

func TestLastUpdateDBSameForDifferentInputZones(t *testing.T) {
	tmp := t.TempDir()
	db := New(app.DefaultConfig(), tmp)

	utc := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	moscow := utc.In(time.FixedZone("MSK", 3*3600))

	db.mu.Lock()
	db.masterDb["a"] = TorrentInfo{UpdateTime: utc}
	db.mu.Unlock()
	got1 := db.LastUpdateDB()

	db.mu.Lock()
	db.masterDb["a"] = TorrentInfo{UpdateTime: moscow}
	db.mu.Unlock()
	got2 := db.LastUpdateDB()

	if got1 != got2 {
		t.Fatalf("zone-shifted inputs produced different output: %q vs %q", got1, got2)
	}
}
