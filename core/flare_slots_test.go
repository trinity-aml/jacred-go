package core

import (
	"testing"
	"time"
)

func TestSerialFlareSlotsAndReleaseAfterConfigChange(t *testing.T) {
	previous := serialFlareSolves.Load()
	defer serialFlareSolves.Store(previous)
	serialFlareSolves.Store(true)
	release := acquireFlareSlot()
	entered := make(chan func(), 1)
	go func() { entered <- acquireFlareSlot() }()
	select {
	case second := <-entered:
		second()
		release()
		t.Fatal("a second browser solve entered the serial gate")
	case <-time.After(20 * time.Millisecond):
	}
	serialFlareSolves.Store(false)
	release()
	select {
	case second := <-entered:
		second()
	case <-time.After(time.Second):
		t.Fatal("changing configuration lost the captured release gate")
	}
}
