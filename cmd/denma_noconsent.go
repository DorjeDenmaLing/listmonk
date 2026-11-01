package main

// denma: people the sending API adds have no consent: they're fenced off from the center's mail.

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

// How long a sign-up lets someone fenced onto a double or single opt-in list.
const (
	denmaNoConsentSignupOptin  = "7 days"
	denmaNoConsentSignupSingle = "15 minutes"
)

// denmaNoConsentSQL makes the table and its triggers, in a center (denmaFeaturesSQL).
const denmaNoConsentSQL = `
CREATE TABLE IF NOT EXISTS denma_no_consent (
    email_hash TEXT PRIMARY KEY,
    key_id     INTEGER NULL,                       -- the API key that added them
    signup_at  TIMESTAMP WITH TIME ZONE NULL,      -- when they last signed up themselves
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
ALTER TABLE denma_no_consent ADD COLUMN IF NOT EXISTS signup_lists INTEGER[] NULL; -- the lists that sign-up was to

-- Skips a fenced subscriber's subscriptions, but for the API's campaign list and their own sign-up.
CREATE OR REPLACE FUNCTION denma_keep_no_consent() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE
    hash   TEXT;
    signup TIMESTAMP WITH TIME ZONE;
    slists INTEGER[];
    dbl    BOOLEAN;
    api    BOOLEAN;
BEGIN
    SELECT n.email_hash, n.signup_at, n.signup_lists INTO hash, signup, slists FROM subscribers s
        JOIN denma_no_consent n ON n.email_hash = denma_email_hash(s.email) WHERE s.id = NEW.subscriber_id;
    IF hash IS NULL OR NEW.status = 'unsubscribed' OR (TG_OP = 'UPDATE' AND OLD.status = NEW.status) THEN
        RETURN NEW;
    END IF;
    -- An API campaign's list (by its tag, which only the API gives), as the API adds its recipients.
    SELECT optin = 'double', '` + denmaAPIListTag + `' = ANY(COALESCE(tags, '{}')) INTO dbl, api FROM lists WHERE id = NEW.list_id;
    IF api THEN
        IF COALESCE(current_setting('denma.api_recipients', true), '') = NEW.list_id::TEXT THEN
            RETURN NEW;
        END IF;
        RETURN NULL;
    END IF;
    IF signup IS NOT NULL THEN
        IF dbl AND NEW.status = 'unconfirmed' AND signup > NOW() - INTERVAL '` + denmaNoConsentSignupOptin + `' THEN
            RETURN NEW;
        END IF;
        -- A single opt-in list they signed up to: their consent.
        IF NOT dbl AND NEW.list_id = ANY(COALESCE(slists, '{}')) AND signup > NOW() - INTERVAL '` + denmaNoConsentSignupSingle + `' THEN
            DELETE FROM denma_no_consent WHERE email_hash = hash;
            RETURN NEW;
        END IF;
    END IF;
    RETURN NULL;
END $$;
DROP TRIGGER IF EXISTS denma_keep_no_consent ON subscriber_lists;
CREATE TRIGGER denma_keep_no_consent BEFORE INSERT OR UPDATE ON subscriber_lists
    FOR EACH ROW EXECUTE FUNCTION denma_keep_no_consent();

-- A fenced subscriber's new address is fenced too; nobody else's address can become a fenced one.
CREATE OR REPLACE FUNCTION denma_keep_no_consent_email() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    IF lower(OLD.email) = lower(NEW.email) THEN
        RETURN NEW;
    END IF;
    IF EXISTS (SELECT 1 FROM denma_no_consent WHERE email_hash = denma_email_hash(OLD.email)) THEN
        INSERT INTO denma_no_consent (email_hash, key_id, signup_at, signup_lists, created_at)
            SELECT denma_email_hash(NEW.email), key_id, signup_at, signup_lists, created_at FROM denma_no_consent
            WHERE email_hash = denma_email_hash(OLD.email)
            ON CONFLICT (email_hash) DO NOTHING;
    ELSIF EXISTS (SELECT 1 FROM denma_no_consent WHERE email_hash = denma_email_hash(NEW.email)) THEN
        RAISE EXCEPTION 'The sending API added this address without consent, so a subscriber''s address can''t be changed to it.';
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS denma_keep_no_consent_email ON subscribers;
CREATE TRIGGER denma_keep_no_consent_email BEFORE UPDATE OF email ON subscribers
    FOR EACH ROW EXECUTE FUNCTION denma_keep_no_consent_email();

-- An API campaign's list is only its own campaign's (denmaCheckAPILists says so first).
CREATE OR REPLACE FUNCTION denma_api_list_campaigns() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    IF EXISTS (` + denmaForeignAPIListsSQL + `) THEN
        RAISE EXCEPTION 'A campaign can''t go to an API campaign''s list: the API added its people without consent.';
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS denma_api_list_campaigns ON campaigns;
CREATE TRIGGER denma_api_list_campaigns BEFORE UPDATE OF status ON campaigns FOR EACH ROW
    WHEN (NEW.status IN ('scheduled', 'running') AND OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION denma_api_list_campaigns();

-- An API campaign's list keeps its tag (` + denmaAPIListTag + `).
CREATE OR REPLACE FUNCTION denma_keep_api_list_tag() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE
    api BOOLEAN;
BEGIN
    IF TG_OP = 'INSERT' THEN
        api := COALESCE(current_setting('denma.api_list', true), '') = 'on';
    ELSE
        api := '` + denmaAPIListTag + `' = ANY(COALESCE(OLD.tags, '{}'));
    END IF;
    NEW.tags := '{}';
    IF api THEN
        NEW.tags := ARRAY['` + denmaAPIListTag + `']::VARCHAR[];
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS denma_keep_api_list_tag ON lists;
CREATE TRIGGER denma_keep_api_list_tag BEFORE INSERT OR UPDATE OF tags ON lists
    FOR EACH ROW EXECUTE FUNCTION denma_keep_api_list_tag();
UPDATE lists SET tags = '{}' WHERE tags <> '{}' AND NOT ('` + denmaAPIListTag + `' = ANY(tags));

-- Only the API puts people on an API campaign's list (denma.api_recipients).
CREATE OR REPLACE FUNCTION denma_keep_api_list_members() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    IF COALESCE(current_setting('denma.api_recipients', true), '') <> NEW.list_id::TEXT
        AND EXISTS (SELECT 1 FROM lists WHERE id = NEW.list_id AND '` + denmaAPIListTag + `' = ANY(COALESCE(tags, '{}'))) THEN
        RETURN NULL;
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS denma_keep_api_list_members ON subscriber_lists;
CREATE TRIGGER denma_keep_api_list_members BEFORE INSERT ON subscriber_lists
    FOR EACH ROW EXECUTE FUNCTION denma_keep_api_list_members();
`

