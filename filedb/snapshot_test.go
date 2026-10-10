package filedb

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jacred/app"
)

func seedBucket(t *testing.T, db *DB, name, url, title string) string {
	t.Helper()
	key := db.KeyDb(name, name)
	b := map[string]TorrentDetails{
		url: {"title": title, "name": name, "originalname": name, "trackerName": "rutracker"},
	}
	if err := db.SaveBucket(key, b, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	return key
}

// flush writes the index. Kept separate from seedBucket because
// SaveChangesToFileNow runs dailyBackup, which takes the daily snapshot — so
// saving inside the seed loop would snapshot a half-seeded database.
func flush(t *testing.T, db *DB) {
	t.Helper()
	if err := db.SaveChangesToFileNow(); err != nil {
		t.Fatal(err)
	}
}

// The property the whole design rests on: a snapshot taken by hard-linking must
// not change when a bucket is rewritten afterwards. That holds only while
// writeBucket replaces the file by rename instead of writing into it — anything
// that opens a bucket for writing in place silently turns every snapshot ever
// taken into a copy of the live data.
func TestSnapshotSurvivesABucketRewrite(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "fdb"), 0o755); err != nil {
		t.Fatal(err)
	}
	db := New(app.Config{}, tmp)
	key := seedBucket(t, db, "snapshot probe", "http://x/a", "ПЕРВОЕ")
	flush(t, db)

	db.dailyDataSnapshot()
	snap := filepath.Join(tmp, "backup", "fdb-"+time.Now().Format("02-01-2006"))
	if st, err := os.Stat(snap); err != nil || !st.IsDir() {
		t.Fatalf("снимок не создан: %v", err)
	}

	// Rewrite the same bucket with different content.
	b := map[string]TorrentDetails{
		"http://x/a": {"title": "ВТОРОЕ", "name": "snapshot probe", "originalname": "snapshot probe", "trackerName": "rutracker"},
	}
	if err := db.SaveBucket(key, b, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	flush(t, db)

	// Live copy must be the new one.
	live, err := db.OpenReadNoCache(key)
	if err != nil {
		t.Fatal(err)
	}
	if got := asString(live["http://x/a"]["title"]); got != "ВТОРОЕ" {
		t.Errorf("живой бакет не обновился: %q", got)
	}

	// Snapshot copy must still be the old one — this is the whole point.
	rel, err := filepath.Rel(filepath.Join(tmp, "fdb"), db.PathDb(key))
	if err != nil {
		t.Fatal(err)
	}
	snapDB := New(app.Config{}, tmp)
	old, err := snapDB.openReadPath(filepath.Join(snap, rel))
	if err != nil {
		t.Fatalf("бакет из снимка не читается: %v", err)
	}
	if got := asString(old["http://x/a"]["title"]); got != "ПЕРВОЕ" {
		t.Errorf("снимок изменился вместе с живыми данными: %q — жёсткие ссылки больше не дают снимок", got)
	}
}

// The index belongs in the snapshot too, or a restore is not self-contained.
func TestSnapshotCarriesTheIndex(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "fdb"), 0o755); err != nil {
		t.Fatal(err)
	}
	db := New(app.Config{}, tmp)
	seedBucket(t, db, "index probe", "http://x/b", "T")
	flush(t, db)

	db.dailyDataSnapshot()
	snap := filepath.Join(tmp, "backup", "fdb-"+time.Now().Format("02-01-2006"))
	if _, err := os.Stat(filepath.Join(snap, "masterDb.bz")); err != nil {
		t.Errorf("в снимке нет masterDb.bz: %v", err)
	}
}

// A half-built snapshot must never be mistaken for a complete one, and the
// leftover must be cleaned up rather than kept forever.
func TestPartialSnapshotIsNeverPublished(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "fdb"), 0o755); err != nil {
		t.Fatal(err)
	}
	db := New(app.Config{}, tmp)
	seedBucket(t, db, "partial probe", "http://x/c", "T")
	flush(t, db)

	backupRoot := filepath.Join(tmp, "backup")
	stale := filepath.Join(backupRoot, "fdb-01-01-2020.partial")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	db.dailyDataSnapshot()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("недостроенный снимок не убран: %v", err)
	}
	// And nothing named .partial is left by a normal run.
	entries, _ := os.ReadDir(backupRoot)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".partial") {
			t.Errorf("после обычного прогона остался %s", e.Name())
		}
	}
}

