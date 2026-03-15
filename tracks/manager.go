package tracks

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"jacred/app"
	"jacred/filedb"
)

type RemoteTorrentInfo struct {
	Title      string `json:"title"`
	Category   string `json:"category"`
	Poster     string `json:"poster"`
	Timestamp  int64  `json:"timestamp"`
	Name       string `json:"name"`
	Hash       string `json:"hash"`
	Stat       int    `json:"stat"`
	StatString string `json:"stat_string"`
}

type Candidate struct {
	Key     string
	Torrent filedb.TorrentDetails
}

type Manager struct {
	Config   app.Config
	FileDB   *filedb.DB
	TracksDB *DB
	DataDir  string
	Client   *http.Client
	rndMu    sync.Mutex
	rnd      *rand.Rand
}

func NewManager(cfg app.Config, fdb *filedb.DB, tracksDB *DB, dataDir string) *Manager {
	return &Manager{
		Config:   cfg,
		FileDB:   fdb,
		TracksDB: tracksDB,
		DataDir:  dataDir,
		Client:   &http.Client{Timeout: 30 * time.Second},
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

func baseURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return strings.TrimRight(strings.TrimSpace(raw), "/")
	}
	u.User = nil
	return strings.TrimRight(u.String(), "/")
}

func addBasicAuthHeader(req *http.Request, raw string) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User == nil {
		return
	}
	username := u.User.Username()
	password, _ := u.User.Password()
	if username == "" && password == "" {
		return
	}
	token := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	req.Header.Set("Authorization", "Basic "+token)
	req.Header.Set("Accept", "application/json")
}

func (m *Manager) doJSON(ctx context.Context, method, rawURL string, payload any) (*http.Response, []byte, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	addBasicAuthHeader(req, rawURL)
	resp, err := m.Client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	data, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, data, readErr
}

func (m *Manager) CheckTorrentExistsWithCategory(ctx context.Context, tsuri, infohash string) (bool, string, bool) {
	resp, body, err := m.doJSON(ctx, http.MethodPost, baseURL(tsuri)+"/torrents", map[string]any{"action": "list"})
	if err != nil {
		return false, "", true
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, "", true
	}
	var torrents []RemoteTorrentInfo
	if err := json.Unmarshal(body, &torrents); err != nil {
		return false, "", false
	}
	if strings.TrimSpace(infohash) == "" {
		return false, "", false
	}
	infohash = strings.ToLower(strings.TrimSpace(infohash))
	for _, t := range torrents {
		if strings.EqualFold(strings.TrimSpace(t.Hash), infohash) || strings.HasSuffix(strings.ToLower(strings.TrimSpace(t.Name)), infohash) {
			return true, t.Category, false
		}
	}
	return false, "", false
}

func (m *Manager) GetTorrentCountByCategory(ctx context.Context, tsuri, category string) (int, bool) {
	resp, body, err := m.doJSON(ctx, http.MethodPost, baseURL(tsuri)+"/torrents", map[string]any{"action": "list"})
	if err != nil {
		return 0, false
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, false
	}
	var torrents []RemoteTorrentInfo
	if err := json.Unmarshal(body, &torrents); err != nil {
		return 0, false
	}
	count := 0
	for _, t := range torrents {
		if strings.EqualFold(strings.TrimSpace(t.Category), strings.TrimSpace(category)) {
			count++
		}
	}
	return count, true
}

func (m *Manager) SelectBestServer(ctx context.Context) string {
	if len(m.Config.TSURI) == 0 || strings.TrimSpace(m.Config.TracksCategory) == "" {
		return ""
	}
	type result struct {
		server string
		count  int
		ok     bool
	}
	results := make([]result, 0, len(m.Config.TSURI))
	for _, srv := range m.Config.TSURI {
		if _, _, serverError := m.CheckTorrentExistsWithCategory(ctx, srv, ""); serverError {
			continue
		}
		count, ok := m.GetTorrentCountByCategory(ctx, srv, m.Config.TracksCategory)
		if !ok {
			continue
		}
		results = append(results, result{server: srv, count: count, ok: true})
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].count == results[j].count {
			return results[i].server < results[j].server
		}
		return results[i].count < results[j].count
	})
	if len(results) == 0 {
		return ""
	}
	return results[0].server
}

func (m *Manager) AddTorrentToServer(ctx context.Context, tsuri, magnet, infohash string, typetask int) (bool, bool, bool) {
	exists, actualCategory, serverError := m.CheckTorrentExistsWithCategory(ctx, tsuri, infohash)
	if serverError {
		return false, false, true
	}
	if exists {
		isCorrect := strings.EqualFold(strings.TrimSpace(actualCategory), strings.TrimSpace(m.Config.TracksCategory))
		if isCorrect {
			return false, true, false
		}
		return false, false, false
	}
	resp, _, err := m.doJSON(ctx, http.MethodPost, baseURL(tsuri)+"/torrents", map[string]any{
		"action":     "add",
		"link":       magnet,
		"save_to_db": false,
		"category":   m.Config.TracksCategory,
	})
	if err != nil {
		m.Log("Ошибка при добавлении торрента на сервере: "+err.Error(), &typetask)
		return false, false, true
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		m.Log(fmt.Sprintf("Ошибка при добавлении торрента (%d)", resp.StatusCode), &typetask)
		return false, false, false
	}
	return true, false, false
}

