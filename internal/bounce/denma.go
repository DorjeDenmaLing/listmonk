package bounce

import (
	"sync"
	"time"
)

// denma: an app is rebuilt in place when its settings are saved (upstream
// restarts the process), so a replaced app's mailbox scanner must stop, or
// the old and new ones both read (and delete from) the mailbox.

// stoppedScanners holds the managers whose mailbox scanner should exit.
var stoppedScanners sync.Map

// StopScanning stops the manager's mailbox scanner before its next scan.
// Webhook bounces it's given are still recorded.
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

// denmaWaitQueue waits for the queue to empty before a scan: the scanner
// drops the bounces that don't fit, and then deletes them from the mailbox.
// A scan reads at most the queue's size.
func (m *Manager) denmaWaitQueue() {
	for len(m.queue) > 0 && !m.scanStopped() {
		time.Sleep(100 * time.Millisecond)
	}
}
