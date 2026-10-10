package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"jacred/app"
	"jacred/background"
)

// Settings and state backup — the half of this installation that nothing can
// re-derive.
//
// `Data/fdb` has two layers of protection already (atomic bucket writes, daily
// hard-link snapshots) and, failing both, the parsers put the records back: a
// cold sweep is slow but it is automatic. Nothing puts `init.yaml` back. The
// 25 hosts, the hand-tuned `parseDelay` values, the crontab's measured timings
// in its comments, the CF registry, the sweep cursors — that is kilobytes, and
// it is the half with no recovery path at all. So this exports it, and
// deliberately does not export the ~7 GB beside it: at a measured 4.1x
// expansion the records are not something to move through a browser, and they
// already have somewhere to come back from.
//
// Credentials are **opt-in**. `init.yaml` holds tracker passwords in clear
// text and `Data/cookie/` holds live sessions — a `cf_clearance` is a
// replayable credential, which is why this codebase refuses to log one. An
// archive sitting in a downloads folder is a worse place for them than a log,
// so the default export carries none and says so in its manifest.

const (
	// backupMaxUpload bounds a restore. The real archive is kilobytes; this is
	// slack for a large crontab and many task maps, not a target.
	backupMaxUpload = 16 << 20
	// backupManifest records what the archive is, so a restore can refuse one
	// it does not understand instead of scattering files.
	backupManifest = "MANIFEST.json"
)

// BackupManifest describes an archive. `Credentials` is the load-bearing field
// on restore: an archive exported without them must not wipe the live ones.
type BackupManifest struct {
	Kind        string    `json:"kind"`
	Version     string    `json:"version"`
	GitSha      string    `json:"gitSha,omitempty"`
	Created     time.Time `json:"created"`
	Credentials bool      `json:"credentials"`
	Files       []string  `json:"files"`
}

const backupKind = "jacred-settings"

// backupStatePatterns are the state files worth carrying. Each is matched
// against the base name inside Data/temp.
//
// stats.json is deliberately absent — it is derived from the database and is
// rebuilt on a timer, so restoring a stale copy would only mislead the stats
// page until the next refresh.
var backupStatePatterns = []string{
	"cf_auto.json",
	"tracker_runs.json",
	"subsplease_shows.json",
	"*_taskParse.json",
	"*_parseAllCycle.json",
}

func backupStateWanted(name string) bool {
	for _, pat := range backupStatePatterns {
		if ok, _ := path.Match(pat, name); ok {
			return true
		}
	}
	return false
}

// stripConfigCredentials blanks every secret-bearing field on a copy of the
// config.
//
// The per-tracker half goes through reflection on purpose. `app.Config` carries
// one `TrackerSettings` field per tracker, and this codebase has already lost a
// tracker to a hand-maintained roster that nothing checked — a 26th list here
// would drift the same way. Reflection means a new tracker is covered by
// existing, and `TestEveryTrackerIsStrippedOfCredentials` proves it by walking
// the same fields independently.
func stripConfigCredentials(cfg app.Config) app.Config {
	cfg.APIKey = ""
	cfg.DevKey = ""
	for i := range cfg.GlobalProxy {
		cfg.GlobalProxy[i].Username = ""
		cfg.GlobalProxy[i].Password = ""
	}
	v := reflect.ValueOf(&cfg).Elem()
	tsType := reflect.TypeOf(app.TrackerSettings{})
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if f.Type() != tsType || !f.CanSet() {
			continue
		}
		ts := f.Interface().(app.TrackerSettings)
		ts.Cookie = ""
		ts.Login = app.LoginSettings{}
		f.Set(reflect.ValueOf(ts))
	}
	return cfg
}

