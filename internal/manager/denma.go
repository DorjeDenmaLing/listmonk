package manager

import (
	"sync"
	"time"
)

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

// denmaQueue is a campaign pipe's queued messages, for its resume checkpoint
// (pipe.lastID, saved as campaigns.last_subscriber_id by the campaign scan
// and when the pipe ends): the highest subscriber ID up to which every queued
// message has finished, sent or failed. Upstream PR #3222 keeps fetching from
// moving the checkpoint, but takes the highest ID sent, while the workers may
// still be sending lower ones (or skip them, once the campaign is paused); a
// restart then skipped those. Messages are queued in ID order and finish in
// any order.
type denmaQueue struct {
	mu     sync.Mutex
	queued []int        // queued and not finished, in order
	done   map[int]bool // finished before an earlier one
}

// denmaQueued records a message queued for a subscriber. Call it before the
// message can reach a worker.
func (p *pipe) denmaQueued(subID int) {
	p.denmaQ.mu.Lock()
	p.denmaQ.queued = append(p.denmaQ.queued, subID)
	p.denmaQ.mu.Unlock()
}

// denmaFinished records a subscriber's message as finished (sent or failed)
// and moves the checkpoint past every message finished in order. A message a
// worker skips (the campaign was stopped) isn't finished, so the checkpoint
// stays before it and a resumed campaign sends it.
func (p *pipe) denmaFinished(subID int) {
	q := &p.denmaQ
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.done == nil {
		q.done = map[int]bool{}
	}
	q.done[subID] = true
	for len(q.queued) > 0 && q.done[q.queued[0]] {
		delete(q.done, q.queued[0])
		if id := uint64(q.queued[0]); id > p.lastID.Load() {
			p.lastID.Store(id)
		}
		q.queued = q.queued[1:]
	}
}

// DenmaStop stops the manager for a shutdown: it picks up no more campaigns,
// and each running campaign stops where it is and saves its progress (its
// pipe's cleanup: the sent count and the exact checkpoint), keeping its
// status, so that the next start resumes it without sending anyone a second
// copy. It returns once every campaign has saved, or false after wait.
func (m *Manager) DenmaStop(wait time.Duration) bool {
	m.StopScanning()
	for deadline := time.Now().Add(wait); ; {
		m.pipesMut.RLock()
		n := len(m.pipes)
		for _, p := range m.pipes {
			p.Stop(false) // and any a scan started meanwhile
		}
		m.pipesMut.RUnlock()
		if n == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}
