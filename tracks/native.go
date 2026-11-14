package tracks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// NativeAnalyzer downloads only the head/tail of the largest video file in a
// torrent into RAM via anacrolix/torrent and feeds it to a local ffprobe over
// a 127.0.0.1 HTTP bridge. No bytes touch disk.
type NativeAnalyzer struct {
	cl          *torrent.Client
	storage     *memStorage
	infoWait    time.Duration
	ffprobePath string // absolute path to ffprobe; empty falls back to PATH lookup
	solveGate   chan struct{}
}

// NewNativeAnalyzer creates a single shared torrent.Client backed by an
// in-memory piece store. ffprobePath is the absolute path to the ffprobe
// binary; passing "" makes the analyzer rely on $PATH at exec time.
// Returns an error if the client can't bind sockets.
func NewNativeAnalyzer(ffprobePath string) (*NativeAnalyzer, error) {
	mem := newMemStorage()
	cfg := torrent.NewDefaultClientConfig()
	cfg.DefaultStorage = mem
	cfg.DataDir = ""
	cfg.NoUpload = true
	cfg.Seed = false
	cfg.NoDefaultPortForwarding = true
	cfg.ListenPort = 0
	cfg.Slogger = slog.New(discardHandler{})
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return &NativeAnalyzer{cl: cl, storage: mem, infoWait: 2 * time.Minute, ffprobePath: ffprobePath, solveGate: make(chan struct{}, 1)}, nil
}

func (n *NativeAnalyzer) Close() {
	if n == nil || n.cl == nil {
		return
	}
	_ = n.cl.Close()
}

var videoExtensions = map[string]struct{}{
	".mkv":  {},
	".mp4":  {},
	".m4v":  {},
	".avi":  {},
	".mov":  {},
	".ts":   {},
	".webm": {},
	".flv":  {},
	".wmv":  {},
}

func isVideoFile(name string) bool {
	_, ok := videoExtensions[strings.ToLower(filepath.Ext(name))]
	return ok
}

