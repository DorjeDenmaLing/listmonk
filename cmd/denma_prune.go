package main

// denma: every hour, expired sign-in sessions are deleted in the hub and every
// running center, and e-mails sent more than denmaMessageDays ago from the
// centers' records (cmd/denma_messages.go). listmonk deletes a schema's
// expired sessions only when its app starts (internal/auth: its loop runs
// once).

import "time"

// denmaSessionTTL is how long a sign-in session lasts: simplesessions'
// default, which listmonk's session store uses.
const denmaSessionTTL = 24 * time.Hour

// pruneCenters deletes what's expired, every hour.
func (d *denmaCenters) pruneCenters() {
	time.Sleep(3 * time.Minute)
	for {
		d.pruneOnce()
		time.Sleep(time.Hour)
	}
}

// pruneOnce deletes expired sessions and old e-mail records, in the hub and
// each running center.
func (d *denmaCenters) pruneOnce() {
	apps := []*App{}
	if hub := d.current(); hub != nil {
		apps = append(apps, hub)
	}
	for _, c := range d.loaded() {
		if c.app != nil {
			apps = append(apps, c.app)
		}
	}
	var sessions, messages int64
	for _, a := range apps {
		res, err := a.db.Exec(`DELETE FROM sessions WHERE created_at <= NOW() - make_interval(secs => $1)`, denmaSessionTTL.Seconds())
		if err != nil {
			a.log.Printf("denma: error deleting expired sessions: %v", err)
			continue
		}
		n, _ := res.RowsAffected()
		sessions += n
		if a.ko.String("denma.center") == "" {
			continue
		}
		res, err = a.db.Exec(`DELETE FROM denma_messages WHERE sent_at < NOW() - make_interval(days => $1)`, denmaMessageDays)
		if err != nil {
			a.log.Printf("denma: error deleting old records of sent e-mails: %v", err)
			continue
		}
		n, _ = res.RowsAffected()
		messages += n
	}
	if sessions > 0 || messages > 0 {
		lo.Printf("denma: deleted %d expired sessions and %d records of e-mails sent over %d days ago", sessions, messages, denmaMessageDays)
	}
}
