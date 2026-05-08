package tracks

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ulikunitz/xz"
)

// evermeetSigningKey is the GPG public key used by evermeet.cx to sign their
// ffmpeg/ffprobe builds. Embedding it here pins the trust root: if upstream
// is compromised, both the archive and the .sig can be swapped — but not the
// signing key, which is verified against this baked-in copy.
//
// Key id 0x476C4B611A660874, fingerprint
//
//	20F6 EA3E 0CFD 6B4C 5344  7A73 476C 4B61 1A66 0874
//
//go:embed evermeet_signing_key.asc
var evermeetSigningKey []byte

// evermeetExpectedFingerprint is the SHA-1 fingerprint of the primary key
// above. Re-checked at runtime as defense-in-depth in case someone swaps the
// .asc file in the repo without noticing.
const evermeetExpectedFingerprint = "20F6EA3E0CFD6B4C53447A73476C4B611A660874"

// ffprobeArtifact describes a downloadable ffprobe build for one platform.
//
// Integrity is verified via one of three mechanisms (in order of strength):
//
//   - PGPSigURL + embedded public key: detached GPG signature next to the
//     archive (evermeet.cx). The trust root is the public key baked into
//     this binary, so upstream compromise can't bypass it. Survives version
//     rotations indefinitely without code changes.
//
//   - SidecarURL/SidecarKind: a side-channel checksum file published next to
//     the archive (johnvansickle's `.md5`, gyan.dev's `.sha256`). Protects
//     against corrupted CDN downloads but not against a compromised host
//     (attacker can rewrite both the archive and the sidecar). Survives
//     upstream version rotations without code changes.
//
//   - PinnedSHA256: hex SHA-256 hardcoded in this file. Strongest pin but
//     requires a code bump on every upstream version rotation. Currently
//     unused; kept as a fallback for any future source without a sidecar
//     or a published signing key.
//
// Exactly one of these must be non-empty per artifact.
type ffprobeArtifact struct {
	URL     string // download URL of the archive
	Format  string // "tar.xz" or "zip"
	BinName string // basename of the ffprobe entry to extract (e.g. "ffprobe" or "ffprobe.exe")

	PGPSigURL    string // optional detached signature URL (verified against evermeetSigningKey)
	SidecarURL   string // optional sidecar checksum URL (e.g. URL+".md5" or URL+".sha256")
	SidecarKind  string // "md5" or "sha256" — required when SidecarURL is set
	PinnedSHA256 string // fallback hex SHA-256 when no sidecar/sig is published

	Note string // optional extra info logged on download (e.g. Rosetta caveat)
}

