package bounce

import (
	"sync"
	"time"
)

// denma: a replaced app's mailbox scanner stops.

// stoppedScanners holds the managers whose mailbox scanner should exit.
var stoppedScanners sync.Map

// StopScanning stops the manager's mailbox scanner before its next scan.
func (m *Manager) StopScanning() {
	stoppedScanners.Store(m, struct{}{})
}

// scanStopped reports whether StopScanning was called.
func (m *Manager) scanStopped() bool {
	_, ok := stoppedScanners.Load(m)
	return ok
}

// DenmaQueued is how many bounces are queued, not yet given to RecordBounceCB.
func (m *Manager) DenmaQueued() int {
	return len(m.queue)
}

// denmaWaitQueue waits for the queue to empty before a scan.
func (m *Manager) denmaWaitQueue() {
	for len(m.queue) > 0 && !m.scanStopped() {
		time.Sleep(100 * time.Millisecond)
	}
}
