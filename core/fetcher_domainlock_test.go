package core

import (
	"sync"
	"testing"
	"time"
)

// TestDomainLockSweepEvictsIdle pokes 100 unique domains then forces a
// sweep by ageing them and probing one more domain. The map should shed
// the idle entries instead of growing monotonically.
func TestDomainLockSweepEvictsIdle(t *testing.T) {
	flareDomainMu.Lock()
	flareDomainLocks = make(map[string]*domainLockEntry)
	flareDomainSweepCnt = 0
	flareDomainMu.Unlock()

	// Seed entries with stale lastUsed.
	stale := time.Now().Add(-3 * flareDomainLockTTL)
	flareDomainMu.Lock()
	for i := 0; i < 100; i++ {
		domain := "stale-" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
		flareDomainLocks[domain] = &domainLockEntry{mu: &sync.Mutex{}, lastUsed: stale}
	}
	flareDomainMu.Unlock()

	// Drive the sweep counter to its trigger threshold (every 64th call).
	for i := 0; i < 64; i++ {
		_ = getDomainLock("hotdomain")
	}

	flareDomainMu.Lock()
	defer flareDomainMu.Unlock()
	if len(flareDomainLocks) > 5 {
		t.Fatalf("expected stale entries swept, still have %d", len(flareDomainLocks))
	}
	if _, ok := flareDomainLocks["hotdomain"]; !ok {
		t.Fatal("hot domain should never be swept")
	}
}

func TestDomainLockSkipsHeldMutex(t *testing.T) {
	flareDomainMu.Lock()
	flareDomainLocks = make(map[string]*domainLockEntry)
	flareDomainSweepCnt = 0
	flareDomainMu.Unlock()

	heldDomain := "held-domain.example"
	mu := getDomainLock(heldDomain)
	mu.Lock()
	defer mu.Unlock()

	// Age the entry so the sweeper considers it eligible.
	flareDomainMu.Lock()
	flareDomainLocks[heldDomain].lastUsed = time.Now().Add(-3 * flareDomainLockTTL)
	flareDomainMu.Unlock()

	// Trigger sweep.
	for i := 0; i < 64; i++ {
		_ = getDomainLock("trigger")
	}

	flareDomainMu.Lock()
	_, stillThere := flareDomainLocks[heldDomain]
	flareDomainMu.Unlock()
	if !stillThere {
		t.Fatal("held mutex must not be evicted")
	}
}