// Snapshots older than the retention window go; today's stays.
func TestOldSnapshotsArePruned(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "fdb"), 0o755); err != nil {
		t.Fatal(err)
	}
	db := New(app.Config{}, tmp)
	seedBucket(t, db, "prune probe", "http://x/d", "T")
	flush(t, db)

	backupRoot := filepath.Join(tmp, "backup")
	old := filepath.Join(backupRoot, "fdb-"+time.Now().AddDate(0, 0, -dataSnapshotKeepDays-1).Format("02-01-2006"))
	keep := filepath.Join(backupRoot, "fdb-"+time.Now().AddDate(0, 0, -1).Format("02-01-2006"))
	for _, d := range []string{old, keep} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	db.dailyDataSnapshot()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("просроченный снимок не удалён")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("свежий снимок удалён: %v", err)
	}
}

// The restore procedure, executed rather than described. The realistic case is
// the one that actually happened: one bucket damaged, the rest of the database
// fine. Restoring it is a single file copy out of the snapshot — no stopping
// the service, no re-parsing 2.58M records.
func TestRestoreASingleBucketFromSnapshot(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "fdb"), 0o755); err != nil {
		t.Fatal(err)
	}
	db := New(app.Config{}, tmp)
	key := seedBucket(t, db, "restore probe", "http://x/a", "ЦЕЛОЕ")
	flush(t, db)
	db.dailyDataSnapshot()

	snap := filepath.Join(tmp, "backup", "fdb-"+time.Now().Format("02-01-2006"))
	live := db.PathDb(key)
	rel, err := filepath.Rel(filepath.Join(tmp, "fdb"), live)
	if err != nil {
		t.Fatal(err)
	}

	// Damage by *replacing* the file, which is how anything that respects the
	// rename rule would leave it. Writing into it instead would go through the
	// shared inode and damage the snapshot as well — see
	// TestInPlaceWriteAlsoDamagesTheSnapshot.
	if err := os.Remove(live); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte("мусор"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := db.OpenReadNoCache(key); err == nil {
		t.Fatal("повреждённый бакет прочитался — проверка бессмысленна")
	}

	// === the procedure ===
	// cp -f Data/backup/fdb-<date>/<rel> Data/fdb/<rel>
	if err := copyFileAtomic(filepath.Join(snap, rel), live); err != nil {
		t.Fatalf("восстановление не удалось: %v", err)
	}

	back, err := db.OpenReadNoCache(key)
	if err != nil {
		t.Fatalf("после восстановления бакет не читается: %v", err)
	}
	if got := asString(back["http://x/a"]["title"]); got != "ЦЕЛОЕ" {
		t.Errorf("восстановлено не то содержимое: %q", got)
	}

	// And the restored file must be an independent copy, not a link back into
	// the snapshot — otherwise the next write would be fine (rename) but a
	// careless in-place tool would corrupt the backup too.
	liveSt, err := os.Stat(live)
	if err != nil {
		t.Fatal(err)
	}
	snapSt, err := os.Stat(filepath.Join(snap, rel))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(liveSt, snapSt) {
		t.Error("восстановленный файл — та же ссылка, что в снимке")
	}
}

// Whole-database restore: point the DB at the snapshot and the index rebuilds
// from the bucket files, so a restore does not depend on masterDb being in
// step with them.
func TestWholeDatabaseRestoreRebuildsTheIndex(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "fdb"), 0o755); err != nil {
		t.Fatal(err)
	}
	db := New(app.Config{}, src)
	seedBucket(t, db, "whole restore", "http://x/a", "T1")
	seedBucket(t, db, "whole restore two", "http://x/b", "T2")
	flush(t, db)
	db.dailyDataSnapshot()
	snap := filepath.Join(src, "backup", "fdb-"+time.Now().Format("02-01-2006"))

	// Restore into a fresh Data dir: copy the snapshot in as Data/fdb, drop the
	// index entirely and let it rebuild.
	dst := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dst, "fdb"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := filepath.Walk(snap, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() || filepath.Base(p) == "masterDb.bz" {
			return err
		}
		rel, rerr := filepath.Rel(snap, p)
		if rerr != nil {
			return rerr
		}
		out := filepath.Join(dst, "fdb", rel)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		return copyFileAtomic(p, out)
	})
	if err != nil {
		t.Fatal(err)
	}

	restored := New(app.Config{}, dst)
	if err := restored.RebuildIndexes(); err != nil {
		t.Fatalf("индекс не пересобрался: %v", err)
	}
	if n := len(restored.MasterEntries()); n != 2 {
		t.Errorf("в пересобранном индексе %d ключей, ожидалось 2", n)
	}
	for _, name := range []string{"whole restore", "whole restore two"} {
		if b, err := restored.OpenReadNoCache(restored.KeyDb(name, name)); err != nil || len(b) != 1 {
			t.Errorf("%q не восстановился: err=%v записей=%d", name, err, len(b))
		}
	}
}

