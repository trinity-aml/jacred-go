package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jacred/app"
)

// The crontab is mostly prose explaining why each tracker is polled the way it
// is. An editor that rewrote the file from its jobs would lose that on the
// first save, so the round-trip through the HTTP API has to be exact.
const sampleCrontab = `# Свежие релизы
*/5  *   *   *   *    curl -s "http://127.0.0.1:9117/cron/rutor/parse"

# Ночной прогон — дорогой, поэтому раз в сутки
30   4   *   *   *    curl -s "http://127.0.0.1:9117/cron/rudub/parse?limit_page=20"
# */15 *  *   *   *    curl -s "http://127.0.0.1:9117/cron/selezen/parse"
`

func schedulerServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "crontab")
	if err := os.WriteFile(path, []byte(sampleCrontab), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Server{Config: app.Config{SchedulerFile: path}}, path
}

func schedulerCall(t *testing.T, s *Server, method, body string) (int, map[string]any) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/admin/scheduler", nil)
	} else {
		r = httptest.NewRequest(method, "/admin/scheduler", strings.NewReader(body))
	}
	r.RemoteAddr = "127.0.0.1:5555"
	w := httptest.NewRecorder()
	s.handleAdminScheduler(w, r)

	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON (%d): %s", w.Code, w.Body.String())
	}
	return w.Code, out
}

func TestSchedulerAPIReadsTheFile(t *testing.T) {
	s, path := schedulerServer(t)
	code, out := schedulerCall(t, s, http.MethodGet, "")
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("GET = %d %v", code, out)
	}
	if out["path"] != path {
		t.Errorf("path = %v", out["path"])
	}
	entries, _ := out["entries"].([]any)
	if len(entries) != 6 {
		t.Fatalf("got %d entries, want 6 (the file has 6 lines)", len(entries))
	}

	var jobs, texts, disabled int
	for _, e := range entries {
		m := e.(map[string]any)
		if m["kind"] == "job" {
			jobs++
			if m["disabled"] == true {
				disabled++
			}
		} else {
			texts++
		}
	}
	if jobs != 3 || texts != 3 || disabled != 1 {
		t.Errorf("jobs=%d texts=%d disabled=%d, want 3/3/1", jobs, texts, disabled)
	}

	// An enabled job gets a next-run time; a disabled one does not, because
	// showing one would imply it is going to fire.
	for _, e := range entries {
		m := e.(map[string]any)
		if m["kind"] != "job" {
			continue
		}
		_, has := m["nextRun"]
		if m["disabled"] == true && has {
			t.Errorf("a disabled job carries a next-run time: %v", m)
		}
		if m["disabled"] != true && !has {
			t.Errorf("an enabled job has no next-run time: %v", m)
		}
	}
}

// Saving what was just read must not touch the file.
func TestSchedulerAPINoOpSavePreservesTheFile(t *testing.T) {
	s, path := schedulerServer(t)
	_, out := schedulerCall(t, s, http.MethodGet, "")
	body, err := json.Marshal(map[string]any{"entries": out["entries"]})
	if err != nil {
		t.Fatal(err)
	}
	code, res := schedulerCall(t, s, http.MethodPost, string(body))
	if code != http.StatusOK || res["ok"] != true {
		t.Fatalf("POST = %d %v", code, res)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != sampleCrontab {
		t.Errorf("a no-op save rewrote the file:\n--- got ---\n%s\n--- want ---\n%s", got, sampleCrontab)
	}
}

// Toggling a job writes exactly one character sequence — the comment marker —
// and leaves every other line alone.
func TestSchedulerAPIToggleChangesOneLine(t *testing.T) {
	s, path := schedulerServer(t)
	_, out := schedulerCall(t, s, http.MethodGet, "")
	entries := out["entries"].([]any)
	for _, e := range entries {
		m := e.(map[string]any)
		if m["kind"] == "job" && strings.Contains(m["url"].(string), "rutor") {
			m["disabled"] = true
		}
	}
	body, _ := json.Marshal(map[string]any{"entries": entries})
	if code, res := schedulerCall(t, s, http.MethodPost, string(body)); code != http.StatusOK {
		t.Fatalf("POST = %d %v", code, res)
	}

	got, _ := os.ReadFile(path)
	gl, wl := strings.Split(string(got), "\n"), strings.Split(sampleCrontab, "\n")
	diffs := 0
	for i := range wl {
		if i < len(gl) && gl[i] != wl[i] {
			diffs++
			if !strings.HasPrefix(strings.TrimSpace(gl[i]), "#") {
				t.Errorf("line %d changed but is not commented out: %q", i+1, gl[i])
			}
		}
	}
	if diffs != 1 {
		t.Errorf("%d lines changed, want 1", diffs)
	}
}

// The scheduler skips a line it cannot parse, so accepting a typo would
// silently stop that job forever. The file must be left alone on a refusal.
func TestSchedulerAPIRefusesBadInput(t *testing.T) {
	for _, c := range []struct {
		name string
		body string
	}{
		{"bad schedule", `{"entries":[{"kind":"job","spec":"99 * * * *","url":"http://127.0.0.1:9117/health"}]}`},
		{"empty schedule", `{"entries":[]}`},
		{"url with a quote", `{"entries":[{"kind":"job","spec":"* * * * *","url":"http://x/\""}]}`},
		{"not a flag", `{"entries":[{"kind":"job","spec":"* * * * *","url":"http://x/","flags":"rm"}]}`},
		{"broken json", `{"entries":`},
	} {
		s, path := schedulerServer(t)
		code, out := schedulerCall(t, s, http.MethodPost, c.body)
		if code == http.StatusOK {
			t.Errorf("%s was accepted", c.name)
		}
		if out["error"] == nil {
			t.Errorf("%s produced no error message", c.name)
		}
		got, _ := os.ReadFile(path)
		if string(got) != sampleCrontab {
			t.Errorf("%s modified the file despite being refused", c.name)
		}
	}
}

// The editor is configuration, and the config endpoints are local-only.
func TestSchedulerAPIIsLocalOnly(t *testing.T) {
	s, _ := schedulerServer(t)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		r := httptest.NewRequest(method, "/admin/scheduler", strings.NewReader(`{"entries":[]}`))
		r.RemoteAddr = "203.0.113.7:5555"
		w := httptest.NewRecorder()
		s.handleAdminScheduler(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s from a public address = %d, want 403", method, w.Code)
		}
	}
}

// With no scheduler running the editor still works — that is the point, since
// the file has to be set up before the scheduler is turned on.
func TestSchedulerAPIWorksWhileDisabled(t *testing.T) {
	s, _ := schedulerServer(t)
	_, out := schedulerCall(t, s, http.MethodGet, "")
	if out["enabled"] != false {
		t.Errorf("enabled = %v, want false", out["enabled"])
	}
	if len(out["entries"].([]any)) == 0 {
		t.Error("no entries returned while the scheduler is off")
	}
}

// A missing file is reported rather than shown as an empty schedule, which
// would invite a save that overwrites nothing with nothing.
func TestSchedulerAPIReportsAMissingFile(t *testing.T) {
	s := &Server{Config: app.Config{SchedulerFile: filepath.Join(t.TempDir(), "nope")}}
	code, out := schedulerCall(t, s, http.MethodGet, "")
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	if out["ok"] != false || out["error"] == nil {
		t.Errorf("a missing file was not reported: %v", out)
	}
}
