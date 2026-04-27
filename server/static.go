package server

import (
	"bytes"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// buildTime is used as Last-Modified for embedded assets. Set once at process
// start so the embedded FS exposes a stable timestamp for caching.
var buildTime = time.Now().UTC().Truncate(time.Second)

// readStatic resolves a slash-separated path to its bytes, mod time, and a
// flag indicating whether it was found. Disk override (s.WWWRoot) wins when
// the file exists; otherwise the embedded FS is used.
func (s *Server) readStatic(name string) ([]byte, time.Time, bool) {
	clean := strings.TrimPrefix(filepath.ToSlash(name), "/")
	if clean == "" || strings.Contains(clean, "..") {
		return nil, time.Time{}, false
	}
	if s.WWWRoot != "" {
		diskPath := filepath.Join(s.WWWRoot, filepath.FromSlash(clean))
		if isWithinRoot(s.WWWRoot, diskPath) {
			if st, err := os.Stat(diskPath); err == nil && !st.IsDir() {
				if b, err := os.ReadFile(diskPath); err == nil {
					return b, st.ModTime(), true
				}
			}
		}
	}
	b, err := fs.ReadFile(staticFS(), clean)
	if err != nil {
		return nil, time.Time{}, false
	}
	return b, buildTime, true
}

func (s *Server) serveStaticBytes(w http.ResponseWriter, r *http.Request, name string, body []byte, modTime time.Time) {
	setStaticHeaders(w, name, modTime)
	if maybeNotModified(w, r, modTime) {
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.ServeContent(w, r, name, modTime, bytes.NewReader(body))
}

func (s *Server) serveHTMLFile(w http.ResponseWriter, r *http.Request, name string) {
	body, modTime, ok := s.readStatic(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.serveStaticBytes(w, r, name, body, modTime)
}

func (s *Server) serveMaybeStatic(w http.ResponseWriter, r *http.Request) {
	clean := canonicalStaticPath(r.URL.Path)
	if clean == "." || clean == "" {
		s.serveHTMLFile(w, r, "index.html")
		return
	}
	// trackers.txt is generated at runtime by background/trackerscron and lives
	// in DataDir, not the embedded UI bundle.
	if strings.EqualFold(clean, "trackers.txt") {
		if s.serveDataFile(w, r, "trackers.txt") {
			return
		}
	}
	if body, modTime, ok := s.readStatic(clean); ok {
		s.serveStaticBytes(w, r, clean, body, modTime)
		return
	}
	if alias := staticAlias(clean); alias != "" {
		if body, modTime, ok := s.readStatic(alias); ok {
			s.serveStaticBytes(w, r, alias, body, modTime)
			return
		}
	}
	if shouldFallbackToIndex(r.URL.Path, r.Header.Get("Accept")) {
		if body, modTime, ok := s.readStatic("index.html"); ok {
			s.serveStaticBytes(w, r, "index.html", body, modTime)
			return
		}
	}
	http.NotFound(w, r)
}

// serveDataFile serves a runtime-generated file from DataDir (e.g. trackers.txt).
// Returns true if the file existed and was served.
func (s *Server) serveDataFile(w http.ResponseWriter, r *http.Request, name string) bool {
	dataDir := "Data"
	if s.DB != nil && s.DB.DataDir != "" {
		dataDir = s.DB.DataDir
	}
	path := filepath.Join(dataDir, name)
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return false
	}
	setStaticHeaders(w, name, st.ModTime())
	if maybeNotModified(w, r, st.ModTime()) {
		return true
	}
	http.ServeFile(w, r, path)
	return true
}

func canonicalStaticPath(path string) string {
	clean := filepath.Clean("/" + path)
	clean = strings.TrimPrefix(clean, "/")
	if clean == "." {
		return ""
	}
	return clean
}

func staticAlias(clean string) string {
	lc := strings.ToLower(strings.TrimSpace(clean))
	switch lc {
	case "manifest", "manifest.webmanifest", "site.webmanifest":
		return "manifest.json"
	case "index", "index.htm":
		return "index.html"
	default:
		return ""
	}
}

func maybeNotModified(w http.ResponseWriter, r *http.Request, modTime time.Time) bool {
	if modTime.IsZero() {
		return false
	}
	if ims := strings.TrimSpace(r.Header.Get("If-Modified-Since")); ims != "" {
		if t, err := http.ParseTime(ims); err == nil {
			if !modTime.UTC().After(t.UTC().Add(1 * time.Second)) {
				w.WriteHeader(http.StatusNotModified)
				return true
			}
		}
	}
	return false
}

func isWithinRoot(root, target string) bool {
	rootAbs, err1 := filepath.Abs(root)
	targetAbs, err2 := filepath.Abs(target)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && rel != "")
}

func shouldFallbackToIndex(path, accept string) bool {
	if path == "/" {
		return true
	}
	lp := strings.ToLower(strings.TrimSpace(path))
	if strings.HasPrefix(lp, "/api/") || strings.HasPrefix(lp, "/cron/") || strings.HasPrefix(lp, "/sync/") || strings.HasPrefix(lp, "/dev/") || strings.HasPrefix(lp, "/jsondb/") || strings.HasPrefix(lp, "/stats/") {
		return false
	}
	if filepath.Ext(lp) != "" {
		return false
	}
	accept = strings.ToLower(accept)
	return accept == "" || strings.Contains(accept, "text/html") || strings.Contains(accept, "*/*")
}

func setStaticHeaders(w http.ResponseWriter, name string, modTime time.Time) {
	ext := strings.ToLower(filepath.Ext(name))
	if ct := mime.TypeByExtension(ext); ct != "" {
		if ext == ".json" || ext == ".webmanifest" {
			if !strings.Contains(strings.ToLower(ct), "charset=") {
				ct += "; charset=utf-8"
			}
		}
		w.Header().Set("Content-Type", ct)
	}
	switch ext {
	case ".html":
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
	case ".json", ".webmanifest":
		w.Header().Set("Cache-Control", "public, max-age=300")
	case ".js", ".css", ".png", ".jpg", ".jpeg", ".gif", ".svg", ".ico", ".woff", ".woff2", ".ttf":
		w.Header().Set("Cache-Control", "public, max-age=86400")
	default:
		w.Header().Set("Cache-Control", "public, max-age=300")
	}
	w.Header().Set("Vary", "Accept-Encoding")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !modTime.IsZero() {
		w.Header().Set("Last-Modified", modTime.UTC().Format(http.TimeFormat))
	}
}
