// Package denmadaily counts the e-mails sent in the last 24 hours against a
// daily limit, by the minute, in memory and in the database (cmd/denma_daily.go).
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
	minutes []Minute      // the last 24 hours', oldest first
	total   int           // their sum
	pending map[int64]int // counted since the last save
	waiting bool          // the limit was reached (logged once)
	db      *sqlx.DB
	log     *log.Logger
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

// expire drops the minutes older than 24 hours. Call with mu held.
func (d *Counter) expire(now time.Time) {
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

// SetLimit sets the limit (the hub's setting; 0 for none).
func (d *Counter) SetLimit(n int) {
	d.mu.Lock()
	d.limit = max(n, 0)
	d.mu.Unlock()
}

// Status is the count against the limit.
type Status struct {
	Sent    int
	Limit   int
	Left    int       // with a limit
	FreesAt time.Time // when one more can go, if it's reached
}

// Status returns the count, the limit and, if it's reached, when the next
// e-mail can go: when enough of the oldest minutes drop out of the 24 hours.
func (d *Counter) Status() Status {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expire(now)
	s := Status{Sent: d.total, Limit: d.limit}
	if d.limit == 0 {
		return s
	}
	s.Left = max(d.limit-d.total, 0)
	if s.Left == 0 {
		need := d.total - d.limit + 1
		for _, m := range d.minutes {
			if need -= m.N; need <= 0 {
				s.FreesAt = time.Unix((m.Minute+dayMinutes+1)*60, 0)
				break
			}
		}
	}
	return s
}

// Left returns how many more e-mails may go now (-1 without a limit), and
// logs when the limit is reached and when sending goes on.
func (d *Counter) Left() int {
	s := d.Status()
	if s.Limit == 0 {
		return -1
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if s.Left == 0 && !d.waiting {
		d.waiting = true
		d.log.Printf("denma: the daily limit of %d e-mails is reached; campaigns and automations wait until about %s",
			s.Limit, s.FreesAt.Format("15:04"))
	} else if s.Left > 0 && d.waiting {
		d.waiting = false
		d.log.Printf("denma: under the daily limit again; sending goes on")
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
