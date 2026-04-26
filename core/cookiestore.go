package core

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// CookieStore persists per-tracker auth cookies to Data/cookie/<name>.txt so
// that Set-Cookie returned by login flows survives a process restart. Cookies
// are written atomically (via temp file + rename) and access is serialized by
// a per-store mutex.
type CookieStore struct {
	dir string
	mu  sync.Mutex
}

func NewCookieStore(dataDir string) *CookieStore {
	if dataDir == "" {
		dataDir = "Data"
	}
	return &CookieStore{dir: filepath.Join(dataDir, "cookie")}
}

func (s *CookieStore) path(name string) string {
	return filepath.Join(s.dir, name+".txt")
}

// Load returns the previously saved cookie for the given tracker name, or "" if
// none exists.
func (s *CookieStore) Load(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path(name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Save writes the cookie atomically. Empty input deletes the stored cookie.
func (s *CookieStore) Save(name, cookie string) error {
	cookie = strings.TrimSpace(cookie)
	if cookie == "" {
		return s.Delete(name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	dst := s.path(name)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, []byte(cookie), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// Delete removes the stored cookie file. Missing file is not an error.
func (s *CookieStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path(name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
