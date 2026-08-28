package bitruapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"jacred/app"
	"jacred/core"
)

// A Cloudflare interstitial and a rate-limit page arrive as ordinary 200-or-403
// bodies; only the payload tells them apart from a .torrent, which always opens
// with a bencode dict. Until this existed the parser counted them as generic
// failures and said nothing, so a run reported failed=80 and no reason.
func TestLooksLikeHTML(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"torrent", []byte("d8:announce35:http://tracker.example/announce"), false},
		{"cloudflare interstitial", []byte(`<!DOCTYPE html><html lang="en-US"><head><title>Just a moment`), true},
		{"html after whitespace", []byte("\r\n\t  <html>"), true},
		{"empty", nil, false},
	}
	for _, c := range cases {
		if got := looksLikeHTML(c.data); got != c.want {
			t.Errorf("%s: looksLikeHTML = %v, want %v", c.name, got, c.want)
		}
	}
}

// apiPage renders a response shaped like bitru's, carrying the given ids.
func apiPage(ids []int, beforeDate int64) string {
	items := make([]string, 0, len(ids))
	for _, id := range ids {
		items = append(items, fmt.Sprintf(`{"item":{"torrent":{"id":%d,"size":100,"added":1700000000,"seeders":1,"leechers":0,"file":"x.torrent"},"info":{"name":"Film %d","year":2020},"template":{"category":"movie","orig_name":"Film %d"}}}`, id, id, id))
	}
	return fmt.Sprintf(`{"error":false,"result":{"items":[%s],"before_date":%d}}`,
		strings.Join(items, ","), beforeDate)
}

func newTestParser(host string) *Parser {
	cfg := app.DefaultConfig()
	cfg.Bitru.Host = host
	cfg.Bitru.FetchMode = "standard"
	cfg.Bitru.ParseDelay = 0
	return &Parser{Config: cfg, Fetcher: core.NewFetcher(cfg)}
}

// bitru's before_date cursor does not reliably advance: asking for the page
// after the one just read returns the same torrents. Measured against the live
// API — page 1 was 99 records, all 99 repeats of page 0 — which made every run
// do double the API calls and report twice the records it really had.
func TestPagingStopsWhenAPageRepeats(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		switch calls {
		case 1:
			_, _ = w.Write([]byte(apiPage([]int{1, 2, 3}, 1700000000)))
		default:
			// The same torrents again, exactly as the live API does.
			_, _ = w.Write([]byte(apiPage([]int{1, 2, 3}, 1699999000)))
		}
	}))
	defer srv.Close()

	p := newTestParser(srv.URL)
	got, err := p.fetchTorrentsFromAPI(context.Background(), 100, nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("collected %d records, want 3 distinct", len(got))
	}
	// One page of new data, one that repeats it and stops the loop. Without the
	// guard this walks to the 50-page cap, re-reading the same three forever.
	if calls > 2 {
		t.Errorf("made %d API calls; a repeated page must stop the paging", calls)
	}
	seen := map[string]bool{}
	for _, rec := range got {
		u := asString(rec["url"])
		if seen[u] {
			t.Errorf("duplicate url survived: %s", u)
		}
		seen[u] = true
	}
}

// A genuinely longer listing must still page through.
func TestPagingContinuesWhileNewRecordsArrive(t *testing.T) {
	pages := [][]int{{1, 2}, {3, 4}, {}}
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := calls
		calls++
		if idx >= len(pages) {
			idx = len(pages) - 1
		}
		_, _ = w.Write([]byte(apiPage(pages[idx], 1700000000-int64(idx))))
	}))
	defer srv.Close()

	p := newTestParser(srv.URL)
	got, err := p.fetchTorrentsFromAPI(context.Background(), 100, nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 4 {
		t.Errorf("collected %d records, want 4 across two pages", len(got))
	}
}

// 429 is not a broken row: the torrent is fine and the next pass will get it, so
// it must be told apart from a download that produced garbage.
func TestDownloadReportsRateLimitDistinctly(t *testing.T) {
	var mode string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "429":
			w.WriteHeader(http.StatusTooManyRequests)
		case "html":
			// Deliberately NOT a Cloudflare challenge body: CF auto-detect is
			// process-wide and keyed by host, so a real challenge here would
			// promote 127.0.0.1 to flaresolverr routing and every later test —
			// on any httptest server — would answer 503.
			_, _ = w.Write([]byte(`<!DOCTYPE html><html><body>Not a torrent</body></html>`))
		default:
			_, _ = w.Write([]byte("d8:announce20:http://t.example/anne"))
		}
	}))
	defer srv.Close()

	p := newTestParser(srv.URL)
	ctx := context.Background()

	mode = "429"
	if _, err := p.download(ctx, srv.URL+"/api.php?download=1", srv.URL); err == nil {
		t.Error("429 produced no error")
	} else if !isRateLimited(err) {
		t.Errorf("429 reported as %v, not as a rate limit", err)
	}

	mode = "html"
	_, err := p.download(ctx, srv.URL+"/api.php?download=1", srv.URL)
	if err == nil {
		t.Error("an HTML body was accepted as a torrent")
	} else if isRateLimited(err) {
		t.Error("an HTML body was reported as a rate limit")
	} else if !strings.Contains(err.Error(), "HTML") {
		t.Errorf("HTML failure does not say so: %v", err)
	}

	mode = "ok"
	if _, err := p.download(ctx, srv.URL+"/api.php?download=1", srv.URL); err != nil {
		t.Errorf("a valid torrent was rejected: %v", err)
	}
}

func isRateLimited(err error) bool {
	return err != nil && strings.Contains(err.Error(), "rate limited")
}

// The backoff schedule has to be finite and ordered, or a throttled run either
// gives up instantly or never ends.
func TestRateLimitBackoffIsSane(t *testing.T) {
	if len(rateLimitBackoff) == 0 {
		t.Fatal("no backoff schedule")
	}
	for i := 1; i < len(rateLimitBackoff); i++ {
		if rateLimitBackoff[i] <= rateLimitBackoff[i-1] {
			t.Errorf("backoff does not grow: %v", rateLimitBackoff)
		}
	}
	if totalBackoff() > 2*60e9 {
		t.Errorf("total backoff %v is longer than a cron interval", totalBackoff())
	}
}
