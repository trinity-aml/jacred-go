package filedb

// TorrentRecord is the typed replacement for TorrentDetails (map[string]any).
// JSON tags match existing map keys for backward compatibility with .gz files.
type TorrentRecord struct {
	TrackerName  string   `json:"trackerName,omitempty"`
	Types        []string `json:"types,omitempty"`
	URL          string   `json:"url,omitempty"`
	Title        string   `json:"title,omitempty"`
	Sid          int      `json:"sid,omitempty"`
	Pir          int      `json:"pir,omitempty"`
	SizeName     string   `json:"sizeName,omitempty"`
	Size         int64    `json:"size,omitempty"`
	Magnet       string   `json:"magnet,omitempty"`
	CreateTime   string   `json:"createTime,omitempty"`
	UpdateTime   string   `json:"updateTime,omitempty"`
	Name         string   `json:"name,omitempty"`
	OriginalName string   `json:"originalname,omitempty"`
	Relased      int      `json:"relased,omitempty"`
	DownloadID   string   `json:"downloadId,omitempty"`
	SearchName   string   `json:"_sn,omitempty"`
	SearchOrig   string   `json:"_so,omitempty"`
	Quality      int      `json:"quality,omitempty"`
	VideoType    string   `json:"videotype,omitempty"`
	Voices       string   `json:"voices,omitempty"`
	Seasons      string   `json:"seasons,omitempty"`
	Languages    string   `json:"languages,omitempty"`
	DownloadURI  string   `json:"_downloadURI,omitempty"`
	TID          string   `json:"_tid,omitempty"`
	FFProbe      any      `json:"ffprobe,omitempty"`
}

// ToMap converts TorrentRecord to legacy TorrentDetails map.
func (r TorrentRecord) ToMap() TorrentDetails {
	m := TorrentDetails{}
	if r.TrackerName != "" {
		m["trackerName"] = r.TrackerName
	}
	if len(r.Types) > 0 {
		m["types"] = r.Types
	}
	if r.URL != "" {
		m["url"] = r.URL
	}
	if r.Title != "" {
		m["title"] = r.Title
	}
	if r.Sid != 0 {
		m["sid"] = r.Sid
	}
	if r.Pir != 0 {
		m["pir"] = r.Pir
	}
	if r.SizeName != "" {
		m["sizeName"] = r.SizeName
	}
	if r.Size != 0 {
		m["size"] = r.Size
	}
	if r.Magnet != "" {
		m["magnet"] = r.Magnet
	}
	if r.CreateTime != "" {
		m["createTime"] = r.CreateTime
	}
	if r.UpdateTime != "" {
		m["updateTime"] = r.UpdateTime
	}
	if r.Name != "" {
		m["name"] = r.Name
	}
	if r.OriginalName != "" {
		m["originalname"] = r.OriginalName
	}
	if r.Relased != 0 {
		m["relased"] = r.Relased
	}
	if r.DownloadID != "" {
		m["downloadId"] = r.DownloadID
	}
	if r.SearchName != "" {
		m["_sn"] = r.SearchName
	}
	if r.SearchOrig != "" {
		m["_so"] = r.SearchOrig
	}
	if r.Quality != 0 {
		m["quality"] = r.Quality
	}
	if r.VideoType != "" {
		m["videotype"] = r.VideoType
	}
	if r.Voices != "" {
		m["voices"] = r.Voices
	}
	if r.Seasons != "" {
		m["seasons"] = r.Seasons
	}
	if r.Languages != "" {
		m["languages"] = r.Languages
	}
	if r.DownloadURI != "" {
		m["_downloadURI"] = r.DownloadURI
	}
	if r.TID != "" {
		m["_tid"] = r.TID
	}
	if r.FFProbe != nil {
		m["ffprobe"] = r.FFProbe
	}
	return m
}

// RecordFromMap converts legacy TorrentDetails map to TorrentRecord.
func RecordFromMap(m TorrentDetails) TorrentRecord {
	return TorrentRecord{
		TrackerName:  asString(m["trackerName"]),
		Types:        asStringSlice(m["types"]),
		URL:          asString(m["url"]),
		Title:        asString(m["title"]),
		Sid:          asInt(m["sid"]),
		Pir:          asInt(m["pir"]),
		SizeName:     asString(m["sizeName"]),
		Size:         asInt64(m["size"]),
		Magnet:       asString(m["magnet"]),
		CreateTime:   asString(m["createTime"]),
		UpdateTime:   asString(m["updateTime"]),
		Name:         asString(m["name"]),
		OriginalName: asString(m["originalname"]),
		Relased:      asInt(m["relased"]),
		DownloadID:   asString(m["downloadId"]),
		SearchName:   asString(m["_sn"]),
		SearchOrig:   asString(m["_so"]),
		Quality:      asInt(m["quality"]),
		VideoType:    asString(m["videotype"]),
		Voices:       asString(m["voices"]),
		Seasons:      asString(m["seasons"]),
		Languages:    asString(m["languages"]),
		DownloadURI:  asString(m["_downloadURI"]),
		TID:          asString(m["_tid"]),
		FFProbe:      m["ffprobe"],
	}
}

func asFloat64(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	default:
		return 0
	}
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	default:
		return 0
	}
}

