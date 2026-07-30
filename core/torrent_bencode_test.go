package core

import (
	"strings"
	"testing"
)

// bencodeTorrent builds a minimal but valid .torrent whose announce URLs carry
// an account-scoped key, the way mazepa stamps `uk=<passkey>` into every
// attachment it serves to a logged-in user.
func bencodeTorrent(announce, name string) []byte {
	info := "d6:lengthi1024e4:name" + itoa(len(name)) + ":" + name + "12:piece lengthi16384ee"
	return []byte("d8:announce" + itoa(len(announce)) + ":" + announce +
		"13:announce-listll" + itoa(len(announce)) + ":" + announce + "ee" +
		"4:info" + info + "e")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// A .torrent that could only be downloaded while logged in must never
// contribute its announce URLs to a magnet: magnets are republished through
// the search API, torznab and /sync, so a personal passkey in there leaks to
// every consumer of the instance.
func TestMagnetWithoutTrackersDropsThePasskey(t *testing.T) {
	const passkey = "VTO8207UF2e"
	data := bencodeTorrent("https://mazepa.to/bt/announce.php?uk="+passkey, "Some Release 1080p")

	safe, err := TorrentBytesToMagnetNoTrackersErr(data)
	if err != nil {
		t.Fatalf("no-trackers magnet: %v", err)
	}
	if strings.Contains(safe, passkey) {
		t.Errorf("passkey leaked into magnet: %s", safe)
	}
	if strings.Contains(safe, "tr=") {
		t.Errorf("magnet still carries trackers: %s", safe)
	}
	if !strings.HasPrefix(safe, "magnet:?xt=urn:btih:") {
		t.Errorf("not a magnet: %s", safe)
	}

	// The info hash must be unaffected — dropping trackers is not allowed to
	// change which torrent the magnet points at.
	full, err := TorrentBytesToMagnetErr(data)
	if err != nil {
		t.Fatalf("full magnet: %v", err)
	}
	if !strings.Contains(full, passkey) {
		t.Fatal("control failed: the tracker-ful magnet should contain the announce, otherwise this test proves nothing")
	}
	if btih(full) != btih(safe) {
		t.Errorf("info hash changed: %q vs %q", btih(full), btih(safe))
	}
}

func btih(magnet string) string {
	const p = "xt=urn:btih:"
	i := strings.Index(magnet, p)
	if i < 0 {
		return ""
	}
	rest := magnet[i+len(p):]
	if j := strings.IndexByte(rest, '&'); j >= 0 {
		return rest[:j]
	}
	return rest
}
