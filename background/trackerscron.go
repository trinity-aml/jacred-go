package background

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"jacred/filedb"
)

var trRe = regexp.MustCompile(`tr=([^&]+)`)
var nestedAnnounceRe = regexp.MustCompile(`[^/]+/[^/]+/announce`)

func RunTrackersCron(ctx context.Context, db *filedb.DB, dataDir, wwwroot string, enabled bool) {
	if !enabled {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(20 * time.Second):
	}
	if err := RunTrackersCronOnce(ctx, db, dataDir, wwwroot); err != nil {
		fmt.Printf("trackers: error / %v\n", err)
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := RunTrackersCronOnce(ctx, db, dataDir, wwwroot); err != nil {
				fmt.Printf("trackers: error / %v\n", err)
			}
		}
	}
}

func RunTrackersCronOnce(ctx context.Context, db *filedb.DB, dataDir, wwwroot string) error {
	trackers := map[string]struct{}{}
	httpClient := &http.Client{Timeout: 7 * time.Second}

	for _, item := range db.UnorderedMasterEntries() {
		// Respect context cancellation and throttle disk reads
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		bucket, err := db.OpenReadNoCache(item.Key)
		if err != nil {
			time.Sleep(time.Millisecond)
			continue
		}
		for _, t := range bucket {
			magnet := strings.TrimSpace(asString(t["magnet"]))
			if magnet == "" || !strings.Contains(magnet, "&") {
				continue
			}
			for _, m := range trRe.FindAllStringSubmatch(magnet, -1) {
				if len(m) != 2 {
					continue
				}
				tr, err := url.QueryUnescape(strings.Split(m[1], "?")[0])
				if err != nil {
					continue
				}
				tr = strings.TrimSpace(strings.ToLower(tr))
				if badTrackerURL(tr) || !checkTrackerWith(ctx, httpClient, tr) {
					continue
				}
				trackers[tr] = struct{}{}
			}
		}
		// 1ms sleep per entry: spreads 123k reads over ~2 min, keeps CPU below saturation
		time.Sleep(time.Millisecond)
	}
	list := make([]string, 0, len(trackers))
	for tr := range trackers {
		list = append(list, tr)
	}
	sort.Strings(list)
	if err := os.MkdirAll(wwwroot, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(wwwroot, "trackers.txt"), []byte(strings.Join(list, "\n")), 0o644)
}

func checkTrackerWith(ctx context.Context, client *http.Client, tracker string) bool {
	if strings.HasPrefix(tracker, "http") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, tracker, nil)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return true
	}
	if strings.HasPrefix(tracker, "udp:") {
		t := strings.TrimPrefix(tracker, "udp://")
		host := strings.Split(strings.Split(t, "/")[0], ":")[0]
		port := "6969"
		if parts := strings.Split(strings.Split(t, "/")[0], ":"); len(parts) > 1 {
			port = parts[1]
		}
		dialer := net.Dialer{Timeout: 7 * time.Second}
		conn, err := dialer.DialContext(ctx, "udp", net.JoinHostPort(host, port))
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}
	return false
}

func badTrackerURL(tracker string) bool {
	return tracker == "" || strings.Contains(tracker, "[") || !strings.Contains(strings.ReplaceAll(tracker, "://", ""), ":") || strings.Contains(tracker, " ") || strings.Contains(tracker, "torrentsmd.eu") || nestedAnnounceRe.MatchString(tracker)
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}
