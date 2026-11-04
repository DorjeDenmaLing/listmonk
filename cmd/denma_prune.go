package main

// denma: every hour, expired sessions and old e-mail records are deleted in the hub and every center.

import (
	"context"
	"database/sql"
	"sort"
	"time"
)

// denmaSessionTTL is how long a sign-in session lasts (simplesessions' default).
const denmaSessionTTL = 24 * time.Hour

// pruneCenters deletes what's expired, every hour.
func (d *denmaCenters) pruneCenters() {
	time.Sleep(3 * time.Minute)
	for {
		d.pruneOnce()
		time.Sleep(time.Hour)
	}
}

// denmaPruneTimeout is how long one app's pruning may take before the next one's starts.
const denmaPruneTimeout = 2 * time.Minute

// denmaExecer is the database as pruning uses it.
type denmaExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// pruneOnce deletes expired sessions and old e-mail records in the hub and each running center.
func (d *denmaCenters) pruneOnce() {
	apps := []*App{}
	if hub := d.current(); hub != nil {
		apps = append(apps, hub)
	}
	loaded := d.loaded()
	sort.Slice(loaded, func(i, j int) bool { return loaded[i].Slug < loaded[j].Slug })
	for _, c := range loaded {
		if c.app != nil {
			apps = append(apps, c.app)
		}
	}
	var sessions, messages int64
	for _, a := range apps {
		s, m := denmaPruneDB(a.db, a.ko.String("denma.center") != "", denmaPruneTimeout, a.log.Printf)
		sessions += s
		messages += m
	}
	if sessions > 0 || messages > 0 {
		lo.Printf("denma: deleted %d expired sessions and %d records of e-mails sent over %d days ago", sessions, messages, denmaMessageDays)
	}
}

// denmaPruneDB prunes one app's database, giving up after timeout; it returns the counts deleted.
func denmaPruneDB(db denmaExecer, center bool, timeout time.Duration, logf func(string, ...any)) (sessions, messages int64) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	res, err := db.ExecContext(ctx, `DELETE FROM sessions WHERE created_at <= NOW() - make_interval(secs => $1)`, denmaSessionTTL.Seconds())
	if err != nil {
		logf("denma: error deleting expired sessions: %v", err)
		return 0, 0
	}
	sessions, _ = res.RowsAffected()
	if !center {
		return sessions, 0
	}
	res, err = db.ExecContext(ctx, `DELETE FROM denma_messages WHERE sent_at < NOW() - make_interval(days => $1)`, denmaMessageDays)
	if err != nil {
		logf("denma: error deleting old records of sent e-mails: %v", err)
		return sessions, 0
	}
	messages, _ = res.RowsAffected()
	if _, err := db.ExecContext(ctx, `DELETE FROM denma_forgotten_sends WHERE sent_at < NOW() - make_interval(days => $1)`, denmaMessageDays); err != nil {
		logf("denma: error deleting old records of deleted subscribers' e-mails: %v", err)
	}
	if _, err := denmaPruneVersions(ctx, db); err != nil { // cmd/denma_versions.go
		logf("denma: error deleting old versions: %v", err)
	}
	return sessions, messages
}
