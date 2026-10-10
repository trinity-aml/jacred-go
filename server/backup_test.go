package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"jacred/app"
	"jacred/filedb"
	"jacred/tracks"
)

// backupServer builds a Server with a throwaway Data dir and a crontab, and
// chdirs into a temp dir because init.yaml is resolved relative to the working
// directory (cmd/main.go does the same).
func backupServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)

	if err := os.MkdirAll(filepath.Join(dir, "Data", "temp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "Data", "cookie"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := app.DefaultConfig()
	cfg.SchedulerFile = filepath.Join(dir, "crontab")
	cfg.Rutor.Login = app.LoginSettings{U: "live-user", P: "live-pass"}
	cfg.Rutor.Cookie = "live_cookie=abc"
	cfg.DevKey = "live-devkey"
	cfg.APIKey = "live-apikey"

	if err := os.WriteFile(cfg.SchedulerFile,
		[]byte("# измеренный тайминг: холодный прогон ~4.5 мин\n0 6 * * * curl -s \"http://127.0.0.1:9117/cron/rutor/parse\"\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Data", "temp", "cf_auto.json"), []byte(`{"rutracker.org":123}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Data", "temp", "rutor_taskParse.json"), []byte(`{"1":{"page":0}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Data", "temp", "stats.json"), []byte(`{"derived":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Data", "cookie", "rutracker.org.json"), []byte(`{"auth_cookie":"bb_session=secret"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Built through New so the restore path's UpdateConfig reaches real
	// parsers; a hand-made Server has none and panics there, which would have
	// left the reload step untested.
	db := filedb.New(cfg, filepath.Join(dir, "Data"))
	s := New(cfg, db, tracks.New(filepath.Join(dir, "Data")), "")
	return s, dir
}

func unpack(t *testing.T, body []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("архив не распаковывается: %v", err)
	}
	defer gz.Close()
	out := map[string][]byte{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		b, _ := readAllLimited(tr)
		out[h.Name] = b
	}
	return out
}

func readAllLimited(r *tar.Reader) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r)
	return buf.Bytes(), err
}

func TestBackupCarriesSettingsAndStateButNotTheDatabase(t *testing.T) {
	s, _ := backupServer(t)
	files, err := s.collectBackup(false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"init.yaml", "crontab", "temp/cf_auto.json", "temp/rutor_taskParse.json", backupManifest} {
		if _, ok := files[want]; !ok {
			t.Errorf("в архиве нет %s", want)
		}
	}
	// stats.json is derived and must not travel: a restored stale copy would
	// misreport until the next refresh.
	if _, ok := files["temp/stats.json"]; ok {
		t.Error("stats.json попал в архив, хотя он производный")
	}
	for name := range files {
		if strings.Contains(name, "fdb") || strings.Contains(name, "masterDb") {
			t.Errorf("данные БД попали в архив настроек: %s", name)
		}
	}
}

func TestDefaultBackupCarriesNoCredentials(t *testing.T) {
	s, _ := backupServer(t)
	files, err := s.collectBackup(false)
	if err != nil {
		t.Fatal(err)
	}
	blob := string(files["init.yaml"])
	for _, secret := range []string{"live-user", "live-pass", "live_cookie", "live-devkey", "live-apikey"} {
		if strings.Contains(blob, secret) {
			t.Errorf("init.yaml в архиве содержит секрет %q", secret)
		}
	}
	if _, ok := files["cookie/rutracker.org.json"]; ok {
		t.Error("сессии попали в архив без запроса credentials")
	}
	var man BackupManifest
	if err := json.Unmarshal(files[backupManifest], &man); err != nil {
		t.Fatal(err)
	}
	if man.Credentials {
		t.Error("манифест утверждает, что credentials есть, хотя их нет")
	}
}

func TestBackupWithCredentialsCarriesThem(t *testing.T) {
	s, _ := backupServer(t)
	files, err := s.collectBackup(true)
	if err != nil {
		t.Fatal(err)
	}
	blob := string(files["init.yaml"])
	for _, secret := range []string{"live-user", "live-pass", "live-devkey"} {
		if !strings.Contains(blob, secret) {
			t.Errorf("запрошен полный архив, но %q в init.yaml нет", secret)
		}
	}
	if _, ok := files["cookie/rutracker.org.json"]; !ok {
		t.Error("запрошен полный архив, но сессий в нём нет")
	}
}

// Every TrackerSettings field on the config must be stripped. This walks the
// fields independently of the implementation, so adding a 26th tracker cannot
// quietly leave its password in an export.
func TestEveryTrackerIsStrippedOfCredentials(t *testing.T) {
	cfg := app.DefaultConfig()
	v := reflect.ValueOf(&cfg).Elem()
	tsType := reflect.TypeOf(app.TrackerSettings{})
	seeded := 0
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if f.Type() != tsType || !f.CanSet() {
			continue
		}
		ts := f.Interface().(app.TrackerSettings)
		ts.Cookie = "secret_cookie=1"
		ts.Login = app.LoginSettings{U: "u", P: "p"}
		f.Set(reflect.ValueOf(ts))
		seeded++
	}
	if seeded < 20 {
		t.Fatalf("ожидалось больше трекеров в конфиге, найдено %d", seeded)
	}

	stripped := stripConfigCredentials(cfg)
	sv := reflect.ValueOf(stripped)
	st := sv.Type()
	for i := 0; i < sv.NumField(); i++ {
		if sv.Field(i).Type() != tsType {
			continue
		}
		ts := sv.Field(i).Interface().(app.TrackerSettings)
		if ts.Cookie != "" || ts.Login.U != "" || ts.Login.P != "" {
			t.Errorf("%s сохранил credentials после очистки", st.Field(i).Name)
		}
	}
}

// A credential-free backup restored onto a working box must not log every
// tracker out. This is the difference between a useful restore and one that
// quietly breaks 25 parsers.
func TestRestoringACredentialFreeBackupKeepsLiveCredentials(t *testing.T) {
	live := app.DefaultConfig()
	live.Rutor.Login = app.LoginSettings{U: "live-user", P: "live-pass"}
	live.Rutor.Cookie = "live_cookie=abc"
	live.DevKey = "live-devkey"

	incoming := stripConfigCredentials(live)
	incoming.Rutor.Host = "https://rutor.new" // a real setting change travels

	merged := mergeConfigCredentials(incoming, live)
	if merged.Rutor.Login.U != "live-user" || merged.Rutor.Login.P != "live-pass" {
		t.Errorf("логин потерян при восстановлении: %+v", merged.Rutor.Login)
	}
	if merged.Rutor.Cookie != "live_cookie=abc" {
		t.Errorf("cookie потеряна при восстановлении: %q", merged.Rutor.Cookie)
	}
	if merged.DevKey != "live-devkey" {
		t.Errorf("devkey потерян: %q", merged.DevKey)
	}
	if merged.Rutor.Host != "https://rutor.new" {
		t.Errorf("настройка из архива не применилась: %q", merged.Rutor.Host)
	}
}

// An archive that does carry credentials is the authority for them — that is
// the whole point of exporting with them.
func TestArchiveCredentialsWinOverLiveOnes(t *testing.T) {
	live := app.DefaultConfig()
	live.Rutor.Login = app.LoginSettings{U: "old", P: "old"}

	incoming := app.DefaultConfig()
	incoming.Rutor.Login = app.LoginSettings{U: "new", P: "new"}

	merged := mergeConfigCredentials(incoming, live)
	if merged.Rutor.Login.U != "new" || merged.Rutor.Login.P != "new" {
		t.Errorf("credentials из архива не победили: %+v", merged.Rutor.Login)
	}
}

// The archive is attacker-supplied once it is uploaded. Names are whitelisted,
// never cleaned and reused.
func TestRestoreRefusesPathsOutsideTheBackup(t *testing.T) {
	hostile := []string{
		"../../etc/passwd",
		"/etc/passwd",
		"temp/../../../root/.ssh/authorized_keys",
		"cookie/../../init.yaml",
		"Data/fdb/2f/15f2",
		"temp/evil.sh",
		"temp/nested/dir/cf_auto.json",
		"build_all.sh",
	}
	for _, name := range hostile {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		body := []byte("pwned")
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		tw.Write(body)
		tw.Close()
		gz.Close()

		files, skipped, err := readBackupArchive(bytes.NewReader(buf.Bytes()))
		if err == nil {
			t.Errorf("%q принят вместо отказа (files=%v)", name, files)
			continue
		}
		if len(skipped) == 0 {
			t.Errorf("%q отклонён, но не отмечен как пропущенный", name)
		}
	}
}

func TestRestoreRefusesAnArchiveOfAnotherKind(t *testing.T) {
	s, _ := backupServer(t)
	files, err := s.collectBackup(false)
	if err != nil {
		t.Fatal(err)
	}
	files[backupManifest] = []byte(`{"kind":"something-else"}`)
	var buf bytes.Buffer
	if err := writeBackupArchive(&buf, files); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/backup/restore", bytes.NewReader(buf.Bytes()))
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	s.handleAdminBackupRestore(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("ожидался 400 на чужой архив, получен %d: %s", rec.Code, rec.Body.String())
	}
}

// A crontab that schedules nothing must be refused rather than written.
//
// Note what is actually dangerous here, because it is not the shell: a line the
// parser does not recognise is kept as prose and never executed, so this
// archive could be written safely — it would simply leave the schedule empty
// and nothing would say so. That is the failure being guarded.
func TestRestoreRefusesAnUnusableCrontab(t *testing.T) {
	s, dir := backupServer(t)
	before, err := os.ReadFile(s.schedulerFilePath())
	if err != nil {
		t.Fatal(err)
	}

	files := map[string][]byte{
		backupManifest: []byte(`{"kind":"` + backupKind + `"}`),
		"crontab":      []byte("* * * * * root /bin/sh -c 'rm -rf /'\n"),
	}
	var buf bytes.Buffer
	if err := writeBackupArchive(&buf, files); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/backup/restore", bytes.NewReader(buf.Bytes()))
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	s.handleAdminBackupRestore(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("негодный crontab принят: %s", rec.Body.String())
	}
	after, err := os.ReadFile(filepath.Join(dir, "crontab"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("crontab изменён, хотя архив был отклонён")
	}
}

// Full round trip through the two HTTP handlers.
func TestBackupRoundTripsThroughTheEndpoints(t *testing.T) {
	s, dir := backupServer(t)

	// init.yaml has to exist for the reload step after a restore.
	if err := os.WriteFile("init.yaml", []byte(app.MarshalYAML(s.GetConfig())), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/backup?credentials=1", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	s.handleAdminBackup(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("экспорт вернул %d: %s", rec.Code, rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("архив не отдан как файл: %q", cd)
	}
	archive := rec.Body.Bytes()
	got := unpack(t, archive)
	if _, ok := got["crontab"]; !ok {
		t.Fatal("в скачанном архиве нет crontab")
	}

	// Damage the live files, then restore.
	if err := os.WriteFile(s.schedulerFilePath(), []byte("# стёрто\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "Data", "temp", "cf_auto.json")); err != nil {
		t.Fatal(err)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/admin/backup/restore", bytes.NewReader(archive))
	req2.RemoteAddr = "127.0.0.1:1234"
	rec2 := httptest.NewRecorder()
	s.handleAdminBackupRestore(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("восстановление вернуло %d: %s", rec2.Code, rec2.Body.String())
	}

	var report BackupRestoreReport
	if err := json.Unmarshal(rec2.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		t.Fatalf("восстановление с ошибками: %+v", report.Errors)
	}

	cron, err := os.ReadFile(s.schedulerFilePath())
	if err != nil {
		t.Fatal(err)
	}
	// The prose in the crontab is the point: it records measured timings, and a
	// backup that dropped it would lose the reasoning behind the schedule.
	if !strings.Contains(string(cron), "измеренный тайминг") {
		t.Errorf("комментарии crontab не восстановлены: %q", cron)
	}
	if _, err := os.Stat(filepath.Join(dir, "Data", "temp", "cf_auto.json")); err != nil {
		t.Errorf("cf_auto.json не восстановлен: %v", err)
	}
	// The replaced file is kept for inspection.
	if _, err := os.Stat(s.schedulerFilePath() + ".before-restore"); err != nil {
		t.Errorf("предыдущий crontab не сохранён: %v", err)
	}
}

func TestBackupEndpointsAreLocalOnly(t *testing.T) {
	s, _ := backupServer(t)
	for _, tc := range []struct {
		name string
		call func(http.ResponseWriter, *http.Request)
		verb string
	}{
		{"export", s.handleAdminBackup, http.MethodGet},
		{"restore", s.handleAdminBackupRestore, http.MethodPost},
	} {
		req := httptest.NewRequest(tc.verb, "/admin/backup", strings.NewReader(""))
		req.RemoteAddr = "8.8.8.8:1234"
		rec := httptest.NewRecorder()
		tc.call(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: внешний адрес получил %d вместо 403", tc.name, rec.Code)
		}
	}
	// And the middleware agrees, so the routes cannot be reached another way.
	for _, p := range []string{"/admin/backup", "/admin/backup/restore"} {
		if !isLocalOnlyPath(p) {
			t.Errorf("%s не считается local-only", p)
		}
	}
}
