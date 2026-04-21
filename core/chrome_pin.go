package core

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// cftBaseURL is the Chrome for Testing download base.
const cftBaseURL = "https://storage.googleapis.com/chrome-for-testing-public"

// cftLatestURL returns the endpoint that maps a major version to a full
// "major.minor.build.patch" version string.
func cftLatestURL(major string) string {
	return "https://googlechromelabs.github.io/chrome-for-testing/LATEST_RELEASE_" + major
}

// cftPlatform returns Chrome for Testing's platform folder name for the
// current GOOS/GOARCH (e.g. "linux64", "mac-arm64", "win64"). Empty string
// means the platform is not supported.
func cftPlatform() string {
	switch runtime.GOOS {
	case "linux":
		if runtime.GOARCH == "amd64" {
			return "linux64"
		}
	case "darwin":
		if runtime.GOARCH == "arm64" {
			return "mac-arm64"
		}
		if runtime.GOARCH == "amd64" {
			return "mac-x64"
		}
	case "windows":
		if runtime.GOARCH == "amd64" {
			return "win64"
		}
		if runtime.GOARCH == "386" {
			return "win32"
		}
	}
	return ""
}

// chromePinCacheDir returns the directory where pinned Chrome installs live.
// Layout: <cache>/jacred/chrome-for-testing/<full-version>/
func chromePinCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = filepath.Join(os.TempDir(), "jacred-cache")
	}
	return filepath.Join(base, "jacred", "chrome-for-testing"), nil
}

// EnsureChromeVersion downloads and extracts the pinned Chrome for Testing
// build + matching chromedriver if not already present. Returns the absolute
// paths to the chrome binary and chromedriver binary.
//
// version may be a major ("146") or a full build ("146.0.7103.92"). When only
// a major is given, the latest patch for that major is resolved via
// googlechromelabs.github.io.
func EnsureChromeVersion(version string) (browserPath, driverPath string, err error) {
	version = strings.TrimSpace(version)
	if version == "" {
		return "", "", fmt.Errorf("chrome_version is empty")
	}
	platform := cftPlatform()
	if platform == "" {
		return "", "", fmt.Errorf("chrome_version: unsupported platform %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	full, err := resolveChromeFullVersion(version)
	if err != nil {
		return "", "", err
	}

	root, err := chromePinCacheDir()
	if err != nil {
		return "", "", err
	}
	installDir := filepath.Join(root, full)

	chromeBin, driverBin := pinnedBinaryPaths(installDir, platform)

	chromeOK := fileExists(chromeBin)
	driverOK := fileExists(driverBin)
	if chromeOK && driverOK {
		return chromeBin, driverBin, nil
	}

	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return "", "", fmt.Errorf("chrome_version: mkdir cache: %w", err)
	}

	if !chromeOK {
		url := fmt.Sprintf("%s/%s/%s/chrome-%s.zip", cftBaseURL, full, platform, platform)
		if err := downloadAndExtract(url, installDir); err != nil {
			return "", "", fmt.Errorf("chrome_version: download chrome: %w", err)
		}
		if !fileExists(chromeBin) {
			return "", "", fmt.Errorf("chrome_version: chrome binary not found after extract at %s", chromeBin)
		}
		_ = os.Chmod(chromeBin, 0o755)
	}

	if !driverOK {
		url := fmt.Sprintf("%s/%s/%s/chromedriver-%s.zip", cftBaseURL, full, platform, platform)
		if err := downloadAndExtract(url, installDir); err != nil {
			return "", "", fmt.Errorf("chrome_version: download chromedriver: %w", err)
		}
		if !fileExists(driverBin) {
			return "", "", fmt.Errorf("chrome_version: chromedriver not found after extract at %s", driverBin)
		}
		_ = os.Chmod(driverBin, 0o755)
	}

	return chromeBin, driverBin, nil
}

// pinnedBinaryPaths returns where the chrome + chromedriver binaries land
// inside installDir after extracting the Chrome for Testing zips. Layout
// mirrors the zip structure: chrome-<platform>/chrome and
// chromedriver-<platform>/chromedriver.
func pinnedBinaryPaths(installDir, platform string) (chromeBin, driverBin string) {
	chromeExe := "chrome"
	driverExe := "chromedriver"
	if runtime.GOOS == "darwin" {
		chromeExe = filepath.Join("Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing")
	}
	if runtime.GOOS == "windows" {
		chromeExe = "chrome.exe"
		driverExe = "chromedriver.exe"
	}
	chromeBin = filepath.Join(installDir, "chrome-"+platform, chromeExe)
	driverBin = filepath.Join(installDir, "chromedriver-"+platform, driverExe)
	return
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// resolveChromeFullVersion turns a major like "146" into "146.0.7103.92" by
// calling googlechromelabs.github.io. Full versions are returned as-is.
func resolveChromeFullVersion(version string) (string, error) {
	if strings.Count(version, ".") >= 3 {
		return version, nil
	}
	major := strings.SplitN(version, ".", 2)[0]
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cftLatestURL(major), nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("resolve Chrome version: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("resolve Chrome version: unexpected status %d for major %s", resp.StatusCode, major)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil {
		return "", err
	}
	full := strings.TrimSpace(string(body))
	if full == "" {
		return "", fmt.Errorf("resolve Chrome version: empty response for major %s", major)
	}
	return full, nil
}

func downloadAndExtract(url, destDir string) error {
	tmp, err := os.CreateTemp("", "cft-*.zip")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
		return fmt.Errorf("download %s: status %d", url, resp.StatusCode)
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

// unzip extracts src into destDir. Guards against ZipSlip by rejecting entries
// whose resolved path escapes destDir.
func unzip(src, destDir string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()

	cleanDest, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}
	cleanDest = filepath.Clean(cleanDest) + string(os.PathSeparator)

	for _, f := range zr.File {
		target := filepath.Join(destDir, f.Name)
		absTarget, err := filepath.Abs(target)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(absTarget+string(os.PathSeparator), cleanDest) && absTarget+string(os.PathSeparator) != cleanDest {
			return fmt.Errorf("unzip: entry escapes dest: %s", f.Name)
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		mode := f.Mode()
		if mode == 0 {
			mode = 0o644
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			rc.Close()
			return err
		}
		if _, err := io.Copy(out, rc); err != nil {
			rc.Close()
			out.Close()
			return err
		}
		rc.Close()
		if err := out.Close(); err != nil {
			return err
		}
	}
	return nil
}
