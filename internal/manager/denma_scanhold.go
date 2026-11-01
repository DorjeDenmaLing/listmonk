package manager

import (
	"errors"
	"sync"

	"github.com/knadh/listmonk/models"
)

// denma: a reloaded center's new manager holds its campaign scanner until the old one has stopped.

// heldScans holds the managers whose campaign scanner picks up nothing yet.
var heldScans sync.Map

// DenmaHoldScan keeps the manager from picking up campaigns until DenmaReleaseScan.
func (m *Manager) DenmaHoldScan() {
	heldScans.Store(m, struct{}{})
}

// DenmaReleaseScan lets a held manager pick up campaigns.
func (m *Manager) DenmaReleaseScan() {
	heldScans.Delete(m)
}

// DenmaScanHeld reports whether the manager is held.
func (m *Manager) DenmaScanHeld() bool { return m.scanHeld() }

// denmaScanState is whether pipes may still be made by a campaign scan.
type denmaScanState struct {
	mu     sync.Mutex
	closed bool
}

var errScanClosed = errors.New("the manager is stopping")

// denmaCloseScan makes denmaScanPipe refuse, once any pipe it is making is in the manager's pipes.
func (m *Manager) denmaCloseScan() {
	m.denmaScan.mu.Lock()
	m.denmaScan.closed = true
	m.denmaScan.mu.Unlock()
}

// denmaScanPipe is newPipe for the campaign scan.
func (m *Manager) denmaScanPipe(c *models.Campaign) (*pipe, error) {
	m.denmaScan.mu.Lock()
	defer m.denmaScan.mu.Unlock()
	if m.denmaScan.closed {
		return nil, errScanClosed
	}
	return m.newPipe(c)
}

// scanHeld reports whether the campaign scanner is held.
func (m *Manager) scanHeld() bool {
	_, ok := heldScans.Load(m)
	return ok
}
