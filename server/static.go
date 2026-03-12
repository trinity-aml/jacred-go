package server

import (
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *Server) serveHTMLFile(w http.ResponseWriter, r *http.Request, name string) {
	path := filepath.Join(s.WWWRoot, name)
	if st, err := os.Stat(path); err == nil && !st.IsDir() {
		setStaticHeaders(w, path)
		if maybeNotModified(w, r, st.ModTime()) {
			return
		}
		http.ServeFile(w, r, path)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) serveMaybeStatic(w http.ResponseWriter, r *http.Request) {
	if s.WWWRoot == "" {
		http.NotFound(w, r)
		return
	}
	clean := canonicalStaticPath(r.URL.Path)
	if clean == "." || clean == "" {
		s.serveHTMLFile(w, r, "index.html")
		return
	}
	path := filepath.Join(s.WWWRoot, filepath.FromSlash(clean))
	if !isWithinRoot(s.WWWRoot, path) {
		http.NotFound(w, r)
		return
	}
	if st, err := os.Stat(path); err == nil && !st.IsDir() {
		setStaticHeaders(w, path)
		if maybeNotModified(w, r, st.ModTime()) {
			return
		}
		http.ServeFile(w, r, path)
		return
	}
	if alias := staticAlias(clean); alias != "" {
		aliasPath := filepath.Join(s.WWWRoot, filepath.FromSlash(alias))
		if st, err := os.Stat(aliasPath); err == nil && !st.IsDir() {
			setStaticHeaders(w, aliasPath)
			if maybeNotModified(w, r, st.ModTime()) {
				return
			}
			http.ServeFile(w, r, aliasPath)
			return
		}
	}
	if shouldFallbackToIndex(r.URL.Path, r.Header.Get("Accept")) {
		fallback := filepath.Join(s.WWWRoot, "index.html")
		if st, err := os.Stat(fallback); err == nil && !st.IsDir() {
			setStaticHeaders(w, fallback)
			if maybeNotModified(w, r, st.ModTime()) {
				return
			}
			http.ServeFile(w, r, fallback)
			return
		}
	}
	http.NotFound(w, r)
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

func setStaticHeaders(w http.ResponseWriter, path string) {
	ext := strings.ToLower(filepath.Ext(path))
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
	if st, err := os.Stat(path); err == nil {
		w.Header().Set("Last-Modified", st.ModTime().UTC().Format(http.TimeFormat))
	}
}
