package background

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"jacred/app"
	"jacred/filedb"
)

// syncV2Root is the response from /sync/fdb/torrents (v2 protocol).
type syncV2Root struct {
	NextRead    bool             `json:"nextread"`
	CountRead   int              `json:"countread"`
	Take        int              `json:"take"`
	Collections []syncCollection `json:"collections"`
}

type syncCollection struct {
	Key   string    `json:"Key"`
	Value syncValue `json:"Value"`
}

type syncValue struct {
	Time     time.Time                          `json:"time"`
	FileTime int64                              `json:"fileTime"`
	Torrents map[string]filedb.TorrentDetails   `json:"torrents"`
}

// syncV1Root is the response from /sync/torrents (v1 protocol).
type syncV1Root struct {
	Take     int           `json:"take"`
	Torrents []syncV1Entry `json:"torrents"`
}

type syncV1Entry struct {
	Key   string                `json:"key"`
	Value filedb.TorrentDetails `json:"value"`
}

// syncConf is the response from /sync/conf.
type syncConf struct {
	Fbd     bool `json:"fbd"`
	Spidr   bool `json:"spidr"`
	Version int  `json:"version"`
}

const (
	syncTimeFormat = "2006-01-02 15:04:05"
)

func formatFileTime(ft int64) string {
	if ft < 0 {
		return "-"
	}
	const ticksPerSecond = int64(10000000)
	const ticksBetweenEpochs = int64(116444736000000000) // Windows FILETIME epoch (1601→1970)
	unixNano := (ft - ticksBetweenEpochs) * 100
	t := time.Unix(0, unixNano).Local()
	return t.Format(syncTimeFormat)
}