// mergeConfigCredentials puts `live`'s secrets back into `in` wherever `in` has
// none.
//
// This is what makes a credential-free backup safe to restore onto a working
// box: without it, restoring yesterday's settings would silently log every
// tracker out and the next run would report a wall of `work_login`. A value
// present in the archive always wins — that is the point of exporting with
// credentials.
func mergeConfigCredentials(in, live app.Config) app.Config {
	if in.APIKey == "" {
		in.APIKey = live.APIKey
	}
	if in.DevKey == "" {
		in.DevKey = live.DevKey
	}
	// Proxies are positional and the list may have been edited; only fill in
	// when the shape still matches, rather than guessing which entry is which.
	if len(in.GlobalProxy) == len(live.GlobalProxy) {
		for i := range in.GlobalProxy {
			if in.GlobalProxy[i].Username == "" {
				in.GlobalProxy[i].Username = live.GlobalProxy[i].Username
			}
			if in.GlobalProxy[i].Password == "" {
				in.GlobalProxy[i].Password = live.GlobalProxy[i].Password
			}
		}
	}
	inV := reflect.ValueOf(&in).Elem()
	liveV := reflect.ValueOf(live)
	tsType := reflect.TypeOf(app.TrackerSettings{})
	for i := 0; i < inV.NumField(); i++ {
		f := inV.Field(i)
		if f.Type() != tsType || !f.CanSet() {
			continue
		}
		cur := f.Interface().(app.TrackerSettings)
		old := liveV.Field(i).Interface().(app.TrackerSettings)
		if cur.Cookie == "" {
			cur.Cookie = old.Cookie
		}
		if cur.Login.U == "" && cur.Login.P == "" {
			cur.Login = old.Login
		}
		f.Set(reflect.ValueOf(cur))
	}
	return in
}

// collectBackup gathers the archive's members in memory. Everything here is
// kilobytes, so building it up front keeps the HTTP path simple and lets a
// failure be reported before a single byte of the response is written.
func (s *Server) collectBackup(withCredentials bool) (map[string][]byte, error) {
	files := map[string][]byte{}

	cfg := s.GetConfig()
	if !withCredentials {
		cfg = stripConfigCredentials(cfg)
	}
	files["init.yaml"] = []byte(app.MarshalYAML(cfg))

	if b, err := os.ReadFile(s.schedulerFilePath()); err == nil {
		files["crontab"] = b
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("crontab: %w", err)
	}

	dataDir := s.dataDir()
	tempDir := filepath.Join(dataDir, "temp")
	entries, err := os.ReadDir(tempDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("%s: %w", tempDir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !backupStateWanted(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(tempDir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		files["temp/"+e.Name()] = b
	}

	if withCredentials {
		cookieDir := filepath.Join(dataDir, "cookie")
		cEntries, err := os.ReadDir(cookieDir)
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("%s: %w", cookieDir, err)
		}
		for _, e := range cEntries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(cookieDir, e.Name()))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", e.Name(), err)
			}
			files["cookie/"+e.Name()] = b
		}
	}

	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)

	man := BackupManifest{
		Kind:        backupKind,
		Version:     s.Version.Version,
		GitSha:      s.Version.GitSha,
		Created:     time.Now().UTC(),
		Credentials: withCredentials,
		Files:       names,
	}
	mb, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return nil, err
	}
	files[backupManifest] = mb
	return files, nil
}

// dataDir is where Data lives. The DB owns the real value; this keeps the
// fallback in one place for tests that construct a Server without one.
func (s *Server) dataDir() string {
	if s.DB != nil && strings.TrimSpace(s.DB.DataDir) != "" {
		return s.DB.DataDir
	}
	return "Data"
}

func writeBackupArchive(w io.Writer, files map[string][]byte) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	now := time.Now()
	for _, n := range names {
		body := files[n]
		if err := tw.WriteHeader(&tar.Header{
			Name:    n,
			Mode:    0o600,
			Size:    int64(len(body)),
			ModTime: now,
		}); err != nil {
			return err
		}
		if _, err := tw.Write(body); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// handleAdminBackup streams a settings archive.
//
//	GET /admin/backup              — settings and state, no credentials
//	GET /admin/backup?credentials=1 — including passwords and live sessions
//
// Local-only, like /admin/config.
func (s *Server) handleAdminBackup(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"badip": true})
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "GET only"})
		return
	}
	withCreds := parseBool(r.URL.Query().Get("credentials"))
	files, err := s.collectBackup(withCreds)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	name := fmt.Sprintf("jacred-settings-%s.tar.gz", time.Now().Format("02-01-2006"))
	if withCreds {
		name = fmt.Sprintf("jacred-settings-secrets-%s.tar.gz", time.Now().Format("02-01-2006"))
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	if err := writeBackupArchive(w, files); err != nil {
		// The status is already sent, so this can only be logged.
		log.Printf("backup: writing the archive failed: %v", err)
	}
}

