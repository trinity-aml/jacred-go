package background

import (
	"context"
	"log"
	"os"
	"sync"
	"time"

	"jacred/app"
)

// ConfigReloader checks init.yaml mtime every 10 seconds and reloads if changed.
// Consumers register callbacks via OnReload.
type ConfigReloader struct {
	path     string
	lastMod  time.Time
	mu       sync.RWMutex
	config   app.Config
	handlers []func(app.Config)
}

func NewConfigReloader(path string, initial app.Config) *ConfigReloader {
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

// Run starts the reload loop. Blocks until ctx is cancelled.
func (cr *ConfigReloader) Run(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
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
