package filedb

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"jacred/core"
)

type SearchParams struct {
	APIKey        string
	Query         string
	Title         string
	TitleOriginal string
	Year          int
	IsSerial      int
	CategoryRaw   string
	UserAgent     string
}

type TorrentsParams struct {
	Search    string
	AltName   string
	Exact     bool
	Type      string
	Sort      string
	Tracker   string
	Voice     string
	VideoType string
	Relased   int
	Quality   int
	Season    int
}

type QualityRow struct {
	Qualitys   map[int]struct{}
	Types      map[string]struct{}
	Languages  map[string]struct{}
	CreateTime time.Time
	UpdateTime time.Time
}

type QualityRowJSON struct {
	Qualitys   []int     `json:"qualitys"`
	Types      []string  `json:"types"`
	Languages  []string  `json:"languages"`
	CreateTime time.Time `json:"createTime"`
	UpdateTime time.Time `json:"updateTime"`
}

type JackettResult struct {
	Results []TorrentDetails
	RqNum   bool
}

func (db *DB) MasterEntries() map[string]TorrentInfo {
	db.mu.RLock()
	defer db.mu.RUnlock()
	out := make(map[string]TorrentInfo, len(db.masterDb))
	for k, v := range db.masterDb {
		out[k] = v
	}
	return out
}

