package main

// denma: opt-outs, kept by hash so only the person can undo an unsubscribe (cmd/denma_unsubscribe.go).

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

// denmaOptOutsSQL makes the opt-outs' table and triggers in a center.
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
    -- Those already opted out: complaints, and the blocklisted with no other reason.
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

-- An address's canonical form: lowercase, local part without a +tag, Gmail's without dots.
CREATE OR REPLACE FUNCTION denma_email_canon(e TEXT) RETURNS TEXT
LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE WHEN d IN ('gmail.com', 'googlemail.com') THEN replace(l, '.', '') || '@gmail.com' ELSE l || '@' || d END
    FROM (SELECT COALESCE(NULLIF(split_part(split_part(x, '@', 1), '+', 1), ''), split_part(x, '@', 1)) AS l,
            substr(x, length(split_part(x, '@', 1)) + 2) AS d
        FROM (SELECT lower(trim(e)) AS x) a) b
$$;
CREATE OR REPLACE FUNCTION denma_canon_hash(e TEXT) RETURNS TEXT
LANGUAGE sql IMMUTABLE SET search_path FROM CURRENT AS $$ SELECT encode(sha256(convert_to(denma_email_canon(e), 'UTF8')), 'hex') $$;
ALTER TABLE denma_optouts ADD COLUMN IF NOT EXISTS canon_hash TEXT NULL;
CREATE INDEX IF NOT EXISTS denma_optouts_canon ON denma_optouts (canon_hash);
CREATE INDEX IF NOT EXISTS denma_subscribers_canon ON subscribers (denma_canon_hash(email));
-- An opt-out's canonical hash, from its subscriber's address when the writer didn't give it.
CREATE OR REPLACE FUNCTION denma_optout_canon() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    IF NEW.canon_hash IS NULL THEN
        SELECT denma_canon_hash(s.email) INTO NEW.canon_hash FROM subscribers s WHERE denma_email_hash(s.email) = NEW.email_hash LIMIT 1;
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS denma_optout_canon ON denma_optouts;
CREATE TRIGGER denma_optout_canon BEFORE INSERT OR UPDATE ON denma_optouts
    FOR EACH ROW EXECUTE FUNCTION denma_optout_canon();
UPDATE denma_optouts o SET canon_hash = denma_canon_hash(s.email) FROM subscribers s
    WHERE o.canon_hash IS NULL AND denma_email_hash(s.email) = o.email_hash;
-- The opt-out an address has, its own or a variant's: a complaint first.
CREATE OR REPLACE FUNCTION denma_optout_kind(e TEXT) RETURNS TEXT
LANGUAGE sql STABLE SET search_path FROM CURRENT AS $$
    SELECT kind FROM denma_optouts WHERE email_hash = denma_email_hash(e) OR canon_hash = denma_canon_hash(e)
    ORDER BY kind = 'complained' DESC LIMIT 1
$$;

-- An opted-out address stays blocklisted and can't be enabled; a changed address keeps the old opt-out.
CREATE OR REPLACE FUNCTION denma_keep_optouts() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE k TEXT;
BEGIN
    IF TG_OP = 'UPDATE' AND lower(OLD.email) <> lower(NEW.email) THEN
        INSERT INTO denma_optouts (email_hash, canon_hash, kind, source, created_at)
            SELECT denma_email_hash(NEW.email), denma_canon_hash(NEW.email), kind, source, created_at FROM denma_optouts
            WHERE email_hash = denma_email_hash(OLD.email) OR canon_hash = denma_canon_hash(OLD.email)
            ORDER BY kind = 'complained' DESC LIMIT 1
            ON CONFLICT (email_hash) DO UPDATE SET kind = EXCLUDED.kind, source = EXCLUDED.source, created_at = EXCLUDED.created_at
                WHERE EXCLUDED.kind = 'complained' AND denma_optouts.kind <> 'complained';
    END IF;
    IF NEW.status = 'blocklisted' THEN RETURN NEW; END IF;
    IF TG_OP = 'UPDATE' AND OLD.status <> 'blocklisted' AND lower(OLD.email) = lower(NEW.email) THEN RETURN NEW; END IF;
    k := denma_optout_kind(NEW.email);
    IF k IS NULL THEN RETURN NEW; END IF;
    IF TG_OP = 'INSERT' THEN
        NEW.status := 'blocklisted';
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.status <> 'blocklisted' THEN -- a changed address: no word on why
        RAISE EXCEPTION '% can''t be used as this subscriber''s address.', NEW.email;
    END IF;
    IF k = 'complained' THEN
        RAISE EXCEPTION '% marked one of this center''s e-mails as spam, so they can''t be subscribed again.', NEW.email;
    END IF;
    RAISE EXCEPTION '% unsubscribed from this center, so only they can subscribe again: by signing up again, or by confirming the e-mail that the Send a confirmation to subscribe again button sends them.', NEW.email;
END $$;
DROP TRIGGER IF EXISTS denma_keep_optouts ON subscribers;
CREATE TRIGGER denma_keep_optouts BEFORE INSERT OR UPDATE ON subscribers
    FOR EACH ROW EXECUTE FUNCTION denma_keep_optouts();

-- ...and its subscriptions unsubscribed.
CREATE OR REPLACE FUNCTION denma_keep_optout_lists() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    IF NEW.status <> 'unsubscribed' AND EXISTS (SELECT 1 FROM subscribers s
        WHERE s.id = NEW.subscriber_id AND s.status = 'blocklisted' AND denma_optout_kind(s.email) IS NOT NULL) THEN
        NEW.status := 'unsubscribed';
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS denma_keep_optout_lists ON subscriber_lists;
CREATE TRIGGER denma_keep_optout_lists BEFORE INSERT OR UPDATE ON subscriber_lists
    FOR EACH ROW EXECUTE FUNCTION denma_keep_optout_lists();

-- A complaint opts out for good, and blocklists.
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
	if err := a.db.Get(&o, `SELECT kind, source, created_at FROM denma_optouts
		WHERE email_hash = denma_email_hash($1) OR canon_hash = denma_canon_hash($1)
		ORDER BY kind = 'complained' DESC, email_hash = denma_email_hash($1) DESC LIMIT 1`, email); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			a.log.Printf("denma: error reading an opt-out: %v", err)
		}
		return nil
	}
	o.Before = o.Source == denmaOptOutBefore
	return &o
}

// denmaCheckOptOut refuses to enable a subscriber who opted out, with words for people.
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
	changed := !strings.EqualFold(cur.Email, email)
	if changed { // a new address keeps the old one's opt-out
		if old := a.denmaOptOutOf(cur.Email); old != nil && (o == nil || old.Kind == denmaOptOutComplained) {
			o, email = old, cur.Email
		}
	}
	if o == nil || (cur.Status != models.SubscriberStatusBlockListed && !changed) {
		return nil
	}
	if changed && cur.Status != models.SubscriberStatusBlockListed { // a changed address: no word on why
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("%s can't be used as this subscriber's address.", email))
	}
	if o.Kind == denmaOptOutComplained {
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("%s marked one of this center's e-mails as spam, so they can't be subscribed again.", email))
	}
	return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf(
		"%s unsubscribed from this center, so only they can subscribe again: by signing up again, or by confirming the e-mail that the Send a confirmation to subscribe again button (on their page) sends them, if they've asked you to.", email))
}
