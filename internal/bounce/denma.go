package bounce

import "sync"

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
