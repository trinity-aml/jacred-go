package tracks

import (
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type DB struct {
	dataDir string
	mu      sync.RWMutex
	items   map[string]FFProbeModel
}

func New(dataDir string) *DB { return &DB{dataDir: dataDir, items: map[string]FFProbeModel{}} }
func (db *DB) Count() int    { db.mu.RLock(); defer db.mu.RUnlock(); return len(db.items) }

func (db *DB) Path(infohash string, createFolder bool) (string, error) {
	infohash = normalizeInfoHash(infohash)
	if len(infohash) != 40 {
		return "", fmt.Errorf("invalid infohash length: %d", len(infohash))
	}
	folder := filepath.Join(db.dataDir, "tracks", infohash[:2], string(infohash[2]))
	if createFolder {
		if err := os.MkdirAll(folder, 0o755); err != nil {
			return "", err
		}
	}
	return filepath.Join(folder, infohash[3:]), nil
}

func TheBad(types []string) bool {
	if len(types) == 0 {
		return true
	}
	for _, t := range types {
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "sport", "tvshow", "docuserial":
			return true
		}
	}
	return false
}

func (db *DB) Load() error {
	base := filepath.Join(db.dataDir, "tracks")
	if _, err := os.Stat(base); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	loaded := map[string]FFProbeModel{}
	err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 3 {
			return nil
		}
		infohash := strings.ToLower(parts[0] + parts[1] + parts[2])
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var model FFProbeModel
		if err := json.Unmarshal(b, &model); err != nil || len(model.Streams) == 0 {
			return nil
		}
		loaded[infohash] = model
		return nil
	})
	if err != nil {
		return err
	}
	db.mu.Lock()
	db.items = loaded
	db.mu.Unlock()
	return nil
}

func (db *DB) GetByInfoHash(infohash string) ([]FFStream, bool) {
	infohash = normalizeInfoHash(infohash)
	db.mu.RLock()
	model, ok := db.items[infohash]
	db.mu.RUnlock()
	if ok && len(model.Streams) > 0 {
		return model.Streams, true
	}
	path, err := db.Path(infohash, false)
	if err != nil {
		return nil, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var loaded FFProbeModel
	if err := json.Unmarshal(b, &loaded); err != nil || len(loaded.Streams) == 0 {
		return nil, false
	}
	db.mu.Lock()
	db.items[infohash] = loaded
	db.mu.Unlock()
	return loaded.Streams, true
}

func (db *DB) GetByMagnet(magnet string, types []string, onlyDB bool) ([]FFStream, bool) {
	if TheBad(types) {
		return nil, false
	}
	infohash, err := InfoHashFromMagnet(magnet)
	if err != nil {
		return nil, false
	}
	streams, ok := db.GetByInfoHash(infohash)
	if ok || onlyDB {
		return streams, ok
	}
	return nil, false
}

func (db *DB) Put(infohash string, model FFProbeModel) error {
	if len(model.Streams) == 0 {
		return errors.New("empty ffprobe streams")
	}
	path, err := db.Path(infohash, true)
	if err != nil {
		return err
	}
	body, err := json.Marshal(model)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return err
	}
	db.mu.Lock()
	db.items[normalizeInfoHash(infohash)] = model
	db.mu.Unlock()
	return nil
}

func (db *DB) LanguagesFromMagnet(magnet string, types []string) []string {
	streams, ok := db.GetByMagnet(magnet, types, true)
	if !ok {
		return nil
	}
	uniq := map[string]struct{}{}
	for _, s := range streams {
		if s.Tags == nil {
			continue
		}
		lang := strings.TrimSpace(strings.ToLower(s.Tags.Language))
		if lang != "" {
			uniq[lang] = struct{}{}
		}
	}
	out := make([]string, 0, len(uniq))
	for lang := range uniq {
		out = append(out, lang)
	}
	sort.Strings(out)
	return out
}

func InfoHashFromMagnet(magnet string) (string, error) {
	magnet = strings.TrimSpace(magnet)
	if magnet == "" {
		return "", errors.New("empty magnet")
	}
	idx := strings.Index(magnet, "xt=urn:btih:")
	if idx == -1 {
		return "", errors.New("btih not found")
	}
	rest := magnet[idx+len("xt=urn:btih:"):]
	if end := strings.Index(rest, "&"); end != -1 {
		rest = rest[:end]
	}
	rest = strings.TrimSpace(rest)
	if len(rest) == 40 && isHex(rest) {
		return strings.ToLower(rest), nil
	}
	if len(rest) == 32 {
		decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(rest))
		if err == nil && len(decoded) > 0 {
			return strings.ToLower(hex.EncodeToString(decoded)), nil
		}
	}
	return "", errors.New("unsupported btih format")
}

func normalizeInfoHash(infohash string) string { return strings.ToLower(strings.TrimSpace(infohash)) }
func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}
