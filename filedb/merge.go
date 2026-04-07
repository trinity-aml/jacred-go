package filedb

import (
	"strings"
	"time"

	"jacred/core"
)

// MergeResult holds the outcome of MergeTorrent.
type MergeResult struct {
	Torrent     TorrentDetails // merged torrent data
	Changed     bool           // any field was modified (or torrent is new)
	IsNew       bool           // torrent didn't exist before
	NeedsFull   bool           // title/types/sizeName/trackerName changed → call UpdateFullDetails
	TimeUpdated bool           // updateTime was set to now (significant change, not just sid/pir)
}

// MergeTorrent performs per-field comparison between existing and incoming torrent data.
// If existing is nil, the torrent is treated as new.
// Port of C# FileDB.AddOrUpdate per-field comparison logic.
//
// "Quiet" updates (sid, pir, createTime) do NOT touch updateTime — they won't
// propagate via sync until the next significant change. This avoids flooding
// sync clients with seeder-count noise.
func MergeTorrent(existing, incoming TorrentDetails, tracksAttempt int) MergeResult {
	if existing == nil {
		return mergeNew(incoming)
	}
	return mergeExisting(existing, incoming, tracksAttempt)
}

func mergeNew(incoming TorrentDetails) MergeResult {
	magnet := strings.TrimSpace(asString(incoming["magnet"]))
	types := asStringSlice(incoming["types"])
	if magnet == "" || len(types) == 0 {
		return MergeResult{}
	}

	t := TorrentDetails{}
	for k, v := range incoming {
		if v != nil {
			t[k] = v
		}
	}

	name := strings.TrimSpace(asString(t["name"]))
	original := strings.TrimSpace(asString(t["originalname"]))
	title := strings.TrimSpace(asString(t["title"]))

	if name == "" {
		if title != "" {
			name = title
		}
		t["name"] = name
	}
	if original == "" {
		if name != "" {
			original = name
		} else if title != "" {
			original = title
		}
		t["originalname"] = original
	}

	t["_sn"] = searchNameWithFallback(name, title)
	t["_so"] = searchNameWithFallback(original, name, title)

	return MergeResult{
		Torrent:     t,
		Changed:     true,
		IsNew:       true,
		NeedsFull:   true,
		TimeUpdated: true,
	}
}

