package app

type LoginSettings struct {
	U string `json:"u"`
	P string `json:"p"`
}

type ProxySettings struct {
	Pattern       string   `json:"pattern"`
	UseAuth       bool     `json:"useAuth"`
	BypassOnLocal bool     `json:"BypassOnLocal"`
	Username      string   `json:"username"`
	Password      string   `json:"password"`
	List          []string `json:"list"`
}

type Evercache struct {
	Enable           bool `json:"enable"`
	ValidHour        int  `json:"validHour"`
	MaxOpenWriteTask int  `json:"maxOpenWriteTask"`
	DropCacheTake    int  `json:"dropCacheTake"`
}

type TracksIntervalConfig struct {
	Task0 int `json:"task0"`
	Task1 int `json:"task1"`
}

type TrackerSettings struct {
	Host       string        `json:"host"`
	Alias      string        `json:"alias,omitempty"`
	Cookie     string        `json:"cookie,omitempty"`
	Log        bool          `json:"log"`
	UseProxy   bool          `json:"useproxy"`
	ReqMinute  int           `json:"reqMinute"`
	ParseDelay int           `json:"parseDelay"`
	Login      LoginSettings `json:"login"`
}

type CFClientConfig struct {
	Profile   string `json:"profile"`   // TLS profile: chrome_144, firefox_117, chrome_133, etc.
	UserAgent string `json:"useragent"` // Custom User-Agent string
}

type Config struct {
	ListenIP           string               `json:"listenip"`
	ListenPort         int                  `json:"listenport"`
	APIKey             string               `json:"apikey,omitempty"`
	DevKey             string               `json:"devkey,omitempty"`
	MergeDuplicates    bool                 `json:"mergeduplicates"`
	MergeNumDuplicates bool                 `json:"mergenumduplicates"`
	Log                bool                 `json:"log"`
	LogParsers         bool                 `json:"logParsers"`
	LogFdb             bool                 `json:"logFdb"`
	LogFdbRetentionDays int                 `json:"logFdbRetentionDays"`
	LogFdbMaxSizeMb    int                  `json:"logFdbMaxSizeMb"`
	LogFdbMaxFiles     int                  `json:"logFdbMaxFiles"`
	FDBPathLevels      int                  `json:"fdbPathLevels"`
	OpenStats          bool                 `json:"openstats"`
	OpenSync           bool                 `json:"opensync"`
	OpenSyncV1         bool                 `json:"opensync_v1"`
	Web                bool                 `json:"web"`
	Tracks             bool                 `json:"tracks"`
	TracksMod          int                  `json:"tracksmod"`
	TracksDelay        int                  `json:"tracksdelay"`
	TracksLog          bool                 `json:"trackslog"`
	TracksAttempt      int                  `json:"tracksatempt"`
	TracksCategory     string               `json:"trackscategory"`
	TracksInterval     TracksIntervalConfig `json:"tracksinterval"`
	TSURI              []string             `json:"tsuri"`
	SyncAPI            string               `json:"syncapi,omitempty"`
	SyncTrackers       []string             `json:"synctrackers,omitempty"`
	DisableTrackers    []string             `json:"disable_trackers,omitempty"`
	SyncSport          bool                 `json:"syncsport"`
	SyncSpidr          bool                 `json:"syncspidr"`
	MaxReadFile        int                  `json:"maxreadfile"`
	Evercache          Evercache            `json:"evercache"`
	TimeStatsUpdate    int                  `json:"timeStatsUpdate"`
	TimeSync           int                  `json:"timeSync"`
	TimeSyncSpidr      int                  `json:"timeSyncSpidr"`
	CFClient           CFClientConfig       `json:"cfclient"`
	Rutor              TrackerSettings      `json:"Rutor"`
	Megapeer           TrackerSettings      `json:"Megapeer"`
	TorrentBy          TrackerSettings      `json:"TorrentBy"`
	Kinozal            TrackerSettings      `json:"Kinozal"`
	NNMClub            TrackerSettings      `json:"NNMClub"`
	Bitru              TrackerSettings      `json:"Bitru"`
	Toloka             TrackerSettings      `json:"Toloka"`
	Mazepa             TrackerSettings      `json:"Mazepa"`
	Rutracker          TrackerSettings      `json:"Rutracker"`
	Selezen            TrackerSettings      `json:"Selezen"`
	Lostfilm           TrackerSettings      `json:"Lostfilm"`
	Animelayer         TrackerSettings      `json:"Animelayer"`
	Anidub             TrackerSettings      `json:"Anidub"`
	Aniliberty         TrackerSettings      `json:"Aniliberty"`
	Knaben             TrackerSettings      `json:"Knaben"`
	Anistar            TrackerSettings      `json:"Anistar"`
	Anifilm            TrackerSettings      `json:"Anifilm"`
	Leproduction       TrackerSettings      `json:"Leproduction"`
	Baibako            TrackerSettings      `json:"Baibako"`
	GlobalProxy        []ProxySettings      `json:"globalproxy"`
}

