package tracks

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"jacred/app"
	"jacred/filedb"
)

type Candidate struct {
	Key     string
	Torrent filedb.TorrentDetails
}

type Manager struct {
	Config   app.Config
	FileDB   *filedb.DB
	TracksDB *DB
	DataDir  string
	Analyzer *NativeAnalyzer
	rndMu    sync.Mutex
	rnd      *rand.Rand
}

func NewManager(cfg app.Config, fdb *filedb.DB, tracksDB *DB, dataDir string, analyzer *NativeAnalyzer) *Manager {
	return &Manager{
		Config:   cfg,
		FileDB:   fdb,
		TracksDB: tracksDB,
		DataDir:  dataDir,
		Analyzer: analyzer,
		rnd:      rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (m *Manager) GetRandomDelay() time.Duration {
	m.rndMu.Lock()
	defer m.rndMu.Unlock()
	return time.Duration(m.rnd.Intn(3001)+2000) * time.Millisecond
}

func (m *Manager) Log(message string, typetask *int) {
	timeNow := time.Now().Format("15:04:05")
	taskInfo := ""
	if typetask != nil {
		taskInfo = fmt.Sprintf(" [task:%d]", *typetask)
	}
	full := fmt.Sprintf("tracks: [%s]%s %s", timeNow, taskInfo, message)
	fmt.Println(full)
	if !m.Config.TracksLog {
		return
	}
	_ = os.MkdirAll(filepath.Join(m.DataDir, "log"), 0o755)
	f, err := os.OpenFile(filepath.Join(m.DataDir, "log", "tracks.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(full + "\n")
}

func Languages(existing []string, streams []FFStream) []string {
	uniq := map[string]struct{}{}
	for _, l := range existing {
		l = strings.TrimSpace(strings.ToLower(l))
		if l != "" {
			uniq[l] = struct{}{}
		}
	}
	for _, s := range streams {
		if s.CodecType != "audio" || s.Tags == nil {
			continue
		}
		lang := strings.TrimSpace(strings.ToLower(s.Tags.Language))
		if lang != "" {
			uniq[lang] = struct{}{}
		}
	}
	if len(uniq) == 0 {
		return nil
	}
	out := make([]string, 0, len(uniq))
	for l := range uniq {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

func (m *Manager) SaveTrackResults(result *FFProbeModel, infohash string, typetask int) error {
	if result == nil || len(result.Streams) == 0 {
		return nil
	}
	if err := m.TracksDB.Put(infohash, *result); err != nil {
		m.Log("Ошибка при сохранении данных в файл: "+err.Error(), &typetask)
		return err
	}
	audioLanguages := make([]string, 0)
	for _, s := range result.Streams {
		if s.CodecType == "audio" && s.Tags != nil && strings.TrimSpace(s.Tags.Language) != "" {
			audioLanguages = append(audioLanguages, strings.ToLower(strings.TrimSpace(s.Tags.Language)))
		}
	}
	if len(audioLanguages) > 0 {
		sort.Strings(audioLanguages)
		m.Log("Обнаружены аудио дорожки на языках: "+strings.Join(audioLanguages, ", "), &typetask)
	}
	return nil
}

func (m *Manager) Add(ctx context.Context, magnet string, currentAttempt int, types []string, torrentKey string, typetask int) error {
	if strings.TrimSpace(magnet) == "" {
		m.Log("Ошибка: magnet-ссылка не может быть пустой", &typetask)
		return nil
	}
	if types != nil && TheBad(types) {
		m.Log("Пропуск добавления треков: недопустимый тип контента ["+strings.Join(types, ", ")+"]", &typetask)
		return nil
	}
	if m.Analyzer == nil {
		m.Log("Ошибка: native analyzer не инициализирован", &typetask)
		return nil
	}
	infohash, err := InfoHashFromMagnet(magnet)
	if err != nil || infohash == "" {
		m.Log("Ошибка парсинга magnet-ссылки: "+err.Error(), &typetask)
		return nil
	}
	m.Log("Начало анализа треков для "+infohash+".", &typetask)

	analyzeCtx, cancelAnalyze := context.WithTimeout(ctx, 10*time.Minute)
	defer cancelAnalyze()
	res, err := m.Analyzer.Analyze(analyzeCtx, magnet)
	if torrentKey == "" {
		torrentKey = m.FileDB.FindTorrentKeyByMagnet(magnet)
	}
	if err == nil && res != nil && len(res.Streams) > 0 {
		if err := m.SaveTrackResults(res, infohash, typetask); err != nil {
			return err
		}
		m.Log("Анализ треков для "+infohash+" успешно завершен!", &typetask)
		return nil
	}
	if err != nil {
		m.Log("Анализ треков завершился с ошибкой: "+err.Error(), &typetask)
	}
	newAttempt := currentAttempt
	if typetask != 1 {
		newAttempt = currentAttempt + 1
		if torrentKey != "" && newAttempt != currentAttempt {
			_ = m.FileDB.UpdateTorrentFfprobeInfo(torrentKey, magnet, newAttempt, nil, nil)
		}
	}
	m.Log(fmt.Sprintf("Анализ треков для %s без результата. Осталось %d попыток.", infohash, m.Config.TracksAttempt-newAttempt), &typetask)
	return nil
}

func (m *Manager) CollectCandidates(typetask int, now time.Time) []Candidate {
	torrents := make([]Candidate, 0)
	for _, item := range m.FileDB.OrderedMasterEntries() {
		bucket, err := m.FileDB.OpenRead(item.Key)
		if err != nil {
			continue
		}
		for _, t := range bucket {
			magnet := strings.TrimSpace(toString(t["magnet"]))
			if magnet == "" {
				continue
			}
			if !includeByTask(t, typetask, now) {
				continue
			}
			types := toStringSlice(t["types"])
			if TheBad(types) || t["ffprobe"] != nil {
				continue
			}
			if toInt(t["ffprobe_tryingdata"]) >= m.Config.TracksAttempt {
				continue
			}
			if typetask == 1 || typetask == 2 || toInt(t["sid"]) > 0 {
				torrents = append(torrents, Candidate{Key: item.Key, Torrent: t})
			}
		}
	}
	sort.Slice(torrents, func(i, j int) bool {
		ti := toTime(torrents[i].Torrent["updateTime"])
		tj := toTime(torrents[j].Torrent["updateTime"])
		return ti.After(tj)
	})
	return torrents
}

func includeByTask(t filedb.TorrentDetails, typetask int, now time.Time) bool {
	createTime := toTime(t["createTime"])
	updateTime := toTime(t["updateTime"])
	switch typetask {
	case 1:
		return !createTime.IsZero() && createTime.After(now.UTC().Add(-24*time.Hour))
	case 2:
		if !createTime.IsZero() && createTime.After(now.UTC().Add(-24*time.Hour)) {
			return false
		}
		return !createTime.IsZero() && createTime.After(now.UTC().AddDate(0, -1, 0))
	case 3:
		if !createTime.IsZero() && createTime.After(now.UTC().AddDate(0, -1, 0)) {
			return false
		}
		return !createTime.IsZero() && createTime.After(now.UTC().AddDate(-1, 0, 0))
	case 4:
		if (!createTime.IsZero() && createTime.After(now.UTC().AddDate(-1, 0, 0))) || (!updateTime.IsZero() && updateTime.After(now.UTC().AddDate(0, -1, 0))) {
			return false
		}
		return true
	case 5:
		if !createTime.IsZero() && createTime.After(now.UTC().AddDate(-1, 0, 0)) {
			return false
		}
		return !updateTime.IsZero() && updateTime.After(now.UTC().AddDate(0, -1, 0))
	default:
		return false
	}
}

func (m *Manager) ProcessOnce(ctx context.Context, typetask int, now time.Time) (int, error) {
	task := typetask
	m.Log(fmt.Sprintf("start typetask=%d", typetask), &task)
	startTime := time.Now()
	candidates := m.CollectCandidates(typetask, now)
	m.Log(fmt.Sprintf("typetask=%d collected %d torrents to process", typetask, len(candidates)), &task)
	processed := 0
	for _, c := range candidates {
		if !m.Config.Tracks {
			m.Log(fmt.Sprintf("end typetask=%d Tracks off in settings", typetask), &task)
			break
		}
		if typetask == 2 && time.Since(startTime) > 10*24*time.Hour {
			break
		}
		if (typetask == 3 || typetask == 4 || typetask == 5) && time.Since(startTime) > 30*24*time.Hour {
			break
		}
		magnet := toString(c.Torrent["magnet"])
		if _, ok := m.TracksDB.GetByMagnet(magnet, nil, false); ok {
			continue
		}
		select {
		case <-ctx.Done():
			return processed, ctx.Err()
		case <-time.After(m.GetRandomDelay()):
		}
		if err := m.Add(ctx, magnet, toInt(c.Torrent["ffprobe_tryingdata"]), toStringSlice(c.Torrent["types"]), c.Key, typetask); err == nil {
			processed++
		}
	}
	m.Log(fmt.Sprintf("end typetask=%d (elapsed %.1fm)", typetask, time.Since(startTime).Minutes()), &task)
	return processed, nil
}

func (m *Manager) RunLoop(ctx context.Context, typetask int) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(20 * time.Second):
	}
	firstRun := typetask == 1
	for {
		if !firstRun {
			minutes := m.Config.TracksInterval.Task0 + typetask
			if typetask == 1 {
				minutes = m.Config.TracksInterval.Task1
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(minutes) * time.Minute):
			}
		}
		firstRun = false
		if !m.Config.Tracks {
			continue
		}
		if m.Config.TracksMod == 1 && (typetask == 3 || typetask == 4) {
			continue
		}
		_, _ = m.ProcessOnce(ctx, typetask, time.Now())
	}
}

func toString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case string:
		var i int
		_, _ = fmt.Sscanf(strings.TrimSpace(n), "%d", &i)
		return i
	default:
		return 0
	}
}

func toStringSlice(v any) []string {
	switch arr := v.(type) {
	case []string:
		return arr
	case []any:
		out := make([]string, 0, len(arr))
		for _, it := range arr {
			out = append(out, toString(it))
		}
		return out
	default:
		return nil
	}
}

func toTime(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case string:
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.9999999Z07:00", "2006-01-02T15:04:05Z07:00", "2006-01-02T15:04:05"} {
			if tt, err := time.Parse(layout, t); err == nil {
				return tt
			}
		}
	}
	return time.Time{}
}
