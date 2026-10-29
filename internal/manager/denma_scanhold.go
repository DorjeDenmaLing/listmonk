package manager

import (
	"errors"
	"sync"

	"github.com/knadh/listmonk/models"
)

// denma: a center's reload builds the new manager while the old one still
// runs its campaigns; the new one's scanner is held until the old one has
// stopped, so that no campaign runs in both.

// heldScans holds the managers whose campaign scanner picks up nothing yet.
var heldScans sync.Map

// DenmaHoldScan keeps the manager from picking up campaigns until
// DenmaReleaseScan. Call it before Run.
func (m *Manager) DenmaHoldScan() {
	heldScans.Store(m, struct{}{})
}

// DenmaReleaseScan lets a held manager pick up campaigns.
func (m *Manager) DenmaReleaseScan() {
	heldScans.Delete(m)
}

// DenmaScanHeld reports whether the manager is held, for the center's other
// jobs, which wait with it.
func (m *Manager) DenmaScanHeld() bool { return m.scanHeld() }

// denmaScanState is whether pipes may still be made by a campaign scan.
type denmaScanState struct {
	mu     sync.Mutex
	closed bool
}

var errScanClosed = errors.New("the manager is stopping")

// denmaCloseScan makes denmaScanPipe refuse, once any pipe it is making is
// in the manager's pipes: a scan in progress when scanning is stopped can't
// start a campaign after the manager's campaigns have been stopped.
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
