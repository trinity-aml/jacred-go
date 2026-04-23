package core

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Persistence for flare sessions across restarts. Files are
// Data/temp/flare/<domain>.json — one per CF-gated domain. cf_clearance
// typically lives 30–120 minutes, so after a quick restart (systemd restart,
// upgrade) we can reuse the existing cookie instead of spawning a fresh Chrome
// solve for every tracker on first request.

var (
	flarePersistMu  sync.Mutex
	flarePersistDir string
)

type persistedFlareSession struct {
	Cookies   string `json:"cookies"`
	UserAgent string `json:"userAgent"`
	Obtained  int64  `json:"obtained"` // unix seconds
}

// SetFlarePersistDir enables on-disk caching of solved CF sessions. Pass an
// absolute or CWD-relative directory; it will be created if missing. Pass ""
// to disable persistence.
func SetFlarePersistDir(dir string) {
	flarePersistMu.Lock()
	defer flarePersistMu.Unlock()
	flarePersistDir = dir
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("flare-persist: mkdir %s: %v", dir, err)
	}
}

func flarePersistPath(domain string) string {
	flarePersistMu.Lock()
	dir := flarePersistDir
	flarePersistMu.Unlock()
	if dir == "" {
		return ""
	}
	// hostnames don't contain path separators, but be defensive
	safe := strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(domain)
	return filepath.Join(dir, safe+".json")
}

func saveFlareSession(domain string, sess *flareSession) {
	path := flarePersistPath(domain)
	if path == "" || sess == nil {
		return
	}
	data, err := json.Marshal(persistedFlareSession{
		Cookies:   sess.cookies,
		UserAgent: sess.userAgent,
		Obtained:  sess.obtained.Unix(),
	})
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("flare-persist: write %s: %v", path, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("flare-persist: rename %s: %v", path, err)
		_ = os.Remove(tmp)
	}
}

func deleteFlareSession(domain string) {
	path := flarePersistPath(domain)
	if path == "" {
		return
	}
	_ = os.Remove(path)
}

// loadFlareSessions reads all persisted sessions and returns the ones that
// are still within ttl. Expired files are deleted on the way out so the
// directory doesn't grow forever.
func loadFlareSessions(ttl time.Duration) map[string]*flareSession {
	flarePersistMu.Lock()
	dir := flarePersistDir
	flarePersistMu.Unlock()
	out := make(map[string]*flareSession)
	if dir == "" {
		return out
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	now := time.Now()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var p persistedFlareSession
		if err := json.Unmarshal(data, &p); err != nil {
			_ = os.Remove(path)
			continue
		}
		obtained := time.Unix(p.Obtained, 0)
		if now.Sub(obtained) > ttl || p.Cookies == "" {
			_ = os.Remove(path)
			continue
		}
		domain := strings.TrimSuffix(e.Name(), ".json")
		out[domain] = &flareSession{
			cookies:   p.Cookies,
			userAgent: p.UserAgent,
			obtained:  obtained,
		}
	}
	return out
}
