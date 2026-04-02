package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"jacred/app"
	"jacred/cron/anidub"
	"jacred/cron/anifilm"
	"jacred/cron/aniliberty"
	"jacred/cron/animelayer"
	"jacred/cron/anistar"
	"jacred/cron/baibako"
	"jacred/cron/bitru"
	"jacred/cron/bitruapi"
	"jacred/cron/kinozal"
	"jacred/cron/knaben"
	"jacred/cron/leproduction"
	"jacred/cron/lostfilm"
	"jacred/cron/mazepa"
	"jacred/cron/megapeer"
	"jacred/cron/nnmclub"
	"jacred/cron/rutor"
	"jacred/cron/rutracker"
	"jacred/cron/selezen"
	"jacred/cron/toloka"
	"jacred/cron/torrentby"
	"jacred/filedb"
	"jacred/tracks"
)

type VersionInfo struct {
	Version   string `json:"version"`
	GitSha    string `json:"gitSha"`
	GitBranch string `json:"gitBranch"`
	BuildDate string `json:"buildDate"`
}

type Server struct {
	Config             app.Config
	DB                 *filedb.DB
	WWWRoot            string
	Version            VersionInfo
	KnabenParser       *knaben.Parser
	AnidubParser       *anidub.Parser
	AnilibertyParser   *aniliberty.Parser
	AnimelayerParser   *animelayer.Parser
	AnistarParser      *anistar.Parser
	AnifilmParser      *anifilm.Parser
	BaibakoParser      *baibako.Parser
	BitruParser        *bitru.Parser
	BitruAPIParser     *bitruapi.Parser
	RutorParser        *rutor.Parser
	MegapeerParser     *megapeer.Parser
	TorrentByParser    *torrentby.Parser
	NNMClubParser      *nnmclub.Parser
	LostfilmParser     *lostfilm.Parser
	RutrackerParser    *rutracker.Parser
	KinozalParser      *kinozal.Parser
	TolokaParser       *toloka.Parser
	SelezenParser      *selezen.Parser
	LeproductionParser *leproduction.Parser
	MazepaParser       *mazepa.Parser
	TracksDB           *tracks.DB
}

