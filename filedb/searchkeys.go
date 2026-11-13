package filedb

import "strings"

// matchingShardKeys takes a small, owned snapshot of just the selected shard
// keys. No caller holds db.mu across file reads or copies the entire alias map.
// limit bounds fuzzy selection only; zero means unbounded.
func (db *DB) matchingShardKeys(terms []string, partial bool, limit int) map[string]struct{} {
	db.mu.RLock()
	defer db.mu.RUnlock()
	var out map[string]struct{}
	add := func(keys []string) {
		if len(keys) == 0 {
			return
		}
		if out == nil {
			out = make(map[string]struct{}, len(keys))
		}
		for _, key := range keys {
			if limit > 0 && len(out) >= limit {
				return
			}
			out[key] = struct{}{}
		}
	}
	// Always reserve exact aliases before spending a fuzzy-search read budget.
	for _, term := range terms {
		if term != "" {
			add(db.fastdb[term])
		}
	}
	if !partial {
		return out
	}
	// Prefer prefixes to interior substrings: F1: The Movie should not lose
	// its budget to unrelated aliases containing f1 in the middle.
	for _, prefix := range []bool{true, false} {
		for alias, keys := range db.fastdb {
			if limit > 0 && len(out) >= limit {
				return out
			}
			for _, term := range terms {
				if term != "" && ((prefix && strings.HasPrefix(alias, term)) || (!prefix && strings.Contains(alias, term))) {
					add(keys)
					break
				}
			}
		}
	}
	return out
}