func DefaultConfig() Config {
	return Config{
		ListenIP:           "any",
		ListenPort:         9117,
		MergeDuplicates:    true,
		MergeNumDuplicates: true,
		FDBPathLevels:      2,
		OpenStats:          true,
		OpenSync:           true,
		OpenSyncV1:         false,
		Web:                true,
		Tracks:             false,
		TracksDelay:        20000,
		TracksAttempt:      20,
		TracksCategory:     "jacred",
		TracksInterval:     TracksIntervalConfig{Task0: 180, Task1: 60},
		TSURI:              []string{"http://127.0.0.1:8090"},
		DisableTrackers:    []string{},
		SyncSport:          true,
		SyncSpidr:          true,
		MaxReadFile:        200,
		Evercache:          Evercache{Enable: true, ValidHour: 1, MaxOpenWriteTask: 2000, DropCacheTake: 200},
		TimeStatsUpdate:    90,
		TimeSync:           60,
		TimeSyncSpidr:      60,
		CFClient:           CFClientConfig{Profile: "chrome_146", UserAgent: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"},
		LogFdbRetentionDays: 7,
		Rutor:              TrackerSettings{Host: "https://rutor.is", ReqMinute: 8, ParseDelay: 7000},
		Megapeer:           TrackerSettings{Host: "https://megapeer.vip", ReqMinute: 8, ParseDelay: 7000},
		TorrentBy:          TrackerSettings{Host: "https://torrent.by", ReqMinute: 8, ParseDelay: 7000},
		Kinozal:            TrackerSettings{Host: "https://kinozal.tv", ReqMinute: 8, ParseDelay: 7000},
		NNMClub:            TrackerSettings{Host: "https://nnmclub.to", ReqMinute: 8, ParseDelay: 7000},
		Bitru:              TrackerSettings{Host: "https://bitru.org", ReqMinute: 8, ParseDelay: 7000},
		Toloka:             TrackerSettings{Host: "https://toloka.to", ReqMinute: 8, ParseDelay: 7000},
		Mazepa:             TrackerSettings{Host: "https://mazepa.to", ReqMinute: 8, ParseDelay: 7000},
		Rutracker:          TrackerSettings{Host: "https://rutracker.org", ReqMinute: 8, ParseDelay: 7000},
		Selezen:            TrackerSettings{Host: "https://use.selezen.club", ReqMinute: 8, ParseDelay: 7000},
		Lostfilm:           TrackerSettings{Host: "https://www.lostfilm.tv", ReqMinute: 8, ParseDelay: 7000},
		Animelayer:         TrackerSettings{Host: "https://animelayer.ru", ReqMinute: 8, ParseDelay: 7000},
		Anidub:             TrackerSettings{Host: "https://tr.anidub.com", ReqMinute: 8, ParseDelay: 7000},
		Aniliberty:         TrackerSettings{Host: "https://aniliberty.top", ReqMinute: 8, ParseDelay: 7000},
		Knaben:             TrackerSettings{Host: "https://api.knaben.org", ReqMinute: 8, ParseDelay: 7000},
		Anistar:            TrackerSettings{Host: "https://anistar.org", ReqMinute: 8, ParseDelay: 7000},
		Anifilm:            TrackerSettings{Host: "https://anifilm.pro", ReqMinute: 8, ParseDelay: 7000},
		Leproduction:       TrackerSettings{Host: "https://www.le-production.tv", ReqMinute: 8, ParseDelay: 7000},
		Baibako:            TrackerSettings{Host: "http://baibako.tv", ReqMinute: 8, ParseDelay: 7000},
		GlobalProxy:        []ProxySettings{{Pattern: `\.onion`, List: []string{"socks5://127.0.0.1:9050"}}},
	}
}
