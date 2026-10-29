package manager

import "sync"

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

// scanHeld reports whether the campaign scanner is held.
func (m *Manager) scanHeld() bool {
	_, ok := heldScans.Load(m)
	return ok
}