func (db *DB) JackettSearch(p SearchParams) (JackettResult, error) {
	fastdb := db.FastDB()
	torrents := map[string]TorrentDetails{}
	rqnum := p.IsSerial == -1 && p.UserAgent == "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/106.0.0.0 Safari/537.36"

	query, title, titleOriginal, year, isSerial := p.Query, p.Title, p.TitleOriginal, p.Year, p.IsSerial
	if rqnum && query != "" {
		if qtitle, qorig, qyear, ok, reject := parseRqNum(query); reject {
			return JackettResult{Results: []TorrentDetails{}, RqNum: rqnum}, nil
		} else if ok {
			title, titleOriginal, year = qtitle, qorig, qyear
		}
	}
	isSerial = mapCategory(isSerial, p.CategoryRaw)

	add := func(t TorrentDetails) {
		if !db.trackerAllowed(asString(t["trackerName"])) {
			return
		}
		urlv := asString(t["url"])
		if urlv == "" {
			return
		}
		if prev, ok := torrents[urlv]; ok {
			if torrentTime(t, "updateTime").After(torrentTime(prev, "updateTime")) {
				torrents[urlv] = cloneTorrent(t)
			}
			return
		}
		torrents[urlv] = cloneTorrent(t)
	}

	if strings.TrimSpace(title) != "" || strings.TrimSpace(titleOriginal) != "" {
		n := core.SearchName(title)
		o := core.SearchName(titleOriginal)
		keys := map[string]struct{}{}
		updateKeys := func(k string) {
			if k == "" {
				return
			}
			for _, val := range fastdb[k] {
				keys[val] = struct{}{}
			}
		}
		updateKeys(n)
		updateKeys(o)
		for _, key := range db.limitKeysMap(keys) {
			bucket, err := db.OpenRead(key)
			if err != nil {
				continue
			}
			for _, t := range bucket {
				if len(asStringSlice(t["types"])) == 0 || strings.Contains(asString(t["title"]), " КПК") {
					continue
				}
				name := asString(t["_sn"])
				if name == "" {
					name = core.SearchName(asString(t["name"]))
				}
				original := asString(t["_so"])
				if original == "" {
					original = core.SearchName(asString(t["originalname"]))
				}
				if (n != "" && n == name) || (o != "" && o == original) {
					if matchJackettExact(t, isSerial, year) {
						add(t)
					}
				}
			}
		}
	} else if strings.TrimSpace(query) != "" && len(strings.TrimSpace(query)) > 1 {
		s := core.SearchName(query)
		if s != "" {
			torrentsSearch := func(exact, exactdb bool) {
				var keys map[string]struct{}
				if exactdb {
					if vals, ok := fastdb[s]; ok && len(vals) > 0 {
						keys = map[string]struct{}{}
						for _, val := range vals {
							keys[val] = struct{}{}
						}
					}
				} else {
					keys = map[string]struct{}{}
					for fk, fv := range fastdb {
						if strings.Contains(fk, s) {
							for _, k := range fv {
								keys[k] = struct{}{}
							}
							if db.limitReads() && len(keys) > db.GetConfig().MaxReadFile {
								break
							}
						}
					}
				}
				for _, key := range db.limitKeysMap(keys) {
					bucket, err := db.OpenRead(key)
					if err != nil {
						continue
					}
					for _, t := range bucket {
						if exact {
							sn := asString(t["_sn"])
							if sn == "" {
								sn = core.SearchName(asString(t["name"]))
							}
							so := asString(t["_so"])
							if so == "" {
								so = core.SearchName(asString(t["originalname"]))
							}
							if sn != s && so != s {
								continue
							}
						}
						if len(asStringSlice(t["types"])) == 0 || strings.Contains(asString(t["title"]), " КПК") {
							continue
						}
						if matchJackettSearch(t, isSerial, year) {
							add(t)
						}
					}
				}
			}

			if isSerial == -1 {
				torrentsSearch(false, true)
				if len(torrents) == 0 {
					torrentsSearch(false, false)
				}
			} else {
				torrentsSearch(true, true)
				if len(torrents) == 0 {
					torrentsSearch(false, false)
				}
			}
		}
	}

	items := orderTorrentValues(torrents)
	if (!rqnum && db.GetConfig().MergeDuplicates) || (rqnum && db.GetConfig().MergeNumDuplicates) {
		items = mergeDuplicateMagnets(items)
	}
	if p.APIKey == "rus" {
		filtered := make([]TorrentDetails, 0, len(items))
		for _, item := range items {
			langs := makeStringSet(asStringSlice(item["languages"]))
			types := makeStringSet(asStringSlice(item["types"]))
			if _, ok := langs["rus"]; ok {
				filtered = append(filtered, item)
				continue
			}
			if _, ok := types["sport"]; ok {
				filtered = append(filtered, item)
				continue
			}
			if _, ok := types["tvshow"]; ok {
				filtered = append(filtered, item)
				continue
			}
			if _, ok := types["docuserial"]; ok {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}
	return JackettResult{Results: items, RqNum: rqnum}, nil
}

func (db *DB) TorrentsSearch(p TorrentsParams) ([]TorrentDetails, error) {
	search := strings.TrimSpace(p.Search)
	if search == "" || len(search) == 1 {
		return []TorrentDetails{}, nil
	}
	mdb := db.MasterEntries()
	torrents := map[string]TorrentDetails{}
	add := func(t TorrentDetails) {
		if !db.trackerAllowed(asString(t["trackerName"])) {
			return
		}
		urlv := asString(t["url"])
		if urlv == "" {
			return
		}
		if prev, ok := torrents[urlv]; ok {
			if torrentTime(t, "updateTime").After(torrentTime(prev, "updateTime")) {
				torrents[urlv] = cloneTorrent(t)
			}
			return
		}
		torrents[urlv] = cloneTorrent(t)
	}
	_s := core.SearchName(search)
	_alt := core.SearchName(p.AltName)
	keys := []string{}
	if p.Exact {
		for key := range mdb {
			if strings.HasPrefix(key, _s+":") || strings.HasSuffix(key, ":"+_s) || (_alt != "" && strings.Contains(key, _alt)) {
				keys = append(keys, key)
			}
		}
	} else {
		for key := range mdb {
			if strings.Contains(key, _s) || (_alt != "" && strings.Contains(key, _alt)) {
				keys = append(keys, key)
			}
		}
		if db.limitReads() && len(keys) > db.GetConfig().MaxReadFile {
			keys = keys[:db.GetConfig().MaxReadFile]
		}
	}
	for _, key := range keys {
		bucket, err := db.OpenRead(key)
		if err != nil {
			continue
		}
		for _, t := range bucket {
			if len(asStringSlice(t["types"])) == 0 {
				continue
			}
			if p.Type != "" && !containsString(asStringSlice(t["types"]), p.Type) {
				continue
			}
			if p.Exact {
				n := asString(t["_sn"])
				if n == "" {
					n = core.SearchName(asString(t["name"]))
				}
				o := asString(t["_so"])
				if o == "" {
					o = core.SearchName(asString(t["originalname"]))
				}
				if n != _s && o != _s && !(_alt != "" && (n == _alt || o == _alt)) {
					continue
				}
			}
			add(t)
		}
	}
	items := orderTorrentValues(torrents)
	items = filterTorrentResults(items, p)
	if len(items) > 2000 {
		items = items[:2000]
	}
	return items, nil
}

func (db *DB) Qualitys(name, originalname, typ string, page, take int) (map[string]map[int]QualityRowJSON, error) {
	_s := core.SearchName(name)
	_so := core.SearchName(originalname)
	if _s == "" && _so == "" {
		return map[string]map[int]QualityRowJSON{}, nil
	}
	mdb := db.MasterEntries()
	keys := make([]string, 0, len(mdb))
	for key := range mdb {
		if _s != "" && _so != "" {
			if strings.Contains(key, _s) || strings.Contains(key, _so) {
				keys = append(keys, key)
			}
		} else if _s != "" {
			if strings.Contains(key, _s) {
				keys = append(keys, key)
			}
		} else if _so != "" && strings.Contains(key, _so) {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		return mdb[keys[i]].UpdateTime.After(mdb[keys[j]].UpdateTime)
	})
	if db.limitReads() && len(keys) > db.GetConfig().MaxReadFile {
		keys = keys[:db.GetConfig().MaxReadFile]
	}
	result := map[string]map[int]*QualityRow{}
	for _, key := range keys {
		bucket, err := db.OpenRead(key)
		if err != nil {
			continue
		}
		for _, t := range bucket {
			if !db.trackerAllowed(asString(t["trackerName"])) || !qualityEligible(t, typ) {
				continue
			}
			k := fmt.Sprintf("%s:%s", core.SearchName(asString(t["name"])), core.SearchName(asString(t["originalname"])))
			if _, ok := result[k]; !ok {
				result[k] = map[int]*QualityRow{}
			}
			rel := asInt(t["relased"])
			langs := makeStringSet(asStringSlice(t["languages"]))
			row, ok := result[k][rel]
			if !ok {
				row = &QualityRow{Qualitys: map[int]struct{}{}, Types: map[string]struct{}{}, Languages: map[string]struct{}{}, CreateTime: torrentTime(t, "createTime"), UpdateTime: torrentTime(t, "updateTime")}
				result[k][rel] = row
			}
			for _, tp := range asStringSlice(t["types"]) {
				row.Types[tp] = struct{}{}
			}
			for lg := range langs {
				row.Languages[lg] = struct{}{}
			}
			q := asInt(t["quality"])
			if q != 0 {
				row.Qualitys[q] = struct{}{}
			}
			ct := torrentTime(t, "createTime")
			ut := torrentTime(t, "updateTime")
			if row.CreateTime.IsZero() || (!ct.IsZero() && row.CreateTime.After(ct)) {
				row.CreateTime = ct
			}
			if ut.After(row.UpdateTime) {
				row.UpdateTime = ut
			}
		}
	}
	ordered := make([]struct {
		key string
		val map[int]*QualityRow
	}, 0, len(result))
	for k, v := range result {
		ordered = append(ordered, struct {
			key string
			val map[int]*QualityRow
		}{k, v})
	}
	sort.Slice(ordered, func(i, j int) bool {
		return maxUpdateTime(ordered[i].val).After(maxUpdateTime(ordered[j].val))
	})
	if take != -1 {
		if page <= 0 {
			page = 1
		}
		if take <= 0 {
			take = 1000
		}
		skip := (page - 1) * take
		if skip > len(ordered) {
			skip = len(ordered)
		}
		end := skip + take
		if end > len(ordered) {
			end = len(ordered)
		}
		ordered = ordered[skip:end]
	}
	out := map[string]map[int]QualityRowJSON{}
	for _, item := range ordered {
		inner := map[int]QualityRowJSON{}
		for year, row := range item.val {
			inner[year] = QualityRowJSON{Qualitys: sortedIntKeys(row.Qualitys), Types: sortedStringKeys(row.Types), Languages: sortedStringKeys(row.Languages), CreateTime: row.CreateTime, UpdateTime: row.UpdateTime}
		}
		out[item.key] = inner
	}
	return out, nil
}

func qualityEligible(t TorrentDetails, typ string) bool {
	types := asStringSlice(t["types"])
	if len(types) == 0 || containsString(types, "sport") || asInt(t["relased"]) == 0 {
		return false
	}
	if typ != "" && !containsString(types, typ) {
		return false
	}
	return true
}

func maxUpdateTime(m map[int]*QualityRow) time.Time {
	var out time.Time
	for _, v := range m {
		if v.UpdateTime.After(out) {
			out = v.UpdateTime
		}
	}
	return out
}

func mapCategory(isSerial int, cat string) int {
	if isSerial != 0 || cat == "" {
		return isSerial
	}
	if strings.Contains(cat, "5020") || strings.Contains(cat, "2010") {
		return 3
	}
	if strings.Contains(cat, "5080") {
		return 4
	}
	if strings.Contains(cat, "5070") {
		return 5
	}
	if strings.HasPrefix(cat, "20") {
		return 1
	}
	if strings.HasPrefix(cat, "50") {
		return 2
	}
	return isSerial
}

var (
	rqnum3         = regexp.MustCompile(`^([^a-zA-Z]+) ([^а-яА-Я]+) ([0-9]{4})$`)
	rqnum2YearOnly = regexp.MustCompile(`^([^a-zA-Z]+) ((19|20)[0-9]{2})$`)
	rqnum2         = regexp.MustCompile(`^([^a-zA-Z]+) ([^а-яА-Я]+)$`)
	rqnumToken     = regexp.MustCompile(`[a-zA-Z0-9]{2}`)
	seasonLike     = regexp.MustCompile(`(?i) (сезон|сери(и|я|й))`)
)

func parseRqNum(query string) (title, titleOriginal string, year int, ok, reject bool) {
	if m := rqnum3.FindStringSubmatch(query); len(m) > 0 {
		if rqnumToken.MatchString(m[2]) {
			y, _ := strconv.Atoi(m[3])
			return m[1], m[2], y, true, false
		}
	}
	if rqnum2YearOnly.MatchString(query) {
		return "", "", 0, false, true
	}
	if m := rqnum2.FindStringSubmatch(query); len(m) > 0 {
		if rqnumToken.MatchString(m[2]) {
			return m[1], m[2], 0, true, false
		}
	}
	return "", "", 0, false, false
}

func matchJackettExact(t TorrentDetails, isSerial, year int) bool {
	types := asStringSlice(t["types"])
	has := func(want ...string) bool { return hasAny(types, want...) }
	released := asInt(t["relased"])
	switch isSerial {
	case 1:
		if !has("movie", "multfilm", "anime", "documovie") || seasonLike.MatchString(asString(t["title"])) {
			return false
		}
		if year > 0 {
			return released == year || released == year-1 || released == year+1
		}
		return true
	case 2:
		if !has("serial", "multserial", "anime", "docuserial", "tvshow") {
			return false
		}
		if year > 0 {
			return released >= year-1
		}
		return true
	case 3:
		if !has("tvshow") {
			return false
		}
		if year > 0 {
			return released >= year-1
		}
		return true
	case 4:
		if !has("docuserial", "documovie") {
			return false
		}
		if year > 0 {
			return released >= year-1
		}
		return true
	case 5:
		if !has("anime") {
			return false
		}
		if year > 0 {
			return released >= year-1
		}
		return true
	default:
		if year > 0 {
			if has("movie", "multfilm", "documovie") {
				return released == year || released == year-1 || released == year+1
			}
			return released >= year-1
		}
		return true
	}
}

func matchJackettSearch(t TorrentDetails, isSerial, year int) bool {
	types := asStringSlice(t["types"])
	has := func(want ...string) bool { return hasAny(types, want...) }
	released := asInt(t["relased"])
	switch isSerial {
	case 1:
		if !has("movie", "multfilm", "anime", "documovie") {
			return false
		}
		if year > 0 {
			return released == year || released == year-1 || released == year+1
		}
		return true
	case 2:
		if !has("serial", "multserial", "anime", "docuserial", "tvshow") {
			return false
		}
		if year > 0 {
			return released >= year-1
		}
		return true
	case 3:
		if !has("tvshow") {
			return false
		}
		if year > 0 {
			return released >= year-1
		}
		return true
	case 4:
		if !has("docuserial", "documovie") {
			return false
		}
		if year > 0 {
			return released >= year-1
		}
		return true
	case 5:
		if !has("anime") {
			return false
		}
		if year > 0 {
			return released >= year-1
		}
		return true
	default:
		return true
	}
}

func orderTorrentValues(m map[string]TorrentDetails) []TorrentDetails {
	out := make([]TorrentDetails, 0, len(m))
	for _, t := range m {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		ti := torrentTime(out[i], "createTime")
		tj := torrentTime(out[j], "createTime")
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		isi := strings.EqualFold(asString(out[i]["trackerName"]), "selezen")
		isj := strings.EqualFold(asString(out[j]["trackerName"]), "selezen")
		if isi != isj {
			return !isi
		}
		return asString(out[i]["trackerName"]) < asString(out[j]["trackerName"])
	})
	return out
}

func mergeDuplicateMagnets(items []TorrentDetails) []TorrentDetails {
	temp := map[string]struct {
		torrent      TorrentDetails
		title        string
		name         string
		announceURLs []string
	}{}
	for _, torrent := range items {
		hex, name, announces := parseMagnet(asString(torrent["magnet"]))
		if hex == "" {
			continue
		}
		if _, ok := temp[hex]; !ok {
			entry := struct {
				torrent      TorrentDetails
				title        string
				name         string
				announceURLs []string
			}{torrent: cloneTorrent(torrent), name: name, announceURLs: append([]string{}, announces...)}
			if strings.EqualFold(asString(torrent["trackerName"]), "kinozal") {
				entry.title = asString(torrent["title"])
			}
			temp[hex] = entry
			continue
		}
		entry := temp[hex]
		trackerName := asString(entry.torrent["trackerName"])
		curTracker := asString(torrent["trackerName"])
		if !strings.Contains(trackerName, curTracker) {
			entry.torrent["trackerName"] = trackerName + ", " + curTracker
		}
		if entry.name == "" && name != "" {
			entry.name = name
			entry.torrent["magnet"] = buildMagnet(hex, entry.name, entry.announceURLs)
		}
		if len(announces) > 0 {
			entry.announceURLs = append(entry.announceURLs, announces...)
			entry.torrent["magnet"] = buildMagnet(hex, entry.name, entry.announceURLs)
		}
		if strings.EqualFold(curTracker, "kinozal") {
			entry.title = asString(torrent["title"])
		}
		mergeStringSliceField(entry.torrent, torrent, "voices")
		mergeStringSliceField(entry.torrent, torrent, "languages")
		if !strings.EqualFold(curTracker, "selezen") {
			if asInt(torrent["sid"]) > asInt(entry.torrent["sid"]) {
				entry.torrent["sid"] = asInt(torrent["sid"])
			}
			if asInt(torrent["pir"]) > asInt(entry.torrent["pir"]) {
				entry.torrent["pir"] = asInt(torrent["pir"])
			}
		}
		if torrentTime(torrent, "createTime").After(torrentTime(entry.torrent, "createTime")) {
			entry.torrent["createTime"] = torrent["createTime"]
		}
		updateMergedTitle(entry.torrent, entry.title)
		temp[hex] = entry
	}
	out := make([]TorrentDetails, 0, len(temp))
	for _, v := range temp {
		out = append(out, v.torrent)
	}
	return out
}

func updateMergedTitle(t TorrentDetails, title string) {
	if strings.TrimSpace(title) == "" {
		return
	}
	result := title
	voices := asStringSlice(t["voices"])
	if len(voices) > 0 {
		result += " | " + strings.Join(uniqueStrings(voices), " | ")
	}
	t["title"] = result
}

func mergeStringSliceField(dst, src TorrentDetails, key string) {
	existing := asStringSlice(dst[key])
	incoming := asStringSlice(src[key])
	if len(incoming) == 0 {
		return
	}
	dst[key] = uniqueStrings(append(existing, incoming...))
}

func parseMagnet(raw string) (hex, name string, announces []string) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", nil
	}
	q := u.Query()
	for _, xt := range q["xt"] {
		if strings.HasPrefix(strings.ToLower(xt), "urn:btih:") {
			hex = strings.ToUpper(strings.TrimSpace(xt[len("urn:btih:"):]))
			break
		}
	}
	name = q.Get("dn")
	announces = uniqueStrings(append([]string{}, q["tr"]...))
	return
}