// The limitation, pinned so it is never a surprise: a hard-link snapshot shares
// an inode with the live file, so anything that writes a bucket **in place**
// damages the backup at the same instant. jacred never does — writeBucket
// replaces by rename — but an operator or an outside tool editing a bucket
// would, and the backup would look fine until the day it was needed.
func TestInPlaceWriteAlsoDamagesTheSnapshot(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "fdb"), 0o755); err != nil {
		t.Fatal(err)
	}
	db := New(app.Config{}, tmp)
	key := seedBucket(t, db, "inplace probe", "http://x/a", "ЦЕЛОЕ")
	flush(t, db)
	db.dailyDataSnapshot()

	snap := filepath.Join(tmp, "backup", "fdb-"+time.Now().Format("02-01-2006"))
	rel, err := filepath.Rel(filepath.Join(tmp, "fdb"), db.PathDb(key))
	if err != nil {
		t.Fatal(err)
	}
	// Write into the live file rather than replacing it.
	if err := os.WriteFile(db.PathDb(key), []byte("мусор"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := db.openReadPath(filepath.Join(snap, rel)); err == nil {
		t.Error("снимок уцелел при записи на месте — значит жёсткие ссылки где-то заменены копиями, " +
			"и расчёт занимаемого места в CLAUDE.md больше не верен")
	}
}

// The endpoint exists because the hand-written procedure was wrong in a way
// that destroys the backup, so its own behaviour is pinned rather than trusted.
func TestRestoreBucketFromSnapshot(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "fdb"), 0o755); err != nil {
		t.Fatal(err)
	}
	db := New(app.Config{}, tmp)
	key := seedBucket(t, db, "endpoint probe", "http://x/a", "ЦЕЛОЕ")
	flush(t, db)
	db.dailyDataSnapshot()

	// It must find the copy and say it is readable, before anything is touched.
	cands := db.SnapshotsForKey(key)
	if len(cands) != 1 || !cands[0].Readable || cands[0].Records != 1 {
		t.Fatalf("снимок не найден или признан нечитаемым: %+v", cands)
	}

	live := db.PathDb(key)
	if err := os.Remove(live); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte("мусор"), 0o644); err != nil {
		t.Fatal(err)
	}

	picked, err := db.RestoreBucketFromSnapshot(key, "")
	if err != nil {
		t.Fatalf("восстановление не удалось: %v", err)
	}
	if picked.Records != 1 {
		t.Errorf("восстановлено записей: %d", picked.Records)
	}
	back, err := db.OpenReadNoCache(key)
	if err != nil {
		t.Fatalf("после восстановления бакет не читается: %v", err)
	}
	if got := asString(back["http://x/a"]["title"]); got != "ЦЕЛОЕ" {
		t.Errorf("восстановлено не то: %q", got)
	}
	// The snapshot must be untouched and still independent.
	if c := db.SnapshotsForKey(key); len(c) != 1 || !c[0].Readable {
		t.Errorf("снимок повреждён восстановлением: %+v", c)
	}
	if _, err := os.Stat(live + bucketTmpSuffix); !os.IsNotExist(err) {
		t.Errorf("временный файл не убран: %v", err)
	}
}

// Unflushed changes would be written straight over the restored file, so the
// restore refuses instead of appearing to work.
func TestRestoreRefusesWhileChangesArePending(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "fdb"), 0o755); err != nil {
		t.Fatal(err)
	}
	db := New(app.Config{}, tmp)
	key := seedBucket(t, db, "pending probe", "http://x/a", "ЦЕЛОЕ")
	flush(t, db)
	db.dailyDataSnapshot()

	// Leave a version in the dirty cache.
	seedBucket(t, db, "pending probe", "http://x/a", "НОВОЕ")
	_, err := db.RestoreBucketFromSnapshot(key, "")
	if err == nil {
		t.Fatal("восстановление прошло при неслитых изменениях — они бы его затёрли")
	}
	if !strings.Contains(err.Error(), "unflushed") {
		t.Errorf("причина отказа не названа: %v", err)
	}
}

