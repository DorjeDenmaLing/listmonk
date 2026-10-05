package main

// denma: opt-outs. Someone who unsubscribes from a center themselves (the
// unsubscribe link or button, or the mail app's one-click unsubscribe;
// cmd/denma_unsubscribe.go), or marks one of its e-mails as spam, has
// withdrawn their consent, and only they can give it again. So no one in the
// center can enable them or subscribe them to lists again, superadmins
// included, by any means (the subscriber page, the API, bulk actions,
// imports, or deleting and adding them again):
//
//   - Someone who unsubscribed can subscribe again only by confirming the
//     e-mail that signing up again sends them (cmd/denma_resubscribe.go), or
//     that an admin sends them, when they've asked, with "Send a confirmation
//     to subscribe again" on their page.
//   - Someone who complained never can, in that center.
//
// Each center keeps them in denma_optouts, by a SHA-256 of the address, so
// that it outlives the subscriber (deleting them keeps the opt-out), and
// database triggers enforce it: an opted-out address can't be enabled (an
// error says why) and is blocklisted if it's added again, and its
// subscriptions stay unsubscribed. Confirming a re-subscription removes an
// "unsubscribed" opt-out, in the same transaction that enables them.
//
// Being blocklisted for anything else (a hard bounce or the domain check,
// which say nothing about consent, or an admin's own blocklisting) can still
// be undone by an admin.
//
// When the table is first made, those already blocklisted with no other
// reason recorded are taken as having unsubscribed, and those who complained
// as complaints.

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
)

const (
	denmaOptOutUnsubscribed = "unsubscribed"
	denmaOptOutComplained   = "complained"

	// The source of opt-outs found when the table was made.
	denmaOptOutBefore = "before"
)

// denmaOptOutsSQL makes the opt-outs' table and triggers, in a center
// (denmaFeaturesSQL).
const denmaOptOutsSQL = `
CREATE OR REPLACE FUNCTION denma_email_hash(e TEXT) RETURNS TEXT
LANGUAGE sql IMMUTABLE AS $$ SELECT encode(sha256(convert_to(lower(trim(e)), 'UTF8')), 'hex') $$;

DO $$ BEGIN
IF to_regclass('denma_optouts') IS NULL THEN
    CREATE TABLE denma_optouts (
        email_hash TEXT PRIMARY KEY,
        kind       TEXT NOT NULL CHECK (kind IN ('unsubscribed', 'complained')),
        source     TEXT NOT NULL DEFAULT '',
        created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
    );
    -- Those already opted out: complaints, and the blocklisted with no
    -- bounce or domain check to say why.
    INSERT INTO denma_optouts (email_hash, kind, source, created_at)
        SELECT denma_email_hash(s.email), 'complained', 'before', MIN(b.created_at)
        FROM bounces b JOIN subscribers s ON s.id = b.subscriber_id WHERE b.type = 'complaint' GROUP BY s.email
        ON CONFLICT DO NOTHING;
    INSERT INTO denma_optouts (email_hash, kind, source, created_at)
        SELECT denma_email_hash(s.email), 'unsubscribed', 'before', s.updated_at FROM subscribers s
        WHERE s.status = 'blocklisted'
            AND NOT (jsonb_typeof(s.attribs) = 'object' AND s.attribs ? 'auto_blocklist_reason')
            AND NOT EXISTS (SELECT 1 FROM bounces b WHERE b.subscriber_id = s.id AND b.type = 'hard')
        ON CONFLICT DO NOTHING;
    UPDATE subscribers SET status = 'blocklisted', updated_at = NOW()
        WHERE status <> 'blocklisted' AND EXISTS (SELECT 1 FROM bounces b WHERE b.subscriber_id = subscribers.id AND b.type = 'complaint');
END IF;
END $$;

-- An opted-out address stays blocklisted: added again, it's blocklisted;
-- enabling it is refused.
CREATE OR REPLACE FUNCTION denma_keep_optouts() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE k TEXT;
BEGIN
    IF NEW.status = 'blocklisted' THEN RETURN NEW; END IF;
    IF TG_OP = 'UPDATE' AND OLD.status <> 'blocklisted' AND lower(OLD.email) = lower(NEW.email) THEN RETURN NEW; END IF;
    SELECT kind INTO k FROM denma_optouts WHERE email_hash = denma_email_hash(NEW.email);
    IF k IS NULL THEN RETURN NEW; END IF;
    IF TG_OP = 'INSERT' THEN
        NEW.status := 'blocklisted';
        RETURN NEW;
    END IF;
    IF k = 'complained' THEN
        RAISE EXCEPTION '% marked one of this center''s e-mails as spam, so they can''t be subscribed again.', NEW.email;
    END IF;
    RAISE EXCEPTION '% unsubscribed from this center, so only they can subscribe again: by signing up again, or by confirming the e-mail that Send a confirmation to subscribe again sends them.', NEW.email;
END $$;
DROP TRIGGER IF EXISTS denma_keep_optouts ON subscribers;
CREATE TRIGGER denma_keep_optouts BEFORE INSERT OR UPDATE ON subscribers
    FOR EACH ROW EXECUTE FUNCTION denma_keep_optouts();

-- ...and its subscriptions unsubscribed.
CREATE OR REPLACE FUNCTION denma_keep_optout_lists() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    IF NEW.status <> 'unsubscribed' AND EXISTS (SELECT 1 FROM subscribers s
        JOIN denma_optouts o ON o.email_hash = denma_email_hash(s.email)
        WHERE s.id = NEW.subscriber_id AND s.status = 'blocklisted') THEN
        NEW.status := 'unsubscribed';
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS denma_keep_optout_lists ON subscriber_lists;
CREATE TRIGGER denma_keep_optout_lists BEFORE INSERT OR UPDATE ON subscriber_lists
    FOR EACH ROW EXECUTE FUNCTION denma_keep_optout_lists();

-- A complaint opts out for good, and blocklists, whatever the bounce
-- settings say.
CREATE OR REPLACE FUNCTION denma_record_complaint() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    INSERT INTO denma_optouts (email_hash, kind, source)
        SELECT denma_email_hash(email), 'complained', 'complaint' FROM subscribers WHERE id = NEW.subscriber_id
        ON CONFLICT (email_hash) DO UPDATE SET kind = 'complained', source = 'complaint', created_at = NOW();
    UPDATE subscribers SET status = 'blocklisted', updated_at = NOW() WHERE id = NEW.subscriber_id AND status <> 'blocklisted';
    UPDATE subscriber_lists SET status = 'unsubscribed', updated_at = NOW() WHERE subscriber_id = NEW.subscriber_id AND status <> 'unsubscribed';
    RETURN NULL;
END $$;
DROP TRIGGER IF EXISTS denma_record_complaint ON bounces;
CREATE TRIGGER denma_record_complaint AFTER INSERT ON bounces
    FOR EACH ROW WHEN (NEW.type = 'complaint') EXECUTE FUNCTION denma_record_complaint();
`

