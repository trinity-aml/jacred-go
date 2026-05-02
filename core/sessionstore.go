package core

import (
	"encoding/json"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SessionStore unifies per-domain persistence for both auth cookies (set by
// tracker login flows) and flaresolverr/CF challenge sessions. One JSON file
// per domain at <dir>/<domain>.json holds both — earlier we wrote auth
// cookies to Data/cookie/<name>.txt and flare sessions to
// Data/temp/flare/<domain>.json separately, which fragmented session state
// across two locations and two key conventions (tracker name vs hostname).
//
// The on-disk format is intentionally simple JSON and tolerant of partial
// presence: a domain may have only an auth cookie, only a flare session, or
// both. Empty fields are omitted via omitempty so files stay small. Writes
// are atomic (temp + rename) and serialized by a per-store mutex.

type domainSession struct {
	Domain         string `json:"domain"`
	AuthCookie     string `json:"auth_cookie,omitempty"`
	AuthSavedAt    int64  `json:"auth_saved_at,omitempty"`
	FlareCookie    string `json:"flare_cookie,omitempty"`
	FlareUserAgent string `json:"flare_user_agent,omitempty"`
	FlareObtained  int64  `json:"flare_obtained,omitempty"`
}

type SessionStore struct {
	dir string
	mu  sync.Mutex
}

var (
	defaultSessionStoreMu sync.Mutex
	defaultSessionStore   *SessionStore
)

// SetSessionStoreDir installs the package-level default store. Pass an
// absolute or CWD-relative directory; it will be created if missing. Pass ""
// to disable persistence (loads return zero values, saves are no-ops).
func SetSessionStoreDir(dir string) {
	defaultSessionStoreMu.Lock()
	defer defaultSessionStoreMu.Unlock()
	if dir == "" {
		defaultSessionStore = nil
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("session-store: mkdir %s: %v", dir, err)
	}
	defaultSessionStore = &SessionStore{dir: dir}
}

// DefaultSessionStore returns the package singleton or nil if unset.
func DefaultSessionStore() *SessionStore {
	defaultSessionStoreMu.Lock()
	defer defaultSessionStoreMu.Unlock()
	return defaultSessionStore
}

// DomainFromHost strips scheme, path, and port from a host config value. For
// already-bare hostnames it just returns the input (lowercased).
func DomainFromHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	u, err := url.Parse(host)
	if err != nil || u.Hostname() == "" {
		return strings.ToLower(strings.Trim(host, "/"))
	}
	return strings.ToLower(u.Hostname())
}

func (s *SessionStore) path(domain string) string {
	safe := strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(domain)
	return filepath.Join(s.dir, safe+".json")
}

// loadDomainLocked reads the JSON file for the given domain. Caller must
// hold s.mu. Returns nil with no error when the file does not exist.
func (s *SessionStore) loadDomainLocked(domain string) (*domainSession, error) {
	b, err := os.ReadFile(s.path(domain))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var d domainSession
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	if d.Domain == "" {
		d.Domain = domain
	}
	return &d, nil
}

// saveDomainLocked writes (or removes if empty) the JSON file. Caller holds s.mu.
func (s *SessionStore) saveDomainLocked(d *domainSession) error {
	if d.AuthCookie == "" && d.FlareCookie == "" {
		_ = os.Remove(s.path(d.Domain))
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	dst := s.path(d.Domain)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// LoadAuth returns the previously saved auth cookie for the domain and the
// time it was last written. Empty cookie/zero time when nothing is stored.
func (s *SessionStore) LoadAuth(domain string) (string, time.Time) {
	if s == nil || domain == "" {
		return "", time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.loadDomainLocked(domain)
	if err != nil || d == nil {
		return "", time.Time{}
	}
	if d.AuthSavedAt == 0 {
		return d.AuthCookie, time.Time{}
	}
	return d.AuthCookie, time.Unix(d.AuthSavedAt, 0)
}

// SaveAuth merges the auth cookie into the per-domain file, preserving any
// existing flare session. Empty cookie clears the auth half but keeps flare.
func (s *SessionStore) SaveAuth(domain, cookie string) error {
	if s == nil || domain == "" {
		return nil
	}
	cookie = strings.TrimSpace(cookie)
	s.mu.Lock()
	defer s.mu.Unlock()
	d, _ := s.loadDomainLocked(domain)
	if d == nil {
		d = &domainSession{Domain: domain}
	}
	d.AuthCookie = cookie
	if cookie != "" {
		d.AuthSavedAt = time.Now().Unix()
	} else {
		d.AuthSavedAt = 0
	}
	return s.saveDomainLocked(d)
}

// DeleteAuth clears the auth cookie. Flare session, if any, is retained.
func (s *SessionStore) DeleteAuth(domain string) error {
	return s.SaveAuth(domain, "")
}

// loadFlareLocked / saveFlareLocked are internal helpers used by flare_persist.go
// to wire SessionStore into the existing fetcher cache code.

func (s *SessionStore) loadFlare(domain string) (*flareSession, bool) {
	if s == nil || domain == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.loadDomainLocked(domain)
	if err != nil || d == nil || d.FlareCookie == "" {
		return nil, false
	}
	return &flareSession{
		cookies:   d.FlareCookie,
		userAgent: d.FlareUserAgent,
		obtained:  time.Unix(d.FlareObtained, 0),
	}, true
}

func (s *SessionStore) saveFlare(domain string, sess *flareSession) {
	if s == nil || domain == "" || sess == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, _ := s.loadDomainLocked(domain)
	if d == nil {
		d = &domainSession{Domain: domain}
	}
	d.FlareCookie = sess.cookies
	d.FlareUserAgent = sess.userAgent
	d.FlareObtained = sess.obtained.Unix()
	if err := s.saveDomainLocked(d); err != nil {
		log.Printf("session-store: save flare %s: %v", domain, err)
	}
}

func (s *SessionStore) deleteFlare(domain string) {
	if s == nil || domain == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, _ := s.loadDomainLocked(domain)
	if d == nil {
		return
	}
	d.FlareCookie = ""
	d.FlareUserAgent = ""
	d.FlareObtained = 0
	_ = s.saveDomainLocked(d)
}

// listFlareSessions scans all domain files and returns the still-valid flare
// sessions. Expired entries have only their flare half cleared (auth cookie
// is preserved); fully-empty files are removed.
func (s *SessionStore) listFlareSessions(ttl time.Duration) map[string]*flareSession {
	out := make(map[string]*flareSession)
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return out
	}
	now := time.Now()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		domain := strings.TrimSuffix(e.Name(), ".json")
		d, err := s.loadDomainLocked(domain)
		if err != nil || d == nil {
			continue
		}
		if d.FlareCookie == "" {
			continue
		}
		obtained := time.Unix(d.FlareObtained, 0)
		if now.Sub(obtained) > ttl {
			d.FlareCookie = ""
			d.FlareUserAgent = ""
			d.FlareObtained = 0
			_ = s.saveDomainLocked(d)
			continue
		}
		out[domain] = &flareSession{
			cookies:   d.FlareCookie,
			userAgent: d.FlareUserAgent,
			obtained:  obtained,
		}
	}
	return out
}
