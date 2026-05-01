package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"

	"jacred/app"
	"jacred/background"
	"jacred/core"
	"jacred/filedb"
	"jacred/server"
	"jacred/tracks"
)

func setupLog(logDir string) (*os.File, error) {
	name := time.Now().Format("2006-01-02") + ".log"
	fp := filepath.Join(logDir, name)
	f, err := os.OpenFile(fp, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file %s: %w", fp, err)
	}
	// Write to both stdout and file
	mw := io.MultiWriter(os.Stdout, f)
	log.SetOutput(mw)
	log.SetFlags(log.LstdFlags)
	return f, nil
}

func cleanOldLogs(logDir string, keepDays int) {
	cutoff := time.Now().AddDate(0, 0, -keepDays)
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(logDir, e.Name()))
		}
	}
}

func main() {
	cfg, err := app.LoadConfig("init.yaml")
	if err != nil {
		log.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join("Data", "fdb"), 0o755)
	_ = os.MkdirAll(filepath.Join("Data", "temp"), 0o755)
	_ = os.MkdirAll(filepath.Join("Data", "log"), 0o755)
	_ = os.MkdirAll(filepath.Join("Data", "tracks"), 0o755)

	// Memory limits
	gcpct := cfg.GCPercent
	if gcpct <= 0 {
		gcpct = 50
	}
	debug.SetGCPercent(gcpct)
	if cfg.MemLimitMB > 0 {
		debug.SetMemoryLimit(int64(cfg.MemLimitMB) * 1024 * 1024)
		log.Printf("memory: limit=%dMB gc=%d%%", cfg.MemLimitMB, gcpct)
	} else {
		log.Printf("memory: no hard limit, gc=%d%%", gcpct)
	}

	// Log to file + stdout (if log: true in config)
	if cfg.Log {
		logFile, err := setupLog(filepath.Join("Data", "log"))
		if err != nil {
			log.Printf("warning: %v (logging to stdout only)", err)
		} else {
			defer logFile.Close()
		}
		cleanOldLogs(filepath.Join("Data", "log"), 14)
	}

	// Initialize shared flaresolverr-go service (one Chrome for all parsers)
	core.InitFlareService(cfg.FlareSolverrGo)

	// Rehydrate solved CF sessions from disk so parsers started immediately
	// after this can reuse a still-valid cf_clearance instead of triggering
	// a fresh Chrome solve. Directory is created if missing.
	core.SetFlarePersistDir(filepath.Join("Data", "temp", "flare"))

	// Load auto-detected CF domains so a restart doesn't waste a fresh
	// standard request on each known CF-protected site before re-flagging it.
	core.SetCFAutoPersistFile(filepath.Join("Data", "temp", "cf_auto.json"))

	db := filedb.New(cfg, "Data")
	if err := db.RebuildIndexes(); err != nil {
		log.Fatal(err)
	}
	// Persist migrated index immediately so old C# epoch values don't survive restarts.
	if err := db.SaveChangesToFileNow(); err != nil {
		log.Printf("warning: failed to persist migrated index: %v", err)
	}
	// Контекст с отменой для всех фоновых горутин
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go db.RunBackgroundJobs(ctx)
	go background.RunTrackersCron(ctx, db, "Data", cfg.Evercache.Enable && cfg.Evercache.ValidHour <= 0)

	tracksDB := tracks.New("Data")
	if err := tracksDB.Load(); err != nil {
		log.Printf("tracks load error: %v", err)
	} else {
		log.Printf("tracks loaded: %d", tracksDB.Count())
	}
	filedb.SetFFProbeLookup(func(magnet string, types []string) []filedb.FFStreamLite {
		streams, ok := tracksDB.GetByMagnet(magnet, types, true)
		if !ok || len(streams) == 0 {
			return nil
		}
		out := make([]filedb.FFStreamLite, 0, len(streams))
		for _, s := range streams {
			lite := filedb.FFStreamLite{CodecType: s.CodecType}
			if s.Tags != nil {
				lite.TagsTitle = s.Tags.Title
			}
			out = append(out, lite)
		}
		return out
	})

	if cfg.Tracks {
		manager := tracks.NewManager(cfg, db, tracksDB, "Data")
		for i := 1; i <= 5; i++ {
			go manager.RunLoop(ctx, i)
		}
	}

	// WWWRoot is an optional disk override for embedded UI assets. When the
	// "wwwroot" directory exists alongside the binary it is preferred per-file
	// (handy for live-editing HTML during dev); otherwise the embedded copy
	// is served and the binary is fully self-contained.
	wwwroot := ""
	if st, err := os.Stat("wwwroot"); err == nil && st.IsDir() {
		wwwroot = "wwwroot"
	}
	srv := server.New(cfg, db, tracksDB, wwwroot)

	// Config hot-reload: check init.yaml mtime every 10 seconds
	reloader := background.NewConfigReloader("init.yaml", cfg)
	reloader.OnReload(func(newCfg app.Config) {
		srv.UpdateConfig(newCfg)
	})
	go reloader.Run(ctx)

	go srv.RunStatsLoop(ctx)
	go background.RunSyncCron(ctx, cfg, db)
	go background.RunSyncSpidr(ctx, cfg, db)
	go background.RunEvercacheCron(ctx, db)
	addr := ":" + strconv.Itoa(cfg.ListenPort)

	httpServer := &http.Server{
		Addr:         addr,
		Handler:      srv.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // no limit — cron parsers can run 10+ minutes
		IdleTimeout:  60 * time.Second,
	}

	// Запуск HTTP-сервера в горутине
	go func() {
		log.Printf("jacred listening on %s", addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server error: %v", err)
		}
	}()

	// Ожидание сигнала завершения
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	log.Printf("received signal %v, shutting down...", sig)

	// Отмена контекста для фоновых горутин
	cancel()

	// Graceful shutdown HTTP-сервера (15 секунд на завершение текущих запросов)
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("http server shutdown error: %v", err)
	}

	// Shutdown flaresolverr-go (close Chrome)
	core.CloseFlareService()

	// Flush dirty buckets and save masterDb
	log.Println("saving database...")
	if n := db.FlushDirtyBuckets(); n > 0 {
		log.Printf("flushed %d dirty buckets", n)
	}
	if err := db.SaveChangesToFileNow(); err != nil {
		log.Printf("error saving masterDb: %v", err)
	} else {
		log.Println("database saved successfully")
	}

	log.Println("jacred stopped")
}