// An unreadable snapshot copy must never be put over the live bucket: that
// turns one problem into two with nothing left to go back to.
func TestRestoreWillNotInstallAnUnreadableCopy(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "fdb"), 0o755); err != nil {
		t.Fatal(err)
	}
	db := New(app.Config{}, tmp)
	key := seedBucket(t, db, "bad snapshot probe", "http://x/a", "ЦЕЛОЕ")
	flush(t, db)
	db.dailyDataSnapshot()

	// Damage the snapshot copy by replacing it (not in place — that would take
	// the live file with it).
	snap := filepath.Join(tmp, "backup", "fdb-"+time.Now().Format("02-01-2006"))
	rel, err := filepath.Rel(filepath.Join(tmp, "fdb"), db.PathDb(key))
	if err != nil {
		t.Fatal(err)
	}
	sp := filepath.Join(snap, rel)
	if err := os.Remove(sp); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sp, []byte("мусор"), 0o644); err != nil {
		t.Fatal(err)
	}

	if c := db.SnapshotsForKey(key); len(c) != 1 || c[0].Readable {
		t.Fatalf("битая копия в снимке признана читаемой: %+v", c)
	}
	if _, err := db.RestoreBucketFromSnapshot(key, ""); err == nil {
		t.Fatal("нечитаемая копия была установлена поверх живого бакета")
	}
	// The live bucket must be exactly as it was.
	back, rerr := db.OpenReadNoCache(key)
	if rerr != nil {
		t.Fatalf("живой бакет пострадал от неудачной попытки: %v", rerr)
	}
	if got := asString(back["http://x/a"]["title"]); got != "ЦЕЛОЕ" {
		t.Errorf("живой бакет изменился: %q", got)
	}
}

// The whole list is already known from the same scan, so a restore must not
// require a human to copy keys out of one endpoint and into another.
func TestRestoreAllUnreadableBuckets(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "fdb"), 0o755); err != nil {
		t.Fatal(err)
	}
	db := New(app.Config{}, tmp)
	names := []string{"bulk one", "bulk two", "bulk three"}
	keys := make([]string, 0, len(names))
	for i, n := range names {
		keys = append(keys, seedBucket(t, db, n, fmt.Sprintf("http://x/%d", i), "ЦЕЛОЕ"))
	}
	flush(t, db)
	db.dailyDataSnapshot()

	// Damage two of the three, by replacing the files.
	for _, k := range keys[:2] {
		p := db.PathDb(k)
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("мусор"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	damaged := db.UnreadableKeys()
	if len(damaged) != 2 {
		t.Fatalf("найдено нечитаемых: %d, ожидалось 2 (%v)", len(damaged), damaged)
	}

	results := db.RestoreAllUnreadable("")
	if len(results) != 2 {
		t.Fatalf("отчётов о восстановлении: %d", len(results))
	}
	for _, res := range results {
		if !res.Restored {
			t.Errorf("%s не восстановлен: %s", res.Key, res.Error)
		}
	}
	if left := db.UnreadableKeys(); len(left) != 0 {
		t.Errorf("после массового восстановления осталось нечитаемых: %v", left)
	}
	// The untouched bucket must be untouched.
	if b, err := db.OpenReadNoCache(keys[2]); err != nil || len(b) != 1 {
		t.Errorf("целый бакет пострадал: err=%v записей=%d", err, len(b))
	}
}

// One key that cannot be restored must not stop the others, and must be named.
func TestBulkRestoreReportsEachKeySeparately(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "fdb"), 0o755); err != nil {
		t.Fatal(err)
	}
	db := New(app.Config{}, tmp)
	withSnap := seedBucket(t, db, "has snapshot", "http://x/a", "ЦЕЛОЕ")
	flush(t, db)
	db.dailyDataSnapshot()

	// A bucket created after the snapshot has no copy to restore from.
	noSnap := seedBucket(t, db, "no snapshot", "http://x/b", "ЦЕЛОЕ")
	flush(t, db)

	for _, k := range []string{withSnap, noSnap} {
		p := db.PathDb(k)
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("мусор"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	results := db.RestoreAllUnreadable("")
	byKey := map[string]BucketRestoreOutcome{}
	for _, res := range results {
		byKey[res.Key] = res
	}
	if r := byKey[withSnap]; !r.Restored {
		t.Errorf("бакет со снимком не восстановлен: %s", r.Error)
	}
	if r := byKey[noSnap]; r.Restored {
		t.Error("бакет без снимка объявлен восстановленным")
	} else if !strings.Contains(r.Error, "no snapshot") {
		t.Errorf("причина для бакета без снимка не названа: %q", r.Error)
	}
}
