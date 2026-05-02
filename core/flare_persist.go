package core

import "time"

// Persistence for flare sessions across restarts is handled by the unified
// SessionStore (see sessionstore.go) which keeps both auth cookies and
// flare/CF challenge sessions in a single per-domain JSON file. The wrappers
// below preserve the old function names used by fetcher.go.

func saveFlareSession(domain string, sess *flareSession) {
	DefaultSessionStore().saveFlare(domain, sess)
}

func deleteFlareSession(domain string) {
	DefaultSessionStore().deleteFlare(domain)
}

// loadFlareSessions returns the still-valid flare sessions stored on disk.
// Expired entries are cleaned up on the way out.
func loadFlareSessions(ttl time.Duration) map[string]*flareSession {
	store := DefaultSessionStore()
	if store == nil {
		return map[string]*flareSession{}
	}
	return store.listFlareSessions(ttl)
}
