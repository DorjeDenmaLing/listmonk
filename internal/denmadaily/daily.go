// Package denmadaily counts the e-mails sent in the last 24 hours against a
// daily limit, by the minute, in memory and in the database (cmd/denma_daily.go).
// Campaigns and automations stop short of the limit by a reserve (a
// percentage of it), which is left for opt-in confirmations, password resets,
// invites and notifications.
//
// Campaign messages each wait for their turn (Wait), which counts them as it
// lets them go, so that messages waiting together can't all take the last
// of the limit. They queue, and are let go in order, one center's message
// after another's in turn, as room frees (a timer wakes them; none polls). A
// claim refused for lack of room (ClaimAll), and an automation's due e-mails
// that got none (Want), are remembered for a while (refused claims as one, the
// largest), and campaign messages
// leave that much room for them, so transactional e-mails and automations
// aren't starved by them. A message whose wait gives up, or
// whose later wait does, gives its count back. Automations send a
// batch at once, so they claim their share of what's left first (Claim): it
// counts against the limit straight away, so that automations in several
// centers running in the same minute can't each take the same room. Each
// claimed e-mail sent turns a claim into a send (AddClaimed); what an
// automation claimed but didn't send it gives back (Release). Each claim is
// kept by its ID, which its e-mails carry, so a late send counts against its
// own claim and no other's. Claims are in memory only, and a claim left
// untouched for claimTimeout is dropped, in case its e-mails were never sent.
//
// Each center also has a Counter of its own (Counters), against its own
// daily cap, which a superadmin sets on its Config page; it has no reserve.
package denmadaily

import (
	"log"
	"slices"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
)

// dayMinutes is the window, in minutes.
const dayMinutes = 24 * 60

// Minute is the e-mails sent in one minute (Unix minutes).
type Minute struct {
	Minute int64 `db:"minute"`
	N      int   `db:"n"`
}

// Counter counts the e-mails sent in the last 24 hours, against the
// hub's limit.
type Counter struct {
	mu      sync.Mutex
	limit   int
	reserve int              // percent of limit that campaigns leave
	minutes []Minute         // the last 24 hours', oldest first
	total   int              // their sum
	pending map[int64]int    // counted since the last save
	waiting bool             // the limit was reached (logged once)
	claimed int              // claimed by automations and transactional e-mails, not sent yet
	claims  map[int64]*claim // the claims, by ID; claimed is their sum
	lastID  int64            // the last claim's ID
	db      *sqlx.DB
	log     *log.Logger
	center  string // a center's counter (Counters); "" for the hub's

	queues  map[string][]*waiter // campaign messages waiting, by center, in order
	order   []string             // the centers waiting, in turn
	turn    int                  // the next center's place in order
	timer   *time.Timer          // wakes the queue when room frees
	demands []demand             // claims refused lately, to leave room for
}

// New returns a counter without a limit, logging to lg.
func New(lg *log.Logger) *Counter {
	return &Counter{pending: map[int64]int{}, log: lg}
}

// flushEvery is how often the counts are saved.
const flushEvery = 10 * time.Second

