package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// kpImdbPattern matches IMDB ids ("tt" + digits) and Kinopoisk ids ("kp" +
// digits). Anchored to the entire string — partial matches against random
// torrent titles must not trigger the resolver.
var kpImdbPattern = regexp.MustCompile(`^(tt|kp)[0-9]+$`)

// kpImdbResolved holds the metadata API response we actually use.
type kpImdbResolved struct {
	Name         string // localized title, e.g. "Побег из Шоушенка"
	OriginalName string // original title, e.g. "The Shawshank Redemption"
}

// kpImdbCache memoizes resolutions with a 24h TTL bounded to 5000 entries
// (random eviction on overflow). Without a cap a flood of unique tt/kp
// values from a misbehaving client would grow the map unbounded.
const (
	kpImdbCacheTTL  = 24 * time.Hour
	kpImdbCacheMax  = 5000
	kpImdbCacheDrop = 500
	kpImdbAPIURL    = "https://api.apbugall.org/"
	kpImdbAPIToken  = "04941a9a3ca3ac16e2b4327347bbc1"
	kpImdbTimeout   = 8 * time.Second
)

type kpImdbCacheEntry struct {
	res     kpImdbResolved
	created time.Time
}

var (
	kpImdbCacheMu sync.RWMutex
	kpImdbCache   = make(map[string]kpImdbCacheEntry, 256)
)

// kpImdbHTTPClient is package-level so we keep one keep-alive pool across
// requests. Tests can swap it out.
var kpImdbHTTPClient = &http.Client{Timeout: kpImdbTimeout}

// resolveKPImdb maps an IMDB/Kinopoisk id to (search, altname). Returns
// ok=false when the input isn't an id pattern; returns ok=true with empty
// strings when the id pattern matched but the API gave no usable title (in
// which case the caller should keep the original input). Mirrors the C#
// implementation in jacred ApiController.cs:683-711.
func resolveKPImdb(ctx context.Context, raw string) (search, altname string, ok bool) {
	id := strings.TrimSpace(raw)
	if id == "" || !kpImdbPattern.MatchString(id) {
		return "", "", false
	}

	if cached, hit := kpImdbCacheGet(id); hit {
		return resolutionToParams(cached)
	}

	resolved, err := fetchKPImdb(ctx, id)
	if err != nil {
		// Negative result is also cached briefly via empty strings — but to
		// keep retry behavior friendly we don't store on hard errors;
		// transient API failures shouldn't poison the cache for 24h.
		return "", "", true
	}
	kpImdbCacheSet(id, resolved)
	return resolutionToParams(resolved)
}

func resolutionToParams(r kpImdbResolved) (search, altname string, ok bool) {
	if r.Name != "" && r.OriginalName != "" {
		return r.OriginalName, r.Name, true
	}
	if r.OriginalName != "" {
		return r.OriginalName, "", true
	}
	if r.Name != "" {
		return r.Name, "", true
	}
	return "", "", true
}

func kpImdbCacheGet(id string) (kpImdbResolved, bool) {
	kpImdbCacheMu.RLock()
	e, ok := kpImdbCache[id]
	kpImdbCacheMu.RUnlock()
	if !ok {
		return kpImdbResolved{}, false
	}
	if time.Since(e.created) > kpImdbCacheTTL {
		kpImdbCacheMu.Lock()
		delete(kpImdbCache, id)
		kpImdbCacheMu.Unlock()
		return kpImdbResolved{}, false
	}
	return e.res, true
}

func kpImdbCacheSet(id string, res kpImdbResolved) {
	kpImdbCacheMu.Lock()
	defer kpImdbCacheMu.Unlock()
	if len(kpImdbCache) >= kpImdbCacheMax {
		dropped := 0
		now := time.Now()
		for k, e := range kpImdbCache {
			if now.Sub(e.created) > kpImdbCacheTTL {
				delete(kpImdbCache, k)
				dropped++
			}
		}
		if dropped == 0 {
			i := 0
			for k := range kpImdbCache {
				delete(kpImdbCache, k)
				i++
				if i >= kpImdbCacheDrop {
					break
				}
			}
		}
	}
	kpImdbCache[id] = kpImdbCacheEntry{res: res, created: time.Now()}
}

// fetchKPImdb queries api.apbugall.org for metadata. The C# original uses
// the same endpoint and token (jacred-cs/Controllers/ApiController.cs:694);
// keeping them identical means existing API quotas/expectations apply.
func fetchKPImdb(ctx context.Context, id string) (kpImdbResolved, error) {
	q := url.Values{"token": {kpImdbAPIToken}}
	if strings.HasPrefix(id, "kp") {
		q.Set("kp", strings.TrimPrefix(id, "kp"))
	} else {
		q.Set("imdb", id)
	}
	endpoint := kpImdbAPIURL + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return kpImdbResolved{}, err
	}
	resp, err := kpImdbHTTPClient.Do(req)
	if err != nil {
		return kpImdbResolved{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return kpImdbResolved{}, errors.New("kp/imdb api: non-2xx")
	}

	var body struct {
		Status string `json:"status"`
		Data   struct {
			Name         string `json:"name"`
			OriginalName string `json:"original_name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return kpImdbResolved{}, err
	}
	return kpImdbResolved{
		Name:         strings.TrimSpace(body.Data.Name),
		OriginalName: strings.TrimSpace(body.Data.OriginalName),
	}, nil
}
