package core

import "sync/atomic"

var serialFlareSolves atomic.Bool
var singleFlareSlot = make(chan struct{}, 1)

// Keep the shared semaphore stable even when configuration is re-initialized.
// Each release captures whether it acquired the additional serial gate.
func acquireFlareSlot() func() {
	serial := serialFlareSolves.Load()
	if serial {
		singleFlareSlot <- struct{}{}
	}
	flareSolveSem <- struct{}{}
	return func() {
		<-flareSolveSem
		if serial {
			<-singleFlareSlot
		}
	}
}