func New(cfg app.Config, db *filedb.DB, tracksDB *tracks.DB, wwwroot string) *Server {
	if tracksDB == nil {
		tracksDB = tracks.New("Data")
		_ = tracksDB.Load()
	}
	return &Server{Config: cfg, DB: db, WWWRoot: wwwroot, Version: VersionInfo{Version: "dev", GitSha: "unknown", GitBranch: "unknown", BuildDate: time.Now().UTC().Format("2006-01-02 15:04:05 UTC")}, KnabenParser: knaben.New(cfg, db), AnidubParser: anidub.New(cfg, db), AnilibertyParser: aniliberty.New(cfg, db), AnimelayerParser: animelayer.New(cfg, db), AnistarParser: anistar.New(cfg, db, "Data"), AnifilmParser: anifilm.New(cfg, db, "Data"), BaibakoParser: baibako.New(cfg, db, "Data"), BitruParser: bitru.New(cfg, db, "Data"), BitruAPIParser: bitruapi.New(cfg, db, "Data"), RutorParser: rutor.New(cfg, db, "Data"), MegapeerParser: megapeer.New(cfg, db), TorrentByParser: torrentby.New(cfg, db, "Data"), NNMClubParser: nnmclub.New(cfg, db, "Data"), LostfilmParser: lostfilm.New(cfg, db), RutrackerParser: rutracker.New(cfg, db, "Data"), KinozalParser: kinozal.New(cfg, db, "Data"), TolokaParser: toloka.New(cfg, db, "Data"), SelezenParser: selezen.New(cfg, db, "Data"), LeproductionParser: leproduction.New(cfg, db, "Data"), MazepaParser: mazepa.New(cfg, db, "Data"), TracksDB: tracksDB}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/stats", s.handleStats)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/version", s.handleVersion)
	mux.HandleFunc("/lastupdatedb", s.handleLastUpdateDB)
	mux.HandleFunc("/api/v1.0/conf", s.handleConf)
	mux.HandleFunc("/api/v1.0/torrents", s.handleTorrents)
	mux.HandleFunc("/api/v1.0/qualitys", s.handleQualitys)
	mux.HandleFunc("/api/v2.0/indexers/", s.handleJackett)
	mux.HandleFunc("/stats/trackers", s.handleStatsTrackers)
	mux.HandleFunc("/stats/trackers/", s.handleStatsTrackers)
	mux.HandleFunc("/stats/torrents", s.handleStatsTorrentsEx)
	mux.HandleFunc("/sync/conf", s.handleSyncConf)
	mux.HandleFunc("/sync/fdb", s.handleSyncFdb)
	mux.HandleFunc("/sync/fdb/torrents", s.handleSyncFdbTorrents)
	mux.HandleFunc("/sync/torrents", s.handleSyncTorrents)
	mux.HandleFunc("/sync/tracks", s.handleSyncTracks)
	mux.HandleFunc("/sync/tracks/check", s.handleSyncTracksCheck)
	mux.HandleFunc("/jsondb/save", s.handleJSONDBSave)
	mux.HandleFunc("/dev/findcorrupt", s.handleDevFindCorrupt)
	mux.HandleFunc("/dev/updatesize", s.handleDevUpdateSize)
	mux.HandleFunc("/dev/updatesearchname", s.handleDevUpdateSearchName)
	mux.HandleFunc("/dev/removenullvalues", s.handleDevRemoveNullValues)
	mux.HandleFunc("/dev/findduplicatekeys", s.handleDevFindDuplicateKeys)
	mux.HandleFunc("/dev/findemptysearchfields", s.handleDevFindEmptySearchFields)
	mux.HandleFunc("/dev/resetchecktime", s.handleDevResetCheckTime)
	mux.HandleFunc("/dev/updatedetails", s.handleDevUpdateDetails)
	mux.HandleFunc("/dev/fixknabennames", s.handleDevFixKnabenNames)
	mux.HandleFunc("/dev/fixbitrunames", s.handleDevFixBitruNames)
	mux.HandleFunc("/dev/removebucket", s.handleDevRemoveBucket)
	mux.HandleFunc("/dev/fixemptysearchfields", s.handleDevFixEmptySearchFields)
	mux.HandleFunc("/dev/migrateanilibertyurls", s.handleDevMigrateAnilibertyUrls)
	mux.HandleFunc("/dev/removeduplicateaniliberty", s.handleDevRemoveDuplicateAniliberty)
	mux.HandleFunc("/dev/fixanimelayerduplicates", s.handleDevFixAnimelayerDuplicates)
	mux.HandleFunc("/cron/knaben/parse", s.handleCronKnabenParse)
	mux.HandleFunc("/stats/refresh", s.handleStatsRefresh)
	mux.HandleFunc("/cron/anidub/parse", s.handleCronAnidubParse)
	mux.HandleFunc("/cron/aniliberty/parse", s.handleCronAnilibertyParse)
	mux.HandleFunc("/cron/animelayer/parse", s.handleCronAnimelayerParse)
	mux.HandleFunc("/cron/bitruapi/parse", s.handleCronBitruAPIParse)
	mux.HandleFunc("/cron/bitruapi/parsefromdate", s.handleCronBitruAPIParseFromDate)
	mux.HandleFunc("/cron/bitru/parse", s.handleCronBitruParse)
	mux.HandleFunc("/cron/bitru/updatetasksparse", s.handleCronBitruUpdateTasksParse)
	mux.HandleFunc("/cron/bitru/parsealltask", s.handleCronBitruParseAllTask)
	mux.HandleFunc("/cron/bitru/parselatest", s.handleCronBitruParseLatest)
	mux.HandleFunc("/cron/rutor/parse", s.handleCronRutorParse)
	mux.HandleFunc("/cron/rutor/updatetasksparse", s.handleCronRutorUpdateTasksParse)
	mux.HandleFunc("/cron/rutor/parsealltask", s.handleCronRutorParseAllTask)
	mux.HandleFunc("/cron/rutor/parselatest", s.handleCronRutorParseLatest)
	mux.HandleFunc("/cron/megapeer/parse", s.handleCronMegapeerParse)
	mux.HandleFunc("/cron/torrentby/parse", s.handleCronTorrentByParse)
	mux.HandleFunc("/cron/torrentby/updatetasksparse", s.handleCronTorrentByUpdateTasksParse)
	mux.HandleFunc("/cron/torrentby/parsealltask", s.handleCronTorrentByParseAllTask)
	mux.HandleFunc("/cron/torrentby/parselatest", s.handleCronTorrentByParseLatest)
	mux.HandleFunc("/cron/nnmclub/parse", s.handleCronNNMClubParse)
	mux.HandleFunc("/cron/nnmclub/updatetasksparse", s.handleCronNNMClubUpdateTasksParse)
	mux.HandleFunc("/cron/nnmclub/parsealltask", s.handleCronNNMClubParseAllTask)
	mux.HandleFunc("/cron/nnmclub/parselatest", s.handleCronNNMClubParseLatest)
	mux.HandleFunc("/cron/rutracker/parse", s.handleCronRutrackerParse)
	mux.HandleFunc("/cron/rutracker/updatetasksparse", s.handleCronRutrackerUpdateTasksParse)
	mux.HandleFunc("/cron/rutracker/parsealltask", s.handleCronRutrackerParseAllTask)
	mux.HandleFunc("/cron/rutracker/parselatest", s.handleCronRutrackerParseLatest)
	mux.HandleFunc("/cron/lostfilm/parse", s.handleCronLostfilmParse)
	mux.HandleFunc("/cron/lostfilm/parsepages", s.handleCronLostfilmParsePages)
	mux.HandleFunc("/cron/lostfilm/parseseasonpacks", s.handleCronLostfilmParseSeasonPacks)
	mux.HandleFunc("/cron/lostfilm/verifypage", s.handleCronLostfilmVerifyPage)
	mux.HandleFunc("/cron/lostfilm/stats", s.handleCronLostfilmStats)
	mux.HandleFunc("/cron/kinozal/parse", s.handleCronKinozalParse)
	mux.HandleFunc("/cron/kinozal/updatetasksparse", s.handleCronKinozalUpdateTasksParse)
	mux.HandleFunc("/cron/kinozal/parsealltask", s.handleCronKinozalParseAllTask)
	mux.HandleFunc("/cron/kinozal/parselatest", s.handleCronKinozalParseLatest)
	mux.HandleFunc("/cron/toloka/parse", s.handleCronTolokaParse)
	mux.HandleFunc("/cron/toloka/updatetasksparse", s.handleCronTolokaUpdateTasksParse)
	mux.HandleFunc("/cron/toloka/parsealltask", s.handleCronTolokaParseAllTask)
	mux.HandleFunc("/cron/toloka/parselatest", s.handleCronTolokaParseLatest)
	mux.HandleFunc("/cron/selezen/parse", s.handleCronSelezenParse)
	mux.HandleFunc("/cron/selezen/updatetasksparse", s.handleCronSelezenUpdateTasksParse)
	mux.HandleFunc("/cron/selezen/parsealltask", s.handleCronSelezenParseAllTask)
	mux.HandleFunc("/cron/selezen/parselatest", s.handleCronSelezenParseLatest)
	mux.HandleFunc("/cron/anistar/parse", s.handleCronAnistarParse)
	mux.HandleFunc("/cron/anifilm/parse", s.handleCronAnifilmParse)
	mux.HandleFunc("/cron/baibako/parse", s.handleCronBaibakoParse)
	mux.HandleFunc("/cron/leproduction/parse", s.handleCronLeproductionParse)
	mux.HandleFunc("/cron/mazepa/parse", s.handleCronMazepaParse)
	mux.HandleFunc("/cron/mazepa/updatetasksparse", s.handleCronMazepaUpdateTasksParse)
	mux.HandleFunc("/cron/mazepa/parsealltask", s.handleCronMazepaParseAllTask)
	mux.HandleFunc("/cron/mazepa/parselatest", s.handleCronMazepaParseLatest)
	return s.middleware(mux)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		s.serveMaybeStatic(w, r)
		return
	}
	s.serveHTMLFile(w, r, "index.html")
}
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/stats" && r.URL.Path != "/stats/" {
		s.serveMaybeStatic(w, r)
		return
	}
	s.serveHTMLFile(w, r, "stats.html")
}
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeCanonicalJSON(w, http.StatusOK, map[string]string{"status": "OK"})
}
func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeCanonicalJSON(w, http.StatusOK, s.Version)
}
func (s *Server) handleLastUpdateDB(w http.ResponseWriter, _ *http.Request) {
	writeJSONOrdered(w, http.StatusOK, [][2]any{{"lastupdatedb", s.DB.LastUpdateDB()}})
}
func (s *Server) handleConf(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("apikey")
	if key == "" {
		key = r.URL.Query().Get("apiKey")
	}
	valid := s.Config.APIKey == "" || (key != "" && subtle.ConstantTimeCompare([]byte(key), []byte(s.Config.APIKey)) == 1)
	writeJSONOrdered(w, http.StatusOK, [][2]any{{"apikey", valid}})
}

