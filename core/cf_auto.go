package core

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// Auto-CF registry: domains observed to serve a CloudFlare challenge during a
// standard-mode fetch. Once flagged, the Fetcher routes future requests for
// the domain through the flare path transparently — even if the tracker
// config doesn't set fetchmode: "flaresolverr". Persisted as a single JSON
// file so a restart doesn't repeat the wasted-first-request that produced
// the detection. Each successful flare-routed fetch refreshes the timestamp,
// so a site that's been silent for cfAutoTTL ages out and starts accepting
// standard requests again.

const cfAutoTTL = 30 * 24 * time.Hour

var (
	cfAutoMu   sync.RWMutex
	cfAutoMap  map[string]time.Time
	cfAutoPath string
)

func init() {
	cfAutoMap = make(map[string]time.Time)
}

// SetCFAutoPersistFile enables on-disk persistence of the auto-CF registry.
// Pass the full file path (e.g. Data/temp/cf_auto.json). Pass "" to disable.
// Existing entries are loaded on the way in; expired ones are dropped.
func SetCFAutoPersistFile(path string) {
	cfAutoMu.Lock()
	cfAutoPath = path
	cfAutoMu.Unlock()
	if path == "" {
		return
	}
	loadCFAutoLocked()
}

func loadCFAutoLocked() {
	cfAutoMu.Lock()
	defer cfAutoMu.Unlock()
	if cfAutoPath == "" {
		return
	}
	b, err := os.ReadFile(cfAutoPath)
	if err != nil {
		return
	}
	var raw map[string]int64
	if err := json.Unmarshal(b, &raw); err != nil {
		return
	}
	cutoff := time.Now().Add(-cfAutoTTL)
	for domain, ts := range raw {
		t := time.Unix(ts, 0)
		if t.Before(cutoff) {
			continue
		}
		cfAutoMap[domain] = t
	}
	if len(cfAutoMap) > 0 {
		log.Printf("cf-auto: loaded %d previously detected CF domain(s)", len(cfAutoMap))
	}
}

func saveCFAutoLocked() {
	if cfAutoPath == "" {
		return
	}
	out := make(map[string]int64, len(cfAutoMap))
	for domain, t := range cfAutoMap {
		out[domain] = t.Unix()
	}
	b, err := json.Marshal(out)
	if err != nil {
		return
	}
	tmp := cfAutoPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("cf-auto: write %s: %v", cfAutoPath, err)
		return
	}
	if err := os.Rename(tmp, cfAutoPath); err != nil {
		log.Printf("cf-auto: rename %s: %v", cfAutoPath, err)
		_ = os.Remove(tmp)
	}
}

// markDomainCF flags a domain as CF-protected. New entries log; refreshes
// quietly update the timestamp so active domains stay flagged.
func markDomainCF(domain string) {
	if domain == "" {
		return
	}
	cfAutoMu.Lock()
	_, existed := cfAutoMap[domain]
	cfAutoMap[domain] = time.Now()
	saveCFAutoLocked()
	cfAutoMu.Unlock()
	if !existed {
		log.Printf("cf-auto: detected CloudFlare on %s — future requests will use flaresolverr", domain)
	}
}

// isDomainCFAuto reports whether the registry has flagged this domain within
// the TTL window.
func isDomainCFAuto(domain string) bool {
	if domain == "" {
		return false
	}
	cfAutoMu.RLock()
	t, ok := cfAutoMap[domain]
	cfAutoMu.RUnlock()
	if !ok {
		return false
	}
	return time.Since(t) < cfAutoTTL
}

// CFAutoSnapshot returns a copy of the current auto-CF registry (domain →
// last-seen timestamp). Useful for /admin/* endpoints.
func CFAutoSnapshot() map[string]time.Time {
	cfAutoMu.RLock()
	defer cfAutoMu.RUnlock()
	out := make(map[string]time.Time, len(cfAutoMap))
	for k, v := range cfAutoMap {
		out[k] = v
	}
	return out
}

// ClearCFAuto removes domain from the registry (or all entries when domain
// is empty). Returns the number of removed entries.
func ClearCFAuto(domain string) int {
	cfAutoMu.Lock()
	defer cfAutoMu.Unlock()
	if domain == "" {
		n := len(cfAutoMap)
		cfAutoMap = make(map[string]time.Time)
		saveCFAutoLocked()
		return n
	}
	if _, ok := cfAutoMap[domain]; !ok {
		return 0
	}
	delete(cfAutoMap, domain)
	saveCFAutoLocked()
	return 1
}