// Pinned ffprobe artifacts.
//
// Linux: johnvansickle.com static builds (de-facto standard for headless Linux).
//        Each archive has a `.md5` sidecar that rotates with the build.
// Windows: gyan.dev release-essentials zip; sibling `.sha256` sidecar.
// macOS: evermeet.cx ffprobe-only zip — verified against an embedded GPG
//        public key (evermeet_signing_key.asc). Apple Silicon falls back to
//        the same Intel binary running under Rosetta 2 — evermeet doesn't
//        ship native arm64 macOS builds.
//
// Note on macOS URL: evermeet's filename embeds the version, so the literal
// URL below rotates as new ffmpeg releases come out and existing one returns
// 404. To keep the build reachable without code bumps we resolve the latest
// release URL at runtime via the JSON info API; see resolveEvermeetLatest.
var ffprobeArtifacts = map[string]ffprobeArtifact{
	"linux/amd64": {
		URL:         "https://johnvansickle.com/ffmpeg/releases/ffmpeg-release-amd64-static.tar.xz",
		Format:      "tar.xz",
		BinName:     "ffprobe",
		SidecarURL:  "https://johnvansickle.com/ffmpeg/releases/ffmpeg-release-amd64-static.tar.xz.md5",
		SidecarKind: "md5",
	},
	"linux/arm64": {
		URL:         "https://johnvansickle.com/ffmpeg/releases/ffmpeg-release-arm64-static.tar.xz",
		Format:      "tar.xz",
		BinName:     "ffprobe",
		SidecarURL:  "https://johnvansickle.com/ffmpeg/releases/ffmpeg-release-arm64-static.tar.xz.md5",
		SidecarKind: "md5",
	},
	"linux/arm": {
		URL:         "https://johnvansickle.com/ffmpeg/releases/ffmpeg-release-armhf-static.tar.xz",
		Format:      "tar.xz",
		BinName:     "ffprobe",
		SidecarURL:  "https://johnvansickle.com/ffmpeg/releases/ffmpeg-release-armhf-static.tar.xz.md5",
		SidecarKind: "md5",
	},
	"linux/386": {
		URL:         "https://johnvansickle.com/ffmpeg/releases/ffmpeg-release-i686-static.tar.xz",
		Format:      "tar.xz",
		BinName:     "ffprobe",
		SidecarURL:  "https://johnvansickle.com/ffmpeg/releases/ffmpeg-release-i686-static.tar.xz.md5",
		SidecarKind: "md5",
	},
	"windows/amd64": {
		URL:         "https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip",
		Format:      "zip",
		BinName:     "ffprobe.exe",
		SidecarURL:  "https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip.sha256",
		SidecarKind: "sha256",
	},
	// darwin/* entries are filled in at startup by resolveEvermeetLatest:
	// URL, Format, BinName, and PGPSigURL are all version-derived. We keep
	// placeholder shapes here so the lookup map matches GOOS/GOARCH; the
	// actual fields are populated lazily.
	"darwin/amd64": {
		Format:  "zip",
		BinName: "ffprobe",
	},
	"darwin/arm64": {
		Format:  "zip",
		BinName: "ffprobe",
		Note:    "Intel x64 binary — runs under Rosetta 2 on Apple Silicon. Install Rosetta if missing: softwareupdate --install-rosetta",
	},
}

// EnsureFFprobe makes a usable ffprobe binary available and returns its
// absolute path. Resolution order:
//
//  1. exec.LookPath("ffprobe") — system PATH wins.
//  2. <dataDir>/bin/ffprobe[.exe] — copy downloaded by a previous run.
//  3. Download a verified archive for runtime.GOOS/GOARCH and extract
//     ffprobe into <dataDir>/bin.
//
// Returns "" + error if no path resolves and the platform isn't covered by
// the auto-download table — in that case the caller should log the error
// and continue without tracks.
func EnsureFFprobe(dataDir string) (string, error) {
	if p, err := exec.LookPath("ffprobe"); err == nil {
		return p, nil
	}
	binDir := filepath.Join(dataDir, "bin")
	binName := "ffprobe"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	local := filepath.Join(binDir, binName)
	if st, err := os.Stat(local); err == nil && !st.IsDir() {
		return local, nil
	}
	key := runtime.GOOS + "/" + runtime.GOARCH
	art, ok := ffprobeArtifacts[key]
	if !ok {
		return "", fmt.Errorf("ffprobe not found and auto-download is not supported on %s — please install ffmpeg/ffprobe manually (apt/brew/winget) and ensure ffprobe is in PATH", key)
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", binDir, err)
	}
	// evermeet.cx URLs include the version, so resolve the current release
	// before logging/downloading.
	if runtime.GOOS == "darwin" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		resolved, err := resolveEvermeetLatest(ctx)
		cancel()
		if err != nil {
			return "", fmt.Errorf("resolve evermeet latest: %w", err)
		}
		art.URL = resolved.URL
		art.PGPSigURL = resolved.SigURL
	}
	log.Printf("tracks: ffprobe not found, downloading static build for %s from %s", key, art.URL)
	if art.Note != "" {
		log.Printf("tracks: note for %s: %s", key, art.Note)
	}
	if err := downloadFFprobe(art, local); err != nil {
		_ = os.Remove(local)
		return "", fmt.Errorf("download ffprobe: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(local, 0o755); err != nil {
			return "", fmt.Errorf("chmod ffprobe: %w", err)
		}
	}
	log.Printf("tracks: ffprobe ready at %s", local)
	return local, nil
}