func buildMagnet(hex, name string, announces []string) string {
	if hex == "" {
		return ""
	}
	magnet := "magnet:?xt=urn:btih:" + strings.ToLower(hex)
	if name != "" {
		magnet += "&dn=" + url.QueryEscape(name)
	}
	for _, announce := range uniqueStrings(announces) {
		if announce == "" {
			continue
		}
		magnet += "&tr=" + url.QueryEscape(announce)
	}
	return magnet
}

func filterTorrentResults(items []TorrentDetails, p TorrentsParams) []TorrentDetails {
	filtered := make([]TorrentDetails, 0, len(items))
	for _, item := range items {
		if p.Tracker != "" && asString(item["trackerName"]) != p.Tracker {
			continue
		}
		if p.Relased > 0 && asInt(item["relased"]) != p.Relased {
			continue
		}
		if p.Quality > 0 && asInt(item["quality"]) != p.Quality {
			continue
		}
		if p.VideoType != "" && asString(item["videotype"]) != p.VideoType {
			continue
		}
		if p.Voice != "" && !containsString(asStringSlice(item["voices"]), p.Voice) {
			continue
		}
		if p.Season > 0 && !containsInt(anyToIntSlice(item["seasons"]), p.Season) {
			continue
		}
		filtered = append(filtered, item)
	}
	switch p.Sort {
	case "sid":
		sort.Slice(filtered, func(i, j int) bool { return asInt(filtered[i]["sid"]) > asInt(filtered[j]["sid"]) })
	case "pir":
		sort.Slice(filtered, func(i, j int) bool { return asInt(filtered[i]["pir"]) > asInt(filtered[j]["pir"]) })
	case "size":
		sort.Slice(filtered, func(i, j int) bool { return asFloat(filtered[i]["size"]) > asFloat(filtered[j]["size"]) })
	case "create":
		sort.Slice(filtered, func(i, j int) bool {
			return torrentTime(filtered[i], "createTime").After(torrentTime(filtered[j], "createTime"))
		})
	case "update":
		sort.Slice(filtered, func(i, j int) bool {
			return torrentTime(filtered[i], "updateTime").After(torrentTime(filtered[j], "updateTime"))
		})
	}
	return filtered
}

