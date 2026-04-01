package filedb

import (
	"regexp"
)

var (
	rutorIDRe    = regexp.MustCompile(`/torrent/(\d+)`)
	torrentByRe  = regexp.MustCompile(`https?://[^/]+/(\d+)/`)
	torrentByRe2 = regexp.MustCompile(`/(\d+)/`)
	megapeerIDRe = regexp.MustCompile(`/torrent/(\d+)`)
	selezenIDRe  = regexp.MustCompile(`/relizy-ot-selezen/(\d+)-`)
	baibakoIDRe  = regexp.MustCompile(`(?i)details\.php\?id=(\d+)`)
)

// GetTorrentIDFromURL extracts the numeric torrent ID from a tracker URL.
// Returns 0 if the tracker is not supported or no ID is found.
func GetTorrentIDFromURL(trackerName, url string) int {
	if url == "" {
		return 0
	}
	switch trackerName {
	case "rutor":
		if m := rutorIDRe.FindStringSubmatch(url); len(m) == 2 {
			return atoid(m[1])
		}
	case "torrentby":
		if m := torrentByRe.FindStringSubmatch(url); len(m) == 2 {
			return atoid(m[1])
		}
		if m := torrentByRe2.FindStringSubmatch(url); len(m) == 2 {
			return atoid(m[1])
		}
	case "megapeer":
		if m := megapeerIDRe.FindStringSubmatch(url); len(m) == 2 {
			return atoid(m[1])
		}
	case "selezen":
		if m := selezenIDRe.FindStringSubmatch(url); len(m) == 2 {
			return atoid(m[1])
		}
	case "baibako":
		if m := baibakoIDRe.FindStringSubmatch(url); len(m) == 2 {
			return atoid(m[1])
		}
	}
	return 0
}

// FindByTrackerID searches bucket for an existing entry with the same numeric tracker ID.
// Returns the old URL key if found, empty string otherwise.
func FindByTrackerID(bucket map[string]TorrentDetails, trackerName, newURL string) (string, bool) {
	newID := GetTorrentIDFromURL(trackerName, newURL)
	if newID == 0 {
		return "", false
	}
	for oldURL := range bucket {
		if asString(bucket[oldURL]["trackerName"]) != trackerName {
			continue
		}
		if GetTorrentIDFromURL(trackerName, oldURL) == newID {
			return oldURL, true
		}
	}
	return "", false
}

func atoid(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}
