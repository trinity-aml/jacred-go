package core

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// releasesFixture mirrors the shape of the real GitHub response, trimmed to
// the tags that matter. Ordering is newest-first, as the API returns it.
const releasesFixture = `[
 {"tag_name":"chromium-v150.0.7871.114.4-pro","assets":[
   {"name":"SHA256SUMS","browser_download_url":"https://x/150/SHA256SUMS","size":100}]},
 {"tag_name":"chromium-v148.0.7778.215.5-pro","assets":[
   {"name":"SHA256SUMS","browser_download_url":"https://x/148/SHA256SUMS","size":100}]},
 {"tag_name":"chromium-v146.0.7680.177.5","assets":[
   {"name":"cloakbrowser-linux-x64.tar.gz","browser_download_url":"https://x/146.5/linux-x64","size":216890134},
   {"name":"cloakbrowser-windows-x64.zip","browser_download_url":"https://x/146.5/win-x64","size":562000000},
   {"name":"SHA256SUMS","browser_download_url":"https://x/146.5/SHA256SUMS","size":100}]},
 {"tag_name":"chromium-v146.0.7680.177.4","assets":[
   {"name":"cloakbrowser-linux-arm64.tar.gz","browser_download_url":"https://x/146.4/linux-arm64","size":200000000},
   {"name":"cloakbrowser-linux-x64.tar.gz","browser_download_url":"https://x/146.4/linux-x64","size":216000000},
   {"name":"SHA256SUMS","browser_download_url":"https://x/146.4/SHA256SUMS","size":100}]},
 {"tag_name":"chromium-v145.0.7632.109.2","assets":[
   {"name":"cloakbrowser-darwin-arm64.tar.gz","browser_download_url":"https://x/145/darwin-arm64","size":190000000},
   {"name":"cloakbrowser-darwin-x64.tar.gz","browser_download_url":"https://x/145/darwin-x64","size":195000000},
   {"name":"SHA256SUMS","browser_download_url":"https://x/145/SHA256SUMS","size":100}]},
 {"tag_name":"chromium-v142.0.7444.175","assets":[
   {"name":"cloakbrowser-darwin-arm64.tar.gz","browser_download_url":"https://x/142/darwin-arm64","size":180000000}]}
]`

func serveReleases(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, releasesFixture)
	}))
	t.Cleanup(srv.Close)
	orig := cloakBrowserReleasesAPI
	cloakBrowserReleasesAPI = srv.URL
	t.Cleanup(func() { cloakBrowserReleasesAPI = orig })
}

