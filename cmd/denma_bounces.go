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
// A complaint lost would leave someone who reported spam able to be mailed
// again, so every bounce is kept in denma.pending_bounces before it's
// recorded: a webhook's before the provider is answered (a 500 if it can't
// be, so the provider sends it again), and the mailbox's as it's read. Each
// is recorded at once (retryBounces); one that can't be, because its center
// isn't running (still starting after a restart, disabled, or failed to
// load) or the database failed, stays kept and is recorded when its center
// has loaded, or every minute.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/internal/bounce/webhooks"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

// denmaBounceCB is the hub's bounce recorder: its own (own) on a single
// install, else one that keeps each bounce (the mailbox's; webhooks' are kept
// by denmaKeepBounces), trying until it can.
func denmaBounceCB(own func(models.Bounce) error, ko *koanf.Koanf, db *sqlx.DB) func(models.Bounce) error {
	webhooks.DenmaSESTopicAllowed = func(arn string) bool { return denmaSESTopicAllowed(ko, arn) }
	if !ko.Bool("denma.multi_center") {
		return func(b models.Bounce) error { return denmaRecordBounce(db, b, own) } // cmd/denma_auto_reports.go
	}
	return func(b models.Bounce) error {
		denmaBouncesKeeping.Add(1)
		defer denmaBouncesKeeping.Add(-1)
		denmaBounceSeen() // for System (cmd/denma_system.go)
		for wait := time.Second; ; wait = min(2*wait, time.Minute) {
			err := denmaKeep(db, []models.Bounce{b})
			if err == nil {
				return nil
			}
			lo.Printf("denma: error keeping a %s bounce to record, trying again in %v: %v", b.Type, wait, err)
			time.Sleep(wait)
		}
	}
}

// denmaBouncesKeeping counts the mailbox's bounces being kept, for shutdown.
var denmaBouncesKeeping atomic.Int64

// denmaKeepBounces keeps a webhook's bounces before the provider is answered,
// with centers: a 500 if they can't be, so the provider sends them again.
// Kept, they're taken from bounces, so listmonk doesn't queue them too.
func (a *App) denmaKeepBounces(bounces *[]models.Bounce) error {
	if !a.ko.Bool("denma.multi_center") || len(*bounces) == 0 {
		return nil
	}
	for range *bounces {
		denmaBounceSeen() // for System (cmd/denma_system.go)
	}
	if err := denmaKeep(a.db, *bounces); err != nil {
		a.log.Printf("denma: error keeping %d bounces to record: %v", len(*bounces), err)
		return echo.NewHTTPError(http.StatusInternalServerError, a.i18n.T("globals.messages.internalError"))
	}
	*bounces = nil
	return nil
}

// denmaKeep keeps bounces in denma.pending_bounces and has them recorded.
func denmaKeep(db *sqlx.DB, bs []models.Bounce) error {
	for i := range bs {
		bs[i].Email = strings.ToLower(strings.TrimSpace(bs[i].Email))
	}
	body, err := json.Marshal(bs)
	if err != nil {
		return err
	}
	if _, err := db.Exec(`INSERT INTO denma.pending_bounces (bounce) SELECT b FROM jsonb_array_elements($1) b`, body); err != nil {
		return err
	}
	denmaBounceKick()
	return nil
}

// denmaBounceKicks wakes watchPendingBounces when bounces are kept.
var denmaBounceKicks = make(chan struct{}, 1)

func denmaBounceKick() {
	select {
	case denmaBounceKicks <- struct{}{}:
	default:
	}
}

// denmaDrainBounces waits for the mailbox's bounces read to be kept, within
// a time limit, so none is lost at shutdown (they've been deleted from the
// mailbox). Idle is seen twice, as a bounce is briefly neither queued nor
// being kept.
func denmaDrainBounces(app *App, within time.Duration) bool {
	if denmaHub != nil {
		app = denmaHub.current()
	}
	if app == nil || app.bounce == nil {
		return true
	}
	idle := 0
	for end := time.Now().Add(within); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if app.bounce.DenmaQueued() > 0 || denmaBouncesKeeping.Load() > 0 {
			idle = 0
		} else if idle++; idle == 2 {
			return true
		}
	}
	return false
}

// denmaSESTopics are the SNS topics SES notifications are taken from
// (internal/bounce/webhooks/denma_ses.go): denma.ses_topics in config.toml,
// and those SES sends the hub's sender's domain's and the sending domains'
// bounces and complaints to. An unknown topic looks again, at most every 10 s.
var denmaSESTopics struct {
	sync.Mutex
	set map[string]bool
	at  time.Time
}