// Analyze adds the magnet, waits for metadata, finds the largest video file,
// serves it via a local HTTP bridge with Range support, and runs ffprobe.
// Pieces are downloaded on demand by anacrolix as ffprobe seeks; the whole
// torrent is dropped from RAM on return.
func (n *NativeAnalyzer) Analyze(ctx context.Context, magnet string) (*FFProbeModel, error) {
	if n == nil || n.cl == nil {
		return nil, errors.New("native analyzer is not initialised")
	}
	// The five task loops share one client and one bounded in-memory probe.
	select {
	case n.solveGate <- struct{}{}:
		defer func() { <-n.solveGate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	spec, err := torrent.TorrentSpecFromMagnetUri(strings.TrimSpace(magnet))
	if err != nil {
		return nil, fmt.Errorf("parse magnet: %w", err)
	}
	// A malformed tr= in somebody else's magnet must not be able to kill this
	// process, and until this filter existed it could.
	if kept, dropped := sanitizeTrackers(spec.Trackers); len(dropped) > 0 {
		spec.Trackers = kept
		// Scheme and host only, never the full URL: an announce path can carry
		// the account's passkey, and this log is kept. Same rule as cookies.
		log.Printf("tracks: dropped %d unusable announce url(s) from the magnet (%s) — they would crash the torrent client",
			len(dropped), strings.Join(dropped, ", "))
	}
	t, _, err := n.cl.AddTorrentSpec(spec)
	if err != nil {
		return nil, fmt.Errorf("add torrent: %w", err)
	}
	defer dropProbeTorrent(spec.InfoHash.HexString(), t.Drop)

	infoCtx, cancelInfo := context.WithTimeout(ctx, n.infoWait)
	select {
	case <-t.GotInfo():
	case <-infoCtx.Done():
		cancelInfo()
		return nil, fmt.Errorf("metadata timeout: %w", infoCtx.Err())
	}
	cancelInfo()

	var biggest *torrent.File
	var biggestSize int64
	for _, f := range t.Files() {
		if !isVideoFile(f.DisplayPath()) {
			continue
		}
		if f.Length() > biggestSize {
			biggestSize = f.Length()
			biggest = f
		}
	}
	if biggest == nil {
		return nil, errors.New("no video file in torrent")
	}

	// Reader demand supplies priorities for just the ranges ffprobe reads.
	// Normal priority on the video would download the entire movie into RAM.
	for _, f := range t.Files() {
		f.SetPriority(torrent.PiecePriorityNone)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bridge listen: %w", err)
	}
	addr := listener.Addr().String()

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rd := biggest.NewReader()
			rd.SetResponsive()
			rd.SetReadahead(4 << 20)
			rd.SetContext(r.Context())
			defer rd.Close()
			http.ServeContent(w, r, biggest.DisplayPath(), time.Time{}, rd)
		}),
	}
	go func() { _ = srv.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = srv.Shutdown(shutdownCtx)
		cancel()
	}()

	streamURL := fmt.Sprintf("http://%s/stream", addr)
	probeCtx, cancelProbe := context.WithTimeout(ctx, 5*time.Minute)
	defer cancelProbe()

	bin := n.ffprobePath
	if bin == "" {
		bin = "ffprobe"
	}
	cmd := exec.CommandContext(probeCtx, bin,
		"-v", "error",
		"-probesize", "33554432", "-analyzeduration", "10000000",
		"-print_format", "json",
		"-show_streams",
		"-i", streamURL,
	)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("ffprobe: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("ffprobe: %w", err)
	}

	var model FFProbeModel
	if err := json.Unmarshal(out, &model); err != nil {
		return nil, fmt.Errorf("parse ffprobe output: %w", err)
	}
	if len(model.Streams) == 0 {
		return nil, errors.New("ffprobe returned no streams")
	}
	return &model, nil
}

// memStorage is a piece-level in-memory implementation of
// storage.ClientImplCloser. Piece buffers are allocated lazily on first
// WriteAt so torrents we never finish only hold pieces we actually fetched.
type memStorage struct {
	mu       sync.Mutex
	torrents map[metainfo.Hash]*memTorrent
}

func newMemStorage() *memStorage {
	return &memStorage{torrents: map[metainfo.Hash]*memTorrent{}}
}

func (s *memStorage) OpenTorrent(_ context.Context, info *metainfo.Info, infoHash metainfo.Hash) (storage.TorrentImpl, error) {
	num := info.NumPieces()
	budget := &pieceBudget{limit: 128 << 20}
	pieces := make([]*memPiece, num)
	for i := 0; i < num; i++ {
		pieces[i] = &memPiece{size: info.Piece(i).V1Length(), budget: budget}
	}
	mt := &memTorrent{pieces: pieces}
	s.mu.Lock()
	s.torrents[infoHash] = mt
	s.mu.Unlock()
	return storage.TorrentImpl{
		Piece: func(p metainfo.Piece) storage.PieceImpl {
			return mt.pieces[p.Index()]
		},
		Close: func() error {
			s.mu.Lock()
			delete(s.torrents, infoHash)
			s.mu.Unlock()
			for _, pc := range mt.pieces {
				pc.mu.Lock()
				pc.closed = true
				pc.data = nil
				pc.mu.Unlock()
			}
			return nil
		},
	}, nil
}

func (s *memStorage) Close() error { return nil }

type memTorrent struct {
	pieces []*memPiece
}

// A probe must never allocate the size of the video, even if it seeks widely.
type pieceBudget struct {
	mu          sync.Mutex
	used, limit int64
}

func (b *pieceBudget) reserve(size int64) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if size > b.limit-b.used {
		return false
	}
	b.used += size
	return true
}

