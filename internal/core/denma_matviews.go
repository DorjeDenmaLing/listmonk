package core

// denma: materialized view refresh on a request, shared by requests that arrive together.

import "sync"

type denmaViewRefreshes struct {
	mu    sync.Mutex
	views map[string]*denmaViewRefresh
}

type denmaViewRefresh struct {
	running bool
	done    *sync.Cond
	// next is closed when the refresh queued behind the running one ends.
	next chan struct{}
}

// denmaRefreshView refreshes the view, or waits for the refresh queued behind the running one.
func (c *Core) denmaRefreshView(name string) error {
	v := &c.denmaViews
	v.mu.Lock()
	if v.views == nil {
		v.views = map[string]*denmaViewRefresh{}
	}
	r := v.views[name]
	if r == nil {
		r = &denmaViewRefresh{done: sync.NewCond(&v.mu)}
		v.views[name] = r
	}
	if ch := r.next; ch != nil {
		v.mu.Unlock()
		<-ch
		return nil
	}
	ch := make(chan struct{})
	r.next = ch
	for r.running {
		r.done.Wait()
	}
	r.next, r.running = nil, true
	v.mu.Unlock()

	err := c.RefreshMatView(name, true)

	v.mu.Lock()
	r.running = false
	r.done.Broadcast()
	v.mu.Unlock()
	close(ch)
	return err
}