// Init makes the counts' table, loads the last 24 hours', and saves new ones
// every flushEvery. Call it once.
func (d *Counter) Init(db *sqlx.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS denma.daily_sends (
		minute BIGINT PRIMARY KEY, -- Unix minutes
		n      INTEGER NOT NULL
	)`); err != nil {
		return err
	}
	var rows []Minute
	if err := db.Select(&rows, `SELECT minute, n FROM denma.daily_sends WHERE minute > $1 ORDER BY minute`,
		time.Now().Unix()/60-dayMinutes); err != nil {
		return err
	}
	d.mu.Lock()
	d.db = db
	d.minutes, d.total = rows, 0
	for _, r := range rows {
		d.total += r.N
	}
	d.mu.Unlock()
	go func() {
		for range time.Tick(flushEvery) {
			d.Save()
		}
	}()
	return nil
}

// Save writes the counts made since the last save, and forgets old ones.
func (d *Counter) Save() {
	d.mu.Lock()
	if d.db == nil || len(d.pending) == 0 {
		d.mu.Unlock()
		return
	}
	var mins []int64
	var ns []int
	for m, n := range d.pending {
		mins, ns = append(mins, m), append(ns, n)
	}
	d.pending = map[int64]int{}
	db := d.db
	d.mu.Unlock()

	if _, err := db.Exec(`INSERT INTO denma.daily_sends (minute, n) SELECT * FROM unnest($1::BIGINT[], $2::INT[])
		ON CONFLICT (minute) DO UPDATE SET n = denma.daily_sends.n + EXCLUDED.n`, pq.Array(mins), pq.Array(ns)); err != nil {
		d.log.Printf("denma: error saving the daily send counts: %v", err)
		// Kept for the next save.
		d.mu.Lock()
		for i, m := range mins {
			d.pending[m] += ns[i]
		}
		d.mu.Unlock()
		return
	}
	if _, err := db.Exec(`DELETE FROM denma.daily_sends WHERE minute <= $1`, time.Now().Unix()/60-dayMinutes-60); err != nil {
		d.log.Printf("denma: error forgetting old daily send counts: %v", err)
	}
}

// claim is what's left of one claim, and when it last changed.
type claim struct {
	n  int
	at time.Time
}

// claimTimeout is how long a claim lasts untouched before it's dropped: a
// day, as its e-mails can wait that long behind campaign messages waiting
// for the limit, and dropping it early would let them go over it.
const claimTimeout = 24 * time.Hour

// expire drops the minutes older than 24 hours, and claims left untouched
// for claimTimeout. Call with mu held.
func (d *Counter) expire(now time.Time) {
	for id, c := range d.claims {
		if now.Sub(c.at) > claimTimeout {
			d.log.Printf("denma: dropping %d e-mails claimed a day ago and not sent", c.n)
			d.claimed -= c.n
			delete(d.claims, id)
		}
	}
	cutoff := now.Unix()/60 - dayMinutes
	i := 0
	for i < len(d.minutes) && d.minutes[i].Minute <= cutoff {
		d.total -= d.minutes[i].N
		i++
	}
	if i > 0 {
		d.minutes = append(d.minutes[:0], d.minutes[i:]...)
	}
}

// Add counts an e-mail sent.
func (d *Counter) Add() {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expire(now)
	d.add(now)
}

// add counts an e-mail sent. Call with mu held.
func (d *Counter) add(now time.Time) {
	m := now.Unix() / 60
	if n := len(d.minutes); n > 0 && d.minutes[n-1].Minute == m {
		d.minutes[n-1].N++
	} else {
		d.minutes = append(d.minutes, Minute{Minute: m, N: 1})
	}
	d.total++
	d.pending[m]++
}

// AddClaimed counts an e-mail sent that claim id had claimed (one dropped
// already, or 0: just counted).
func (d *Counter) AddClaimed(id int64) {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expire(now)
	d.add(now)
	d.unclaim(id, 1, now)
}

// unclaim takes up to n off claim id. Call with mu held.
func (d *Counter) unclaim(id int64, n int, now time.Time) {
	c := d.claims[id]
	if c == nil || n <= 0 {
		return
	}
	n = min(n, c.n)
	c.n -= n
	d.claimed -= n
	c.at = now
	if c.n == 0 {
		delete(d.claims, id)
	}
}

// Claim takes up to n of what's left for campaigns and automations, for an
// automation's batch, and returns how many it got, and the claim's ID for
// its e-mails (AddClaimed) and what isn't sent (Release): 0 if none was
// claimed. Without a limit, it gets all n, unclaimed (nothing to count
// against).
func (d *Counter) Claim(n int) (int, int64) {
	got, id := d.claim(n, false)
	return got, id
}

// ClaimAll is Claim, all or none: ok is false, with nothing claimed, if n
// isn't there; campaign messages then leave n of room for it for a while.
func (d *Counter) ClaimAll(n int) (id int64, ok bool) {
	got, id := d.claim(n, true)
	return id, got == max(n, 0)
}

func (d *Counter) claim(n int, all bool) (int, int64) {
	n = max(n, 0)
	if d.Left() < 0 { // logs that the limit's reached
		return n, 0
	}
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expire(now)
	left := max(d.limit-d.limit*d.reserve/100-d.total-d.claimed, 0)
	got := min(n, left)
	if all && got < n {
		if n <= min(d.limit-d.limit*d.reserve/100, holdMax) { // one that could ever fit
			d.want("", n, now)
		}
		got = 0
	} else if all {
		d.served(got)
	}
	if got == 0 {
		return 0, 0
	}
	if d.claims == nil {
		d.claims = map[int64]*claim{}
	}
	d.lastID++
	d.claims[d.lastID] = &claim{n: got, at: now}
	d.claimed += got
	return got, d.lastID
}

// Release gives back n of claim id's e-mails that weren't sent.
func (d *Counter) Release(id int64, n int) {
	now := time.Now()
	d.mu.Lock()
	d.unclaim(id, n, now)
	d.dispatch(now)
	d.mu.Unlock()
}

// demand is room that a claimant needs, for a while: refused claims' (no
// key: one, the largest of them, renewed by each), or an automation's (key,
// replaced as it's seen again).
type demand struct {
	key   string
	n     int
	until time.Time
}

const (
	demandTTL = 5 * time.Minute // how long campaign messages leave room for a demand
	holdMax   = 500             // the most room they leave
)

// Want sets the room that key (an automation) needs and didn't get, for a
// while; 0 clears it. Campaign messages leave that room.
func (d *Counter) Want(key string, n int) {
	now := time.Now()
	d.mu.Lock()
	d.want(key, n, now)
	d.dispatch(now)
	d.mu.Unlock()
}

// want records the room n for key ("": a refused claim's). Call with mu
// held.
func (d *Counter) want(key string, n int, now time.Time) {
	d.hold(now)
	if key == "" && n > 0 {
		for i := range d.demands {
			if x := &d.demands[i]; x.key == "" {
				x.n, x.until = max(x.n, n), now.Add(demandTTL)
				return
			}
		}
	}
	if key != "" {
		d.demands = slices.DeleteFunc(d.demands, func(x demand) bool { return x.key == key })
	}
	if n <= 0 {
		return
	}
	if len(d.demands) >= 1000 {
		d.demands = d.demands[1:]
	}
	d.demands = append(d.demands, demand{key: key, n: n, until: now.Add(demandTTL)})
}

// served takes n e-mails off the refused claims' demand: a claim got them.
// Call with mu held.
func (d *Counter) served(n int) {
	for i := 0; i < len(d.demands) && n > 0; {
		x := &d.demands[i]
		if x.key != "" {
			i++
			continue
		}
		k := min(n, x.n)
		x.n -= k
		n -= k
		if x.n == 0 {
			d.demands = slices.Delete(d.demands, i, i+1)
		} else {
			i++
		}
	}
}

// hold returns the room left for refused claims, dropping those that have
// expired. Call with mu held.
func (d *Counter) hold(now time.Time) int {
	sum := 0
	live := d.demands[:0]
	for _, x := range d.demands {
		if x.until.After(now) {
			live = append(live, x)
			sum += x.n
		}
	}
	d.demands = live
	return min(sum, holdMax)
}

// SetLimit sets the limit (the hub's setting; 0 for none) and the reserve,
// the percentage of it that campaigns and automations leave (0-100).
func (d *Counter) SetLimit(n, reservePct int) {
	d.mu.Lock()
	d.limit = max(n, 0)
	d.reserve = min(max(reservePct, 0), 100)
	d.dispatch(time.Now())
	d.mu.Unlock()
}

// Status is the count against the limit.
type Status struct {
	Sent          int
	Limit         int
	CampaignLimit int       // the limit less the reserve: campaigns' and automations'
	Reserve       int       // the reserve, in percent
	Left          int       // for campaigns, with a limit
	Held          int       // of Left, kept for refused claims and automations (Want)
	Claimed       int       // claimed by automations and transactional e-mails, not sent yet
	FreesAt       time.Time // when one more campaign message can go, if it's reached
}

// Status returns the count, the limit and, if campaigns' share of it is
// reached, when the next campaign message can go: when enough of the oldest
// minutes drop out of the 24 hours.
func (d *Counter) Status() Status {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expire(now)
	s := Status{Sent: d.total, Limit: d.limit, Reserve: d.reserve, Claimed: d.claimed}
	if d.limit == 0 {
		return s
	}
	s.CampaignLimit = d.limit - d.limit*d.reserve/100
	s.Left = max(s.CampaignLimit-d.total-d.claimed, 0)
	s.Held = min(d.hold(now), s.Left)
	if s.Left == 0 {
		s.FreesAt = d.freesAt(0)
	}
	return s
}

// freesAt returns when campaigns' share has room for one more, after hold
// e-mails more: when enough of the oldest minutes drop out; zero if they
// can't. Call with mu held.
func (d *Counter) freesAt(hold int) time.Time {
	need := d.total + d.claimed + hold - (d.limit - d.limit*d.reserve/100) + 1
	for _, m := range d.minutes {
		if need -= m.N; need <= 0 {
			return time.Unix((m.Minute+dayMinutes+1)*60, 0)
		}
	}
	return time.Time{}
}

// Left returns how many more campaign and automation e-mails may go now (-1
// without a limit), and logs when their share of the limit is reached and
// when sending goes on.
func (d *Counter) Left() int {
	s := d.Status()
	if s.Limit == 0 {
		return -1
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if s.Left == 0 && !d.waiting {
		d.waiting = true
		if d.center != "" {
			d.log.Printf("denma: center %s: %d e-mails sent in 24 hours and %d claimed, its daily cap of %d; its campaigns, automations and API sends wait until about %s",
				d.center, s.Sent, s.Claimed, s.Limit, s.FreesAt.Format("15:04"))
		} else if s.Claimed > 0 {
			d.log.Printf("denma: %d e-mails sent in 24 hours and %d claimed, the daily limit of %d less its %d%% reserve; campaigns, automations and transactional e-mails wait",
				s.Sent, s.Claimed, s.Limit, s.Reserve)
		} else {
			d.log.Printf("denma: %d e-mails sent in 24 hours, the daily limit of %d less its %d%% reserve; campaigns and automations wait until about %s",
				s.Sent, s.Limit, s.Reserve, s.FreesAt.Format("15:04"))
		}
	} else if s.Left > 0 && d.waiting {
		d.waiting = false
		if d.center != "" {
			d.log.Printf("denma: center %s: under its daily cap again; sending goes on", d.center)
		} else {
			d.log.Printf("denma: under the daily limit again; sending goes on")
		}
	}
	return s.Left
}

// pollEvery is how often a waiting campaign message checks that its
// campaign is still running.
const pollEvery = 2 * time.Second

// waiter is a campaign message waiting for room.
type waiter struct {
	center  string
	ready   chan struct{} // closed when it's let go
	granted bool
	minute  int64 // the minute it was counted in
}

// Wait blocks a campaign message of center until the limit lets it go, in
// its turn, counts it as sent, and returns true, with a function that gives
// the count back if the message isn't sent after all; or false once stopped()
// (its campaign was paused or stopped). Its send mustn't count it again
// (manager.DenmaDailyCounted).
func (d *Counter) Wait(center string, stopped func() bool) (bool, func()) {
	w := &waiter{center: center, ready: make(chan struct{})}
	d.mu.Lock()
	if d.queues == nil {
		d.queues = map[string][]*waiter{}
	}
	if len(d.queues[center]) == 0 {
		d.order = append(d.order, center)
	}
	d.queues[center] = append(d.queues[center], w)
	d.dispatch(time.Now())
	granted, resumed := w.granted, d.waiting
	d.mu.Unlock()
	if granted {
		if resumed {
			d.Left() // logs that sending goes on
		}
		return true, func() { d.giveBack(w.minute) }
	}
	d.Left() // logs that the limit's reached
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		select {
		case <-w.ready:
			if d.isWaiting() {
				d.Left() // logs that sending goes on
			}
			return true, func() { d.giveBack(w.minute) }
		case <-tick.C:
			if !stopped() {
				continue
			}
			d.mu.Lock()
			if w.granted {
				d.giveBackLocked(w.minute, time.Now())
			} else {
				d.dequeue(w)
			}
			d.mu.Unlock()
			return false, nil
		}
	}
}

func (d *Counter) isWaiting() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.waiting
}

// dequeue takes a waiter out of its queue. Call with mu held.
func (d *Counter) dequeue(w *waiter) {
	q := d.queues[w.center]
	for i, x := range q {
		if x == w {
			q = append(q[:i], q[i+1:]...)
			break
		}
	}
	if len(q) > 0 {
		d.queues[w.center] = q
		return
	}
	delete(d.queues, w.center)
	for i, c := range d.order {
		if c == w.center {
			d.order = append(d.order[:i], d.order[i+1:]...)
			if i < d.turn {
				d.turn--
			}
			break
		}
	}
}

// next takes the first waiter of the next center in turn. Call with mu held,
// and a waiter queued.
func (d *Counter) next() *waiter {
	if d.turn >= len(d.order) {
		d.turn = 0
	}
	c := d.order[d.turn]
	q := d.queues[c]
	w := q[0]
	if len(q) == 1 {
		delete(d.queues, c)
		d.order = append(d.order[:d.turn], d.order[d.turn+1:]...)
	} else {
		d.queues[c] = q[1:]
		d.turn++
	}
	return w
}

// dispatch lets waiting campaign messages go while there's room (always
// without a limit), one center's after another's, counting each, and sets
// the timer to wake the rest when room frees. Call with mu held.
func (d *Counter) dispatch(now time.Time) {
	d.expire(now)
	hold := d.hold(now)
	for len(d.order) > 0 {
		if d.limit > 0 && d.limit-d.limit*d.reserve/100-d.total-d.claimed-hold <= 0 {
			break
		}
		w := d.next()
		d.add(now)
		w.granted, w.minute = true, now.Unix()/60
		close(w.ready)
	}
	if len(d.order) == 0 {
		if d.timer != nil {
			d.timer.Stop()
		}
		return
	}
	wake := time.Minute // in case claims expire, or room frees otherwise
	if at := d.freesAt(hold); !at.IsZero() {
		wake = min(wake, max(time.Until(at), 10*time.Millisecond))
	}
	for _, x := range d.demands {
		wake = min(wake, max(time.Until(x.until), 10*time.Millisecond))
	}
	if d.timer == nil {
		d.timer = time.AfterFunc(wake, d.wake)
	} else {
		d.timer.Reset(wake)
	}
}

// wake is the timer's: room may have freed.
func (d *Counter) wake() {
	d.mu.Lock()
	d.dispatch(time.Now())
	d.mu.Unlock()
}

// giveBack takes back the count of a message let go (by Wait) and not sent.
func (d *Counter) giveBack(minute int64) {
	now := time.Now()
	d.mu.Lock()
	d.giveBackLocked(minute, now)
	d.mu.Unlock()
}

// giveBackLocked is giveBack, with mu held.
func (d *Counter) giveBackLocked(minute int64, now time.Time) {
	d.expire(now)
	for i := len(d.minutes) - 1; i >= 0; i-- {
		if d.minutes[i].Minute == minute {
			if d.minutes[i].N > 0 {
				d.minutes[i].N--
				d.total--
				if d.pending[minute]--; d.pending[minute] == 0 {
					delete(d.pending, minute)
				}
			}
			break
		}
	}
	d.dispatch(now)
}

// Counters is each center's Counter, against its own daily cap: one table
// (denma.center_sends) and one saver for them all, so that a center's count
// outlives its reloads.
type Counters struct {
	mu  sync.Mutex
	m   map[string]*Counter
	db  *sqlx.DB
	log *log.Logger
}

// NewCounters returns the centers' counters, logging to lg.
func NewCounters(lg *log.Logger) *Counters {
	return &Counters{m: map[string]*Counter{}, log: lg}
}

// Get returns a center's counter, made without a cap on first use.
func (cs *Counters) Get(center string) *Counter {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	c := cs.m[center]
	if c == nil {
		c = New(cs.log)
		c.center = center
		cs.m[center] = c
	}
	return c
}

// Init makes the counts' table, loads the last 24 hours', and saves new ones
// every flushEvery. Call it once.
func (cs *Counters) Init(db *sqlx.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS denma.center_sends (
		center TEXT NOT NULL,
		minute BIGINT NOT NULL, -- Unix minutes
		n      INTEGER NOT NULL,
		PRIMARY KEY (center, minute)
	)`); err != nil {
		return err
	}
	var rows []struct {
		Center string `db:"center"`
		Minute
	}
	if err := db.Select(&rows, `SELECT center, minute, n FROM denma.center_sends WHERE minute > $1 ORDER BY center, minute`,
		time.Now().Unix()/60-dayMinutes); err != nil {
		return err
	}
	for _, r := range rows {
		c := cs.Get(r.Center)
		c.mu.Lock()
		c.minutes = append(c.minutes, r.Minute)
		c.total += r.N
		c.mu.Unlock()
	}
	cs.mu.Lock()
	cs.db = db
	cs.mu.Unlock()
	go func() {
		for range time.Tick(flushEvery) {
			cs.Save()
		}
	}()
	return nil
}