type memPiece struct {
	mu       sync.Mutex
	size     int64
	data     []byte
	complete bool
	closed   bool
	budget   *pieceBudget
}

func (p *memPiece) ReadAt(b []byte, off int64) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.data == nil || off >= p.size {
		return 0, io.EOF
	}
	n := copy(b, p.data[off:])
	if int64(n) < int64(len(b)) && off+int64(n) >= p.size {
		return n, io.EOF
	}
	return n, nil
}

func (p *memPiece) WriteAt(b []byte, off int64) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return len(b), nil
	}
	if off < 0 || off > p.size || int64(len(b)) > p.size-off {
		return 0, io.ErrShortWrite
	}
	if p.data == nil {
		if !p.budget.reserve(p.size) {
			return 0, errors.New("probe piece memory budget exceeded")
		}
		p.data = make([]byte, p.size)
	}
	if off+int64(len(b)) > p.size {
		return 0, io.ErrShortWrite
	}
	return copy(p.data[off:], b), nil
}

func (p *memPiece) MarkComplete() error {
	p.mu.Lock()
	p.complete = true
	p.mu.Unlock()
	return nil
}

func (p *memPiece) MarkNotComplete() error {
	p.mu.Lock()
	p.complete = false
	p.mu.Unlock()
	return nil
}

func (p *memPiece) Completion() storage.Completion {
	p.mu.Lock()
	defer p.mu.Unlock()
	return storage.Completion{Complete: p.complete, Ok: true}
}

// discardHandler swallows anacrolix's slog output. Errors that matter to us
// surface via Analyze's return values; the rest is internal client chatter.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }

// sanitizeTrackers removes announce URLs that anacrolix/torrent cannot accept,
// and returns a redacted description of each one it dropped.
//
// This is not defensive polish. `AddTorrentSpec` hands every tracker to
// `Client.startTrackerAnnouncer`, which asserts
// `panicif.NotEq(u.String(), string(urlStr))` — the parsed URL must serialise
// back to exactly the string it came from — and a failed assertion is a
// **panic that takes the whole process down**. Measured on the production box:
// `panic: http: != http://` killed jacred **12 times in 14 days**, six of them
// in one day, with the stack running
// `initTrackerClient` ← `startScrapingTrackerWithInfohash` ← our Analyze.
// The offending announce was the literal string `http://`: `url.Parse` accepts
// it, and `String()` gives back `http:`, because an empty Host drops the `//`.
//
// Two conditions, and both are load-bearing:
//
//   - **It must name a host.** This is what catches the production value:
//     `http://`, `udp://` and `not a url` all parse to an empty Host.
//   - **It must round-trip** (`u.String() == raw`), which is the library's own
//     assertion reproduced here rather than approximated by a scheme
//     whitelist. Its *unique* contribution is announces that name a real host
//     and still fail to serialise back — measured:
//     `http://tracker.example/annoünce`, `…/a b` and `…/a|b` all have a
//     non-empty Host and all get escaped by `String()`, so only this check
//     rejects them. Unescaped and non-ASCII characters in an announce path are
//     not hypothetical in magnets from these trackers.
//
// Both halves were verified by removing each in turn and watching the test
// fail on exactly the inputs the other one does not cover. Checks that sound
// equivalent are not: `http:` round-trips cleanly and names nothing, while
// `…/a b` names a host and does not round-trip.
//
// Checking `url.Parse`'s error instead would catch none of them: it returns
// nil for every string above, including `not a url`. That is the trap this
// function exists to document.
//
// Dropping a tracker is safe for a probe: peers are also found over DHT, and
// three parsers here (anibelka, mazepa, rudub) already publish magnets with no
// announce at all because each `.torrent` embeds the account's passkey.
func sanitizeTrackers(tiers [][]string) (kept [][]string, dropped []string) {
	for _, tier := range tiers {
		var keptTier []string
		for _, raw := range tier {
			if trackerIsUsable(raw) {
				keptTier = append(keptTier, raw)
				continue
			}
			dropped = append(dropped, redactTracker(raw))
		}
		// An empty tier is not worth carrying forward.
		if len(keptTier) > 0 {
			kept = append(kept, keptTier)
		}
	}
	return kept, dropped
}