func denmaSESTopicAllowed(ko *koanf.Koanf, arn string) bool {
	c := &denmaSESTopics
	c.Lock()
	defer c.Unlock()
	if arn == "" || (c.set[arn] && time.Since(c.at) < 10*time.Minute) || time.Since(c.at) < 10*time.Second {
		return arn != "" && c.set[arn]
	}

	set := map[string]bool{}
	for _, t := range ko.Strings("denma.ses_topics") {
		set[strings.TrimSpace(t)] = true
	}
	if denmaHub != nil {
		var stored []string
		if err := denmaHub.db().Select(&stored, `SELECT DISTINCT t FROM denma.sending_domains,
			LATERAL (VALUES (state->'ses'->>'bounce_topic'), (state->'ses'->>'complaint_topic')) v(t) WHERE t <> ''`); err != nil {
			denmaHub.current().log.Printf("SES topics: the sending domains: %v", err)
		}
		for _, t := range stored {
			set[t] = true
		}
		if s, _ := denmaHub.ses(); s != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if ref := denmaHub.domainRef(ctx, s); ref.Topic != "" {
				set[ref.Topic] = true
			} else {
				// SES couldn't say: keep what it said before.
				for t := range c.set {
					set[t] = true
				}
			}
			cancel()
		}
	}
	delete(set, "")
	c.set, c.at = set, time.Now()
	return set[arn]
}

// denmaCenterCache remembers which center a UUID is in, up to a limit (it's
// emptied when full).
type denmaCenterCache struct {
	mu sync.Mutex
	m  map[string]string
}

// denmaCenterCacheMax is how many UUIDs a denmaCenterCache keeps.
const denmaCenterCacheMax = 20000

func (c *denmaCenterCache) get(id string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.m[id]
	return s, ok
}

func (c *denmaCenterCache) put(id, slug string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= denmaCenterCacheMax {
		c.m = map[string]string{}
	}
	c.m[id] = slug
}

func (c *denmaCenterCache) drop(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, id)
}

// The center each bounced campaign, and each subscriber, is in.
var (
	denmaBounceCampaigns   denmaCenterCache
	denmaBounceSubscribers denmaCenterCache
)

// errDenmaNotRunning is a bounce's center not running.
var errDenmaNotRunning = errors.New("its center isn't running")

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
	if b.Type == models.BounceTypeComplaint {
		if err := denmaComplaintOptOut(ctr.app.db, b); err != nil {
			return err
		}
	}
	err := denmaRecordBounce(ctr.app.db, b, ctr.app.core.RecordBounce) // and its automation's (cmd/denma_auto_reports.go)
	var httpErr *echo.HTTPError
	if errors.As(err, &httpErr) {
		lo.Printf("denma: %s bounce for %s in center %s not recorded: %v", b.Type, b.Email, slug, err)
		return nil
	}
	return err
}

// denmaComplaintOptOut makes a complaint final in a center, by the address
// (its subscriber's, and the one complaining), before listmonk records it:
// its "delete" action would leave no subscriber for denma_record_complaint
// (cmd/denma_optouts.go) to find, as would one deleted before, and an
// unsubscribe's opt-out becomes a complaint's.
func denmaComplaintOptOut(db *sqlx.DB, b models.Bounce) error {
	_, err := db.Exec(`INSERT INTO denma_optouts (email_hash, canon_hash, kind, source)
		SELECT DISTINCT ON (denma_email_hash(e)) denma_email_hash(e), denma_canon_hash(e), 'complained', 'complaint' FROM (
			SELECT email FROM subscribers WHERE $1 <> '' AND uuid::TEXT = $1
			UNION ALL SELECT $2 WHERE $2 <> '') x(e)
		ON CONFLICT (email_hash) DO UPDATE SET kind = 'complained', source = 'complaint', created_at = NOW()`,
		b.SubscriberUUID, b.Email)
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

// denmaBounceRetries runs one retryBounces at a time.
var denmaBounceRetries sync.Mutex

// denmaBounceBatch is how many kept bounces are read at a time.
const denmaBounceBatch = 500

// retryBounces records the kept bounces that can be now: those due (tried
// again with a growing wait if the database fails), and all of a center that
// has started. Those for a center that isn't running wait for it. Only a
// bounce that waited is logged as recorded.
func (d *denmaCenters) retryBounces() {
	denmaBounceRetries.Lock()
	defer denmaBounceRetries.Unlock()
	// A burst is read in batches; at most 20 at a time, the rest next time.
	for i := 0; i < 20 && d.retryBatch() == denmaBounceBatch; i++ {
	}
}

// retryBatch records a batch of kept bounces, and returns how many it read,
// or -1 if a recorded one couldn't be deleted (it would be recorded again).
func (d *denmaCenters) retryBatch() int {
	running := []string{}
	for _, c := range d.loaded() {
		running = append(running, c.Slug)
	}
	var rows []struct {
		ID        int64           `db:"id"`
		Center    string          `db:"center"`
		Bounce    json.RawMessage `db:"bounce"`
		Tries     int             `db:"tries"`
		LastError string          `db:"last_error"`
	}
	db := d.current().db
	if err := db.Select(&rows, `SELECT id, center, bounce, tries, last_error FROM denma.pending_bounces
		WHERE next_at <= NOW() AND (center = '' OR center = ANY($1)) ORDER BY id LIMIT $2`, pq.Array(running), denmaBounceBatch); err != nil {
		lo.Printf("denma: error reading the bounces kept to record: %v", err)
		return 0
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
			if _, err := db.Exec(`DELETE FROM denma.pending_bounces WHERE id = $1`, r.ID); err != nil {
				lo.Printf("denma: error deleting kept bounce %d, recorded: %v", r.ID, err)
				return -1
			}
			if r.LastError != "" {
				lo.Printf("denma: kept %s bounce for %s recorded", b.Type, b.Email)
			}
			continue
		}
		if r.LastError == "" {
			lo.Printf("denma: %s bounce for %s not recorded yet (%v): kept, to be recorded later", b.Type, b.Email, err)
		}
		if errors.Is(err, errDenmaNotRunning) {
			_, err = db.Exec(`UPDATE denma.pending_bounces SET center = $2, last_error = $3 WHERE id = $1`, r.ID, slug, err.Error())
		} else {
			if r.LastError != "" {
				lo.Printf("denma: kept %s bounce for %s still not recorded (try %d): %v", b.Type, b.Email, r.Tries+1, err)
			}
			_, err = db.Exec(`UPDATE denma.pending_bounces SET tries = tries + 1, last_error = $2,
				next_at = NOW() + LEAST(POWER(2, tries), 60) * INTERVAL '1 minute' WHERE id = $1`, r.ID, err.Error())
		}
		if err != nil {
			lo.Printf("denma: error updating kept bounce %d: %v", r.ID, err)
		}
	}
	return len(rows)
}

