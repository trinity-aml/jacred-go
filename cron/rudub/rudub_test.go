package rudub

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"jacred/core"
)

// listing_vf4.html is a trimmed anonymous capture of browse.php (2026-09-25).
// Nothing needed scrubbing — the listing carries no passkey, no session cookie
// and no account name; the passkey lives only inside the .torrent files, which
// is exactly why magnets here are built without announces.
func load(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

const host = "https://rudub.world"

func TestParseCards(t *testing.T) {
	cards := parseCards(load(t, "listing_vf4.html"), host)
	if len(cards) == 0 {
		t.Fatal("no cards parsed")
	}
	for _, c := range cards {
		if !strings.HasPrefix(c.url, host+"/details.php?id=") {
			t.Errorf("url = %q", c.url)
		}
		if c.downloadID == "" {
			t.Errorf("%s: no download id — the magnet could never be built", c.title)
		}
		if c.name == "" {
			t.Errorf("%s: empty name — the bucket key would be empty", c.title)
		}
		if c.sizeName == "" {
			t.Errorf("%s: no size", c.title)
		}
		if c.createTime.IsZero() {
			t.Errorf("%s: no date", c.title)
		}
		if len(c.types) == 0 {
			t.Errorf("%s: no types — mergeNew drops a record with none", c.title)
		}
	}
}

// The listing cell holds both counters; reading only one would report every
// release as having no peers.
func TestActivityCounters(t *testing.T) {
	cards := parseCards(load(t, "listing_vf4.html"), host)
	var withPeers int
	for _, c := range cards {
		if c.pir > 0 {
			withPeers++
		}
	}
	if withPeers == 0 {
		t.Error("no card carried a leecher count; the Активность regex is not matching")
	}
}

func TestTitleNormalisation(t *testing.T) {
	got := normalizeTitle("Вне закона: Ярость тигров (The Outlaws: Tigers Wrath)<br>Сезон 1 Серии 01-04 (HD1080p WEBRip) (Обновляемая) (Золото)")
	want := "Вне закона: Ярость тигров (The Outlaws: Tigers Wrath) Сезон 1 Серии 01-04 (HD1080p WEBRip)"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestParseTitleFields(t *testing.T) {
	for _, c := range []struct {
		title    string
		name     string
		original string
		relased  int
	}{
		{
			"Вне закона: Ярость тигров (The Outlaws: Tigers Wrath) Сезон 1 Серии 01-04 (HD1080p WEBRip)",
			"Вне закона: Ярость тигров", "The Outlaws: Tigers Wrath", 0,
		},
		{
			// A bare year group is a year, not an original title.
			"Уборщица (2022) (The Cleaning Lady) Сезон 4 Серии 01-12 (HD1080p WEBRip)",
			"Уборщица", "The Cleaning Lady", 2022,
		},
		{
			// Nested parens must stay balanced.
			"Контора (The Office (US)) Сезон 1 (HD1080p)",
			"Контора", "The Office (US)", 0,
		},
		{
			"Без оригинала Сезон 1 HD1080p",
			"Без оригинала Сезон 1 HD1080p", "", 0,
		},
	} {
		name, original, relased := parseTitleFields(c.title)
		if name != c.name || original != c.original || relased != c.relased {
			t.Errorf("%q\n got  name=%q original=%q relased=%d\n want name=%q original=%q relased=%d",
				c.title, name, original, relased, c.name, c.original, c.relased)
		}
	}
}

// Upstream falls back to the card's upload year. Measured over two live pages,
// only 2 of 60 titles carry a year at all, and the one that did said 2022 on a
// card uploaded in 2025 — so the fallback would stamp a wrong year on ~97% of
// records, and Jackett matches years within ±1.
func TestYearIsNotInventedFromTheUploadDate(t *testing.T) {
	cards := parseCards(load(t, "listing_vf4.html"), host)
	for _, c := range cards {
		if c.relased == 0 {
			continue
		}
		if c.createTime.Year() == c.relased && !strings.Contains(c.title, "("+strconv.Itoa(c.relased)+")") {
			t.Errorf("%q: relased=%d looks copied from the upload date", c.title, c.relased)
		}
	}
}

func TestQualityGate(t *testing.T) {
	for _, c := range []struct {
		title string
		want  bool
	}{
		{"Фильм (Movie) (HD1080p WEBRip)", true},
		{"Фильм (Movie) (HD2160p)", true},
		{"Фильм (Movie) (HD720p WEBRip)", false},
		{"Фильм (Movie) (WEBRip XviD)", false},
		{"Фильм (Movie) (WEBRip x264)", false},
		{"Фильм (Movie) (720p)", false},
		// A combined release naming both is kept.
		{"Фильм (Movie) (HD1080p / HD720p)", true},
		{"", false},
	} {
		if got := isPreferredQuality(c.title); got != c.want {
			t.Errorf("isPreferredQuality(%q) = %v, want %v", c.title, got, c.want)
		}
	}
}

func TestDetectTypes(t *testing.T) {
	for _, c := range []struct {
		title string
		want  string
	}{
		{"Шоу (Show) Сезон 1 Серии 01-04 (HD1080p)", "serial"},
		{"Фильм (Movie) (HD1080p)", "movie"},
		{"Что-то / s01e05 (HD1080p)", "serial"},
	} {
		got := detectTypes(c.title)
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("detectTypes(%q) = %v, want [%s]", c.title, got, c.want)
		}
	}
}

// A refusal arrives as HTTP 200 with an HTML body, so the payload is what
// decides. Keying off the status alone would store an error page as a torrent.
func TestTorrentPayloadDetection(t *testing.T) {
	if !looksLikeTorrent([]byte("d8:announce76:https://tr.example/announce")) {
		t.Error("a bencoded dictionary was not recognised")
	}
	for _, body := range [][]byte{
		[]byte("<!DOCTYPE html><html><body>login</body></html>"),
		[]byte("<html>"),
		{},
	} {
		if looksLikeTorrent(body) {
			t.Errorf("%.20q was taken for a torrent", body)
		}
	}
	if !looksLikeHTML([]byte("<!DOCTYPE html><html>")) {
		t.Error("HTML not recognised")
	}
}

// A body that is not a listing must not be parsed into zero rows and reported
// as a quiet day.
func TestValidationMarkerRejectsNonListings(t *testing.T) {
	if !strings.Contains(load(t, "listing_vf4.html"), validationMarker) {
		t.Fatal("the captured listing lacks the marker; the guard would stop every run")
	}
	for _, body := range []string{"", "<html><body>Вход</body></html>", "<!DOCTYPE html>"} {
		if strings.Contains(body, validationMarker) {
			t.Errorf("a non-listing satisfies the marker: %.30q", body)
		}
	}
}

func TestCP1251Decoding(t *testing.T) {
	// "Дата" in cp1251.
	cp := []byte{0xC4, 0xE0, 0xF2, 0xE0}
	if got := decodeBody(cp); got != "Дата" {
		t.Errorf("cp1251 decode = %q, want %q", got, "Дата")
	}
	// An already-valid UTF-8 body must pass through untouched.
	if got := decodeBody([]byte("Размер")); got != "Размер" {
		t.Errorf("utf-8 passthrough = %q", got)
	}
}

// Every rudub .torrent embeds a passkey in its announce — measured on
// anonymous downloads, where two different torrents carried the same key. So
// unlike anibelka, staying logged out does not avoid it; only dropping the
// announces does. The synthetic torrent below carries a fake passkey rather
// than a captured one, because a real key in a committed fixture is exactly
// what this guards against.
func TestMagnetNeverCarriesTheAnnouncePasskey(t *testing.T) {
	const passkey = "deadbeefdeadbeefdeadbeefdeadbeef"
	announce := "https://tr.rudub.space/announce.php?passkey=" + passkey
	torrent := buildBencodedTorrent(announce, "Some.Release.1080p", 1048576)

	if !looksLikeTorrent(torrent) {
		t.Fatal("the synthetic torrent is not recognised as bencode")
	}
	magnet, err := core.TorrentBytesToMagnetNoTrackersErr(torrent)
	if err != nil {
		t.Fatalf("magnet: %v", err)
	}
	if magnet == "" {
		t.Fatal("empty magnet")
	}
	for _, forbidden := range []string{passkey, "passkey", "announce", "tr=", "tr.rudub.space"} {
		if strings.Contains(magnet, forbidden) {
			t.Errorf("magnet leaked %q: %s", forbidden, magnet)
		}
	}
	if !strings.HasPrefix(magnet, "magnet:?xt=urn:btih:") {
		t.Errorf("magnet = %q", magnet)
	}
}

// buildBencodedTorrent writes a minimal single-file torrent.
func buildBencodedTorrent(announce, name string, length int) []byte {
	var b strings.Builder
	b.WriteString("d8:announce")
	fmt.Fprintf(&b, "%d:%s", len(announce), announce)
	b.WriteString("4:infod6:lengthi")
	fmt.Fprintf(&b, "%d", length)
	b.WriteString("e4:name")
	fmt.Fprintf(&b, "%d:%s", len(name), name)
	b.WriteString("12:piece lengthi262144e6:pieces20:")
	b.WriteString(strings.Repeat("\x01", 20))
	b.WriteString("ee")
	return []byte(b.String())
}

// The test above proves the no-trackers builder is safe; this one proves the
// parser actually calls it. Without this, swapping in the ordinary builder
// passes every other test in the package while republishing the tracker's
// passkey through the search API, torznab and /sync.
func TestParserUsesTheNoTrackersMagnetBuilder(t *testing.T) {
	src, err := os.ReadFile("rudub.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	code := string(src)
	if !strings.Contains(code, "core.TorrentBytesToMagnetNoTrackersErr(") {
		t.Error("the no-trackers magnet builder is gone — every magnet would carry the announce passkey")
	}
	for _, forbidden := range []string{
		"core.TorrentBytesToMagnet(",
		"core.TorrentBytesToMagnetErr(",
	} {
		if strings.Contains(code, forbidden) {
			t.Errorf("%s builds a magnet with announces; rudub stamps a passkey into every one", forbidden)
		}
	}
}