func mergeExisting(t, incoming TorrentDetails, tracksAttempt int) MergeResult {
	changed := false
	updateFull := false
	timeUpdated := false

	upt := func(uptfull bool, updatetime bool) {
		changed = true
		if updatetime {
			t["updateTime"] = time.Now().UTC().Format(time.RFC3339Nano)
			t["ffprobe_tryingdata"] = 0
			timeUpdated = true
		}
		if uptfull {
			updateFull = true
		}
	}

	// types
	if inTypes := asStringSlice(incoming["types"]); len(inTypes) > 0 {
		exTypes := asStringSlice(t["types"])
		if len(exTypes) == 0 {
			t["types"] = incoming["types"]
			upt(true, true)
		} else {
			for _, typ := range inTypes {
				if typ != "" && !containsStr(exTypes, typ) {
					upt(true, true)
					break
				}
			}
			t["types"] = incoming["types"]
		}
	}

	// trackerName
	if v := asString(incoming["trackerName"]); v != "" && v != asString(t["trackerName"]) {
		t["trackerName"] = v
		upt(true, true)
	}

	// title
	if v := asString(incoming["title"]); v != "" && v != asString(t["title"]) {
		t["title"] = v
		upt(true, true)
	}

	// createTime (quiet — no updateTime)
	inCreate := parseTimeField(incoming["createTime"])
	exCreate := parseTimeField(t["createTime"])
	if !inCreate.IsZero() && inCreate.After(exCreate) {
		t["createTime"] = inCreate.UTC().Format(time.RFC3339Nano)
		upt(false, false)
	}

	// magnet
	if v := strings.TrimSpace(asString(incoming["magnet"])); v != "" && v != asString(t["magnet"]) {
		t["ffprobe_tryingdata"] = 0
		t["magnet"] = v
		upt(false, true)
	}

	// sid (quiet — no updateTime)
	if inSid := asInt(incoming["sid"]); inSid != asInt(t["sid"]) {
		exSid := asInt(t["sid"])
		if exSid == 0 && inSid >= 2 && asInt(t["ffprobe_tryingdata"]) >= tracksAttempt && tracksAttempt > 0 {
			t["ffprobe_tryingdata"] = 0
		}
		t["sid"] = inSid
		upt(false, false)
	}

	// pir (quiet — no updateTime)
	if v := asInt(incoming["pir"]); v != asInt(t["pir"]) {
		t["pir"] = v
		upt(false, false)
	}

	// sizeName
	if v := strings.TrimSpace(asString(incoming["sizeName"])); v != "" && v != asString(t["sizeName"]) {
		t["sizeName"] = v
		upt(true, true)
	}

	// name + _sn
	inName := strings.TrimSpace(asString(incoming["name"]))
	exName := strings.TrimSpace(asString(t["name"]))
	if inName != "" && inName != exName {
		t["name"] = inName
		t["_sn"] = core.SearchName(inName)
		upt(false, true)
	} else if exName == "" {
		fallback := strings.TrimSpace(asString(incoming["title"]))
		if fallback == "" {
			fallback = strings.TrimSpace(asString(t["title"]))
		}
		if fallback != "" {
			t["name"] = fallback
			t["_sn"] = core.SearchName(fallback)
			upt(false, true)
		}
	}
	if strings.TrimSpace(asString(t["_sn"])) == "" {
		if sn := searchNameWithFallback(asString(t["name"]), asString(t["title"])); sn != "" {
			t["_sn"] = sn
			upt(false, true)
		}
	}

	// originalname + _so
	inOrig := strings.TrimSpace(asString(incoming["originalname"]))
	exOrig := strings.TrimSpace(asString(t["originalname"]))
	if inOrig != "" && inOrig != exOrig {
		t["originalname"] = inOrig
		t["_so"] = core.SearchName(inOrig)
		upt(false, true)
	} else if exOrig == "" {
		fallback := strings.TrimSpace(asString(t["name"]))
		if fallback == "" {
			fallback = strings.TrimSpace(asString(incoming["title"]))
		}
		if fallback != "" {
			t["originalname"] = fallback
			t["_so"] = core.SearchName(fallback)
			upt(false, true)
		}
	}
	if strings.TrimSpace(asString(t["_so"])) == "" {
		if so := searchNameWithFallback(asString(t["originalname"]), asString(t["name"]), asString(t["title"])); so != "" {
			t["_so"] = so
			upt(false, true)
		}
	}

	// relased
	if v := asInt(incoming["relased"]); v > 0 && v != asInt(t["relased"]) {
		t["relased"] = v
		upt(false, true)
	}

	// url (update if found by tracker ID)
	if v := asString(incoming["url"]); v != "" && v != asString(t["url"]) {
		t["url"] = v
		upt(false, true)
	}

	// Clear computed fields so UpdateFullDetails (called in SaveBucket) recomputes them.
	// Without this, the guard `if t["quality"] != nil && t["_sn"] != ""` would skip reprocessing.
	if updateFull {
		delete(t, "quality")
		delete(t, "videotype")
		delete(t, "voices")
		delete(t, "languages")
		delete(t, "seasons")
		delete(t, "size")
	}

	return MergeResult{
		Torrent:     t,
		Changed:     changed,
		IsNew:       false,
		NeedsFull:   updateFull,
		TimeUpdated: timeUpdated,
	}
}

func searchNameWithFallback(values ...string) string {
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" {
			if sn := core.SearchName(v); sn != "" {
				return sn
			}
		}
	}
	return ""
}

func containsStr(slice []string, val string) bool {
	for _, s := range slice {
		if s == val {
			return true
		}
	}
	return false
}

func parseTimeField(v any) time.Time {
	if v == nil {
		return time.Time{}
	}
	if tm, ok := v.(time.Time); ok {
		return tm
	}
	s := strings.TrimSpace(asString(v))
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.9999999Z07:00",
		"2006-01-02T15:04:05Z07:00",
		time.RFC3339,
	} {
		if tm, err := time.Parse(layout, s); err == nil {
			return tm.UTC()
		}
	}
	return time.Time{}
}
