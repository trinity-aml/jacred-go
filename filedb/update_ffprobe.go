package filedb

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

var btihRe = regexp.MustCompile(`(?i)[?&]xt=urn:btih:([A-Z0-9]+)`)

func extractBTIH(magnet string) string {
	m := btihRe.FindStringSubmatch(strings.TrimSpace(magnet))
	if len(m) != 2 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(m[1]))
}

func (db *DB) FindTorrentKeyByMagnet(magnet string) string {
	magnet = strings.TrimSpace(magnet)
	if magnet == "" {
		return ""
	}
	targetHash := extractBTIH(magnet)
	for _, item := range db.OrderedMasterEntries() {
		bucket, err := db.OpenRead(item.Key)
		if err != nil {
			continue
		}
		for _, t := range bucket {
			tm := strings.TrimSpace(asString(t["magnet"]))
			if tm == "" {
				continue
			}
			if strings.EqualFold(tm, magnet) {
				return item.Key
			}
			if targetHash != "" {
				if got := extractBTIH(tm); got != "" && got == targetHash {
					return item.Key
				}
			}
		}
	}
	return ""
}

func (db *DB) UpdateTorrentFfprobeInfo(torrentKey, magnet string, ffprobeTryingData int, ffprobe any, languages []string) error {
	if strings.TrimSpace(torrentKey) == "" || strings.TrimSpace(magnet) == "" {
		return errors.New("torrentKey and magnet are required")
	}
	path := db.PathDb(torrentKey)
	bucket, err := db.OpenRead(torrentKey)
	if err != nil {
		return err
	}

	var targetURL string
	var target TorrentDetails
	for u, t := range bucket {
		tm := strings.TrimSpace(asString(t["magnet"]))
		if tm == "" {
			continue
		}
		if strings.EqualFold(tm, magnet) || (extractBTIH(tm) != "" && extractBTIH(tm) == extractBTIH(magnet)) {
			targetURL, target = u, t
			break
		}
	}
	if target == nil {
		return fmt.Errorf("torrent with magnet not found in key %s", torrentKey)
	}

	updated := false
	if asInt(target["ffprobe_tryingdata"]) != ffprobeTryingData {
		target["ffprobe_tryingdata"] = ffprobeTryingData
		updated = true
	}
	if ffprobe != nil {
		target["ffprobe"] = ffprobe
		updated = true
	}
	if languages != nil {
		uniq := map[string]struct{}{}
		out := make([]string, 0, len(languages))
		for _, l := range languages {
			l = strings.TrimSpace(strings.ToLower(l))
			if l == "" {
				continue
			}
			if _, ok := uniq[l]; ok {
				continue
			}
			uniq[l] = struct{}{}
			out = append(out, l)
		}
		sort.Strings(out)
		if len(out) > 0 {
			target["languages"] = out
		} else {
			delete(target, "languages")
		}
		updated = true
	}
	if !updated {
		return nil
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	target["updateTime"] = now
	bucket[targetURL] = target
	if err := writeBucket(path, bucket); err != nil {
		return err
	}

	db.mu.Lock()
	db.masterDb[torrentKey] = TorrentInfo{UpdateTime: parseDotNetTime(now), FileTime: ToFileTimeUTC(parseDotNetTime(now))}
	db.mu.Unlock()
	return db.SaveChangesToFile()
}