func (m *Manager) AnalyzeWithExternalAPI(ctx context.Context, tsuri, infohash string) (*FFProbeModel, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL(tsuri)+"/ffp/"+strings.ToUpper(infohash)+"/1", nil)
	if err != nil {
		return nil, 0, err
	}
	addBasicAuthHeader(req, tsuri)
	resp, err := m.Client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, nil
	}
	var res FFProbeModel
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, resp.StatusCode, err
	}
	if len(res.Streams) == 0 {
		return nil, resp.StatusCode, nil
	}
	return &res, resp.StatusCode, nil
}

func (m *Manager) CleanupTorrent(ctx context.Context, tsuri, infohash string, typetask int) {
	exists, actualCategory, serverError := m.CheckTorrentExistsWithCategory(ctx, tsuri, infohash)
	if serverError || !exists {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(actualCategory), strings.TrimSpace(m.Config.TracksCategory)) {
		return
	}
	_, _, _ = m.doJSON(ctx, http.MethodPost, baseURL(tsuri)+"/torrents", map[string]any{"action": "rem", "hash": infohash})
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
	if len(m.Config.TSURI) == 0 {
		m.Log("Ошибка: не настроены tsuri серверы", &typetask)
		return nil
	}
	if strings.TrimSpace(m.Config.TracksCategory) == "" {
		m.Log("Ошибка: не настроена trackscategory", &typetask)
		return nil
	}
	infohash, err := InfoHashFromMagnet(magnet)
	if err != nil || infohash == "" {
		m.Log("Ошибка парсинга magnet-ссылки: "+err.Error(), &typetask)
		return nil
	}
	m.Log("Начало анализа треков для "+infohash+".", &typetask)

	selectCtx, cancelSelect := context.WithTimeout(ctx, time.Minute)
	tsuri := m.SelectBestServer(selectCtx)
	cancelSelect()
	if tsuri == "" {
		m.Log("Все серверы недоступны. Выход.", &typetask)
		return nil
	}

	analyzeCtx, cancelAnalyze := context.WithTimeout(ctx, 3*time.Minute)
	defer cancelAnalyze()
	added, existsInCorrectCategory, serverError := m.AddTorrentToServer(analyzeCtx, tsuri, magnet, infohash, typetask)
	if serverError {
		m.Log("Сервер вернул ошибку при получении списка торрентов. Выход.", &typetask)
		return nil
	}
	shouldAnalyze := added || existsInCorrectCategory
	if !shouldAnalyze {
		m.Log("Не удалось добавить торрент на сервер и он не существует в правильной категории. Завершение.", &typetask)
		return nil
	}
	if added {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	res, apiStatusCode, err := m.AnalyzeWithExternalAPI(analyzeCtx, tsuri, infohash)
	m.CleanupTorrent(context.Background(), tsuri, infohash, typetask)
	if err != nil {
		m.Log("Критическая ошибка при анализе треков: "+err.Error(), &typetask)
	}
	if torrentKey == "" {
		torrentKey = m.FileDB.FindTorrentKeyByMagnet(magnet)
	}
	if res != nil && len(res.Streams) > 0 {
		if err := m.SaveTrackResults(res, infohash, typetask); err != nil {
			return err
		}
		m.Log("Анализ треков для "+infohash+" успешно завершен!", &typetask)
		return nil
	}
	if typetask != 1 && torrentKey != "" {
		newAttempt := currentAttempt + 1
		if apiStatusCode == 400 {
			newAttempt = m.Config.TracksAttempt
		}
		if newAttempt != currentAttempt {
			_ = m.FileDB.UpdateTorrentFfprobeInfo(torrentKey, magnet, newAttempt, nil, nil)
		}
		m.Log(fmt.Sprintf("Анализ треков для %s без результата. Код ответа API: %d. Осталось %d попыток.", infohash, apiStatusCode, m.Config.TracksAttempt-newAttempt), &typetask)
	}
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
	candidates := m.CollectCandidates(typetask, now)
	m.Log(fmt.Sprintf("typetask=%d collected %d torrents to process", typetask, len(candidates)), &task)
	processed := 0
	for _, c := range candidates {
		if !m.Config.Tracks {
			m.Log(fmt.Sprintf("end typetask=%d Tracks off in settings", typetask), &task)
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
	m.Log(fmt.Sprintf("end typetask=%d", typetask), &task)
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
