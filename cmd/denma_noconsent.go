package main

// denma: people the sending API adds have given the center no consent. A
// transactional e-mail (POST /v1/messages) or an API campaign (POST
// /v1/campaigns) adds each new address as a subscriber, so that its bounces,
// complaints and unsubscribes are recorded (cmd/denma_api_send.go); the other
// system vouches for nothing else. So until they sign up themselves, they're
// fenced off from the center's mail:
//
//   - They can't join a list by any means: the subscriber page, bulk
//     actions, imports, automations or listmonk's API. The database skips
//     the subscription (denma_keep_no_consent), as it does for opt-outs
//     (cmd/denma_optouts.go), so a bulk action or an import carries on for
//     everyone else. Only the API puts them on its campaign's list, in the
//     transaction that adds its recipients (denma.api_recipients).
//   - No campaign but an API campaign goes to an API campaign's list, and
//     that only to its own (denma_api_list_campaigns).
//   - Signing up themselves lifts it: a sign-up (a sign-up webhook, the public
//     form) marks them (signup_at), which lets them onto a double opt-in list,
//     unconfirmed, for the confirmation link's life; confirming it on the
//     opt-in page lifts the fence (denmaNoConsentOptin), and nothing else
//     confirms it for them. A single opt-in list they sign up to lifts it at
//     once. So do confirming a re-subscription (cmd/denma_resubscribe.go) and
//     a Server webhook vouching for their consent (cmd/denma_signup.go).
//   - Changing their address keeps the fence (denma_keep_no_consent_email),
//     and nobody's address can be changed to a fenced one.
//   - Campaigns to a tag and automations don't count their API campaign
//     lists as a subscription (denmaAPIListActive), and the hub's figures
//     leave them out.
//
// Each center keeps them in denma_no_consent, by a SHA-256 of the address
// (denma_email_hash), so that deleting and adding them again keeps it.

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

// denmaNoConsentSignupOptin is how long a sign-up lets someone fenced onto a
// double opt-in list, unconfirmed: the confirmation link's life
// (denmaResubTTL is the same). denmaNoConsentSignupSingle is how long it
// lets them onto a single opt-in list, which their sign-up does at once.
const (
	denmaNoConsentSignupOptin  = "7 days"
	denmaNoConsentSignupSingle = "15 minutes"
)

// denmaNoConsentSQL makes the table and its triggers, in a center
// (denmaFeaturesSQL). It follows denmaOptOutsSQL (denma_email_hash) and
// denmaAPISQL (denma_api_campaigns).
const denmaNoConsentSQL = `
CREATE TABLE IF NOT EXISTS denma_no_consent (
    email_hash TEXT PRIMARY KEY,
    key_id     INTEGER NULL,                       -- the API key that added them
    signup_at  TIMESTAMP WITH TIME ZONE NULL,      -- when they last signed up themselves
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

-- A fenced subscriber's subscriptions: skipped, but for the API adding them
-- to its campaign's list, leaving a list, and their own sign-up. Confirming
-- it is skipped too: the opt-in page lifts the fence first
-- (denmaNoConsentOptin). Before denma_keep_optout_lists (triggers run by name).
CREATE OR REPLACE FUNCTION denma_keep_no_consent() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE
    hash   TEXT;
    signup TIMESTAMP WITH TIME ZONE;
    dbl    BOOLEAN;
    api    BOOLEAN;
BEGIN
    SELECT n.email_hash, n.signup_at INTO hash, signup FROM subscribers s
        JOIN denma_no_consent n ON n.email_hash = denma_email_hash(s.email) WHERE s.id = NEW.subscriber_id;
    IF hash IS NULL OR NEW.status = 'unsubscribed' OR (TG_OP = 'UPDATE' AND OLD.status = NEW.status) THEN
        RETURN NEW;
    END IF;
    -- An API campaign's list (by its tag, which only the API gives:
    -- denma_keep_api_list_tag), as the API adds its recipients.
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
        IF NOT dbl AND signup > NOW() - INTERVAL '` + denmaNoConsentSignupSingle + `' THEN
            DELETE FROM denma_no_consent WHERE email_hash = hash;
            RETURN NEW;
        END IF;
    END IF;
    RETURN NULL;
END $$;
DROP TRIGGER IF EXISTS denma_keep_no_consent ON subscriber_lists;
CREATE TRIGGER denma_keep_no_consent BEFORE INSERT OR UPDATE ON subscriber_lists
    FOR EACH ROW EXECUTE FUNCTION denma_keep_no_consent();

-- A fenced subscriber's new address is fenced too, and nobody else's
-- address can become a fenced one.
CREATE OR REPLACE FUNCTION denma_keep_no_consent_email() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    IF lower(OLD.email) = lower(NEW.email) THEN
        RETURN NEW;
    END IF;
    IF EXISTS (SELECT 1 FROM denma_no_consent WHERE email_hash = denma_email_hash(OLD.email)) THEN
        INSERT INTO denma_no_consent (email_hash, key_id, signup_at, created_at)
            SELECT denma_email_hash(NEW.email), key_id, signup_at, created_at FROM denma_no_consent
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

-- An API campaign's list is only its own campaign's: no other campaign can
-- be started or scheduled to one (denmaCheckAPILists says so first).
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

-- An API campaign's list has its tag (` + denmaAPIListTag + `), by which the fence lets its
-- recipients on, and campaigns to a tag and automations leave it out
-- (denmaAPIListActive). Only the API gives it, making the list with
-- denma.api_list set for its transaction (apiNewCampaignList); no other list
-- can have it, and an API campaign's list keeps it.
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
    NEW.tags := array_remove(COALESCE(NEW.tags, '{}'), '` + denmaAPIListTag + `');
    IF api THEN
        NEW.tags := NEW.tags || '` + denmaAPIListTag + `'::VARCHAR;
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS denma_keep_api_list_tag ON lists;
CREATE TRIGGER denma_keep_api_list_tag BEFORE INSERT OR UPDATE OF tags ON lists
    FOR EACH ROW EXECUTE FUNCTION denma_keep_api_list_tag();
`