func downloadFFprobe(art ffprobeArtifact, dest string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var (
		buf []byte
		err error
	)

	switch {
	case art.PGPSigURL != "":
		// Fetch the archive without on-the-fly hashing — we need the bytes
		// for OpenPGP detached-signature verification.
		buf, err = fetchURL(ctx, art.URL)
		if err != nil {
			return err
		}
		sig, err := fetchURL(ctx, art.PGPSigURL)
		if err != nil {
			return fmt.Errorf("fetch signature from %s: %w", art.PGPSigURL, err)
		}
		if err := verifyPGPSignature(buf, sig); err != nil {
			return fmt.Errorf("pgp verification failed: %w", err)
		}
	case art.SidecarURL != "":
		kind := strings.ToLower(strings.TrimSpace(art.SidecarKind))
		if kind != "md5" && kind != "sha256" {
			return fmt.Errorf("invalid SidecarKind %q for %s", art.SidecarKind, art.URL)
		}
		expected, err := fetchSidecarHex(ctx, art.SidecarURL)
		if err != nil {
			return fmt.Errorf("fetch checksum from %s: %w", art.SidecarURL, err)
		}
		var actual string
		buf, actual, err = fetchAndHash(ctx, art.URL, kind)
		if err != nil {
			return err
		}
		if !strings.EqualFold(actual, expected) {
			return fmt.Errorf("%s mismatch (got %s, expected %s) — archive may be corrupted, retry; if it persists please file an issue", kind, actual, expected)
		}
	case art.PinnedSHA256 != "":
		var actual string
		buf, actual, err = fetchAndHash(ctx, art.URL, "sha256")
		if err != nil {
			return err
		}
		if !strings.EqualFold(actual, art.PinnedSHA256) {
			return fmt.Errorf("sha256 mismatch (got %s, expected %s) — please update jacred or install ffprobe manually", actual, art.PinnedSHA256)
		}
	default:
		return fmt.Errorf("no integrity verification configured for %s", art.URL)
	}

	switch art.Format {
	case "tar.xz":
		return extractFromTarXZ(buf, art.BinName, dest)
	case "zip":
		return extractFromZip(buf, art.BinName, dest)
	default:
		return fmt.Errorf("unknown archive format %q", art.Format)
	}
}

// evermeetReleaseInfo is the subset of evermeet.cx's JSON we need.
type evermeetReleaseInfo struct {
	URL    string
	SigURL string
}

// resolveEvermeetLatest queries evermeet.cx's JSON info API to find the
// current release zip URL plus its detached PGP signature. Used because
// the literal versioned URL changes with each upstream release.
func resolveEvermeetLatest(ctx context.Context) (evermeetReleaseInfo, error) {
	const infoURL = "https://evermeet.cx/ffmpeg/info/ffprobe/release"
	req, err := http.NewRequestWithContext(ctx, "GET", infoURL, nil)
	if err != nil {
		return evermeetReleaseInfo{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return evermeetReleaseInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return evermeetReleaseInfo{}, fmt.Errorf("status %d from %s", resp.StatusCode, infoURL)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return evermeetReleaseInfo{}, err
	}
	// Minimal hand-decode to avoid dragging encoding/json dependency choices
	// into a tiny lookup. JSON shape (verified): {"download":{"zip":{"url":"...","sig":"..."}}}.
	zipURL := extractJSONField(body, "url")
	sigURL := extractJSONField(body, "sig")
	if zipURL == "" || sigURL == "" {
		return evermeetReleaseInfo{}, fmt.Errorf("could not parse evermeet info JSON")
	}
	return evermeetReleaseInfo{URL: zipURL, SigURL: sigURL}, nil
}

// extractJSONField scans for the first occurrence of "<field>":"<value>" in
// the byte slice. Sufficient for evermeet's flat info object — full JSON
// parsing would add weight without a real benefit here.
func extractJSONField(body []byte, field string) string {
	needle := []byte(`"` + field + `":"`)
	idx := bytes.Index(body, needle)
	if idx < 0 {
		return ""
	}
	rest := body[idx+len(needle):]
	end := bytes.IndexByte(rest, '"')
	if end < 0 {
		return ""
	}
	return string(rest[:end])
}

// verifyPGPSignature validates a detached binary signature against the
// embedded evermeet signing key. Returns nil on success.
func verifyPGPSignature(archive, sig []byte) error {
	keyring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(evermeetSigningKey))
	if err != nil {
		return fmt.Errorf("read embedded key: %w", err)
	}
	if len(keyring) != 1 {
		return fmt.Errorf("embedded key file should hold exactly one entity, got %d", len(keyring))
	}
	got := strings.ToUpper(hex.EncodeToString(keyring[0].PrimaryKey.Fingerprint))
	if got != evermeetExpectedFingerprint {
		return fmt.Errorf("embedded key fingerprint mismatch (got %s, expected %s) — repository file may be tampered", got, evermeetExpectedFingerprint)
	}
	if _, err := openpgp.CheckDetachedSignature(keyring, bytes.NewReader(archive), bytes.NewReader(sig), nil); err != nil {
		return err
	}
	return nil
}