// fakeArchive builds a .tar.gz shaped like the real one: chrome and
// chromedriver flat at the top, plus a symlink, since the Chromium bundle
// relies on those surviving extraction.
func fakeArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"chrome", "chromedriver"} {
		body := []byte("#!/bin/false\n" + name)
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: "libEGL.so", Linkname: "chrome", Typeflag: tar.TypeSymlink, Mode: 0o777,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// downloadServer serves a release list whose asset URLs point back at itself,
// so EnsureCloakBrowser can be driven end to end: resolve, download, verify,
// extract, prune. corrupt makes the served archive disagree with the published
// checksum.
func downloadServer(t *testing.T, tag string, corrupt bool) (*httptest.Server, *int) {
	t.Helper()
	archive := fakeArchive(t)
	sum := sha256.Sum256(archive)
	asset, _ := cloakBrowserAsset()
	served := 0

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/releases", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `[{"tag_name":%q,"assets":[
			{"name":%q,"browser_download_url":%q,"size":%d},
			{"name":"SHA256SUMS","browser_download_url":%q,"size":100}]}]`,
			tag, asset, srv.URL+"/dl/"+asset, len(archive), srv.URL+"/dl/SHA256SUMS")
	})
	mux.HandleFunc("/dl/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "version=%s\n%s  %s\n", tag, hex.EncodeToString(sum[:]), asset)
	})
	mux.HandleFunc("/dl/"+asset, func(w http.ResponseWriter, r *http.Request) {
		served++
		body := archive
		if corrupt {
			body = append(append([]byte{}, archive...), "tampered"...)
		}
		w.Write(body)
	})

	orig := cloakBrowserReleasesAPI
	cloakBrowserReleasesAPI = srv.URL + "/releases"
	t.Cleanup(func() { cloakBrowserReleasesAPI = orig })
	return srv, &served
}

// The asset names are upstream's, not ours: macOS builds are published as
// "darwin", so the old "macos" spelling matched nothing and silently ruled the
// platform out. Guard the exact strings.
func TestCloakBrowserAssetFor(t *testing.T) {
	cases := []struct {
		goos, goarch  string
		asset, prefix string
	}{
		{"linux", "amd64", "cloakbrowser-linux-x64.tar.gz", cloakBrowserFreeTag},
		{"linux", "arm64", "cloakbrowser-linux-arm64.tar.gz", cloakBrowserFreeTag},
		{"windows", "amd64", "cloakbrowser-windows-x64.zip", cloakBrowserFreeTag},
		// darwin is deliberately unpinned — no v146 tag ships a macOS build.
		{"darwin", "arm64", "cloakbrowser-darwin-arm64.tar.gz", ""},
		{"darwin", "amd64", "cloakbrowser-darwin-x64.tar.gz", ""},
		{"freebsd", "amd64", "", ""},
		{"windows", "arm64", "", ""},
	}
	for _, c := range cases {
		asset, prefix := cloakBrowserAssetFor(c.goos, c.goarch)
		if asset != c.asset || prefix != c.prefix {
			t.Errorf("%s/%s: got (%q,%q), want (%q,%q)", c.goos, c.goarch, asset, prefix, c.asset, c.prefix)
		}
	}
}

func TestResolveCloakBrowserAsset(t *testing.T) {
	serveReleases(t)

	t.Run("linux amd64 takes newest v146", func(t *testing.T) {
		rel, err := resolveCloakBrowserAsset(cloakBrowserAssetFor("linux", "amd64"))
		if err != nil {
			t.Fatal(err)
		}
		if rel.tag != "chromium-v146.0.7680.177.5" {
			t.Errorf("tag = %q", rel.tag)
		}
		if rel.sumsURL != "https://x/146.5/SHA256SUMS" {
			t.Errorf("sumsURL = %q", rel.sumsURL)
		}
		if rel.sizeMB != 206 {
			t.Errorf("sizeMB = %d, want 206", rel.sizeMB)
		}
	})

	// The newest tag does not carry every platform, so the walk has to keep
	// going rather than stop at the first free tag it sees.
	t.Run("linux arm64 falls back to an older tag that has the asset", func(t *testing.T) {
		rel, err := resolveCloakBrowserAsset(cloakBrowserAssetFor("linux", "arm64"))
		if err != nil {
			t.Fatal(err)
		}
		if rel.tag != "chromium-v146.0.7680.177.4" {
			t.Errorf("tag = %q, want the .4 tag", rel.tag)
		}
	})

	// With the v146 pin this returned an error and the platform fell back to
	// stock Chrome. Unpinned it reaches the v145 line that actually has it.
	t.Run("darwin resolves across the version line", func(t *testing.T) {
		for _, arch := range []string{"arm64", "amd64"} {
			rel, err := resolveCloakBrowserAsset(cloakBrowserAssetFor("darwin", arch))
			if err != nil {
				t.Fatalf("darwin/%s: %v", arch, err)
			}
			if rel.tag != "chromium-v145.0.7632.109.2" {
				t.Errorf("darwin/%s: tag = %q", arch, rel.tag)
			}
		}
	})

	// Pro tags are skipped on the suffix alone, independent of the version
	// prefix — that is what still holds if a paid release ever ships binaries.
	t.Run("pro releases are never selected", func(t *testing.T) {
		rel, err := resolveCloakBrowserAsset("SHA256SUMS", "")
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(rel.tag, cloakBrowserProSuffix) {
			t.Errorf("selected pro tag %q", rel.tag)
		}
		if rel.tag != "chromium-v146.0.7680.177.5" {
			t.Errorf("tag = %q", rel.tag)
		}
	})

	t.Run("missing asset is an error", func(t *testing.T) {
		if _, err := resolveCloakBrowserAsset("cloakbrowser-plan9-x64.tar.gz", ""); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestFetchCloakBrowserSum(t *testing.T) {
	body := "version=146.0.7680.177.5\n" +
		"4a12bcde95fa1bb1beef2b41ab5e5c27c36be78e3be3d0dac8c64d705216670e  cloakbrowser-linux-x64.tar.gz\n" +
		"b213795cb32c3169f766c74ce1d0275fc89d3df256de39c04da7fb4c23b7fdbe  cloakbrowser-windows-x64.zip\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	got, err := fetchCloakBrowserSum(srv.URL, "cloakbrowser-linux-x64.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if got != "4a12bcde95fa1bb1beef2b41ab5e5c27c36be78e3be3d0dac8c64d705216670e" {
		t.Errorf("got %q", got)
	}
	// The `version=` header line must not be mistaken for a checksum row.
	if _, err := fetchCloakBrowserSum(srv.URL, "146.0.7680.177.5"); err == nil {
		t.Error("version= line was parsed as an entry")
	}
	if _, err := fetchCloakBrowserSum(srv.URL, "cloakbrowser-darwin-x64.tar.gz"); err == nil {
		t.Error("expected error for absent asset")
	}
}

func TestVerifySHA256(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "blob")
	payload := []byte("cloakbrowser archive contents")
	if err := os.WriteFile(p, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	hexsum := hex.EncodeToString(sum[:])

	if err := verifySHA256(p, hexsum); err != nil {
		t.Errorf("matching sum rejected: %v", err)
	}
	if err := verifySHA256(p, strings.ToUpper(hexsum)); err != nil {
		t.Errorf("uppercase sum rejected: %v", err)
	}
	if err := verifySHA256(p, strings.Repeat("0", 64)); err == nil {
		t.Error("mismatched sum accepted")
	}
}

// installTag lays down a fake extracted install.
func installTag(t *testing.T, root, tag string, complete bool) {
	t.Helper()
	dir := filepath.Join(root, tag)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "chrome"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if complete {
		if err := os.WriteFile(filepath.Join(dir, "chromedriver"), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFindCloakBrowserInTag(t *testing.T) {
	root := t.TempDir()
	installTag(t, root, "chromium-v146.0.7680.177.5", true)
	installTag(t, root, "chromium-v146.0.7680.177.4", false) // half-extracted

	b, d := findCloakBrowserInTag(root, "chromium-v146.0.7680.177.5")
	if b == "" || d == "" {
		t.Fatal("complete install not found")
	}
	if !strings.Contains(b, "177.5") {
		t.Errorf("resolved outside the requested tag: %s", b)
	}
	// A tag missing chromedriver is not a usable install.
	if b, d := findCloakBrowserInTag(root, "chromium-v146.0.7680.177.4"); b != "" || d != "" {
		t.Error("incomplete install reported as usable")
	}
	if b, _ := findCloakBrowserInTag(root, "chromium-v999"); b != "" {
		t.Error("absent tag reported as usable")
	}
}

// The old lookup walked the cache root and took whatever filepath.Walk hit
// first, so an older tag sitting beside a newer one won forever. Ordering is
// by mtime, which also survives dotted versions that do not sort lexically.
func TestNewestLocalCloakBrowser(t *testing.T) {
	root := t.TempDir()
	installTag(t, root, "chromium-v146.0.7680.177.10", true)
	installTag(t, root, "chromium-v146.0.7680.177.5", true)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "chromium-v146.0.7680.177.5"), old, old); err != nil {
		t.Fatal(err)
	}

	b, d, tag := newestLocalCloakBrowser(root)
	if tag != "chromium-v146.0.7680.177.10" {
		t.Errorf("tag = %q, want the .10 install", tag)
	}
	if b == "" || d == "" {
		t.Error("binaries not reported")
	}

	if _, _, tag := newestLocalCloakBrowser(filepath.Join(root, "nope")); tag != "" {
		t.Error("expected empty result for a missing cache dir")
	}
}

func useCacheDir(t *testing.T, dir string) {
	t.Helper()
	orig := cloakBrowserCacheDirFn
	cloakBrowserCacheDirFn = func() (string, error) { return dir, nil }
	t.Cleanup(func() { cloakBrowserCacheDirFn = orig })
}

// A cached install of the resolved tag is used as-is: no download. This is the
// steady state on every restart, so it must not re-fetch 200 MB.
func TestEnsureCloakBrowserServesCachedTagWithoutDownloading(t *testing.T) {
	const tag = "chromium-v146.0.7680.177.5"
	_, served := downloadServer(t, tag, false)
	root := t.TempDir()
	useCacheDir(t, root)
	installTag(t, root, tag, true)

	b, d, err := EnsureCloakBrowser()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b, tag) || !strings.Contains(d, tag) {
		t.Errorf("resolved outside the cached tag: %s / %s", b, d)
	}
	if *served != 0 {
		t.Errorf("re-downloaded a cached install (%d fetches)", *served)
	}
}

// The upgrade path: a cache holding only an older tag must not satisfy a newer
// resolved release. Previously any install short-circuited the lookup, so the
// first download ever made won forever.
func TestEnsureCloakBrowserUpgradesToNewerTag(t *testing.T) {
	const newTag = "chromium-v146.0.7680.177.5"
	const oldTag = "chromium-v146.0.7680.177.1"
	_, served := downloadServer(t, newTag, false)
	root := t.TempDir()
	useCacheDir(t, root)
	installTag(t, root, oldTag, true)

	b, d, err := EnsureCloakBrowser()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b, newTag) {
		t.Errorf("browser = %s, want the %s install", b, newTag)
	}
	if !strings.Contains(d, newTag) {
		t.Errorf("driver = %s, want the %s install", d, newTag)
	}
	if *served != 1 {
		t.Errorf("download count = %d, want 1", *served)
	}
	// Symlinks in the bundle survive extraction.
	if fi, err := os.Lstat(filepath.Join(root, newTag, "libEGL.so")); err != nil {
		t.Errorf("symlink missing: %v", err)
	} else if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink was flattened into a regular file")
	}
	// The superseded install is ~700 MB extracted; it must not accumulate.
	if _, err := os.Stat(filepath.Join(root, oldTag)); !os.IsNotExist(err) {
		t.Errorf("superseded install survived: %v", err)
	}
}

// A checksum that is present and does not match is fatal: no install is left
// behind, and a good cached one is kept rather than replaced by the bad build.
func TestEnsureCloakBrowserRejectsCorruptArchive(t *testing.T) {
	const newTag = "chromium-v146.0.7680.177.5"
	const oldTag = "chromium-v146.0.7680.177.1"
	downloadServer(t, newTag, true)
	root := t.TempDir()
	useCacheDir(t, root)
	installTag(t, root, oldTag, true)

	b, _, err := EnsureCloakBrowser()
	if err != nil {
		t.Fatalf("expected a fallback to the cached install, got %v", err)
	}
	if !strings.Contains(b, oldTag) {
		t.Errorf("browser = %s, want the cached %s", b, oldTag)
	}
	if _, err := os.Stat(filepath.Join(root, newTag)); !os.IsNotExist(err) {
		t.Error("a failed verification left an install behind")
	}

	// Nothing cached: the corrupt download has to surface as an error rather
	// than a half-unpacked Chromium.
	empty := t.TempDir()
	useCacheDir(t, empty)
	if _, _, err := EnsureCloakBrowser(); err == nil {
		t.Error("corrupt archive accepted")
	} else if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("error did not name the cause: %v", err)
	}
	if _, err := os.Stat(filepath.Join(empty, newTag)); !os.IsNotExist(err) {
		t.Error("a failed verification left an install behind")
	}
}

// GitHub being unreachable or rate-limiting must not cost us a working
// install — that would drop CF bypass for every gated tracker.
func TestEnsureCloakBrowserFallsBackWhenAPIDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
	}))
	defer srv.Close()
	orig := cloakBrowserReleasesAPI
	cloakBrowserReleasesAPI = srv.URL
	defer func() { cloakBrowserReleasesAPI = orig }()

	root := t.TempDir()
	useCacheDir(t, root)
	installTag(t, root, "chromium-v146.0.7680.177.5", true)

	b, d, err := EnsureCloakBrowser()
	if err != nil {
		t.Fatalf("cached install not used when API is down: %v", err)
	}
	if b == "" || d == "" {
		t.Error("empty paths returned")
	}

	// With nothing cached there is nothing to fall back to, so it must fail
	// rather than hand back empty paths.
	useCacheDir(t, t.TempDir())
	if _, _, err := EnsureCloakBrowser(); err == nil {
		t.Error("expected an error with an empty cache and a dead API")
	}
}

func TestPruneCloakBrowserInstalls(t *testing.T) {
	root := t.TempDir()
	installTag(t, root, "chromium-v146.0.7680.177.5", true)
	installTag(t, root, "chromium-v146.0.7680.177.4", true)

	pruneCloakBrowserInstalls(root, "chromium-v146.0.7680.177.5")

	if _, err := os.Stat(filepath.Join(root, "chromium-v146.0.7680.177.5")); err != nil {
		t.Errorf("kept install was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "chromium-v146.0.7680.177.4")); !os.IsNotExist(err) {
		t.Errorf("superseded install survived: %v", err)
	}
}