// denmaForeignAPIListsSQL finds a campaign's (NEW.id) API campaign lists
// that aren't its own.
const denmaForeignAPIListsSQL = `SELECT 1 FROM campaign_lists cl JOIN lists l ON l.id = cl.list_id
        WHERE cl.campaign_id = NEW.id AND '` + denmaAPIListTag + `' = ANY(COALESCE(l.tags, '{}'))
        AND NOT EXISTS (SELECT 1 FROM denma_api_campaigns ac WHERE ac.campaign_id = NEW.id AND ac.list_id = l.id)`

// denmaCheckAPILists refuses to start or schedule a campaign to an API
// campaign's list that isn't its own (denma_api_list_campaigns).
func (a *App) denmaCheckAPILists(id int) error {
	if !a.denmaHasOptOuts() {
		return nil
	}
	var bad bool
	q := `SELECT EXISTS (` + strings.ReplaceAll(denmaForeignAPIListsSQL, "NEW.id", "$1") + `)`
	if err := a.db.Get(&bad, q, id); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if bad {
		return echo.NewHTTPError(http.StatusBadRequest, "A campaign can't go to an API campaign's list: the API added its people without consent. Choose other lists.")
	}
	return nil
}

// denmaNoConsentMigrateSQL fences, once (features version 22), those the API
// added before: subscribers it sent to or subscribed (denma_api_contacts)
// with no consent recorded and no list but API campaigns' lists.
const denmaNoConsentMigrateSQL = `INSERT INTO denma_no_consent (email_hash, key_id, created_at)
SELECT denma_email_hash(s.email), MIN(k.key_id), MIN(k.created_at)
FROM subscribers s JOIN denma_api_contacts k ON k.email_hash = denma_email_hash(s.email)
WHERE COALESCE(s.attribs->>'consent_at', '') = '' AND s.attribs->'consents' IS NULL
    AND NOT EXISTS (SELECT 1 FROM subscriber_lists sl WHERE sl.subscriber_id = s.id
        AND sl.list_id NOT IN (SELECT list_id FROM denma_api_campaigns WHERE list_id IS NOT NULL))
GROUP BY 1
ON CONFLICT (email_hash) DO NOTHING`

// denmaAPIListActive is true for a subscription (alias dsl) that isn't to an
// API campaign's list (alias dl), which is no consent to the center's mail:
// for campaigns to a tag and automations.
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

// denmaNoConsentSignup marks that someone fenced has signed up themselves,
// before their sign-up adds them to lists. Anyone else is left alone.
func (a *App) denmaNoConsentSignup(email string) {
	if !a.denmaHasOptOuts() || email == "" {
		return
	}
	if _, err := a.db.Exec(`UPDATE denma_no_consent SET signup_at = NOW() WHERE email_hash = denma_email_hash($1)`, email); err != nil {
		a.log.Printf("denma: error recording a sign-up's consent: %v", err)
	}
}

// denmaNoConsentOptin lifts the fence of someone confirming their own
// sign-up on the opt-in page, with their e-mail's token
// (cmd/denma_optintoken.go), before it's confirmed.
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