// denmaForeignAPIListsSQL finds a campaign's (NEW.id) API campaign lists that aren't its own.
const denmaForeignAPIListsSQL = `SELECT 1 FROM campaign_lists cl JOIN lists l ON l.id = cl.list_id
        WHERE cl.campaign_id = NEW.id AND '` + denmaAPIListTag + `' = ANY(COALESCE(l.tags, '{}'))
        AND NOT EXISTS (SELECT 1 FROM denma_api_campaigns ac WHERE ac.campaign_id = NEW.id AND ac.list_id = l.id)`

// denmaCheckAPILists refuses to start or schedule a campaign to another's API campaign list.
func (a *App) denmaCheckAPILists(id int) error {
	if !a.denmaHasOptOuts() {
		return nil
	}
	var bad bool
	q := `SELECT EXISTS (` + strings.ReplaceAll(denmaForeignAPIListsSQL, "NEW.id", "$1") + `)`
	if err := a.db.Get(&bad, q, id); err != nil {
		return denmaInternal(err)
	}
	if bad {
		return echo.NewHTTPError(http.StatusBadRequest, "A campaign can't go to an API campaign's list: the API added its people without consent. Choose other lists.")
	}
	return nil
}

// denmaNoConsentMigrateSQL fences, once, those the API added before with no consent recorded.
const denmaNoConsentMigrateSQL = `INSERT INTO denma_no_consent (email_hash, key_id, created_at)
SELECT denma_email_hash(s.email), MIN(k.key_id), MIN(k.created_at)
FROM subscribers s JOIN denma_api_contacts k ON k.email_hash = denma_email_hash(s.email)
WHERE COALESCE(s.attribs->>'consent_at', '') = '' AND s.attribs->'consents' IS NULL
    AND NOT EXISTS (SELECT 1 FROM subscriber_lists sl WHERE sl.subscriber_id = s.id
        AND sl.list_id NOT IN (SELECT list_id FROM denma_api_campaigns WHERE list_id IS NOT NULL))
GROUP BY 1
ON CONFLICT (email_hash) DO NOTHING`