// Save writes every center's counts made since the last save, and forgets
// old ones.
func (cs *Counters) Save() {
	cs.mu.Lock()
	db := cs.db
	list := make([]*Counter, 0, len(cs.m))
	for _, c := range cs.m {
		list = append(list, c)
	}
	cs.mu.Unlock()
	if db == nil {
		return
	}

	var (
		centers []string
		mins    []int64
		ns      []int
	)
	for _, c := range list {
		c.mu.Lock()
		for m, n := range c.pending {
			centers, mins, ns = append(centers, c.center), append(mins, m), append(ns, n)
		}
		c.pending = map[int64]int{}
		c.mu.Unlock()
	}
	if len(centers) == 0 {
		return
	}
	if _, err := db.Exec(`INSERT INTO denma.center_sends (center, minute, n) SELECT * FROM unnest($1::TEXT[], $2::BIGINT[], $3::INT[])
		ON CONFLICT (center, minute) DO UPDATE SET n = denma.center_sends.n + EXCLUDED.n`,
		pq.Array(centers), pq.Array(mins), pq.Array(ns)); err != nil {
		cs.log.Printf("denma: error saving the centers' daily send counts: %v", err)
		// Kept for the next save.
		for i, center := range centers {
			c := cs.Get(center)
			c.mu.Lock()
			c.pending[mins[i]] += ns[i]
			c.mu.Unlock()
		}
		return
	}
	if _, err := db.Exec(`DELETE FROM denma.center_sends WHERE minute <= $1`, time.Now().Unix()/60-dayMinutes-60); err != nil {
		cs.log.Printf("denma: error forgetting old center send counts: %v", err)
	}
}