// RunSyncCron periodically syncs torrents from a remote jacred instance.
func RunSyncCron(ctx context.Context, cfg app.Config, db *filedb.DB) {
	syncAPI := strings.TrimSpace(cfg.SyncAPI)
	if syncAPI == "" {
		return
	}
	syncAPI = strings.TrimRight(syncAPI, "/")

	tempDir := filepath.Join("Data", "temp")
	_ = os.MkdirAll(tempDir, 0o755)

	lastSyncPath := filepath.Join(tempDir, "lastsync.txt")
	starSyncPath := filepath.Join(tempDir, "starsync.txt")

	// Initial delay
	select {
	case <-ctx.Done():
		return
	case <-time.After(20 * time.Second):
	}

	client := &http.Client{Timeout: 300 * time.Second}

	for {
		cycleStart := time.Now()
		cycleTotal := 0
		var cycleElapsed time.Duration

		log.Printf("sync: start / %s", time.Now().Format(syncTimeFormat))

		lastsync := readInt64File(lastSyncPath, -1)

		// Check sync protocol
		conf, err := httpGetJSON[syncConf](ctx, client, syncAPI+"/sync/conf")
		if err != nil {
			log.Printf("sync: conf error: %v", err)
			goto wait
		}

		if conf.Fbd {
			// v2 protocol
			starsync := readInt64File(starSyncPath, -1)
			log.Printf("sync: loaded state lastsync=%d (%s) starsync=%d (%s)", lastsync, formatFileTime(lastsync), starsync, formatFileTime(starsync))

			batchIndex := 0
			lastSave := time.Now()

			for {
				batchIndex++
				batchStart := time.Now()

				url := fmt.Sprintf("%s/sync/fdb/torrents?time=%d&start=%d", syncAPI, lastsync, starsync)
				root, err := httpGetJSON[syncV2Root](ctx, client, url)
				if err != nil {
					log.Printf("sync: fetch error batch=%d: %v", batchIndex, err)
					break
				}

				if len(root.Collections) == 0 {
					starsync = lastsync
					writeInt64File(starSyncPath, starsync)
					log.Printf("sync: saved state (starsync.txt)")
					break
				}

				// Filter and import — batch per collection (one read+write per bucket key)
				filteredTracker, filteredSport, imported := 0, 0, 0

				for _, col := range root.Collections {
					toImport := make(map[string]filedb.TorrentDetails, len(col.Value.Torrents))

					for tURL, t := range col.Value.Torrents {
						tn := asString(t["trackerName"])
						if len(cfg.SyncTrackers) > 0 && tn != "" && !containsIgnoreCase(cfg.SyncTrackers, tn) {
							filteredTracker++
							continue
						}
						if !cfg.SyncSport && isSportType(t["types"]) {
							filteredSport++
							continue
						}
						if tURL == "" {
							tURL = asString(t["url"])
						}
						if tURL == "" {
							continue
						}
						toImport[tURL] = t
					}

					if len(toImport) == 0 {
						continue
					}
					if col.Key == "" || col.Key == ":" {
						// Fallback: compute key per entry
						for tURL, t := range toImport {
							importTorrent(db, t, tURL)
						}
					} else {
						importCollection(db, col.Key, toImport)
					}
					imported += len(toImport)
					runtime.Gosched() // yield between collection saves
				}

				if filteredTracker > 0 || filteredSport > 0 {
					log.Printf("sync:   incoming %d; filtered out %d by tracker, %d by sport", root.CountRead, filteredTracker, filteredSport)
				}

				cycleTotal += imported
				batchElapsed := time.Since(batchStart)
				log.Printf("sync: [%d] time=%d (%s) | %d torrents, nextread=%v, %s",
					batchIndex, lastsync, formatFileTime(lastsync), imported, root.NextRead, batchElapsed.Truncate(time.Millisecond))

				// Update lastsync from last collection (normalize in case remote uses old C# epoch)
				lastCol := root.Collections[len(root.Collections)-1]
				lastsync = filedb.NormalizeFileTime(lastCol.Value.FileTime)

				if root.NextRead {
					// Save periodically
					if time.Since(lastSave) > 5*time.Minute {
						lastSave = time.Now()
						_ = db.SaveChangesToFile()
						writeInt64File(lastSyncPath, lastsync)
						log.Printf("sync: saved state (lastsync.txt)")
					}
					// Brief pause between batches to avoid pegging CPU
					select {
					case <-ctx.Done():
						return
					case <-time.After(100 * time.Millisecond):
					}
					continue
				}

				starsync = lastsync
				writeInt64File(starSyncPath, starsync)
				log.Printf("sync: saved state (starsync.txt)")
				break
			}
		} else {
			// v1 protocol
			for {
				url := fmt.Sprintf("%s/sync/torrents?time=%d", syncAPI, lastsync)
				root, err := httpGetJSON[syncV1Root](ctx, client, url)
				if err != nil {
					log.Printf("sync: v1 fetch error: %v", err)
					break
				}

				if root.Torrents == nil || len(root.Torrents) == 0 {
					break
				}

				imported := 0
				for _, entry := range root.Torrents {
					tn := asString(entry.Value["trackerName"])
					if len(cfg.SyncTrackers) > 0 && tn != "" && !containsIgnoreCase(cfg.SyncTrackers, tn) {
						continue
					}
					if !cfg.SyncSport && isSportType(entry.Value["types"]) {
						continue
					}
					importTorrent(db, entry.Value, entry.Key)
					imported++
				}

				cycleTotal += imported
				lastEntry := root.Torrents[len(root.Torrents)-1]
				lastsync = filedb.ToFileTimeUTC(asTime(lastEntry.Value["updateTime"]))

				if root.Take != len(root.Torrents) {
					break
				}
			}
		}

		_ = db.SaveChangesToFile()
		writeInt64File(lastSyncPath, lastsync)

		cycleElapsed = time.Since(cycleStart)
		log.Printf("sync: end / %s (cycle added %d torrents in %s)",
			time.Now().Format(syncTimeFormat), cycleTotal, cycleElapsed.Truncate(time.Millisecond))

	wait:
		// Random delay 60-300 seconds + configured timeSync minutes
		randomDelay := time.Duration(60+rand.Intn(240)) * time.Second
		syncMinutes := cfg.TimeSync
		if syncMinutes < 20 {
			syncMinutes = 20
		}
		totalDelay := randomDelay + time.Duration(syncMinutes)*time.Minute

		select {
		case <-ctx.Done():
			return
		case <-time.After(totalDelay):
		}
	}
}