// denmaAPIListActive is true for a subscription (dsl) that isn't to an API campaign's list (dl).
const denmaAPIListActive = `NOT ('` + denmaAPIListTag + `' = ANY(COALESCE(dl.tags, '{}')))`

// denmaNoConsentAdd fences addresses (lowercase) the API has just added.
func denmaNoConsentAdd(db interface {
	Exec(string, ...any) (sql.Result, error)
}, keyID int, emails []string) error {
	if len(emails) == 0 {
		return nil
	}
	_, err := db.Exec(`INSERT INTO denma_no_consent (email_hash, key_id)
		SELECT denma_email_hash(e), NULLIF($1, 0) FROM unnest($2::TEXT[]) AS e ON CONFLICT DO NOTHING`, keyID, pq.Array(emails))
	return err
}

// denmaNoConsentSignup marks that someone fenced has signed up themselves to lists (by ID or UUID).
func (a *App) denmaNoConsentSignup(email string, listIDs []int, listUUIDs []string) {
	if !a.denmaHasOptOuts() || email == "" {
		return
	}
	if _, err := a.db.Exec(`UPDATE denma_no_consent SET signup_at = NOW(),
		signup_lists = ARRAY(SELECT id FROM lists WHERE id = ANY($2) OR uuid::TEXT = ANY($3))
		WHERE email_hash = denma_email_hash($1)`, email, pq.Array(listIDs), pq.Array(listUUIDs)); err != nil {
		a.log.Printf("denma: error recording a sign-up's consent: %v", err)
	}
}

// denmaNoConsentOptin lifts the fence of someone confirming their own sign-up on the opt-in page.
func (a *App) denmaNoConsentOptin(subUUID, token string) {
	if !a.denmaHasOptOuts() {
		return
	}
	var subID int
	if err := a.db.Get(&subID, `SELECT id FROM subscribers WHERE uuid = $1`, subUUID); err != nil || !denmaOptinTokenOK(a.db, subID, token) {
		return
	}
	if _, err := a.db.Exec(`DELETE FROM denma_no_consent n USING subscribers s
		WHERE s.id = $1 AND n.email_hash = denma_email_hash(s.email)
		AND n.signup_at > NOW() - INTERVAL '`+denmaNoConsentSignupOptin+`'`, subID); err != nil {
		a.log.Printf("denma: error recording a confirmation's consent: %v", err)
	}
}

// denmaNoConsentLift lifts someone's fence: they've given their consent.
func denmaNoConsentLift(db interface {
	Exec(string, ...any) (sql.Result, error)
}, email string) error {
	_, err := db.Exec(`DELETE FROM denma_no_consent WHERE email_hash = denma_email_hash($1)`, email)
	return err
}

// denmaNoConsent is a subscriber's fence, for their page.
type denmaNoConsent struct {
	CreatedAt time.Time  `db:"created_at"`
	SignupAt  *time.Time `db:"signup_at"`
}

// denmaNoConsentOf returns an address's fence, or nil.
func (a *App) denmaNoConsentOf(email string) *denmaNoConsent {
	if !a.denmaHasOptOuts() || email == "" {
		return nil
	}
	var n denmaNoConsent
	if err := a.db.Get(&n, `SELECT created_at, signup_at FROM denma_no_consent WHERE email_hash = denma_email_hash($1)`, email); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			a.log.Printf("denma: error reading a subscriber's consent: %v", err)
		}
		return nil
	}
	return &n
}