// fetchURL downloads the URL into memory.
func fetchURL(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d from %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
}

// fetchSidecarHex downloads a checksum sidecar and pulls the first hex digest
// out of it. Handles both `<hex>  <filename>` (johnvansickle, GNU md5sum
// style) and `<hex>` (gyan.dev, plain hex) layouts.
func fetchSidecarHex(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	first := strings.TrimSpace(strings.SplitN(string(body), "\n", 2)[0])
	tok := strings.Fields(first)
	if len(tok) == 0 {
		return "", fmt.Errorf("empty checksum file")
	}
	hex := strings.ToLower(tok[0])
	for _, r := range hex {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return "", fmt.Errorf("first token is not hex: %q", hex)
		}
	}
	return hex, nil
}

// fetchAndHash downloads the URL into memory and returns its hex digest of
// the requested kind in one pass via TeeReader.
func fetchAndHash(ctx context.Context, url, kind string) ([]byte, string, error) {
	var h hash.Hash
	switch kind {
	case "md5":
		h = md5.New()
	case "sha256":
		h = sha256.New()
	default:
		return nil, "", fmt.Errorf("unsupported hash kind %q", kind)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("status %d from %s", resp.StatusCode, url)
	}
	buf, err := io.ReadAll(io.TeeReader(resp.Body, h))
	if err != nil {
		return nil, "", fmt.Errorf("read body: %w", err)
	}
	return buf, hex.EncodeToString(h.Sum(nil)), nil
}

// extractFromTarXZ scans the tarball for an entry whose basename matches
// binName (e.g. "ffprobe") and copies it to dest. Looking up by basename
// makes us tolerant of upstream version-suffixed top directories
// (e.g. `ffmpeg-7.0.2-amd64-static/ffprobe` → `ffmpeg-7.1-amd64-static/ffprobe`).
func extractFromTarXZ(archive []byte, binName, dest string) error {
	xr, err := xz.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("xz: %w", err)
	}
	tr := tar.NewReader(xr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("file %q not found in archive", binName)
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if path.Base(hdr.Name) != binName {
			continue
		}
		return writeStreamTo(dest, tr)
	}
}

// extractFromZip is the zip equivalent of extractFromTarXZ — basename-based
// lookup, version-tolerant.
func extractFromZip(archive []byte, binName, dest string) error {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return fmt.Errorf("zip: %w", err)
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if path.Base(f.Name) != binName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		return writeStreamTo(dest, rc)
	}
	return fmt.Errorf("file %q not found in archive", binName)
}

func writeStreamTo(dest string, src io.Reader) error {
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, src); err != nil {
		return err
	}
	return nil
}
