package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const camoufoxLatestAPI = "https://api.github.com/repos/daijro/camoufox/releases/latest"

// camoufoxAssetSuffix returns the daijro/camoufox asset-name suffix for the
// current platform (e.g. "lin.x86_64.zip"). Empty when unsupported.
func camoufoxAssetSuffix() string {
	switch runtime.GOOS {
	case "linux":
		switch runtime.GOARCH {
		case "amd64":
			return "lin.x86_64.zip"
		case "arm64":
			return "lin.arm64.zip"
		case "386":
			return "lin.i686.zip"
		}
	case "darwin":
		switch runtime.GOARCH {
		case "arm64":
			return "mac.arm64.zip"
		case "amd64":
			return "mac.x86_64.zip"
		}
	case "windows":
		switch runtime.GOARCH {
		case "amd64":
			return "win.x86_64.zip"
		case "386":
			return "win.i686.zip"
		}
	}
	return ""
}

func camoufoxCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = filepath.Join(os.TempDir(), "jacred-cache")
	}
	return filepath.Join(base, "jacred", "camoufox"), nil
}

func camoufoxBinaryName() string {
	if runtime.GOOS == "windows" {
		return "camoufox.exe"
	}
	return "camoufox"
}

// findCamoufoxInDir recursively locates the camoufox executable under dir.
// Returns empty string when not found.
func findCamoufoxInDir(dir string) string {
	name := camoufoxBinaryName()
	var found string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && filepath.Base(path) == name {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// FindCamoufoxBinary checks standard locations for an existing Camoufox
// install. Returns empty string when nothing is found. Order:
//  1. camoufox on PATH
//  2. ~/.cache/camoufox/camoufox (daijro's recommended layout)
//  3. jacred's own cache dir (populated by EnsureCamoufox)
func FindCamoufoxBinary() string {
	if p, err := exec.LookPath("camoufox"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, rel := range []string{".cache/camoufox/camoufox", ".cache/camoufox/camoufox-bin"} {
			full := filepath.Join(home, rel)
			if info, err := os.Stat(full); err == nil && !info.IsDir() {
				return full
			}
		}
	}
	if dir, err := camoufoxCacheDir(); err == nil {
		if p := findCamoufoxInDir(dir); p != "" {
			return p
		}
	}
	return ""
}

// EnsureCamoufox returns a path to a Camoufox binary. If one is already
// installed in a standard location (PATH, ~/.cache/camoufox, or jacred's
// cache) it is returned. Otherwise the latest daijro/camoufox release for
// the current GOOS/GOARCH is downloaded and extracted into jacred's cache.
// Downloads are ~280 MB on macOS, ~680 MB on Linux.
func EnsureCamoufox() (string, error) {
	if p := FindCamoufoxBinary(); p != "" {
		return p, nil
	}
	suffix := camoufoxAssetSuffix()
	if suffix == "" {
		return "", fmt.Errorf("camoufox: unsupported platform %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	installDir, err := camoufoxCacheDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return "", fmt.Errorf("camoufox: mkdir %s: %w", installDir, err)
	}

	assetURL, sizeMB, err := resolveCamoufoxAsset(suffix)
	if err != nil {
		return "", err
	}

	log.Printf("camoufox: downloading %s (~%d MB) to %s", filepath.Base(assetURL), sizeMB, installDir)
	if err := downloadAndExtractCamoufox(assetURL, installDir); err != nil {
		return "", fmt.Errorf("camoufox: %w", err)
	}

	bin := findCamoufoxInDir(installDir)
	if bin == "" {
		return "", fmt.Errorf("camoufox: binary %q not found under %s after extract", camoufoxBinaryName(), installDir)
	}
	_ = os.Chmod(bin, 0o755)
	log.Printf("camoufox: ready at %s", bin)
	return bin, nil
}

// resolveCamoufoxAsset queries the daijro/camoufox GitHub releases API for the
// latest release and returns the download URL + size (MB) of the asset whose
// name ends with the given platform suffix.
func resolveCamoufoxAsset(suffix string) (url string, sizeMB int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, camoufoxLatestAPI, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "jacred-go")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("camoufox: query github api: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", 0, fmt.Errorf("camoufox: github api status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var release struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", 0, fmt.Errorf("camoufox: decode release: %w", err)
	}
	for _, a := range release.Assets {
		if strings.HasSuffix(a.Name, suffix) {
			return a.URL, int(a.Size / (1024 * 1024)), nil
		}
	}
	return "", 0, fmt.Errorf("camoufox: no asset with suffix %q in release %s", suffix, release.TagName)
}

// downloadAndExtractCamoufox streams the zip into a temp file and unzips it
// into destDir. 30-minute timeout accommodates the large archive (~680 MB
// linux, ~280 MB macOS) on slow connections.
func downloadAndExtractCamoufox(rawURL, destDir string) error {
	tmp, err := os.CreateTemp("", "camoufox-*.zip")
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
	return unzip(tmpPath, destDir)
}