// readBackupArchive unpacks an uploaded archive into memory, accepting only the
// members a backup can legitimately contain.
//
// Nothing from the archive is ever used to build an output path. Names are
// matched against a whitelist and mapped to a destination this code chooses, so
// a crafted `../../etc/…` entry is rejected rather than sanitised — the usual
// cleaning approach has a long history of being outsmarted, and there is no
// reason to accept a name we did not expect in the first place.
func readBackupArchive(r io.Reader) (map[string][]byte, []string, error) {
	gz, err := gzip.NewReader(io.LimitReader(r, backupMaxUpload))
	if err != nil {
		return nil, nil, fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	var skipped []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("not a readable tar archive: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			skipped = append(skipped, h.Name+" (not a regular file)")
			continue
		}
		name := path.Clean(h.Name)
		if !backupMemberAllowed(name) {
			skipped = append(skipped, h.Name)
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, backupMaxUpload))
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", h.Name, err)
		}
		files[name] = b
	}
	if len(files) == 0 {
		return nil, skipped, fmt.Errorf("the archive holds nothing this server recognises")
	}
	return files, skipped, nil
}

// backupMemberAllowed is the whitelist. A name has to be exactly one of the
// forms this server writes.
func backupMemberAllowed(name string) bool {
	switch name {
	case backupManifest, "init.yaml", "crontab":
		return true
	}
	dir, base := path.Split(name)
	if strings.Contains(base, "/") || base == "" || base == "." || base == ".." {
		return false
	}
	switch dir {
	case "temp/":
		return backupStateWanted(base)
	case "cookie/":
		return strings.HasSuffix(base, ".json")
	}
	return false
}

// BackupRestoreReport says what a restore did, per file, so a partially
// applicable archive is legible rather than silently half-applied.
type BackupRestoreReport struct {
	OK          bool            `json:"ok"`
	Manifest    *BackupManifest `json:"manifest,omitempty"`
	Restored    []string        `json:"restored"`
	Skipped     []string        `json:"skipped,omitempty"`
	Errors      []string        `json:"errors,omitempty"`
	Credentials string          `json:"credentials"`
}

// handleAdminBackupRestore applies an uploaded settings archive.
//
// Everything is validated before anything is written: `init.yaml` has to load
// through the same parser startup uses, and the crontab through the same one
// the scheduler uses — saving a file the scheduler would skip is how a job
// stops running forever without anyone noticing. Writes are temp+rename, and
// the file being replaced is kept beside it as `<name>.before-restore`.
func (s *Server) handleAdminBackupRestore(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"badip": true})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "POST only"})
		return
	}
	defer r.Body.Close()

	body, err := readUploadedArchive(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	files, skipped, err := readBackupArchive(bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error(), "skipped": skipped})
		return
	}

	report := BackupRestoreReport{Skipped: skipped, Credentials: "kept existing"}

	if mb, ok := files[backupManifest]; ok {
		var man BackupManifest
		if err := json.Unmarshal(mb, &man); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unreadable " + backupManifest + ": " + err.Error()})
			return
		}
		if man.Kind != backupKind {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok": false, "error": fmt.Sprintf("this is a %q archive, not %q", man.Kind, backupKind),
			})
			return
		}
		report.Manifest = &man
		if man.Credentials {
			report.Credentials = "restored from the archive"
		}
	}

	// Validate first, write second.
	var newCfg *app.Config
	if raw, ok := files["init.yaml"]; ok {
		cfg, err := loadConfigFromBytes(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "init.yaml in the archive is not loadable: " + err.Error()})
			return
		}
		merged := mergeConfigCredentials(cfg, s.GetConfig())
		newCfg = &merged
	}
	if raw, ok := files["crontab"]; ok {
		entries, _ := background.ParseCrontabEntries(bytes.NewReader(raw))
		if errs := background.ValidateCrontabEntries(entries); len(errs) > 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok": false, "error": "crontab in the archive is not usable", "problems": errs,
			})
			return
		}
		// A restore replaces the whole file at once, so "at least one job
		// survived parsing" is the check that matters here — stricter than the
		// per-rule editor's "not empty", and for a different reason. A line the
		// parser does not recognise is kept as *prose* rather than rejected
		// (that is what preserves the file's measured-timing comments), so a
		// crontab whose job lines were all mistyped parses cleanly, writes
		// cleanly, and schedules nothing. Nothing would then run and nothing
		// would say so.
		jobs := 0
		for _, e := range entries {
			if e.Kind == "job" {
				jobs++
			}
		}
		if jobs == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok": false,
				"error": "the crontab in the archive defines no runnable job; " +
					"restoring it would leave nothing scheduled",
			})
			return
		}
	}

	if newCfg != nil {
		if err := writeFileWithBackup("init.yaml", []byte(app.MarshalYAML(*newCfg))); err != nil {
			report.Errors = append(report.Errors, "init.yaml: "+err.Error())
		} else {
			report.Restored = append(report.Restored, "init.yaml")
		}
	}
	if raw, ok := files["crontab"]; ok {
		p := s.schedulerFilePath()
		if err := writeFileWithBackup(p, raw); err != nil {
			report.Errors = append(report.Errors, "crontab: "+err.Error())
		} else {
			report.Restored = append(report.Restored, "crontab")
		}
	}

	dataDir := s.dataDir()
	for name, raw := range files {
		dir, base := path.Split(name)
		var dest string
		switch dir {
		case "temp/":
			dest = filepath.Join(dataDir, "temp", base)
		case "cookie/":
			dest = filepath.Join(dataDir, "cookie", base)
		default:
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			report.Errors = append(report.Errors, name+": "+err.Error())
			continue
		}
		if err := writeFileWithBackup(dest, raw); err != nil {
			report.Errors = append(report.Errors, name+": "+err.Error())
			continue
		}
		report.Restored = append(report.Restored, name)
	}

	sort.Strings(report.Restored)

	// Reload through the startup parser so the running process matches the
	// file, exactly as a save from /settings does.
	if newCfg != nil {
		reloaded, err := app.LoadConfig("init.yaml")
		if err != nil {
			report.Errors = append(report.Errors, "written, but reload failed: "+err.Error())
		} else {
			s.UpdateConfig(reloaded)
		}
	}

	report.OK = len(report.Errors) == 0
	status := http.StatusOK
	if !report.OK {
		status = http.StatusInternalServerError
	}
	log.Printf("backup: restored %d file(s) from an archive, %d error(s), credentials %s",
		len(report.Restored), len(report.Errors), report.Credentials)
	writeJSON(w, status, report)
}

