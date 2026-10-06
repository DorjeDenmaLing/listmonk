package main

// denma: bounces for all centers, taken by the hub. Every center sends
// through the same mail provider (the hub's settings), which reports bounces
// to one place: one SES/SNS topic, webhook URL or bounce mailbox. So only the
// hub has a bounce manager (cmd/main.go): it receives the webhooks, at the
// same /webhooks/service/<service> as a single install (a center's
// /c/<slug>/webhooks/service/… is passed to it too), and is the only one to
// scan the mailbox. Each bounce is recorded in the center whose mail it was:
//
//  1. the center with the bounce's campaign (its X-Listmonk-Campaign header;
//     SES includes headers only if the identity's notifications are set to
//     include the original headers);
//  2. else the one with its subscriber (X-Listmonk-Subscriber, from mailbox
//     bounces);
//  3. else the only center with the address.
//
// An address in several centers with neither header can't be placed: a hard
// bounce goes on the shared blocklist (the address can't receive mail from
// any center, cmd/denma_blocklist.go); a soft bounce or a complaint is only
// logged, as it may concern one center's mail only.
//
// The provider gives each bounce once (the webhook has answered before it's
// recorded), so one that can't be recorded now, because its center isn't
// running (still starting after a restart, disabled, or failed to load) or
// the database failed, is kept in denma.pending_bounces and recorded later
// (retryBounces): when its center has loaded, and every minute. A complaint
// lost would leave someone who reported spam able to be mailed again.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

// denmaBounceCB is the hub's bounce recorder: its own (own) on a single
// install, else the router.
func denmaBounceCB(own func(models.Bounce) error, ko *koanf.Koanf) func(models.Bounce) error {
	if !ko.Bool("denma.multi_center") {
		return own
	}
	return func(b models.Bounce) error {
		denmaBounceSeen() // for System (cmd/denma_system.go)
		if denmaHub == nil {
			return fmt.Errorf("bounce for %s before the centers started", b.Email)
		}
		return denmaHub.routeBounce(b)
	}
}

// denmaBounceCampaigns caches which center each bounced campaign is in.
var denmaBounceCampaigns sync.Map // campaign UUID -> slug

// errDenmaNotRunning is a bounce's center not running.
var errDenmaNotRunning = errors.New("its center isn't running")

// routeBounce records a bounce in its center, or keeps it to record later.
func (d *denmaCenters) routeBounce(b models.Bounce) error {
	b.Email = strings.ToLower(strings.TrimSpace(b.Email))
	slug, err := d.recordBounce(b)
	if err != nil {
		d.keepBounce(b, slug, err)
	}
	return nil
}

// recordBounce records a bounce in its center. If it can't be now, it returns
// why, and the center if it's known.
func (d *denmaCenters) recordBounce(b models.Bounce) (string, error) {
	slugs, err := d.bounceCenters(b)
	if err != nil {
		return "", fmt.Errorf("finding its center: %w", err)
	}
	switch {
	case len(slugs) == 1:
		return slugs[0], d.recordIn(slugs[0], b)
	case len(slugs) == 0:
		lo.Printf("denma: %s bounce for %s, which no center has: not recorded", b.Type, b.Email)
	case b.Type == models.BounceTypeHard:
		lo.Printf("denma: hard bounce for %s, which %d centers have (no campaign or subscriber to tell whose mail it was): blocklisted in all", b.Email, len(slugs))
		denmaBlockEverywhere(b.Email, "hard bounce", "")
	default:
		lo.Printf("denma: %s bounce for %s, which %d centers have (no campaign or subscriber to tell whose mail it was): not recorded", b.Type, b.Email, len(slugs))
	}
	return "", nil
}

// recordIn records a bounce in a center, if it's running. A bounce listmonk
// refuses (a type it doesn't know) is logged and not kept.
func (d *denmaCenters) recordIn(slug string, b models.Bounce) error {
	ctr := d.get(slug)
	if ctr == nil {
		return errDenmaNotRunning
	}
	err := ctr.app.core.RecordBounce(b)
	var httpErr *echo.HTTPError
	if errors.As(err, &httpErr) {
		lo.Printf("denma: %s bounce for %s in center %s not recorded: %v", b.Type, b.Email, slug, err)
		return nil
	}
	return err
}

