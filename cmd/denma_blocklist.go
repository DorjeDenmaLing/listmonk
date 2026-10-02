package main

// denma: one blocklist for every center, for addresses that can't receive
// mail. When an address hard-bounces in a center, or fails the domain check
// (cmd/denma_emailcheck.go), it's added to denma.blocked_emails and
// blocklisted in every center that has it, with the reason and time in the
// subscriber's auto_blocklist_reason and auto_blocklist_time attributes. A
// center that gets the address later (a sign-up, an import) blocklists it
// too, before any opt-in.
//
// Hard bounces are found by each center's per-minute e-mail check job:
// those recorded in the last two days, and older ones whose subscriber is
// still blocklisted (so an address a center deliberately unblocked after an
// old bounce stays unblocked there). Complaints aren't shared: someone who
// marks one center's mail as spam may still want another's.
//
// Multi-center mode only; otherwise a center's own blocklist is the only one.

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
)

// denmaBlockEverywhere adds an address to the shared blocklist and
// blocklists it in every center (including the one that found it). It does
// nothing if the address is already on it. The reason is the same in every
// center and doesn't name one (the shared list records which found it).
func denmaBlockEverywhere(email, reason, slug string) {
	if denmaHub == nil {
		return
	}
	email = strings.ToLower(strings.TrimSpace(email))
	db := denmaHub.current().db

	var added bool
	if err := db.Get(&added, `INSERT INTO denma.blocked_emails (email, reason, center) VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING RETURNING true`, email, reason, slug); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			lo.Printf("denma: error adding %s to the shared blocklist: %v", email, err)
		}
		return
	}

	var schemas []string
	if err := db.Select(&schemas, `SELECT schema_name FROM denma.centers ORDER BY slug`); err != nil {
		lo.Printf("denma: error getting the centers to blocklist %s in: %v", email, err)
		return
	}
	n := 0
	for _, s := range schemas {
		res, err := db.Exec(denmaBlocklistSQL(pq.QuoteIdentifier(s)+"."), email, denmaReasonAttr(reason))
		if err != nil {
			lo.Printf("denma: error blocklisting %s in %s: %v", email, s, err)
			continue
		}
		if c, _ := res.RowsAffected(); c > 0 {
			n++
		}
	}
	lo.Printf("denma: blocklisted %s for every center (%s); it was subscribed in %d", email, reason, n)
}

// denmaSharedBlock is the shared blocklist's reason for an address, if it's on it.
func denmaSharedBlock(db *sqlx.DB, email string) (string, bool) {
	if denmaHub == nil {
		return "", false
	}
	var reason string
	if err := db.Get(&reason, `SELECT reason FROM denma.blocked_emails WHERE email = LOWER($1)`, email); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			lo.Printf("denma: error checking the shared blocklist for %s: %v", email, err)
		}
		return "", false
	}
	return reason, true
}

// shareHardBounces adds the center's hard-bounced addresses to the shared
// blocklist. Run by the e-mail check job.
func (a *App) shareHardBounces() {
	if denmaHub == nil {
		return
	}
	var emails []string
	if err := a.db.Select(&emails, `SELECT DISTINCT LOWER(s.email) FROM bounces b
		JOIN subscribers s ON s.id = b.subscriber_id
		WHERE b.type = 'hard' AND (s.status = 'blocklisted' OR b.created_at > NOW() - INTERVAL '2 days')
			AND NOT EXISTS (SELECT 1 FROM denma.blocked_emails g WHERE g.email = LOWER(s.email))
		LIMIT 200`); err != nil {
		a.log.Printf("denma: error getting hard bounces: %v", err)
		return
	}
	for _, e := range emails {
		denmaBlockEverywhere(e, "hard bounce", a.ko.String("denma.center"))
	}
}

// applySharedBlocks blocklists the center's subscribers added in the last
// day whose address is on the shared blocklist (once each; applied
// remembers them). Run by the e-mail check job.
func (a *App) applySharedBlocks(applied map[int]time.Time) {
	if denmaHub == nil {
		return
	}
	var subs []struct {
		ID     int    `db:"id"`
		Email  string `db:"email"`
		Reason string `db:"reason"`
	}
	if err := a.db.Select(&subs, `SELECT s.id, s.email, g.reason FROM subscribers s
		JOIN denma.blocked_emails g ON g.email = LOWER(s.email)
		WHERE s.status <> 'blocklisted' AND s.created_at > NOW() - INTERVAL '1 day'`); err != nil {
		a.log.Printf("denma: error checking new subscribers against the shared blocklist: %v", err)
		return
	}
	for _, s := range subs {
		if _, ok := applied[s.ID]; ok {
			continue // unblocked here since; leave it
		}
		applied[s.ID] = time.Now()
		a.log.Printf("denma: blocklisting %s: on the shared blocklist (%s)", s.Email, s.Reason)
		if _, err := a.db.Exec(denmaBlocklistSQL(""), s.Email, denmaReasonAttr(s.Reason)); err != nil {
			a.log.Printf("denma: error blocklisting %s: %v", s.Email, err)
		}
	}
}
