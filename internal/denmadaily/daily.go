// Package denmadaily counts the e-mails sent in the last 24 hours against a
// daily limit, by the minute, in memory and in the database (cmd/denma_daily.go).
// Campaigns and automations stop short of the limit by a reserve (a
// percentage of it), which is left for opt-in confirmations, password resets,
// invites and notifications.
//
// Campaign messages each wait for their turn (Wait). Automations send a
// batch at once, so they claim their share of what's left first (Claim): it
// counts against the limit straight away, so that automations in several
// centers running in the same minute can't each take the same room. Each
// claimed e-mail sent turns a claim into a send (AddClaimed); what an
// automation claimed but didn't send it gives back (Release). Claims are
// in memory only, and a claim left untouched for claimTimeout is dropped,
// in case e-mails claimed were never sent.
//
// Each center also has a Counter of its own (Counters), against its own
// daily cap, which a superadmin sets on its Config page; it has no reserve.
package denmadaily

import (
	"log"
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
	reserve int           // percent of limit that campaigns leave
	minutes []Minute      // the last 24 hours', oldest first
	total   int           // their sum
	pending map[int64]int // counted since the last save
	waiting bool          // the limit was reached (logged once)
	claimed int           // claimed by automations and transactional e-mails, not sent yet
	claimAt time.Time     // when claims last changed
	db      *sqlx.DB
	log     *log.Logger
	center  string // a center's counter (Counters); "" for the hub's
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

// claimTimeout is how long claims last untouched before they're dropped.
const claimTimeout = time.Hour

// expire drops the minutes older than 24 hours, and claims left untouched
// for claimTimeout. Call with mu held.
func (d *Counter) expire(now time.Time) {
	if d.claimed > 0 && now.Sub(d.claimAt) > claimTimeout {
		d.log.Printf("denma: dropping %d e-mails claimed an hour ago and not sent", d.claimed)
		d.claimed = 0
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
	m := now.Unix() / 60
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expire(now)
	if n := len(d.minutes); n > 0 && d.minutes[n-1].Minute == m {
		d.minutes[n-1].N++
	} else {
		d.minutes = append(d.minutes, Minute{Minute: m, N: 1})
	}
	d.total++
	d.pending[m]++
}

// AddClaimed counts an e-mail sent that an automation had claimed.
func (d *Counter) AddClaimed() {
	d.Add()
	d.mu.Lock()
	if d.claimed > 0 {
		d.claimed--
		d.claimAt = time.Now()
	}
	d.mu.Unlock()
}

// Claim takes up to n of what's left for campaigns and automations, for an
// automation's batch, and returns how many it got, and whether they're
// claimed: without a limit, all n, unclaimed (nothing to count against).
func (d *Counter) Claim(n int) (int, bool) {
	if d.Left() < 0 {
		return n, false
	}
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expire(now)
	left := max(d.limit-d.limit*d.reserve/100-d.total-d.claimed, 0)
	got := min(max(n, 0), left)
	if got > 0 {
		d.claimed += got
		d.claimAt = now
	}
	return got, true
}

// Release gives back n claimed e-mails that weren't sent.
func (d *Counter) Release(n int) {
	if n <= 0 {
		return
	}
	d.mu.Lock()
	d.claimed = max(d.claimed-n, 0)
	d.claimAt = time.Now()
	d.mu.Unlock()
}

// SetLimit sets the limit (the hub's setting; 0 for none) and the reserve,
// the percentage of it that campaigns and automations leave (0-100).
func (d *Counter) SetLimit(n, reservePct int) {
	d.mu.Lock()
	d.limit = max(n, 0)
	d.reserve = min(max(reservePct, 0), 100)
	d.mu.Unlock()
}

// Status is the count against the limit.
type Status struct {
	Sent          int
	Limit         int
	CampaignLimit int       // the limit less the reserve: campaigns' and automations'
	Reserve       int       // the reserve, in percent
	Left          int       // for campaigns, with a limit
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
	if s.Left == 0 {
		need := d.total + d.claimed - s.CampaignLimit + 1
		for _, m := range d.minutes {
			if need -= m.N; need <= 0 {
				s.FreesAt = time.Unix((m.Minute+dayMinutes+1)*60, 0)
				break
			}
		}
	}
	return s
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

// pollEvery is how often a waiting campaign message checks again.
const pollEvery = 2 * time.Second

// Wait blocks a campaign message until the limit lets it go, and returns
// true; or false once stopped() (its campaign was paused or stopped).
func (d *Counter) Wait(stopped func() bool) bool {
	for {
		if d.Left() != 0 {
			return true
		}
		if stopped() {
			return false
		}
		wait := pollEvery
		if s := d.Status(); !s.FreesAt.IsZero() {
			wait = min(wait, max(time.Until(s.FreesAt), 10*time.Millisecond))
		}
		time.Sleep(wait)
	}
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
