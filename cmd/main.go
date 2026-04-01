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
	"strconv"
	"syscall"
	"time"

	"jacred/app"
	"jacred/background"
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

	db := filedb.New(cfg, "Data")
	if err := db.RebuildIndexes(); err != nil {
		log.Fatal(err)
	}
	tracksDB := tracks.New("Data")
	if err := tracksDB.Load(); err != nil {
		log.Printf("tracks load error: %v", err)
	} else {
		log.Printf("tracks loaded: %d", tracksDB.Count())
	}

	// Контекст с отменой для всех фоновых горутин
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if cfg.Tracks {
		manager := tracks.NewManager(cfg, db, tracksDB, "Data")
		for i := 1; i <= 5; i++ {
			go manager.RunLoop(ctx, i)
		}
	}
	go background.RunTrackersCron(ctx, db, "Data", "wwwroot", cfg.Evercache.Enable && cfg.Evercache.ValidHour <= 0)

	srv := server.New(cfg, db, tracksDB, "wwwroot")
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

	// Сохраняем masterDb перед выходом
	log.Println("saving database...")
	if err := db.SaveChangesToFile(); err != nil {
		log.Printf("error saving masterDb: %v", err)
	} else {
		log.Println("database saved successfully")
	}

	log.Println("jacred stopped")
}