// watchPendingBounces records kept bounces as they're kept, and every minute.
func (d *denmaCenters) watchPendingBounces() {
	tick := time.NewTicker(time.Minute)
	for {
		select {
		case <-tick.C:
		case <-denmaBounceKicks:
		}
		d.retryBounces()
	}
}

// bounceCenters returns the slugs of the centers the bounce may be for: the
// one with its campaign, else its subscriber, else every one with its address.
func (d *denmaCenters) bounceCenters(b models.Bounce) ([]string, error) {
	if id, err := uuid.FromString(b.CampaignUUID); err == nil {
		slugs, err := d.centersWithUUID(&denmaBounceCampaigns, "campaigns", id.String())
		if err != nil || len(slugs) == 1 {
			return slugs, err
		}
	}
	if id, err := uuid.FromString(b.SubscriberUUID); err == nil {
		slugs, err := d.centersWithUUID(&denmaBounceSubscribers, "subscribers", id.String())
		if err != nil || len(slugs) == 1 {
			return slugs, err
		}
	}
	if b.Email == "" {
		return nil, nil
	}
	return d.findInCenters(`SELECT %s FROM %s.subscribers WHERE LOWER(email) = $1`, b.Email)
}

// centersWithUUID returns the centers with a row of table (campaigns or
// subscribers) with that UUID. A center in the cache is asked whether it
// still has it, rather than all of them.
func (d *denmaCenters) centersWithUUID(cache *denmaCenterCache, table, id string) ([]string, error) {
	if slug, ok := cache.get(id); ok {
		if has, err := d.centerHasUUID(slug, table, id); err == nil && has {
			return []string{slug}, nil
		}
		cache.drop(id)
	}
	slugs, err := d.findInCenters(`SELECT %s FROM %s.`+table+` WHERE uuid = $1::UUID`, id)
	if err == nil && len(slugs) == 1 {
		cache.put(id, slugs[0])
	}
	return slugs, err
}

// centerHasUUID reports whether a registered center has a row of table with
// that UUID.
func (d *denmaCenters) centerHasUUID(slug, table, id string) (bool, error) {
	db := d.current().db
	var schema string
	if err := db.Get(&schema, `SELECT schema_name FROM denma.centers WHERE slug = $1`, slug); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	var has bool
	err := db.Get(&has, `SELECT EXISTS (SELECT 1 FROM `+pq.QuoteIdentifier(schema)+`.`+table+` WHERE uuid = $1::UUID)`, id)
	return has, err
}

// denmaCenterSchema is a registered center's slug and schema.
type denmaCenterSchema struct {
	Slug   string `db:"slug"`
	Schema string `db:"schema_name"`
}

// denmaCenterSchemas returns the registered centers (only slugs, if given)
// whose schemas have all the tables (a center that failed to install may
// have none).
func denmaCenterSchemas(db *sqlx.DB, slugs []string, tables ...string) ([]denmaCenterSchema, error) {
	where := ""
	for _, t := range tables {
		where += ` AND to_regclass(quote_ident(schema_name) || '.` + t + `') IS NOT NULL`
	}
	var cs []denmaCenterSchema
	err := db.Select(&cs, `SELECT slug, schema_name FROM denma.centers WHERE ($1::TEXT[] IS NULL OR slug = ANY($1))`+where+` ORDER BY id`, pq.Array(slugs))
	return cs, err
}

// findInCenters runs a query (with %s for the slug literal and the schema)
// in every registered center at once, running or not, and returns the slugs
// of those where it finds a row.
func (d *denmaCenters) findInCenters(q, arg string) ([]string, error) {
	db := d.current().db
	cs, err := denmaCenterSchemas(db, nil, "campaigns", "subscribers")
	if err != nil {
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