// denmaOptOut is a subscriber's opt-out, for their page.
type denmaOptOut struct {
	Kind      string    `db:"kind" json:"kind"`
	Source    string    `db:"source" json:"source"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	Before    bool      `db:"-" json:"before"` // found when the table was made
}

// denmaHasOptOuts reports whether the app is a center (which has the table).
func (a *App) denmaHasOptOuts() bool {
	return denmaHub != nil && a.ko.String("denma.center") != ""
}

// denmaOptOutOf returns an address's opt-out, or nil.
func (a *App) denmaOptOutOf(email string) *denmaOptOut {
	if !a.denmaHasOptOuts() || email == "" {
		return nil
	}
	var o denmaOptOut
	if err := a.db.Get(&o, `SELECT kind, source, created_at FROM denma_optouts WHERE email_hash = denma_email_hash($1)`, email); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			a.log.Printf("denma: error reading an opt-out: %v", err)
		}
		return nil
	}
	o.Before = o.Source == denmaOptOutBefore
	return &o
}

// denmaRecordUnsubscribe records that a subscriber unsubscribed themselves
// (denmaUnsubscribe). A complaint already recorded stays one.
func (a *App) denmaRecordUnsubscribe(subUUID string) {
	if !a.denmaHasOptOuts() {
		return
	}
	if _, err := a.db.Exec(`INSERT INTO denma_optouts (email_hash, kind, source)
		SELECT denma_email_hash(email), 'unsubscribed', 'unsubscribe' FROM subscribers WHERE uuid = $1
		ON CONFLICT (email_hash) DO NOTHING`, subUUID); err != nil {
		a.log.Printf("denma: error recording an unsubscribe: %v", err)
	}
}

// denmaCheckOptOut refuses to enable a subscriber who opted out (the admin's
// and the API's update), with words for people; the database refuses it
// anyway (denma_keep_optouts).
func (a *App) denmaCheckOptOut(id int, email, status string) error {
	if !a.denmaHasOptOuts() || status == models.SubscriberStatusBlockListed {
		return nil
	}
	var cur models.Subscriber
	if err := a.db.Get(&cur, `SELECT email, status FROM subscribers WHERE id = $1`, id); err != nil {
		return nil // listmonk says it's not found
	}
	if status == "" {
		status = cur.Status
	}
	if status == models.SubscriberStatusBlockListed {
		return nil
	}
	if email == "" {
		email = cur.Email
	}
	o := a.denmaOptOutOf(email)
	if o == nil || (cur.Status != models.SubscriberStatusBlockListed && strings.EqualFold(cur.Email, email)) {
		return nil
	}
	if o.Kind == denmaOptOutComplained {
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("%s marked one of this center's e-mails as spam, so they can't be subscribed again.", email))
	}
	return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf(
		"%s unsubscribed from this center, so only they can subscribe again: by signing up again, or by confirming the e-mail that Send a confirmation to subscribe again (on their page) sends them, if they've asked you to.", email))
}
