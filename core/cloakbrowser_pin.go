package core

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
// following /releases/latest) is what keeps the download key-free. The
// `-pro` suffix filter is the independent guard: it stays correct even if a
// paid release ever ships binaries.
const (
	cloakBrowserFreeTag   = "chromium-v146."
	cloakBrowserProSuffix = "-pro"
	cloakBrowserSumsAsset = "SHA256SUMS"
)

// cloakBrowserReleasesAPI is a var, not a const, so tests can point the
// resolver at a local fixture server.
var cloakBrowserReleasesAPI = "https://api.github.com/repos/CloakHQ/CloakBrowser/releases?per_page=30"

// cloakBrowserAsset returns the release asset for this platform and the tag
// prefix that constrains which releases may supply it. An empty prefix means
// "any free release", used where a version pin would exclude the platform
// outright.
func cloakBrowserAsset() (asset, tagPrefix string) {
	return cloakBrowserAssetFor(runtime.GOOS, runtime.GOARCH)
}

// cloakBrowserAssetFor is the table behind cloakBrowserAsset, split out so the
// platform mapping can be tested without cross-compiling.
func cloakBrowserAssetFor(goos, goarch string) (asset, tagPrefix string) {
	switch goos {
	case "linux":
		switch goarch {
		case "amd64":
			return "cloakbrowser-linux-x64.tar.gz", cloakBrowserFreeTag
		case "arm64":
			return "cloakbrowser-linux-arm64.tar.gz", cloakBrowserFreeTag
		}
	case "darwin":
		// Upstream calls these "darwin", not "macos", and has shipped no macOS
		// build since v145 — so the v146 pin would rule the platform out even
		// with the names fixed. Left unpinned: take the newest free release
		// that actually carries the asset.
		switch goarch {
		case "arm64":
			return "cloakbrowser-darwin-arm64.tar.gz", ""
		case "amd64":
			return "cloakbrowser-darwin-x64.tar.gz", ""
		}
	case "windows":
		if goarch == "amd64" {
			return "cloakbrowser-windows-x64.zip", cloakBrowserFreeTag
		}
	}
	return "", ""
}

// cloakBrowserCacheDirFn is indirected so tests can exercise the resolve /
// cache / fallback flow against a temp dir.
var cloakBrowserCacheDirFn = cloakBrowserCacheDir

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
// this platform when the cache does not already hold that exact tag (~207 MB
// on Linux, ~700 MB once extracted).
//
// The driver comes out of the same archive on purpose: chromedriver must match
// the browser's major version, and taking both from one tag makes that true by
// construction instead of by configuration.
//
// Resolution happens *before* the cache is consulted. Checking the cache first
// (as this used to) means the first install ever downloaded wins forever: a
// newer free build is never picked up, and because the lookup walked the whole
// cache root in lexical order, a second tag alongside it would leave the older
// one winning permanently.
func EnsureCloakBrowser() (browserPath, driverPath string, err error) {
	installDir, err := cloakBrowserCacheDirFn()
	if err != nil {
		return "", "", err
	}
	browserName, driverName := cloakBrowserBinaryNames()

	asset, tagPrefix := cloakBrowserAsset()
	if asset == "" {
		return "", "", fmt.Errorf("cloakbrowser: unsupported platform %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	rel, err := resolveCloakBrowserAsset(asset, tagPrefix)
	if err != nil {
		// GitHub being down or rate-limiting us is not a reason to give up a
		// working install — that would drop CF bypass for every gated tracker.
		if b, d, tag := newestLocalCloakBrowser(installDir); b != "" {
			log.Printf("cloakbrowser: %v — falling back to cached %s", err, tag)
			return b, d, nil
		}
		return "", "", err
	}

	if b, d := findCloakBrowserInTag(installDir, rel.tag); b != "" && d != "" {
		return b, d, nil
	}

	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return "", "", fmt.Errorf("cloakbrowser: mkdir %s: %w", installDir, err)
	}

	destDir := filepath.Join(installDir, rel.tag)
	log.Printf("cloakbrowser: downloading %s %s (~%d MB) to %s", rel.tag, asset, rel.sizeMB, destDir)
	if err := downloadAndExtractCloakBrowser(rel, destDir); err != nil {
		if b, d, tag := newestLocalCloakBrowser(installDir); b != "" {
			log.Printf("cloakbrowser: %v — falling back to cached %s", err, tag)
			return b, d, nil
		}
		return "", "", fmt.Errorf("cloakbrowser: %w", err)
	}

	b, d := findCloakBrowserInTag(installDir, rel.tag)
	if b == "" || d == "" {
		return "", "", fmt.Errorf("cloakbrowser: %q/%q not found under %s after extract", browserName, driverName, destDir)
	}
	_ = os.Chmod(b, 0o755)
	_ = os.Chmod(d, 0o755)
	pruneCloakBrowserInstalls(installDir, rel.tag)
	log.Printf("cloakbrowser: ready %s browser=%s driver=%s", rel.tag, b, d)
	return b, d, nil
}

// findCloakBrowserInTag locates an already-extracted install of one specific
// tag. Both binaries must be present, otherwise a half-finished extract would
// be reported as a usable install.
func findCloakBrowserInTag(installDir, tag string) (browser, driver string) {
	browserName, driverName := cloakBrowserBinaryNames()
	root := filepath.Join(installDir, tag)
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return "", ""
	}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
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

// newestLocalCloakBrowser returns the most recently installed complete tag in
// the cache. Used only when the release list is unreachable. Ordering is by
// mtime rather than by name because tag names are dotted version strings that
// do not sort lexically (…177.10 would land before …177.5).
func newestLocalCloakBrowser(installDir string) (browser, driver, tag string) {
	entries, err := os.ReadDir(installDir)
	if err != nil {
		return "", "", ""
	}
	var newest time.Time
	for _, e := range entries {
		if !e.IsDir() || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		b, d := findCloakBrowserInTag(installDir, e.Name())
		if b == "" || d == "" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newest) {
			newest, browser, driver, tag = info.ModTime(), b, d, e.Name()
		}
	}
	return browser, driver, tag
}