var jackettPathRe = regexp.MustCompile(`(?i)^/api/v2\.0/indexers/[^/]+/results$`)

func (s *Server) handleJackett(w http.ResponseWriter, r *http.Request) {
	if !jackettPathRe.MatchString(r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	categoryRaw := firstCategoryValue(q)
	res, err := s.DB.JackettSearch(filedb.SearchParams{
		APIKey:        firstQueryURL(q, "apikey", "apiKey"),
		Query:         firstQueryURL(q, "query", "q"),
		Title:         q.Get("title"),
		TitleOriginal: q.Get("title_original"),
		Year:          atoi(q.Get("year")),
		IsSerial:      parseOptionalInt(q, "is_serial", -1),
		CategoryRaw:   categoryRaw,
		UserAgent:     r.UserAgent(),
	})
	if err != nil {
		writeCanonicalJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "jacred": true, "Results": []any{}})
		return
	}
	writeCanonicalJSON(w, http.StatusOK, map[string]any{"Results": buildResults(res.Results, res.RqNum), "jacred": true})
}

func (s *Server) handleTorrents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	items, err := s.DB.TorrentsSearch(filedb.TorrentsParams{
		Search:    firstQueryURL(q, "search", "q"),
		AltName:   firstQueryURL(q, "altname", "altName"),
		Exact:     parseBool(firstQueryURL(q, "exact")),
		Type:      firstQueryURL(q, "type"),
		Sort:      firstQueryURL(q, "sort"),
		Tracker:   firstQueryURL(q, "tracker", "trackerName"),
		Voice:     firstQueryURL(q, "voice", "voices"),
		VideoType: firstQueryURL(q, "videotype", "videoType"),
		Relased:   atoi(firstQueryURL(q, "relased", "released")),
		Quality:   atoi(firstQueryURL(q, "quality")),
		Season:    atoi(firstQueryURL(q, "season")),
	})
	if err != nil {
		writeCanonicalJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, i := range items {
		out = append(out, map[string]any{
			"tracker":      i["trackerName"],
			"url":          normalizeDetailURL(i["url"]),
			"title":        i["title"],
			"size":         i["size"],
			"sizeName":     i["sizeName"],
			"createTime":   i["createTime"],
			"updateTime":   i["updateTime"],
			"sid":          i["sid"],
			"pir":          i["pir"],
			"magnet":       i["magnet"],
			"name":         i["name"],
			"originalname": i["originalname"],
			"relased":      i["relased"],
			"videotype":    i["videotype"],
			"quality":      i["quality"],
			"voices":       i["voices"],
			"seasons":      i["seasons"],
			"types":        i["types"],
		})
	}
	writeCanonicalJSON(w, http.StatusOK, out)
}

