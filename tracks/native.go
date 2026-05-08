package tracks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
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
	cl           *torrent.Client
	storage      *memStorage
	infoWait     time.Duration
	ffprobePath  string // absolute path to ffprobe; empty falls back to PATH lookup
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
	return &NativeAnalyzer{cl: cl, storage: mem, infoWait: 2 * time.Minute, ffprobePath: ffprobePath}, nil
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
	spec, err := torrent.TorrentSpecFromMagnetUri(strings.TrimSpace(magnet))
	if err != nil {
		return nil, fmt.Errorf("parse magnet: %w", err)
	}
	t, _, err := n.cl.AddTorrentSpec(spec)
	if err != nil {
		return nil, fmt.Errorf("add torrent: %w", err)
	}
	defer t.Drop()

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

	for _, f := range t.Files() {
		if f != biggest {
			f.SetPriority(torrent.PiecePriorityNone)
		}
	}
	biggest.SetPriority(torrent.PiecePriorityNormal)

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
	pieces := make([]*memPiece, num)
	for i := 0; i < num; i++ {
		pieces[i] = &memPiece{size: info.Piece(i).V1Length()}
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
			return nil
		},
	}, nil
}

func (s *memStorage) Close() error { return nil }

type memTorrent struct {
	pieces []*memPiece
}

type memPiece struct {
	mu       sync.Mutex
	size     int64
	data     []byte
	complete bool
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
	if p.data == nil {
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
