package core

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// CloakBrowser is a Chromium rebuilt with source-level fingerprint patches
// (github.com/CloakHQ/CloakBrowser). It is the only browser we have found that
// clears rutracker's Cloudflare managed challenge: Camoufox and stock Chrome
// both get the interactive Turnstile widget and loop on it, while CloakBrowser
// passes invisibly in ~7 s with no click.
//
// Only the v146 line is a public release asset — v148+ are Pro-only and their
// tags publish nothing but checksums. Pinning the prefix here (rather than
// following /releases/latest) is what keeps the download key-free.
const (
	cloakBrowserReleasesAPI = "https://api.github.com/repos/CloakHQ/CloakBrowser/releases?per_page=30"
	cloakBrowserFreeTag     = "chromium-v146."
	cloakBrowserProSuffix   = "-pro"
)

// cloakBrowserAsset returns the release asset name for the current platform.
// Empty when unsupported.
func cloakBrowserAsset() string {
	switch runtime.GOOS {
	case "linux":
		switch runtime.GOARCH {
		case "amd64":
			return "cloakbrowser-linux-x64.tar.gz"
		case "arm64":
			return "cloakbrowser-linux-arm64.tar.gz"
		}
	case "darwin":
		switch runtime.GOARCH {
		case "arm64":
			return "cloakbrowser-macos-arm64.tar.gz"
		case "amd64":
			return "cloakbrowser-macos-x64.tar.gz"
		}
	case "windows":
		if runtime.GOARCH == "amd64" {
			return "cloakbrowser-windows-x64.zip"
		}
	}
	return ""
}

func cloakBrowserCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = filepath.Join(os.TempDir(), "jacred-cache")
	}
	return filepath.Join(base, "jacred", "cloakbrowser"), nil
}

func cloakBrowserBinaryNames() (browser, driver string) {
	if runtime.GOOS == "windows" {
		return "chrome.exe", "chromedriver.exe"
	}
	return "chrome", "chromedriver"
}

// EnsureCloakBrowser returns paths to the CloakBrowser binary and the
// chromedriver shipped alongside it, downloading the newest free release for
// this platform on first use (~217 MB on Linux).
//
// The driver comes out of the same archive on purpose: chromedriver must match
// the browser's major version, and taking both from one tag makes that true by
// construction instead of by configuration.
func EnsureCloakBrowser() (browserPath, driverPath string, err error) {
	installDir, err := cloakBrowserCacheDir()
	if err != nil {
		return "", "", err
	}
	browserName, driverName := cloakBrowserBinaryNames()

	if b, d := findCloakBrowserInDir(installDir); b != "" && d != "" {
		return b, d, nil
	}

	asset := cloakBrowserAsset()
	if asset == "" {
		return "", "", fmt.Errorf("cloakbrowser: unsupported platform %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return "", "", fmt.Errorf("cloakbrowser: mkdir %s: %w", installDir, err)
	}

	assetURL, tag, sizeMB, err := resolveCloakBrowserAsset(asset)
	if err != nil {
		return "", "", err
	}

	destDir := filepath.Join(installDir, tag)
	log.Printf("cloakbrowser: downloading %s %s (~%d MB) to %s", tag, asset, sizeMB, destDir)
	if err := downloadAndExtractCloakBrowser(assetURL, destDir); err != nil {
		return "", "", fmt.Errorf("cloakbrowser: %w", err)
	}

	b, d := findCloakBrowserInDir(installDir)
	if b == "" || d == "" {
		return "", "", fmt.Errorf("cloakbrowser: %q/%q not found under %s after extract", browserName, driverName, installDir)
	}
	_ = os.Chmod(b, 0o755)
	_ = os.Chmod(d, 0o755)
	log.Printf("cloakbrowser: ready browser=%s driver=%s", b, d)
	return b, d, nil
}

// findCloakBrowserInDir locates an already-extracted install. Both binaries
// must be present, otherwise a half-finished extract would be reported as a
// usable install.
func findCloakBrowserInDir(dir string) (browser, driver string) {
	browserName, driverName := cloakBrowserBinaryNames()
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		switch filepath.Base(path) {
		case browserName:
			browser = path
		case driverName:
			driver = path
		}
		if browser != "" && driver != "" {
			return filepath.SkipAll
		}
		return nil
	})
	if browser == "" || driver == "" {
		return "", ""
	}
	return browser, driver
}

// resolveCloakBrowserAsset walks the release list newest-first and returns the
// first free release that actually carries this platform's asset. Not every
// v146 tag was published for every platform, so "newest tag" alone is not
// enough — it has to be the newest tag that has the file.
func resolveCloakBrowserAsset(assetName string) (url, tag string, sizeMB int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cloakBrowserReleasesAPI, nil)
	if err != nil {
		return "", "", 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "jacred-go")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", 0, fmt.Errorf("cloakbrowser: query github api: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", "", 0, fmt.Errorf("cloakbrowser: github api status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var releases []struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return "", "", 0, fmt.Errorf("cloakbrowser: decode releases: %w", err)
	}
	for _, r := range releases {
		if !strings.HasPrefix(r.TagName, cloakBrowserFreeTag) || strings.HasSuffix(r.TagName, cloakBrowserProSuffix) {
			continue
		}
		for _, a := range r.Assets {
			if a.Name == assetName {
				return a.URL, r.TagName, int(a.Size / (1024 * 1024)), nil
			}
		}
	}
	return "", "", 0, fmt.Errorf("cloakbrowser: no %s asset in any %s* release", assetName, cloakBrowserFreeTag)
}

// downloadAndExtractCloakBrowser streams the archive to a temp file and
// unpacks it. Extraction goes to a temp sibling first and is renamed into
// place, so an interrupted download can't leave a partial install that
// findCloakBrowserInDir would later accept.
func downloadAndExtractCloakBrowser(rawURL, destDir string) error {
	suffix := ".tar.gz"
	if strings.HasSuffix(rawURL, ".zip") {
		suffix = ".zip"
	}
	tmp, err := os.CreateTemp("", "cloakbrowser-*"+suffix)
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		tmp.Close()
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		tmp.Close()
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		tmp.Close()
		return fmt.Errorf("download %s: status %d", rawURL, resp.StatusCode)
	}
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	stageDir := destDir + ".tmp"
	_ = os.RemoveAll(stageDir)
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return err
	}
	if suffix == ".zip" {
		err = unzip(tmpPath, stageDir)
	} else {
		err = untarGz(tmpPath, stageDir)
	}
	if err != nil {
		_ = os.RemoveAll(stageDir)
		return err
	}
	_ = os.RemoveAll(destDir)
	return os.Rename(stageDir, destDir)
}

// untarGz extracts a .tar.gz into destDir, rejecting entries that would escape
// it. Symlinks are preserved — the Chromium bundle relies on them.
func untarGz(src, destDir string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(destDir, filepath.Clean("/"+hdr.Name))
		if !strings.HasPrefix(target, filepath.Clean(destDir)+string(os.PathSeparator)) {
			return fmt.Errorf("tar entry escapes destination: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		}
	}
}
