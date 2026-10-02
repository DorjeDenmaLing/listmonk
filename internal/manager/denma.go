package manager

import "sync"

// denma: many centers run in one process, each with its own campaign manager,
// and a center's settings save replaces its manager without restarting the
// process (upstream restarts the process, so Close() was enough).

// SetNotify sets the function that sends campaign status notifications (the
// center's own notifier rather than the process-wide one New() uses). Call it
// before Run().
func (m *Manager) SetNotify(fn func(subject string, data any) error) {
	m.fnNotify = fn
}

// stoppedScans holds the managers whose campaign scanner should exit.
var stoppedScans sync.Map

// StopScanning stops the manager picking up campaigns, so that a replacement
// manager doesn't run alongside it. Messages already queued are still sent
// until Close().
func (m *Manager) StopScanning() {
	stoppedScans.Store(m, struct{}{})
}

// scanStopped reports (once) whether StopScanning was called.
func (m *Manager) scanStopped() bool {
	_, ok := stoppedScans.LoadAndDelete(m)
	return ok
}