func (s *Server) handleQualitys(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page := parseOptionalInt(q, "page", 1)
	take := parseOptionalInt(q, "take", 1000)
	items, err := s.DB.Qualitys(firstQueryURL(q, "name"), firstQueryURL(q, "originalname", "originalName"), firstQueryURL(q, "type"), page, take)
	if err != nil {
		writeCanonicalJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeCanonicalJSON(w, http.StatusOK, items)
}

func (s *Server) handleCronAnidubParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	res, err := s.AnidubParser.Parse(context.Background(), parseOptionalInt(q, "parseFrom", 0), parseOptionalInt(q, "parseTo", 0))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "status": res.Status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": res.Status, "parsed": res.Parsed, "added": res.Added, "updated": res.Updated, "skipped": res.Skipped, "failed": res.Failed, "text": fmt.Sprintf("parsed=%d +%d ~%d =%d failed=%d", res.Parsed, res.Added, res.Updated, res.Skipped, res.Failed)})
}

func (s *Server) handleCronKnabenParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	res, err := s.KnabenParser.Parse(context.Background(), parseOptionalInt(q, "from", 0), parseOptionalInt(q, "size", 300), parseOptionalInt(q, "pages", 1), q.Get("query"), parseOptionalInt(q, "hours", 0), defaultString(q.Get("orderBy"), "date"), q.Get("categories"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "status": res.Status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  res.Status,
		"fetched": res.Fetched,
		"added":   res.Added,
		"updated": res.Updated,
		"skipped": res.Skipped,
		"failed":  res.Failed,
		"text":    fmt.Sprintf("fetched=%d +%d ~%d =%d failed=%d", res.Fetched, res.Added, res.Updated, res.Skipped, res.Failed),
	})
}

func (s *Server) handleCronBitruParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res, err := s.BitruParser.Parse(context.Background(), parseOptionalInt(r.URL.Query(), "page", 1))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"status": res.Status, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": res.Status, "fetched": res.Fetched, "added": res.Added, "updated": res.Updated, "skipped": res.Skipped, "failed": res.Failed, "by_category": res.PerCategory})
}

func (s *Server) handleCronBitruUpdateTasksParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res, err := s.BitruParser.UpdateTasksParse(context.Background())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "tasks": res})
}

func (s *Server) handleCronBitruParseAllTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res, err := s.BitruParser.ParseAllTask(context.Background())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": res})
}

func (s *Server) handleCronBitruParseLatest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res, err := s.BitruParser.ParseLatest(context.Background(), parseOptionalInt(r.URL.Query(), "pages", 5))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": res})
}

func (s *Server) handleCronAnilibertyParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	res, err := s.AnilibertyParser.Parse(context.Background(), parseOptionalInt(q, "parseFrom", 0), parseOptionalInt(q, "parseTo", 0))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "status": res.Status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": res.Status, "parsed": res.Parsed, "added": res.Added, "updated": res.Updated, "skipped": res.Skipped, "failed": res.Failed, "lastPage": res.LastPage, "text": fmt.Sprintf("parsed=%d +%d ~%d =%d failed=%d", res.Parsed, res.Added, res.Updated, res.Skipped, res.Failed)})
}

func (s *Server) handleCronAnimelayerParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res, err := s.AnimelayerParser.Parse(context.Background(), parseOptionalInt(r.URL.Query(), "maxpage", 1))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "status": res.Status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": res.Status, "parsed": res.Parsed, "added": res.Added, "updated": res.Updated, "skipped": res.Skipped, "failed": res.Failed, "text": fmt.Sprintf("parsed=%d +%d ~%d =%d failed=%d", res.Parsed, res.Added, res.Updated, res.Skipped, res.Failed)})
}

