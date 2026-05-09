package background

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"jacred/app"
)

// ConfigReloader watches init.yaml and reloads it on change.
// Consumers register callbacks via OnReload.
//
// The watcher uses inotify/kqueue via fsnotify when available — that means
// reloads land within milliseconds of the editor saving and we don't burn a
// stat() syscall every 10 seconds against the entire process lifetime. If
// fsnotify can't be initialized (rare; e.g. inotify watch limits exhausted)
// we transparently fall back to the old 10s polling loop.
//
// We watch the parent directory rather than the file itself: many editors
// save by writing a temp file then renaming over the target, which would
// orphan a per-file watch (the new inode is a different file). Watching the
// directory and filtering by basename catches both write-in-place and
// atomic-rename patterns.
type ConfigReloader struct {
	path     string
	lastMod  time.Time
	mu       sync.RWMutex
	config   app.Config
	handlers []func(app.Config)
}

const configPollInterval = 10 * time.Second

func NewConfigReloader(path string, initial app.Config) *ConfigReloader {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	var mtime time.Time
	if info, err := os.Stat(path); err == nil {
		mtime = info.ModTime()
	}
	return &ConfigReloader{
		path:    path,
		lastMod: mtime,
		config:  initial,
	}
}

// OnReload registers a callback that fires when config changes.
func (cr *ConfigReloader) OnReload(fn func(app.Config)) {
	cr.mu.Lock()
	cr.handlers = append(cr.handlers, fn)
	cr.mu.Unlock()
}

// Current returns the current config snapshot.
func (cr *ConfigReloader) Current() app.Config {
	cr.mu.RLock()
	defer cr.mu.RUnlock()
	return cr.config
}

// Run blocks until ctx is cancelled.
func (cr *ConfigReloader) Run(ctx context.Context) {
	if cr.runWatch(ctx) {
		return
	}
	log.Printf("config reload: fsnotify unavailable, falling back to %s polling", configPollInterval)
	cr.runPoll(ctx)
}

// runWatch installs an fsnotify watch and processes events. Returns true on
// clean exit (ctx cancelled or events channel closed); false if the watcher
// failed to set up — caller should fall back to polling.
func (cr *ConfigReloader) runWatch(ctx context.Context) bool {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("config reload: fsnotify init: %v", err)
		return false
	}
	defer watcher.Close()
	dir := filepath.Dir(cr.path)
	if err := watcher.Add(dir); err != nil {
		log.Printf("config reload: watch %s: %v", dir, err)
		return false
	}
	base := filepath.Base(cr.path)
	// Catch any change that happened between NewConfigReloader and Run.
	cr.check()

	// Coalesce bursts of events (editors typically emit several writes per
	// save) into a single reload via a small debounce.
	var debounce *time.Timer
	defer func() {
		if debounce != nil {
			debounce.Stop()
		}
	}()
	scheduleReload := func() {
		if debounce != nil {
			debounce.Stop()
		}
		debounce = time.AfterFunc(150*time.Millisecond, cr.check)
	}

	for {
		select {
		case <-ctx.Done():
			return true
		case ev, ok := <-watcher.Events:
			if !ok {
				return true
			}
			if filepath.Base(ev.Name) != base {
				continue
			}
			// Chmod covers `touch init.yaml` (mtime-only updates produce
			// IN_ATTRIB on Linux); the others cover regular edits and
			// editor atomic-rename saves.
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove|fsnotify.Chmod) == 0 {
				continue
			}
			scheduleReload()
		case err, ok := <-watcher.Errors:
			if !ok {
				return true
			}
			log.Printf("config reload: fsnotify error: %v", err)
		}
	}
}

func (cr *ConfigReloader) runPoll(ctx context.Context) {
	ticker := time.NewTicker(configPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cr.check()
		}
	}
}

func (cr *ConfigReloader) check() {
	info, err := os.Stat(cr.path)
	if err != nil {
		return
	}
	mtime := info.ModTime()
	cr.mu.RLock()
	changed := mtime != cr.lastMod
	cr.mu.RUnlock()
	if !changed {
		return
	}

	cfg, err := app.LoadConfig(cr.path)
	if err != nil {
		log.Printf("config reload: error reading %s: %v", cr.path, err)
		return
	}

	cr.mu.Lock()
	cr.lastMod = mtime
	cr.config = cfg
	handlers := make([]func(app.Config), len(cr.handlers))
	copy(handlers, cr.handlers)
	cr.mu.Unlock()

	log.Printf("config reload: %s updated, notifying %d handlers", cr.path, len(handlers))
	for _, fn := range handlers {
		fn(cfg)
	}
}