// denmaInitPendingBounces creates the table of bounces kept to record later.
func denmaInitPendingBounces(db *sqlx.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS denma.pending_bounces (
		id         BIGSERIAL PRIMARY KEY,
		center     TEXT NOT NULL DEFAULT '', -- its center's slug; '' if not found yet
		bounce     JSONB NOT NULL,           -- models.Bounce
		tries      INTEGER NOT NULL DEFAULT 0,
		last_error TEXT NOT NULL DEFAULT '',
		next_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
		created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
	)`)
	return err
}

// keepBounce keeps a bounce that couldn't be recorded (why), for slug's
// center if it's known, to record later.
func (d *denmaCenters) keepBounce(b models.Bounce, slug string, why error) {
	body, err := json.Marshal(b)
	if err == nil {
		_, err = d.current().db.Exec(`INSERT INTO denma.pending_bounces (center, bounce, last_error) VALUES ($1, $2, $3)`, slug, body, why.Error())
	}
	if err != nil {
		lo.Printf("denma: %s bounce for %s couldn't be recorded (%v) or kept: %v", b.Type, b.Email, why, err)
		return
	}
	lo.Printf("denma: %s bounce for %s not recorded yet (%v): kept, to be recorded later", b.Type, b.Email, why)
}

// denmaBounceRetries runs one retryBounces at a time.
var denmaBounceRetries sync.Mutex

// retryBounces records the kept bounces that can be now: those due (tried
// again with a growing wait if the database fails), and all of a center that
// has started. Those for a center that isn't running wait for it.
func (d *denmaCenters) retryBounces() {
	denmaBounceRetries.Lock()
	defer denmaBounceRetries.Unlock()

	running := []string{}
	for _, c := range d.loaded() {
		running = append(running, c.Slug)
	}
	var rows []struct {
		ID     int64           `db:"id"`
		Center string          `db:"center"`
		Bounce json.RawMessage `db:"bounce"`
		Tries  int             `db:"tries"`
	}
	db := d.current().db
	if err := db.Select(&rows, `SELECT id, center, bounce, tries FROM denma.pending_bounces
		WHERE next_at <= NOW() AND (center = '' OR center = ANY($1)) ORDER BY id LIMIT 500`, pq.Array(running)); err != nil {
		lo.Printf("denma: error reading the bounces kept to record: %v", err)
		return
	}
	for _, r := range rows {
		var b models.Bounce
		if err := json.Unmarshal(r.Bounce, &b); err != nil {
			lo.Printf("denma: kept bounce %d is unreadable, dropped: %v", r.ID, err)
			_, _ = db.Exec(`DELETE FROM denma.pending_bounces WHERE id = $1`, r.ID)
			continue
		}
		slug, err := r.Center, error(nil)
		if slug != "" {
			err = d.recordIn(slug, b)
		} else {
			slug, err = d.recordBounce(b)
		}
		if err == nil {
			_, err = db.Exec(`DELETE FROM denma.pending_bounces WHERE id = $1`, r.ID)
			if err == nil {
				lo.Printf("denma: kept %s bounce for %s recorded", b.Type, b.Email)
			}
			continue
		}
		if errors.Is(err, errDenmaNotRunning) {
			_, err = db.Exec(`UPDATE denma.pending_bounces SET center = $2 WHERE id = $1`, r.ID, slug)
		} else {
			lo.Printf("denma: kept %s bounce for %s still not recorded (try %d): %v", b.Type, b.Email, r.Tries+1, err)
			_, err = db.Exec(`UPDATE denma.pending_bounces SET tries = tries + 1, last_error = $2,
				next_at = NOW() + LEAST(POWER(2, tries), 60) * INTERVAL '1 minute' WHERE id = $1`, r.ID, err.Error())
		}
		if err != nil {
			lo.Printf("denma: error updating kept bounce %d: %v", r.ID, err)
		}
	}
}

// watchPendingBounces records kept bounces every minute.
func (d *denmaCenters) watchPendingBounces() {
	for range time.Tick(time.Minute) {
		d.retryBounces()
	}
}

// bounceCenters returns the slugs of the centers the bounce may be for: the
// one with its campaign, else its subscriber, else every one with its address.
func (d *denmaCenters) bounceCenters(b models.Bounce) ([]string, error) {
	if id, err := uuid.FromString(b.CampaignUUID); err == nil {
		if s, ok := denmaBounceCampaigns.Load(id.String()); ok {
			return []string{s.(string)}, nil
		}
		slugs, err := d.findInCenters(`SELECT %s FROM %s.campaigns WHERE uuid = $1::UUID`, id.String())
		if err != nil || len(slugs) == 1 {
			if len(slugs) == 1 {
				denmaBounceCampaigns.Store(id.String(), slugs[0])
			}
			return slugs, err
		}
	}
	if id, err := uuid.FromString(b.SubscriberUUID); err == nil {
		slugs, err := d.findInCenters(`SELECT %s FROM %s.subscribers WHERE uuid = $1::UUID`, id.String())
		if err != nil || len(slugs) == 1 {
			return slugs, err
		}
	}
	if b.Email == "" {
		return nil, nil
	}
	return d.findInCenters(`SELECT %s FROM %s.subscribers WHERE LOWER(email) = $1`, b.Email)
}

// findInCenters runs a query (with %s for the slug literal and the schema)
// in every registered center at once, running or not, and returns the slugs
// of those where it finds a row.
func (d *denmaCenters) findInCenters(q, arg string) ([]string, error) {
	db := d.current().db
	var cs []struct {
		Slug   string `db:"slug"`
		Schema string `db:"schema_name"`
	}
	// Only schemas with listmonk's tables (a center that failed to install
	// may have none).
	if err := db.Select(&cs, `SELECT slug, schema_name FROM denma.centers
		WHERE to_regclass(quote_ident(schema_name) || '.campaigns') IS NOT NULL
		AND to_regclass(quote_ident(schema_name) || '.subscribers') IS NOT NULL ORDER BY id`); err != nil {
		return nil, err
	}
	if len(cs) == 0 {
		return nil, nil
	}
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = "(" + fmt.Sprintf(q, pq.QuoteLiteral(c.Slug), pq.QuoteIdentifier(c.Schema)) + " LIMIT 1)"
	}
	var slugs []string
	if err := db.Select(&slugs, strings.Join(parts, " UNION ALL "), arg); err != nil {
		return nil, err
	}
	return slugs, nil
}