func (s *Server) handleCronBitruAPIParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	res, err := s.BitruAPIParser.Parse(context.Background(), parseOptionalInt(q, "limit", 100))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "status": res.Status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  res.Status,
		"fetched": res.Fetched,
		"added":   res.Added,
		"updated": res.Updated,
		"skipped": res.Skipped,
		"failed":  res.Failed,
		"text":    fmt.Sprintf("fetched=%d +%d ~%d =%d failed=%d", res.Fetched, res.Added, res.Updated, res.Skipped, res.Failed),
	})
}

func (s *Server) handleCronBitruAPIParseFromDate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	res, err := s.BitruAPIParser.ParseFromDate(context.Background(), q.Get("lastnewtor"), parseOptionalInt(q, "limit", 100))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "status": res.Status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  res.Status,
		"fetched": res.Fetched,
		"added":   res.Added,
		"updated": res.Updated,
		"skipped": res.Skipped,
		"failed":  res.Failed,
		"text":    fmt.Sprintf("fetched=%d +%d ~%d =%d failed=%d", res.Fetched, res.Added, res.Updated, res.Skipped, res.Failed),
	})
}

func (s *Server) handleCronRutorParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res, err := s.RutorParser.Parse(context.Background(), parseOptionalInt(r.URL.Query(), "page", 0))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "status": res.Status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      res.Status,
		"fetched":     res.Fetched,
		"added":       res.Added,
		"updated":     res.Updated,
		"skipped":     res.Skipped,
		"failed":      res.Failed,
		"by_category": res.PerCategory,
		"text":        fmt.Sprintf("fetched=%d +%d ~%d =%d failed=%d", res.Fetched, res.Added, res.Updated, res.Skipped, res.Failed),
	})
}

func (s *Server) handleCronRutorUpdateTasksParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res, err := s.RutorParser.UpdateTasksParse(context.Background())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "tasks": res})
}

func (s *Server) handleCronRutorParseAllTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	text, err := s.RutorParser.ParseAllTask(context.Background())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": text})
}

func (s *Server) handleCronRutorParseLatest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	text, err := s.RutorParser.ParseLatest(context.Background(), parseOptionalInt(r.URL.Query(), "pages", 5))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": text})
}

func (s *Server) handleCronMegapeerParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res, err := s.MegapeerParser.Parse(context.Background(), parseOptionalInt(r.URL.Query(), "maxpage", 1))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "status": res.Status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      res.Status,
		"fetched":     res.Fetched,
		"added":       res.Added,
		"updated":     res.Updated,
		"skipped":     res.Skipped,
		"failed":      res.Failed,
		"by_category": res.PerCategory,
		"text":        fmt.Sprintf("fetched=%d +%d ~%d =%d failed=%d", res.Fetched, res.Added, res.Updated, res.Skipped, res.Failed),
	})
}

func (s *Server) handleCronTorrentByParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res, err := s.TorrentByParser.Parse(context.Background(), parseOptionalInt(r.URL.Query(), "page", 0))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "status": res.Status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      res.Status,
		"fetched":     res.Fetched,
		"added":       res.Added,
		"updated":     res.Updated,
		"skipped":     res.Skipped,
		"failed":      res.Failed,
		"by_category": res.PerCategory,
		"text":        fmt.Sprintf("fetched=%d +%d ~%d =%d failed=%d", res.Fetched, res.Added, res.Updated, res.Skipped, res.Failed),
	})
}

func (s *Server) handleCronTorrentByUpdateTasksParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res, err := s.TorrentByParser.UpdateTasksParse(context.Background())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "tasks": res})
}

func (s *Server) handleCronTorrentByParseAllTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	textRes, err := s.TorrentByParser.ParseAllTask(context.Background())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "text": textRes})
}

func (s *Server) handleCronTorrentByParseLatest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	textRes, err := s.TorrentByParser.ParseLatest(context.Background(), parseOptionalInt(r.URL.Query(), "pages", 5))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "text": textRes})
}

func (s *Server) handleCronRutrackerParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res, err := s.RutrackerParser.Parse(context.Background(), parseOptionalInt(r.URL.Query(), "page", 0))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "status": res.Status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": res.Status, "fetched": res.Fetched, "added": res.Added, "updated": res.Updated, "skipped": res.Skipped, "duplicates": res.Duplicates, "failed": res.Failed, "by_category": res.PerCategory, "text": fmt.Sprintf("fetched=%d +%d ~%d =%d dup=%d failed=%d", res.Fetched, res.Added, res.Updated, res.Skipped, res.Duplicates, res.Failed)})
}

func (s *Server) handleCronRutrackerUpdateTasksParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res, err := s.RutrackerParser.UpdateTasksParse(context.Background())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "tasks": res})
}

func (s *Server) handleCronRutrackerParseAllTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	textRes, err := s.RutrackerParser.ParseAllTask(context.Background())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "text": textRes})
}

func (s *Server) handleCronRutrackerParseLatest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	textRes, err := s.RutrackerParser.ParseLatest(context.Background(), parseOptionalInt(r.URL.Query(), "pages", 5))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "text": textRes})
}