func (db *DB) trackerAllowed(name string) bool {
	if len(db.GetConfig().SyncTrackers) > 0 && !containsStringFold(db.GetConfig().SyncTrackers, name) {
		return false
	}
	if len(db.GetConfig().DisableTrackers) > 0 && containsStringFold(db.GetConfig().DisableTrackers, name) {
		return false
	}
	return true
}

func (db *DB) limitReads() bool {
	return !db.GetConfig().Evercache.Enable || db.GetConfig().Evercache.ValidHour > 0
}

func (db *DB) limitKeysMap(keys map[string]struct{}) []string {
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	if db.limitReads() && len(out) > db.GetConfig().MaxReadFile {
		out = out[:db.GetConfig().MaxReadFile]
	}
	return out
}

func containsString(vals []string, want string) bool {
	for _, v := range vals {
		if v == want {
			return true
		}
	}
	return false
}
func containsStringFold(vals []string, want string) bool {
	for _, v := range vals {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}
func containsInt(vals []int, want int) bool {
	for _, v := range vals {
		if v == want {
			return true
		}
	}
	return false
}
func uniqueStrings(vals []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
func sortedIntKeys(m map[int]struct{}) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
func sortedStringKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func makeStringSet(vals []string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, v := range vals {
		if v != "" {
			out[v] = struct{}{}
		}
	}
	return out
}
func hasAny(hay []string, want ...string) bool {
	set := makeStringSet(hay)
	for _, v := range want {
		if _, ok := set[v]; ok {
			return true
		}
	}
	return false
}
func anyToIntSlice(v any) []int {
	switch x := v.(type) {
	case []any:
		out := make([]int, 0, len(x))
		for _, it := range x {
			out = append(out, asInt(it))
		}
		return out
	case []int:
		return x
	default:
		return nil
	}
}
func asFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	default:
		return 0
	}
}
func cloneTorrent(src TorrentDetails) TorrentDetails {
	dst := TorrentDetails{}
	for k, v := range src {
		switch vv := v.(type) {
		case []any:
			cp := make([]any, len(vv))
			copy(cp, vv)
			dst[k] = cp
		case []string:
			cp := make([]string, len(vv))
			copy(cp, vv)
			dst[k] = cp
		case []int:
			cp := make([]int, len(vv))
			copy(cp, vv)
			dst[k] = cp
		default:
			dst[k] = v
		}
	}
	return dst
}
