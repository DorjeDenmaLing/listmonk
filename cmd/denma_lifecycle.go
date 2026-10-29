package main

// denma: stopping a center's App when it is reloaded or disabled. The App
// isn't stopped under lifeMu (it takes up to a minute), and the App that
// replaces it picks up no campaign until the old one has saved its progress,
// so that no campaign runs in both.

import (
	"time"

	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/internal/manager"
)

// denmaHoldScan holds a new App's campaign scanner (denma.hold_scan, set by
// denmaCenters.load for an App that replaces another).
func denmaHoldScan(mgr *manager.Manager, ko *koanf.Koanf) {
	if ko.Bool("denma.hold_scan") {
		mgr.DenmaHoldScan()
	}
}

// stopsOf returns the stops of center slug's Apps under way now, which end
// when their channel closes. Call it under lifeMu, where stops begin.
func (d *denmaCenters) stopsOf(slug string) []chan struct{} {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return append([]chan struct{}(nil), d.stopping[slug]...)
}

// beginStop notes that an App of center slug is being stopped, and returns
// the function that ends it. Call it under lifeMu.
func (d *denmaCenters) beginStop(slug string) (end func()) {
	ch := make(chan struct{})
	d.mu.Lock()
	if d.stopping == nil {
		d.stopping = map[string][]chan struct{}{}
	}
	d.stopping[slug] = append(d.stopping[slug], ch)
	d.mu.Unlock()
	return func() {
		d.mu.Lock()
		d.stopping[slug] = removeChan(d.stopping[slug], ch)
		if len(d.stopping[slug]) == 0 {
			delete(d.stopping, slug)
		}
		d.mu.Unlock()
		close(ch)
	}
}

func removeChan(l []chan struct{}, ch chan struct{}) []chan struct{} {
	out := make([]chan struct{}, 0, len(l))
	for _, c := range l {
		if c != ch {
			out = append(out, c)
		}
	}
	return out
}

// denmaStragglerWait is how long an App whose campaigns didn't stop in time is
// waited for to finish them.
const denmaStragglerWait = 10 * time.Minute

// retireThen retires old, once ended outside lifeMu (end, from beginStop),
// and then lets the App that replaced it, next, pick up campaigns, after the
// stops before had finished.
func (d *denmaCenters) retireThen(old, next *App, before []chan struct{}, end func()) {
	stopped := denmaRetire(old, false)
	// Its campaigns are all marked stopped; they're done once they've saved.
	for deadline := time.Now().Add(denmaStragglerWait); !stopped && old.manager.HasRunningCampaigns() && time.Now().Before(deadline); {
		time.Sleep(5 * time.Second)
	}
	end()
	d.releaseScan(next, before)
}

// releaseScan lets next pick up campaigns once the stops before have ended.
func (d *denmaCenters) releaseScan(next *App, before []chan struct{}) {
	if next == nil {
		return
	}
	for _, ch := range before {
		<-ch
	}
	next.manager.DenmaReleaseScan()
}
