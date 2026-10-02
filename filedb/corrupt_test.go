package filedb

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jacred/app"
)

// A damaged bucket reaches the operator as the parser's `save error: <err>`.
// Production 2026-10-02 logged `invalid character ':' after object key:value
// pair` once per page for a whole rutracker category (f=1997, sport — its titles
// repeat, so most of its records share one bucket key, which is why *every* page
// failed). That text named neither the file nor the key, so the bad bucket could
// not be found from the log at all.
func TestBucketReadErrorNamesTheFile(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "fdb"), 0o755); err != nil {
		t.Fatal(err)
	}
	db := New(app.Config{}, tmp)
	key := db.KeyDb("sport thing", "sport thing")
	path := db.PathDb(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	// Not gzip at all — the shape a truncated or half-written file takes.
	if err := os.WriteFile(path, []byte(`{"a": "b": "c"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := db.OpenReadNoCache(key)
	if err == nil {
		t.Fatal("битый бакет прочитался без ошибки")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("ошибка не называет файл: %v", err)
	}

	// Valid gzip carrying invalid JSON — the exact production shape.
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(`{"a": "b": "c"}`)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = db.OpenReadNoCache(key)
	if err == nil {
		t.Fatal("битый JSON внутри gzip прочитался без ошибки")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("ошибка не называет файл: %v", err)
	}
}

// /dev/findcorrupt used to `continue` past any bucket it could not read — so the
// one kind of corruption that blocks every save was the one kind it never
// reported, and the endpoint answered "clean" while the tracker was stuck.
func TestFindCorruptReportsUnreadableBuckets(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "fdb"), 0o755); err != nil {
		t.Fatal(err)
	}
	db := New(app.Config{}, tmp)

	key := db.KeyDb("sport thing", "sport thing")
	good := map[string]TorrentDetails{
		"http://x/a": {"title": "A", "name": "a", "originalname": "a", "trackerName": "rutracker"},
	}
	if err := db.SaveBucket(key, good, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveChangesToFile(); err != nil {
		t.Fatal(err)
	}
	// Now damage the file the index still points at.
	if err := os.WriteFile(db.PathDb(key), []byte("not gzip at all"), 0o644); err != nil {
		t.Fatal(err)
	}

	rep := db.FindCorrupt(5)
	corrupt, _ := rep["corrupt"].(map[string]any)
	entry, _ := corrupt["unreadableBucket"].(map[string]any)
	if entry == nil {
		t.Fatal("в отчёте нет раздела unreadableBucket")
	}
	if n, _ := entry["count"].(int); n != 1 {
		t.Errorf("нечитаемых бакетов посчитано %v, ожидался 1", entry["count"])
	}
	sample, _ := entry["sample"].([]map[string]any)
	if len(sample) != 1 {
		t.Fatalf("в выборке %d записей, ожидалась 1", len(sample))
	}
	if p, _ := sample[0]["path"].(string); !strings.Contains(p, "fdb") {
		t.Errorf("выборка не называет путь: %v", sample[0])
	}
}

// The quarantine existed and sat on the right path (saveTorrents reads through
// OpenReadOrEmpty) — but isCorruptBucketErr only recognised truncation and gzip
// damage, so intact gzip wrapping malformed JSON slipped past it. That is the
// gap production fell into on 2026-10-02: the parser re-read the same bad bucket
// on every page of a category, which is the exact failure the quarantine's own
// comment says it prevents.
func TestCorruptJSONBucketIsQuarantined(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "fdb"), 0o755); err != nil {
		t.Fatal(err)
	}
	db := New(app.Config{}, tmp)
	key := db.KeyDb("nba 2024 2025", "nba 2024 2025")
	path := db.PathDb(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	// Valid gzip, malformed JSON — the production shape.
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(`{"http://x/a": {"title": "A"}: }`)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	bucket, err := db.OpenReadOrEmpty(key)
	if err != nil {
		t.Fatalf("битый бакет не был помещён в карантин, ошибка дошла до вызывающего: %v", err)
	}
	if len(bucket) != 0 {
		t.Errorf("ожидался пустой бакет, получено %d записей", len(bucket))
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Errorf("битая копия не сохранена рядом как .corrupt: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("битый файл остался на месте, следующая запись не создаст свежий: %v", err)
	}
}

// A value of the wrong type is a schema problem, not file damage — quarantining
// on it would throw away records that read perfectly well.
func TestWrongTypeIsNotTreatedAsCorruption(t *testing.T) {
	if isCorruptBucketErr(&json.UnmarshalTypeError{Value: "number", Type: nil}) {
		t.Error("UnmarshalTypeError принят за повреждение файла")
	}
	if !isCorruptBucketErr(&json.SyntaxError{}) {
		t.Error("SyntaxError не распознан как повреждение файла")
	}
}
