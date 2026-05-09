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
// both. Empty fields are omitted via omitempty so files stay small.
//
// Writes are batched: changes go to an in-memory cache and a debounce timer
// flushes dirty domains to disk. This avoids the read-merge-write per save
// pattern that dominated CPU during CF-solve bursts.

type domainSession struct {
	Domain         string `json:"domain"`
	AuthCookie     string `json:"auth_cookie,omitempty"`
	AuthSavedAt    int64  `json:"auth_saved_at,omitempty"`
	FlareCookie    string `json:"flare_cookie,omitempty"`
	FlareUserAgent string `json:"flare_user_agent,omitempty"`
	FlareObtained  int64  `json:"flare_obtained,omitempty"`
}

type SessionStore struct {
	dir        string
	mu         sync.Mutex
	cache      map[string]*domainSession // nil entry = no session (file should not exist)
	loaded     map[string]bool           // tracks whether disk was consulted
	dirty      map[string]struct{}
	flushTimer *time.Timer
	flushDelay time.Duration
}

const sessionFlushDelay = 5 * time.Second

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
	defaultSessionStore = &SessionStore{
		dir:        dir,
		cache:      make(map[string]*domainSession),
		loaded:     make(map[string]bool),
		dirty:      make(map[string]struct{}),
		flushDelay: sessionFlushDelay,
	}
}

// DefaultSessionStore returns the package singleton or nil if unset.
func DefaultSessionStore() *SessionStore {
	defaultSessionStoreMu.Lock()
	defer defaultSessionStoreMu.Unlock()
	return defaultSessionStore
}

// FlushSessionStore forces pending session writes to disk immediately. Call on
// shutdown to ensure no auth/flare cookies are lost.
func FlushSessionStore() {
	DefaultSessionStore().Flush()
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

// loadDomainLocked returns the cached session for domain, populating the
// cache from disk on first access. Caller must hold s.mu. The returned
// pointer is the same one stored in the cache — callers may mutate it and
// then call saveDomainLocked to persist.
func (s *SessionStore) loadDomainLocked(domain string) (*domainSession, error) {
	if s.loaded[domain] {
		return s.cache[domain], nil
	}
	b, err := os.ReadFile(s.path(domain))
	if err != nil {
		if os.IsNotExist(err) {
			s.loaded[domain] = true
			s.cache[domain] = nil
			return nil, nil
		}
		return nil, err
	}
	var d domainSession
	if err := json.Unmarshal(b, &d); err != nil {
		// Mark as loaded with empty cache to avoid re-parsing the broken
		// file on every call; first save will overwrite it.
		s.loaded[domain] = true
		s.cache[domain] = nil
		return nil, err
	}
	if d.Domain == "" {
		d.Domain = domain
	}
	s.loaded[domain] = true
	s.cache[domain] = &d
	return &d, nil
}

// saveDomainLocked records the change in the in-memory cache and schedules a
// debounced flush. Caller holds s.mu. When both halves of d are empty the
// entry is queued for removal from disk.
func (s *SessionStore) saveDomainLocked(d *domainSession) error {
	if d.AuthCookie == "" && d.FlareCookie == "" {
		s.cache[d.Domain] = nil
	} else {
		s.cache[d.Domain] = d
	}
	s.loaded[d.Domain] = true
	s.dirty[d.Domain] = struct{}{}
	s.scheduleFlushLocked()
	return nil
}

func (s *SessionStore) scheduleFlushLocked() {
	if s.flushTimer != nil {
		return
	}
	s.flushTimer = time.AfterFunc(s.flushDelay, func() {
		s.mu.Lock()
		s.flushLocked()
		s.mu.Unlock()
	})
}

// flushLocked writes all dirty entries to disk. Caller holds s.mu. Errors
// are logged but do not stop the loop (one bad domain shouldn't strand
// others). The dirty set is cleared in full even on partial failure to
// prevent retry storms — next change re-queues the same domain.
func (s *SessionStore) flushLocked() {
	if s.flushTimer != nil {
		s.flushTimer.Stop()
		s.flushTimer = nil
	}
	if len(s.dirty) == 0 {
		return
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		log.Printf("session-store: mkdir %s: %v", s.dir, err)
		return
	}
	for domain := range s.dirty {
		s.writeDomainLocked(domain)
	}
	s.dirty = make(map[string]struct{})
}

func (s *SessionStore) writeDomainLocked(domain string) {
	d := s.cache[domain]
	p := s.path(domain)
	if d == nil {
		_ = os.Remove(p)
		return
	}
	b, err := json.Marshal(d)
	if err != nil {
		log.Printf("session-store: marshal %s: %v", domain, err)
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("session-store: write %s: %v", p, err)
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		log.Printf("session-store: rename %s: %v", p, err)
		_ = os.Remove(tmp)
	}
}

// Flush persists pending changes immediately. Call on shutdown.
func (s *SessionStore) Flush() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.flushLocked()
	s.mu.Unlock()
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
// is preserved); fully-empty files are removed. The disk scan also seeds the
// in-memory cache so subsequent loads avoid re-reading files.
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