func (s *Server) handleCronKinozalParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res, err := s.KinozalParser.Parse(context.Background(), parseOptionalInt(r.URL.Query(), "page", 0))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "status": res.Status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": res.Status, "fetched": res.Fetched, "added": res.Added, "updated": res.Updated, "skipped": res.Skipped, "failed": res.Failed, "by_category": res.PerCategory, "text": fmt.Sprintf("fetched=%d +%d ~%d =%d failed=%d", res.Fetched, res.Added, res.Updated, res.Skipped, res.Failed)})
}

func (s *Server) handleCronKinozalUpdateTasksParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res, err := s.KinozalParser.UpdateTasksParse(context.Background())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "tasks": res})
}

func (s *Server) handleCronKinozalParseAllTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	textRes, err := s.KinozalParser.ParseAllTask(context.Background())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "text": textRes})
}

func (s *Server) handleCronKinozalParseLatest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	textRes, err := s.KinozalParser.ParseLatest(context.Background(), parseOptionalInt(r.URL.Query(), "pages", 5))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "text": textRes})
}

func (s *Server) handleCronTolokaParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res, err := s.TolokaParser.Parse(context.Background(), parseOptionalInt(r.URL.Query(), "page", 0))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "status": res.Status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": res.Status, "fetched": res.Fetched, "added": res.Added, "updated": res.Updated, "skipped": res.Skipped, "failed": res.Failed, "by_category": res.PerCategory, "text": fmt.Sprintf("fetched=%d +%d ~%d =%d failed=%d", res.Fetched, res.Added, res.Updated, res.Skipped, res.Failed)})
}

func (s *Server) handleCronTolokaUpdateTasksParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res, err := s.TolokaParser.UpdateTasksParse(context.Background())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "tasks": res})
}

func (s *Server) handleCronTolokaParseAllTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	textRes, err := s.TolokaParser.ParseAllTask(context.Background())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "text": textRes})
}

func (s *Server) handleCronTolokaParseLatest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	textRes, err := s.TolokaParser.ParseLatest(context.Background(), parseOptionalInt(r.URL.Query(), "pages", 5))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "text": textRes})
}

func (s *Server) handleCronSelezenParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	res, err := s.SelezenParser.Parse(context.Background(), parseOptionalInt(q, "parseFrom", 0), parseOptionalInt(q, "parseTo", 0))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "status": res.Status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": res.Status, "parsed": res.Parsed, "added": res.Added, "updated": res.Updated, "skipped": res.Skipped, "failed": res.Failed, "text": fmt.Sprintf("parsed=%d +%d ~%d =%d failed=%d", res.Parsed, res.Added, res.Updated, res.Skipped, res.Failed)})
}

func (s *Server) handleCronSelezenUpdateTasksParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res, err := s.SelezenParser.UpdateTasksParse(context.Background())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "tasks": res})
}

func (s *Server) handleCronSelezenParseAllTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	text, err := s.SelezenParser.ParseAllTask(context.Background())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": text})
}

func (s *Server) handleCronSelezenParseLatest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	text, err := s.SelezenParser.ParseLatest(context.Background(), parseOptionalInt(r.URL.Query(), "pages", 5))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": text})
}

func (s *Server) handleCronAnistarParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	lp := parseOptionalInt(r.URL.Query(), "limit_page", 0)
	if lp == 0 {
		lp = parseOptionalInt(r.URL.Query(), "limitPage", 0)
	}
	res, err := s.AnistarParser.Parse(context.Background(), lp)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "status": res.Status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": res.Status, "fetched": res.Fetched, "added": res.Added, "updated": res.Updated, "skipped": res.Skipped, "failed": res.Failed, "text": fmt.Sprintf("fetched=%d +%d ~%d =%d failed=%d", res.Fetched, res.Added, res.Updated, res.Skipped, res.Failed)})
}

func (s *Server) handleCronAnifilmParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	fullparse := parseBool(r.URL.Query().Get("fullparse"))
	res, err := s.AnifilmParser.Parse(context.Background(), fullparse)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "status": res.Status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": res.Status, "fetched": res.Fetched, "added": res.Added, "updated": res.Updated, "skipped": res.Skipped, "failed": res.Failed, "text": fmt.Sprintf("fetched=%d +%d ~%d =%d failed=%d", res.Fetched, res.Added, res.Updated, res.Skipped, res.Failed)})
}

func (s *Server) handleCronBaibakoParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res, err := s.BaibakoParser.Parse(context.Background(), parseOptionalInt(r.URL.Query(), "maxpage", 10))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "status": res.Status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": res.Status, "fetched": res.Fetched, "added": res.Added, "updated": res.Updated, "skipped": res.Skipped, "failed": res.Failed, "text": fmt.Sprintf("fetched=%d +%d ~%d =%d failed=%d", res.Fetched, res.Added, res.Updated, res.Skipped, res.Failed)})
}

