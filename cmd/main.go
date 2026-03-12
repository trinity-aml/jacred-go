package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"jacred/app"
	"jacred/background"
	"jacred/filedb"
	"jacred/server"
	"jacred/tracks"
)

func main() {
	cfg, err := app.LoadConfig("init.yaml")
	if err != nil {
		log.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join("Data", "fdb"), 0o755)
	_ = os.MkdirAll(filepath.Join("Data", "temp"), 0o755)
	_ = os.MkdirAll(filepath.Join("Data", "log"), 0o755)
	_ = os.MkdirAll(filepath.Join("Data", "tracks"), 0o755)
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
	ctx := context.Background()
	if cfg.Tracks {
		manager := tracks.NewManager(cfg, db, tracksDB, "Data")
		for i := 1; i <= 5; i++ {
			go manager.RunLoop(ctx, i)
		}
	}
	go background.RunTrackersCron(ctx, db, "Data", "wwwroot", cfg.Evercache.Enable && cfg.Evercache.ValidHour <= 0)
	srv := server.New(cfg, db, tracksDB, "wwwroot")
	addr := ":" + strconv.Itoa(cfg.ListenPort)
	log.Printf("jacred listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, srv.Handler()))
}