// pruneCloakBrowserInstalls drops every install except the one just made.
// Each is ~700 MB extracted, so keeping the previous tag around after a
// successful upgrade costs more than it is worth.
func pruneCloakBrowserInstalls(installDir, keepTag string) {
	entries, err := os.ReadDir(installDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == keepTag {
			continue
		}
		if err := os.RemoveAll(filepath.Join(installDir, e.Name())); err == nil {
			log.Printf("cloakbrowser: removed superseded install %s", e.Name())
		}
	}
}

// cloakBrowserRelease is one resolved download: the archive plus the checksum
// file published beside it in the same release.
type cloakBrowserRelease struct {
	assetName string
	assetURL  string
	sumsURL   string // empty when the release publishes no SHA256SUMS
	tag       string
	sizeMB    int
}

// resolveCloakBrowserAsset walks the release list newest-first and returns the
// first free release that actually carries this platform's asset. Not every
// tag was published for every platform, so "newest tag" alone is not enough —
// it has to be the newest tag that has the file. tagPrefix narrows that to one
// version line; empty accepts any free release.
func resolveCloakBrowserAsset(assetName, tagPrefix string) (cloakBrowserRelease, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cloakBrowserReleasesAPI, nil)
	if err != nil {
		return cloakBrowserRelease{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "jacred-go")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return cloakBrowserRelease{}, fmt.Errorf("cloakbrowser: query github api: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return cloakBrowserRelease{}, fmt.Errorf("cloakbrowser: github api status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
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
		return cloakBrowserRelease{}, fmt.Errorf("cloakbrowser: decode releases: %w", err)
	}
	for _, r := range releases {
		if !strings.HasPrefix(r.TagName, tagPrefix) || strings.HasSuffix(r.TagName, cloakBrowserProSuffix) {
			continue
		}
		out := cloakBrowserRelease{tag: r.TagName, assetName: assetName}
		for _, a := range r.Assets {
			switch a.Name {
			case assetName:
				out.assetURL, out.sizeMB = a.URL, int(a.Size/(1024*1024))
			case cloakBrowserSumsAsset:
				out.sumsURL = a.URL
			}
		}
		if out.assetURL != "" {
			return out, nil
		}
	}
	scope := tagPrefix + "*"
	if tagPrefix == "" {
		scope = "any free"
	}
	return cloakBrowserRelease{}, fmt.Errorf("cloakbrowser: no %s asset in %s release", assetName, scope)
}

// fetchCloakBrowserSum pulls the expected SHA-256 for assetName out of the
// release's SHA256SUMS. Format is a `version=` header line followed by
// standard `<hex>  <filename>` rows.
func fetchCloakBrowserSum(sumsURL, assetName string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sumsURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "jacred-go")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(body), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == assetName {
			return f[0], nil
		}
	}
	return "", fmt.Errorf("no entry for %s", assetName)
}

// verifySHA256 hashes path and compares it against want.
func verifySHA256(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", got, want)
	}
	return nil
}

// downloadAndExtractCloakBrowser streams the archive to a temp file, verifies
// it against the release's SHA256SUMS, and unpacks it. Extraction goes to a
// temp sibling first and is renamed into place, so an interrupted download
// can't leave a partial install that findCloakBrowserInTag would later accept.
//
// The checksum shares GitHub's TLS with the archive, so it is not a defence
// against a compromised publisher — it catches truncated and corrupted
// downloads before ~700 MB of unusable Chromium is unpacked and executed. A
// release with no SHA256SUMS (some older tags) warns rather than fails, since
// that says nothing about the archive's integrity; a checksum that is present
// and does not match is fatal.
func downloadAndExtractCloakBrowser(rel cloakBrowserRelease, destDir string) error {
	rawURL := rel.assetURL
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

	if rel.sumsURL == "" {
		log.Printf("cloakbrowser: release %s publishes no %s — archive not verified", rel.tag, cloakBrowserSumsAsset)
	} else if want, err := fetchCloakBrowserSum(rel.sumsURL, rel.assetName); err != nil {
		log.Printf("cloakbrowser: could not read %s for %s (%v) — archive not verified", cloakBrowserSumsAsset, rel.assetName, err)
	} else if err := verifySHA256(tmpPath, want); err != nil {
		return fmt.Errorf("verify %s: %w", rel.assetName, err)
	} else {
		log.Printf("cloakbrowser: sha256 verified for %s", rel.assetName)
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