func (s *Server) handleCronLeproductionParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res, err := s.LeproductionParser.Parse(context.Background(), parseOptionalInt(r.URL.Query(), "limit_page", 0))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "status": res.Status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": res.Status, "fetched": res.Fetched, "added": res.Added, "updated": res.Updated, "skipped": res.Skipped, "failed": res.Failed, "text": fmt.Sprintf("fetched=%d +%d ~%d =%d failed=%d", res.Fetched, res.Added, res.Updated, res.Skipped, res.Failed)})
}

func (s *Server) handleCronMazepaParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res, err := s.MazepaParser.Parse(context.Background())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "status": res.Status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": res.Status, "fetched": res.Fetched, "added": res.Added, "updated": res.Updated, "skipped": res.Skipped, "failed": res.Failed, "text": fmt.Sprintf("fetched=%d +%d ~%d =%d failed=%d", res.Fetched, res.Added, res.Updated, res.Skipped, res.Failed)})
}

func (s *Server) handleCronMazepaUpdateTasksParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res, err := s.MazepaParser.UpdateTasksParse(context.Background())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "tasks": res})
}

func (s *Server) handleCronMazepaParseAllTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	text, err := s.MazepaParser.ParseAllTask(context.Background())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": text})
}

func (s *Server) handleCronMazepaParseLatest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	text, err := s.MazepaParser.ParseLatest(context.Background(), parseOptionalInt(r.URL.Query(), "pages", 5))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": text})
}

func buildResults(items []filedb.TorrentDetails, rqnum bool) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, t := range items {
		category, categoryDesc := categories(t)
		result := map[string]any{
			"Tracker":      t["trackerName"],
			"Details":      normalizeDetailURL(t["url"]),
			"Title":        t["title"],
			"Size":         parseSize(t),
			"PublishDate":  t["createTime"],
			"Category":     category,
			"CategoryDesc": categoryDesc,
			"Seeders":      asInt(t["sid"]),
			"Peers":        asInt(t["pir"]),
			"MagnetUri":    t["magnet"],
			"ffprobe":      nullableField(rqnum, t["ffprobe"]),
			"languages":    nullableField(rqnum, t["languages"]),
			"info":         nil,
		}
		if !rqnum {
			result["info"] = map[string]any{
				"name":         t["name"],
				"originalname": t["originalname"],
				"sizeName":     t["sizeName"],
				"relased":      asInt(t["relased"]),
				"videotype":    asString(t["videotype"]),
				"quality":      asInt(t["quality"]),
				"voices":       emptyToNil(t["voices"]),
				"seasons":      emptyToNil(t["seasons"]),
				"types":        t["types"],
			}
		}
		out = append(out, result)
	}
	return out
}

func categories(t filedb.TorrentDetails) ([]int, string) {
	var types []string
	switch v := t["types"].(type) {
	case []any:
		for _, it := range v {
			types = append(types, asString(it))
		}
	case []string:
		types = append(types, v...)
	}
	set := map[int]struct{}{}
	desc := ""
	for _, raw := range types {
		switch raw {
		case "movie":
			desc = "Movies"
			set[2000] = struct{}{}
		case "serial":
			desc = "TV"
			set[5000] = struct{}{}
		case "documovie", "docuserial":
			desc = "TV/Documentary"
			set[5080] = struct{}{}
		case "tvshow":
			desc = "TV/Foreign"
			set[5020] = struct{}{}
			set[2010] = struct{}{}
		case "anime":
			desc = "TV/Anime"
			set[5070] = struct{}{}
		}
	}
	out := make([]int, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	return out, desc
}
func defaultString(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func parseSize(t filedb.TorrentDetails) float64 {
	switch v := t["size"].(type) {
	case float64:
		return v
	case json.Number:
		f, _ := v.Float64()
		return f
	}
	return 0
}
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Recovery: не даём панике убить весь процесс
		defer func() {
			if err := recover(); err != nil {
				log.Printf("PANIC [%s %s]: %v\n%s", r.Method, r.URL.Path, err, debug.Stack())
				if !headerWritten(w) {
					http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
				}
			}
		}()

		// Case-insensitive маршрутизация для API-путей
		lowerPath := strings.ToLower(r.URL.Path)
		if lowerPath != r.URL.Path {
			r.URL.Path = lowerPath
		}

		remote := parseRemoteIP(r.RemoteAddr)
		fromLocal := isLocalOrPrivate(remote)
		path := r.URL.Path
		setCommonCORSHeaders(w, fromLocal || !isLocalOnlyPath(path))
		if !fromLocal && isLocalOnlyPath(path) {
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
			} else {
				w.WriteHeader(http.StatusForbidden)
			}
			return
		}
		if fromLocal && s.Config.DevKey != "" && isLocalOnlyPath(path) && !devKeyMatches(r, s.Config.DevKey) {
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
			} else {
				w.WriteHeader(http.StatusUnauthorized)
			}
			return
		}
		if s.Config.APIKey != "" && !isPathWhitelisted(path) {
			key := getAPIKey(r)
			if key == "" || subtle.ConstantTimeCompare([]byte(key), []byte(s.Config.APIKey)) != 1 {
				if r.Method == http.MethodOptions {
					w.WriteHeader(http.StatusNoContent)
				} else {
					w.WriteHeader(http.StatusUnauthorized)
				}
				return
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// headerWritten проверяет, был ли уже записан заголовок ответа (best-effort).
func headerWritten(w http.ResponseWriter) bool {
	// Если Content-Type уже установлен, скорее всего заголовок был записан
	return w.Header().Get("Content-Type") != ""
}
func setCommonCORSHeaders(w http.ResponseWriter, allowPrivate bool) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key, x-api-key")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	if allowPrivate {
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
	}
}

