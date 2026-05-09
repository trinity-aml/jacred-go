package background

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"jacred/app"
)

// TestConfigReloaderFiresOnWrite verifies fsnotify path picks up an
// in-place write to init.yaml without waiting for the 10s poll tick.
// Must finish well under that interval to prove the fast path is used.
func TestConfigReloaderFiresOnWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "init.yaml")
	initial := app.DefaultConfig()
	initial.ListenPort = 9117
	if err := os.WriteFile(path, []byte(app.MarshalYAML(initial)), 0o644); err != nil {
		t.Fatal(err)
	}

	cr := NewConfigReloader(path, initial)

	var fired atomic.Int32
	gotPort := atomic.Int32{}
	cr.OnReload(func(c app.Config) {
		fired.Add(1)
		gotPort.Store(int32(c.ListenPort))
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		cr.Run(ctx)
		close(done)
	}()
	// Give Run a beat to install the watcher.
	time.Sleep(100 * time.Millisecond)

	// Mtime resolution on some filesystems (ext4 with relatime, FAT) is
	// coarse — bump beyond a second so cr.check sees the change.
	time.Sleep(1100 * time.Millisecond)
	updated := initial
	updated.ListenPort = 9118
	if err := os.WriteFile(path, []byte(app.MarshalYAML(updated)), 0o644); err != nil {
		t.Fatal(err)
	}

	// fsnotify event + 150ms debounce should land well within 2s.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && fired.Load() == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	if fired.Load() == 0 {
		t.Fatalf("reload did not fire on write")
	}
	if gotPort.Load() != 9118 {
		t.Fatalf("reload payload wrong: port=%d", gotPort.Load())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after cancel")
	}
}
