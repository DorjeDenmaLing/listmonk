package manager

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/knadh/listmonk/models"
)

type scanCountStore struct {
	*mockStore
	scans atomic.Int32
}

func (s *scanCountStore) NextCampaigns(a, b, c []int64) ([]*models.Campaign, error) {
	s.scans.Add(1)
	return nil, nil
}

// A held manager's scanner scans nothing until it's released, and a stopped
// one exits whether held or not.
func TestScanHeld(t *testing.T) {
	st := &scanCountStore{mockStore: &mockStore{}}
	m := newTestManager(st, mockMessenger{})
	m.DenmaHoldScan()
	go m.scanCampaigns(5 * time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	if n := st.scans.Load(); n != 0 {
		t.Fatalf("a held manager scanned %d times", n)
	}
	m.DenmaReleaseScan()
	time.Sleep(100 * time.Millisecond)
	if st.scans.Load() == 0 {
		t.Fatal("a released manager didn't scan")
	}
	m.StopScanning()
	time.Sleep(50 * time.Millisecond)
	n := st.scans.Load()
	time.Sleep(50 * time.Millisecond)
	if st.scans.Load() != n {
		t.Fatal("a stopped manager went on scanning")
	}
}

func TestScanStoppedWhileHeld(t *testing.T) {
	st := &scanCountStore{mockStore: &mockStore{}}
	m := newTestManager(st, mockMessenger{})
	m.DenmaHoldScan()
	m.StopScanning()
	done := make(chan struct{})
	go func() { m.scanCampaigns(5 * time.Millisecond); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a stopped, held scanner didn't exit")
	}
	if st.scans.Load() != 0 {
		t.Fatal("it scanned")
	}
}