// RunSyncSpidr periodically syncs spidr data from remote.
func RunSyncSpidr(ctx context.Context, cfg app.Config, db *filedb.DB) {
	syncAPI := strings.TrimSpace(cfg.SyncAPI)
	if syncAPI == "" || !cfg.SyncSpidr {
		return
	}
	syncAPI = strings.TrimRight(syncAPI, "/")

	client := &http.Client{Timeout: 300 * time.Second}

	for {
		spidrMinutes := cfg.TimeSyncSpidr
		if spidrMinutes < 20 {
			spidrMinutes = 20
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(spidrMinutes) * time.Minute):
		}

		conf, err := httpGetJSON[syncConf](ctx, client, syncAPI+"/sync/conf")
		if err != nil || !conf.Spidr {
			continue
		}

		cycleStart := time.Now()
		cycleTotal := 0
		var lastsyncSpidr int64 = -1
		batchIndex := 0

		log.Printf("sync_spidr: start / %s", time.Now().Format(syncTimeFormat))

		for {
			batchIndex++
			batchStart := time.Now()

			url := fmt.Sprintf("%s/sync/fdb/torrents?time=%d&spidr=true", syncAPI, lastsyncSpidr)
			root, err := httpGetJSON[syncV2Root](ctx, client, url)
			if err != nil {
				log.Printf("sync_spidr: fetch error: %v", err)
				break
			}

			if len(root.Collections) == 0 {
				break
			}

			batchCount := 0
			for _, col := range root.Collections {
				for url, t := range col.Value.Torrents {
					importTorrent(db, t, url)
					batchCount++
				}
			}

			cycleTotal += batchCount
			batchElapsed := time.Since(batchStart)
			log.Printf("sync_spidr: [%d] time=%d (%s) | %d collections, %d torrents, nextread=%v, %s",
				batchIndex, lastsyncSpidr, formatFileTime(lastsyncSpidr),
				len(root.Collections), batchCount, root.NextRead, batchElapsed.Truncate(time.Millisecond))

			lastCol := root.Collections[len(root.Collections)-1]
			lastsyncSpidr = lastCol.Value.FileTime

			if !root.NextRead {
				break
			}
		}

		cycleElapsed := time.Since(cycleStart)
		log.Printf("sync_spidr: end / %s (cycle added %d torrents in %s)",
			time.Now().Format(syncTimeFormat), cycleTotal, cycleElapsed.Truncate(time.Millisecond))
	}
}

// importCollection merges all torrents in toImport into the bucket at key.
// One read + one write instead of N reads + N writes.
// The masterDb timestamp is always time.Now() so each collection gets a unique,
// sequential FileTime regardless of the torrent updateTime from the source.
// This is critical for sync pagination: if we used latestTime (old torrent data),
// all collections would share the same FileTime and become unreachable after the
// first page is returned.
func importCollection(db *filedb.DB, key string, toImport map[string]filedb.TorrentDetails) {
	bucket, err := db.OpenReadOrEmpty(key)
	if err != nil {
		return
	}
	for tURL, t := range toImport {
		bucket[tURL] = t
	}
	_ = db.SaveBucket(key, bucket, time.Now().UTC())
}

// importTorrent adds or updates a single torrent in the DB.
func importTorrent(db *filedb.DB, t filedb.TorrentDetails, url string) {
	name := asString(t["name"])
	original := asString(t["originalname"])
	if name == "" {
		name = asString(t["title"])
	}
	if original == "" {
		original = name
	}
	key := db.KeyDb(name, original)
	if strings.TrimSpace(key) == "" || key == ":" {
		return
	}

	bucket, err := db.OpenReadOrEmpty(key)
	if err != nil {
		return
	}

	if url == "" {
		url = asString(t["url"])
	}
	if url == "" {
		return
	}

	bucket[url] = t
	ut := asTime(t["updateTime"])
	if ut.IsZero() {
		ut = time.Now().UTC()
	}
	_ = db.SaveBucket(key, bucket, ut)
}

// Helper functions

func httpGetJSON[T any](ctx context.Context, client *http.Client, url string) (T, error) {
	var zero T
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return zero, err
	}
	req.Header.Set("User-Agent", "jacred-go/sync")

	resp, err := client.Do(req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return zero, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var result T
	if err := json.NewDecoder(io.LimitReader(resp.Body, 100<<20)).Decode(&result); err != nil {
		return zero, fmt.Errorf("json: %w", err)
	}
	return result, nil
}

func readInt64File(path string, def int64) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return def
	}
	s := strings.TrimSpace(string(data))
	var v int64
	_, err = fmt.Sscanf(s, "%d", &v)
	if err != nil {
		return def
	}
	return v
}

func writeInt64File(path string, v int64) {
	_ = os.WriteFile(path, []byte(fmt.Sprintf("%d", v)), 0o644)
}

func containsIgnoreCase(list []string, val string) bool {
	for _, s := range list {
		if strings.EqualFold(s, val) {
			return true
		}
	}
	return false
}

func isSportType(v any) bool {
	switch t := v.(type) {
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok && strings.EqualFold(s, "sport") {
				return true
			}
		}
	case []string:
		for _, s := range t {
			if strings.EqualFold(s, "sport") {
				return true
			}
		}
	}
	return false
}

func asTime(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case string:
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
			if parsed, err := time.Parse(layout, t); err == nil {
				return parsed
			}
		}
	}
	return time.Time{}
}