// trackerIsUsable reports whether anacrolix/torrent will accept this announce
// URL without asserting. See sanitizeTrackers for why these two checks.
func trackerIsUsable(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return false
	}
	if u.Host == "" {
		return false
	}
	return u.String() == raw
}

// redactTracker describes an announce URL by scheme and host only. The path and
// query are withheld deliberately: an announce can carry the account's passkey,
// and Data/log is retained. For the degenerate values this filter actually
// catches the scheme alone identifies them.
func redactTracker(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return "<unparseable>"
	}
	switch {
	case u.Scheme == "" && u.Host == "":
		return "<no scheme or host>"
	case u.Host == "":
		return u.Scheme + ":// (no host)"
	default:
		return u.Scheme + "://" + u.Host
	}
}

// dropProbeTorrent calls Drop and survives the library asserting on its own
// internal invariant.
//
// `(*Torrent).close` ends with `assertAllPiecesRelativeAvailabilityZero`, which
// **panics** — killing the process — if any piece still has a non-zero relative
// availability after every peer has been closed. Measured on the production box:
// `panic: piece 0 has relative availability 1` killed jacred **3 times in 14
// days**, with the stack running
// `assertAllPiecesRelativeAvailabilityZero` (torrent.go:1153) ← `(*Torrent).close`
// ← `(*Torrent).Drop` ← this function's caller.
//
// Relative availability is maintained by the peer connections: a HAVE or
// bitfield increments it, a peer closing decrements it, and both are no-ops
// while the info is still missing, after which it is recomputed from the peer
// set. The invariant is the library's, the accounting is the library's, and the
// violation is a race inside it between a peer's piece bookkeeping and the drop
// — there is nothing a caller can pass or avoid to prevent it. anacrolix/torrent
// v1.61.0 is the newest release, and this repo cannot move off it (see the
// `go get -u` note in CLAUDE.md: missinggo v2.11 moves to roaring/v2 and
// torrent's own peerconn.go stops compiling), so there is no upstream fix to
// take.
//
// **Recovering here is safe, and specifically why matters:**
//
//   - `Drop` takes the client lock with `defer t.cl.unlock()`, so the lock is
//     released as the panic unwinds. Nothing deadlocks.
//   - The storage close is *spawned as a goroutine before* the assertion, so
//     memStorage's Close still runs: the 128 MB of piece buffers are freed and
//     the torrent leaves the storage map either way.
//   - `t.closed.Set()` and the short-infohash index cleanup both happen at the
//     top of `close`, before the assertion — so the torrent is marked closed and
//     a later probe of the same magnet builds a fresh one rather than finding
//     this corpse.
//
// What is left behind is the entry in `Client.torrents` (`g.MustDelete` comes
// *after* the assertion) and an unclosed `pieceStateChanges` pubsub: a stale
// struct plus one piece header per piece, tens of kilobytes, three times a
// fortnight. That is the whole cost, and it buys not losing the process.
//
// The probe result is already in hand by the time this runs — Drop is deferred
// teardown — so nothing the caller wanted is lost.
//
// **What this does not cover:** `close` can also be reached from the client's
// own goroutine (`Client.Close`, internal drops). A panic there is not on this
// call stack and no recover of ours can catch it. Every occurrence observed so
// far came through this path.
func dropProbeTorrent(infoHash string, drop func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("tracks: recovered a panic while dropping the probe torrent %s: %v — "+
				"this is an assertion inside anacrolix/torrent, the probe itself had already finished, "+
				"and only the client's own bookkeeping is affected", infoHash, r)
		}
	}()
	drop()
}