func isLocalOnlyPath(path string) bool {
	lp := strings.ToLower(path)
	return strings.HasPrefix(lp, "/cron/") || path == "/jsondb" || strings.HasPrefix(lp, "/jsondb/") || strings.HasPrefix(lp, "/dev/")
}
func isPathWhitelisted(path string) bool {
	switch path {
	case "/", "/stats", "/stats/", "/health", "/version", "/lastupdatedb", "/api/v1.0/conf":
		return true
	}
	return strings.HasPrefix(path, "/sync/")
}
func firstQueryURL(v url.Values, keys ...string) string {
	for _, key := range keys {
		if s := strings.TrimSpace(v.Get(key)); s != "" {
			return s
		}
	}
	return ""
}

func getAPIKey(r *http.Request) string {
	if v := r.URL.Query().Get("apikey"); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("X-Api-Key")); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(strings.ToLower(v), "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return ""
}
func devKeyMatches(r *http.Request, key string) bool {
	if v := strings.TrimSpace(r.Header.Get("X-Dev-Key")); v != "" {
		return subtle.ConstantTimeCompare([]byte(v), []byte(key)) == 1
	}
	if v := r.URL.Query().Get("devkey"); v != "" {
		return subtle.ConstantTimeCompare([]byte(v), []byte(key)) == 1
	}
	return false
}
func parseRemoteIP(remoteAddr string) net.IP {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	return net.ParseIP(host)
}
func isLocalOrPrivate(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() {
		return true
	}
	if ip.To4() == nil {
		return strings.HasPrefix(strings.ToLower(ip.String()), "fe80:")
	}
	return false
}
func atoi(v string) int { n := 0; _, _ = fmt.Sscanf(v, "%d", &n); return n }
func parseBool(v string) bool {
	b, _ := strconv.ParseBool(v)
	return b
}
func parseOptionalInt(q url.Values, key string, def int) int {
	if v := q.Get(key); v != "" {
		return atoi(v)
	}
	return def
}
func firstCategoryValue(q url.Values) string {
	for key, vals := range q {
		if strings.HasPrefix(key, "category") && len(vals) > 0 {
			return vals[0]
		}
	}
	return ""
}
func (s *Server) handleCronNNMClubParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res, err := s.NNMClubParser.Parse(context.Background(), parseOptionalInt(r.URL.Query(), "page", 0))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "status": res.Status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": res.Status, "fetched": res.Fetched, "added": res.Added, "updated": res.Updated, "skipped": res.Skipped, "failed": res.Failed})
}

func (s *Server) handleCronNNMClubUpdateTasksParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res, err := s.NNMClubParser.UpdateTasksParse(context.Background())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "tasks": res})
}

func (s *Server) handleCronNNMClubParseAllTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	text, err := s.NNMClubParser.ParseAllTask(context.Background())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": text})
}

func (s *Server) handleCronNNMClubParseLatest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	text, err := s.NNMClubParser.ParseLatest(context.Background(), parseOptionalInt(r.URL.Query(), "pages", 5))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": text})
}

func normalizeDetailURL(v any) any {
	s := asString(v)
	if strings.HasPrefix(s, "http") {
		return s
	}
	return nil
}
func nullableField(rqnum bool, v any) any {
	if rqnum {
		return nil
	}
	return emptyToNil(v)
}
func emptyToNil(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case []any:
		if len(x) == 0 {
			return nil
		}
	case []string:
		if len(x) == 0 {
			return nil
		}
	case []int:
		if len(x) == 0 {
			return nil
		}
	}
	return v
}
func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
func asInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case int:
		return n
	}
	return 0
}

func (s *Server) handleCronLostfilmParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res, err := s.LostfilmParser.Parse(context.Background())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "status": res.Status})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleCronLostfilmParsePages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	res, err := s.LostfilmParser.ParsePages(context.Background(), parseOptionalInt(q, "pageFrom", 1), parseOptionalInt(q, "pageTo", 1))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "status": res.Status})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleCronLostfilmParseSeasonPacks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	textRes, err := s.LostfilmParser.ParseSeasonPacks(context.Background(), r.URL.Query().Get("series"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": textRes})
}

func (s *Server) handleCronLostfilmVerifyPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	items, status, err := s.LostfilmParser.VerifyPage(context.Background(), r.URL.Query().Get("series"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if status != "ok" {
		writeJSON(w, http.StatusOK, map[string]any{"error": status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(items), "items": items})
}

func (s *Server) handleCronLostfilmStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.LostfilmParser.Stats())
}
