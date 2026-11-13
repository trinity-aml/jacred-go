package server

import (
	"context"
	"jacred/app"
	"testing"
	"time"
)

func TestNegativeStatsIntervalSkipsStartupScan(t *testing.T) {
	s := &Server{Config: app.Config{TimeStatsUpdate: -1, OpenStats: true}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { s.RunStatsLoop(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disabled stats loop still running")
	}
}