// readUploadedArchive accepts the archive either as a raw body or as the only
// part of a multipart form, so the page can post a file input directly and curl
// can pipe bytes.
func readUploadedArchive(r *http.Request) ([]byte, error) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(backupMaxUpload); err != nil {
			return nil, fmt.Errorf("unreadable upload: %w", err)
		}
		if r.MultipartForm != nil {
			defer r.MultipartForm.RemoveAll()
		}
		for _, hs := range r.MultipartForm.File {
			for _, h := range hs {
				f, err := h.Open()
				if err != nil {
					return nil, err
				}
				defer f.Close()
				return io.ReadAll(io.LimitReader(f, backupMaxUpload))
			}
		}
		return nil, fmt.Errorf("the upload carried no file")
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, backupMaxUpload))
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("the request carried no archive")
	}
	return b, nil
}

// loadConfigFromBytes validates a config by loading it the way startup does.
// There is no parser that works on bytes, and adding a second one is how two
// parsers drift apart, so this writes a temp file and uses the real loader.
func loadConfigFromBytes(raw []byte) (app.Config, error) {
	f, err := os.CreateTemp("", "jacred-init-*.yaml")
	if err != nil {
		return app.Config{}, err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return app.Config{}, err
	}
	if err := f.Close(); err != nil {
		return app.Config{}, err
	}
	return app.LoadConfig(name)
}

// writeFileWithBackup replaces a file atomically, keeping the previous content
// beside it. Temp+rename is the rule everywhere in this codebase for the same
// reason: a crash mid-write must leave the old file or the new one, never half
// of either.
func writeFileWithBackup(dest string, body []byte) error {
	if _, err := os.Stat(dest); err == nil {
		prev, rerr := os.ReadFile(dest)
		if rerr == nil {
			if werr := writeFileAtomically(dest+".before-restore", prev); werr != nil {
				return fmt.Errorf("could not preserve the previous %s: %w", filepath.Base(dest), werr)
			}
		}
	}
	return writeFileAtomically(dest, body)
}

func writeFileAtomically(dest string, body []byte) error {
	dir := filepath.Dir(dest)
	f, err := os.CreateTemp(dir, filepath.Base(dest)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	fail := func(e error) error {
		f.Close()
		os.Remove(tmp)
		return e
	}
	if _, err := f.Write(body); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
