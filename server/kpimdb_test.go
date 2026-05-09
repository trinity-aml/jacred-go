package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func resetKPImdbCache() {
	kpImdbCacheMu.Lock()
	kpImdbCache = make(map[string]kpImdbCacheEntry, 256)
	kpImdbCacheMu.Unlock()
}

// withMockAPI redirects the resolver to a test server, restoring the
// original config on cleanup.
func withMockAPI(t *testing.T, fn func(http.ResponseWriter, *http.Request)) func() {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(fn))
	origURL := kpImdbAPIURL
	origClient := kpImdbHTTPClient
	// kpImdbAPIURL is a const; we can't override it directly. The test
	// instead points kpImdbHTTPClient at a transport that rewrites the
	// destination, so the resolver still issues the same URL.
	transport := &rewriteTransport{base: srv.Client().Transport, target: srv.URL}
	if transport.base == nil {
		transport.base = http.DefaultTransport
	}
	kpImdbHTTPClient = &http.Client{Transport: transport, Timeout: kpImdbTimeout}
	resetKPImdbCache()
	return func() {
		kpImdbHTTPClient = origClient
		_ = origURL
		srv.Close()
	}
}

type rewriteTransport struct {
	base   http.RoundTripper
	target string
}

func (r *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	// Replace scheme+host with the test server while keeping the path/query.
	target, err := req.URL.Parse(r.target)
	if err != nil {
		return nil, err
	}
	clone.URL.Scheme = target.Scheme
	clone.URL.Host = target.Host
	clone.Host = target.Host
	return r.base.RoundTrip(clone)
}

func TestResolveKPImdbNotAnID(t *testing.T) {
	cleanup := withMockAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("API should not be called for non-id input")
	})
	defer cleanup()

	for _, s := range []string{"", "Матрица", "tt", "kp", "tt-123", " ttabc"} {
		if _, _, ok := resolveKPImdb(context.Background(), s); ok {
			t.Fatalf("ok=true for non-id input %q", s)
		}
	}
}

func TestResolveKPImdbHappyPath(t *testing.T) {
	var imdbHits, kpHits atomic.Int32
	cleanup := withMockAPI(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("token") != kpImdbAPIToken {
			t.Errorf("missing/wrong token: %q", q.Get("token"))
		}
		if q.Get("imdb") != "" {
			imdbHits.Add(1)
		}
		if q.Get("kp") != "" {
			kpHits.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"success","data":{"name":"Побег","original_name":"The Shawshank Redemption"}}`))
	})
	defer cleanup()

	search, alt, ok := resolveKPImdb(context.Background(), "tt0111161")
	if !ok || search != "The Shawshank Redemption" || alt != "Побег" {
		t.Fatalf("imdb path got search=%q alt=%q ok=%v", search, alt, ok)
	}
	if imdbHits.Load() != 1 {
		t.Fatalf("expected 1 imdb call, got %d", imdbHits.Load())
	}

	search, alt, ok = resolveKPImdb(context.Background(), "kp326")
	if !ok || search != "The Shawshank Redemption" || alt != "Побег" {
		t.Fatalf("kp path got search=%q alt=%q ok=%v", search, alt, ok)
	}
	if kpHits.Load() != 1 {
		t.Fatalf("expected 1 kp call, got %d", kpHits.Load())
	}

	// Second call must hit the cache, not the API.
	resolveKPImdb(context.Background(), "tt0111161")
	if imdbHits.Load() != 1 {
		t.Fatalf("cache miss on repeat: imdb hits = %d", imdbHits.Load())
	}
}

func TestResolveKPImdbStripsKpPrefix(t *testing.T) {
	var lastKp string
	cleanup := withMockAPI(t, func(w http.ResponseWriter, r *http.Request) {
		lastKp = r.URL.Query().Get("kp")
		w.Write([]byte(`{"status":"success","data":{"name":"x","original_name":"y"}}`))
	})
	defer cleanup()

	resolveKPImdb(context.Background(), "kp9876543")
	if lastKp != "9876543" {
		t.Fatalf("kp prefix not stripped: got %q", lastKp)
	}
}

func TestResolveKPImdbAPIErrorReturnsEmpty(t *testing.T) {
	cleanup := withMockAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer cleanup()

	search, alt, ok := resolveKPImdb(context.Background(), "tt0111161")
	if !ok {
		t.Fatal("ok must be true even on API failure (id matched the pattern)")
	}
	if search != "" || alt != "" {
		t.Fatalf("expected empty resolution on error, got %q / %q", search, alt)
	}
}

func TestResolveKPImdbPartialResponse(t *testing.T) {
	cleanup := withMockAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"name":"","original_name":"Only Original"}}`))
	})
	defer cleanup()

	search, alt, ok := resolveKPImdb(context.Background(), "tt000")
	if !ok || search != "Only Original" || alt != "" {
		t.Fatalf("got search=%q alt=%q ok=%v", search, alt, ok)
	}
}

func TestKPImdbCacheBounded(t *testing.T) {
	resetKPImdbCache()
	for i := 0; i < kpImdbCacheMax+1000; i++ {
		kpImdbCacheSet("tt"+strings.Repeat("0", 6)+itoaSimple(i), kpImdbResolved{Name: "n", OriginalName: "o"})
	}
	kpImdbCacheMu.RLock()
	defer kpImdbCacheMu.RUnlock()
	if len(kpImdbCache) > kpImdbCacheMax {
		t.Fatalf("cache exceeds max=%d: have %d", kpImdbCacheMax, len(kpImdbCache))
	}
}

func itoaSimple(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
