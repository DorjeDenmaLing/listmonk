package main

// denma: automations (sidebar, in a center; not the hub), as Mailchimp's
// are: when something happens to someone, after an optional wait, they're
// added to or removed from lists, tagged or untagged, and sent an e-mail,
// any of these.
//
// What sets one off (its trigger):
//   - joins: they join one of its lists (single opt-in ones).
//   - confirms: they confirm their subscription to one of its lists (double
//     opt-in ones), by the link in the confirmation e-mail, or are added
//     confirmed. Never before: someone waiting to confirm has given no
//     consent yet.
//   People added by an import or by hand count too.
//   - tagged: they get one of its tags (cmd/denma_tags.go).
//   - opens, clicks: they open its campaign, or click a link in it. Only with
//     Settings -> Privacy's individual tracking on, which ties them to people.
//
// Only what happens while an automation is on counts, not what happened
// before. It runs once per person, or each time it happens again (each_time).
// Its conditions (if_tags, unless_tags) are checked when the wait is over;
// someone who doesn't meet them is skipped (for that time).
//
// Its actions keep to the subscriber's consent: it never acts on someone
// blocklisted, never re-adds someone who unsubscribed from a list, adds
// people as confirmed only if they've confirmed a subscription in the
// center, and removing from a list deletes the subscription (it isn't an
// unsubscribe, so unsubscribe everywhere doesn't apply). Imported
// subscribers (attribs.imported_at, cmd/denma_features.go) aren't sent its
// e-mail, but its other actions apply. Automations that would set each other
// off in a loop (one adding to a list or tag that sets off another, and back)
// can't be turned on together.
//
// A cron job (every minute, per center) runs what's due; e-mails go through
// the center's e-mail messenger. denma_automation_sends records each run, per
// automation and subscriber, with what it did, for the subscriber's Activity
// tab; denma_list_joins and denma_tag_log record when people joined lists and
// got tags.
//
// Its e-mails' unsubscribe links take the automation's UUID in place of a
// campaign's (public.go calls denmaUnsubscribe). Their "view in browser" link
// is /automation/<uuid>/<subscriber>.
//
// Every new center starts with one (denmaDefaultAutomation): website signups,
// on a double opt-in list, move on to its newsletter once they've confirmed.
// It replaced the Website signups setting, which centers that had it got as
// an automation (features version 16).

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"maps"
	"net/http"
	"net/mail"
	"net/textproto"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	txttpl "text/template"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/jmoiron/sqlx"
	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
	null "gopkg.in/volatiletech/null.v6"
)

const (
	denmaAutoGet    = "automations:get"
	denmaAutoManage = "automations:manage"

	// denmaAutoBatch is the most people one automation runs for a minute.
	denmaAutoBatch = 500
	// denmaAutoMaxDelay is the longest wait, in minutes: a year.
	denmaAutoMaxDelay = 365 * 24 * 60
	// denmaAutoMaxTags is the most tags in each of an automation's fields.
	denmaAutoMaxTags = 20
)

// What sets an automation off.
const (
	denmaTrigJoins    = "joins"
	denmaTrigConfirms = "confirms"
	denmaTrigTagged   = "tagged"
	denmaTrigOpens    = "opens"
	denmaTrigClicks   = "clicks"
)

// denmaAutoTables are made in each center's schema when it loads (and in a
// single install's when it starts); each part is made once.
const denmaAutoTables = `
CREATE TABLE IF NOT EXISTS denma_automations (
    id            SERIAL PRIMARY KEY,
    uuid          UUID NOT NULL UNIQUE,
    name          TEXT NOT NULL,
    template_id   INTEGER NULL REFERENCES templates(id) ON DELETE SET NULL,
    subject       TEXT NOT NULL,
    from_email    TEXT NOT NULL DEFAULT '',
    delay_minutes INTEGER NOT NULL DEFAULT 0,
    active        BOOLEAN NOT NULL DEFAULT false,
    active_since  TIMESTAMP WITH TIME ZONE NULL,
    created_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
-- Its lists: those that set it off (trigger), and those it adds people to or
-- removes them from.
CREATE TABLE IF NOT EXISTS denma_automation_lists (
    automation_id INTEGER NOT NULL REFERENCES denma_automations(id) ON DELETE CASCADE,
    list_id       INTEGER NOT NULL REFERENCES lists(id) ON DELETE CASCADE,
    PRIMARY KEY (automation_id, list_id)
);
-- Its runs: one row per subscriber, for the last time it ran for them.
CREATE TABLE IF NOT EXISTS denma_automation_sends (
    automation_id INTEGER NOT NULL REFERENCES denma_automations(id) ON DELETE CASCADE,
    subscriber_id INTEGER NOT NULL REFERENCES subscribers(id) ON DELETE CASCADE,
    status        TEXT NOT NULL DEFAULT 'sent', -- sent (its e-mail), done (no e-mail), failed, skipped
    error         TEXT NOT NULL DEFAULT '',
    sent_at       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (automation_id, subscriber_id)
);

-- Triggers, conditions and actions besides the e-mail.
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = current_schema()
        AND table_name = 'denma_automations' AND column_name = 'trigger_type') THEN
        ALTER TABLE denma_automations
            ADD COLUMN trigger_type        TEXT NOT NULL DEFAULT 'joins',
            ADD COLUMN trigger_tags        TEXT[] NOT NULL DEFAULT '{}',
            ADD COLUMN trigger_campaign_id INTEGER NULL REFERENCES campaigns(id) ON DELETE SET NULL,
            ADD COLUMN if_tags             TEXT[] NOT NULL DEFAULT '{}', -- only if they have any of these
            ADD COLUMN unless_tags         TEXT[] NOT NULL DEFAULT '{}', -- and none of these
            ADD COLUMN add_tags            TEXT[] NOT NULL DEFAULT '{}',
            ADD COLUMN remove_tags         TEXT[] NOT NULL DEFAULT '{}',
            ADD COLUMN send_email          BOOLEAN NOT NULL DEFAULT true,
            ADD COLUMN each_time           BOOLEAN NOT NULL DEFAULT false;
        ALTER TABLE denma_automation_lists ADD COLUMN kind TEXT NOT NULL DEFAULT 'trigger'; -- trigger, add, remove
        ALTER TABLE denma_automation_lists DROP CONSTRAINT denma_automation_lists_pkey;
        ALTER TABLE denma_automation_lists ADD PRIMARY KEY (automation_id, list_id, kind);
        ALTER TABLE denma_automation_sends
            ADD COLUMN trigger_at TIMESTAMP WITH TIME ZONE NULL, -- the last event it ran for
            ADD COLUMN runs       INTEGER NOT NULL DEFAULT 1,
            ADD COLUMN did        TEXT NOT NULL DEFAULT '';
        CREATE INDEX denma_automation_sends_sub ON denma_automation_sends (subscriber_id);
    END IF;
END $$;

-- When each subscriber last joined each list (on a double opt-in list, when
-- they confirmed), for automations; and, on confirming a double opt-in list,
-- attribs.consent_confirmed_at.
DO $$ BEGIN
    IF to_regclass('denma_list_joins') IS NULL THEN
        CREATE TABLE denma_list_joins (
            subscriber_id INTEGER NOT NULL,
            list_id       INTEGER NOT NULL,
            joined_at     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
            PRIMARY KEY (subscriber_id, list_id),
            FOREIGN KEY (subscriber_id, list_id) REFERENCES subscriber_lists (subscriber_id, list_id) ON DELETE CASCADE
        );
        CREATE INDEX denma_list_joins_list ON denma_list_joins (list_id, joined_at);
        INSERT INTO denma_list_joins (subscriber_id, list_id, joined_at)
            SELECT sl.subscriber_id, sl.list_id,
                CASE WHEN l.optin = 'double' THEN GREATEST(sl.created_at, sl.updated_at) ELSE sl.created_at END
            FROM subscriber_lists sl JOIN lists l ON l.id = sl.list_id
            WHERE sl.status <> 'unsubscribed' AND (l.optin <> 'double' OR sl.status = 'confirmed');
    END IF;
END $$;
CREATE OR REPLACE FUNCTION denma_log_join() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE
    dbl BOOLEAN;
BEGIN
    SELECT optin = 'double' INTO dbl FROM lists WHERE id = NEW.list_id;
    IF (dbl AND NEW.status = 'confirmed' AND (TG_OP = 'INSERT' OR OLD.status <> 'confirmed'))
        OR (NOT dbl AND NEW.status <> 'unsubscribed' AND (TG_OP = 'INSERT' OR OLD.status = 'unsubscribed')) THEN
        INSERT INTO denma_list_joins (subscriber_id, list_id) VALUES (NEW.subscriber_id, NEW.list_id)
            ON CONFLICT (subscriber_id, list_id) DO UPDATE SET joined_at = NOW();
    END IF;
    IF dbl AND TG_OP = 'UPDATE' AND NEW.status = 'confirmed' AND OLD.status = 'unconfirmed' THEN
        -- Attributes may be JSON null, which || would make an array.
        UPDATE subscribers
        SET attribs = (CASE WHEN jsonb_typeof(attribs) = 'object' THEN attribs ELSE '{}'::JSONB END) || jsonb_build_object('consent_confirmed_at',
                to_char(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')),
            updated_at = NOW()
        WHERE id = NEW.subscriber_id;
    END IF;
    RETURN NULL;
END;
$$;

-- When each subscriber got each of their tags, for automations.
DO $$ BEGIN
    IF to_regclass('denma_tag_log') IS NULL THEN
        CREATE TABLE denma_tag_log (
            subscriber_id INTEGER NOT NULL REFERENCES subscribers(id) ON DELETE CASCADE,
            tag           TEXT NOT NULL,
            added_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
            PRIMARY KEY (subscriber_id, tag)
        );
        CREATE INDEX denma_tag_log_tag ON denma_tag_log (tag, added_at);
        INSERT INTO denma_tag_log (subscriber_id, tag)
            SELECT s.id, t FROM subscribers s, jsonb_array_elements_text(s.attribs->'tags') t
            WHERE jsonb_typeof(s.attribs->'tags') = 'array'
            ON CONFLICT DO NOTHING;
    END IF;
END $$;
CREATE OR REPLACE FUNCTION denma_log_tags() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE
    now_tags TEXT[];
    had_tags TEXT[] := '{}';
BEGIN
    now_tags := ARRAY(SELECT jsonb_array_elements_text(CASE WHEN jsonb_typeof(NEW.attribs->'tags') = 'array'
        THEN NEW.attribs->'tags' ELSE '[]'::JSONB END));
    IF TG_OP = 'UPDATE' THEN
        had_tags := ARRAY(SELECT jsonb_array_elements_text(CASE WHEN jsonb_typeof(OLD.attribs->'tags') = 'array'
            THEN OLD.attribs->'tags' ELSE '[]'::JSONB END));
    END IF;
    IF now_tags = had_tags THEN
        RETURN NULL;
    END IF;
    DELETE FROM denma_tag_log WHERE subscriber_id = NEW.id AND NOT (tag = ANY(now_tags));
    INSERT INTO denma_tag_log (subscriber_id, tag)
        SELECT NEW.id, t FROM unnest(now_tags) t WHERE NOT (t = ANY(had_tags))
        ON CONFLICT DO NOTHING;
    RETURN NULL;
END;
$$;

-- Joining a double opt-in list was always when they confirmed; those
-- automations say so.
UPDATE denma_automations a SET trigger_type = 'confirms' WHERE trigger_type = 'joins'
    AND EXISTS (SELECT 1 FROM denma_automation_lists al WHERE al.automation_id = a.id AND al.kind = 'trigger')
    AND NOT EXISTS (SELECT 1 FROM denma_automation_lists al JOIN lists l ON l.id = al.list_id
        WHERE al.automation_id = a.id AND al.kind = 'trigger' AND l.optin <> 'double');

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
        WHERE t.tgname = 'denma_log_join' AND c.relnamespace = current_schema()::REGNAMESPACE) THEN
        CREATE TRIGGER denma_log_join AFTER INSERT OR UPDATE OF status ON subscriber_lists
            FOR EACH ROW EXECUTE FUNCTION denma_log_join();
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
        WHERE t.tgname = 'denma_log_tags' AND c.relnamespace = current_schema()::REGNAMESPACE) THEN
        CREATE TRIGGER denma_log_tags AFTER INSERT OR UPDATE OF attribs ON subscribers
            FOR EACH ROW EXECUTE FUNCTION denma_log_tags();
    END IF;
END $$;`

// denmaAutoPendingSQL is what's happened to people that the active
// automations matching cond (a condition on a, the automation) haven't run
// for yet: one row per automation and subscriber, with the first and last of
// those events. Each kind of trigger's events come from their own table.
func denmaAutoPendingSQL(cond string) string {
	on := `a.active AND (NOT a.send_email OR a.template_id IS NOT NULL) AND (` + cond + `)`
	return `
WITH ev AS (
    SELECT a.id AS automation_id, a.each_time, a.delay_minutes, j.subscriber_id, j.joined_at AS at
    FROM denma_automations a
    JOIN denma_automation_lists al ON al.automation_id = a.id AND al.kind = 'trigger'
    JOIN denma_list_joins j ON j.list_id = al.list_id AND j.joined_at >= a.active_since
    JOIN subscriber_lists sl ON sl.subscriber_id = j.subscriber_id AND sl.list_id = j.list_id
    JOIN lists l ON l.id = sl.list_id
    WHERE ` + on + ` AND a.trigger_type IN ('joins', 'confirms')
        AND sl.status <> 'unsubscribed' AND (l.optin <> 'double' OR sl.status = 'confirmed')
    UNION ALL
    SELECT a.id, a.each_time, a.delay_minutes, t.subscriber_id, t.added_at
    FROM denma_automations a
    JOIN denma_tag_log t ON t.tag = ANY(a.trigger_tags) AND t.added_at >= a.active_since
    WHERE ` + on + ` AND a.trigger_type = 'tagged'
    UNION ALL
    SELECT a.id, a.each_time, a.delay_minutes, v.subscriber_id, v.created_at
    FROM denma_automations a
    JOIN campaign_views v ON v.campaign_id = a.trigger_campaign_id AND v.created_at >= a.active_since
    WHERE ` + on + ` AND a.trigger_type = 'opens' AND v.subscriber_id IS NOT NULL
    UNION ALL
    SELECT a.id, a.each_time, a.delay_minutes, k.subscriber_id, k.created_at
    FROM denma_automations a
    JOIN link_clicks k ON k.campaign_id = a.trigger_campaign_id AND k.created_at >= a.active_since
    WHERE ` + on + ` AND a.trigger_type = 'clicks' AND k.subscriber_id IS NOT NULL
)
SELECT ev.automation_id, ev.subscriber_id, ev.delay_minutes, MIN(ev.at) AS first_at, MAX(ev.at) AS last_at
FROM ev
JOIN subscribers s ON s.id = ev.subscriber_id
LEFT JOIN denma_automation_sends d ON d.automation_id = ev.automation_id AND d.subscriber_id = ev.subscriber_id
WHERE s.status <> 'blocklisted' AND (d.subscriber_id IS NULL OR (ev.each_time AND ev.at > d.trigger_at))
GROUP BY ev.automation_id, ev.subscriber_id, ev.delay_minutes`
}

// denmaAutomation is an automation, with its lists and its template's and
// campaign's names.
type denmaAutomation struct {
	ID                int            `db:"id" json:"id"`
	UUID              string         `db:"uuid" json:"uuid"`
	Name              string         `db:"name" json:"name"`
	TemplateID        null.Int       `db:"template_id" json:"template_id"`
	Subject           string         `db:"subject" json:"subject"`
	FromEmail         string         `db:"from_email" json:"from_email"`
	DelayMinutes      int            `db:"delay_minutes" json:"delay_minutes"`
	Active            bool           `db:"active" json:"active"`
	ActiveSince       null.Time      `db:"active_since" json:"active_since"`
	CreatedAt         time.Time      `db:"created_at" json:"created_at"`
	UpdatedAt         time.Time      `db:"updated_at" json:"updated_at"`
	TriggerType       string         `db:"trigger_type" json:"trigger_type"`
	TriggerTags       pq.StringArray `db:"trigger_tags" json:"trigger_tags"`
	TriggerCampaignID null.Int       `db:"trigger_campaign_id" json:"trigger_campaign_id"`
	IfTags            pq.StringArray `db:"if_tags" json:"if_tags"`
	UnlessTags        pq.StringArray `db:"unless_tags" json:"unless_tags"`
	AddTags           pq.StringArray `db:"add_tags" json:"add_tags"`
	RemoveTags        pq.StringArray `db:"remove_tags" json:"remove_tags"`
	SendEmail         bool           `db:"send_email" json:"send_email"`
	EachTime          bool           `db:"each_time" json:"each_time"`

	ListIDs         pq.Int64Array  `db:"list_ids" json:"list_ids"` // its trigger's
	ListNames       pq.StringArray `db:"list_names" json:"list_names"`
	AddListIDs      pq.Int64Array  `db:"add_list_ids" json:"add_list_ids"`
	AddListNames    pq.StringArray `db:"add_list_names" json:"add_list_names"`
	RemoveListIDs   pq.Int64Array  `db:"remove_list_ids" json:"remove_list_ids"`
	RemoveListNames pq.StringArray `db:"remove_list_names" json:"remove_list_names"`
	CampaignName    string         `db:"campaign_name" json:"campaign_name"`
	TemplateName    string         `db:"template_name" json:"template_name"`
	TemplateType    string         `db:"template_type" json:"template_type"`
	Sent            int            `db:"sent" json:"sent"`
	Runs            int            `db:"runs" json:"runs"`
	Failed          int            `db:"failed" json:"failed"`
	Skipped         int            `db:"skipped" json:"skipped"`
	LastSentAt      null.Time      `db:"last_sent_at" json:"last_sent_at"` // its last run
	Waiting         int            `db:"waiting" json:"waiting"`
}

// denmaAutoListsSQL is an automation's lists of a kind, as ids or names.
func denmaAutoListsSQL(col, kind, as string) string {
	return `ARRAY(SELECT l.` + col + ` FROM denma_automation_lists al JOIN lists l ON l.id = al.list_id
        WHERE al.automation_id = a.id AND al.kind = '` + kind + `' ORDER BY l.name) AS ` + as
}

var denmaAutoSelectSQL = `
SELECT a.*,
    ` + denmaAutoListsSQL("id", "trigger", "list_ids") + `,
    ` + denmaAutoListsSQL("name", "trigger", "list_names") + `,
    ` + denmaAutoListsSQL("id", "add", "add_list_ids") + `,
    ` + denmaAutoListsSQL("name", "add", "add_list_names") + `,
    ` + denmaAutoListsSQL("id", "remove", "remove_list_ids") + `,
    ` + denmaAutoListsSQL("name", "remove", "remove_list_names") + `,
    COALESCE(c.name, '') AS campaign_name,
    COALESCE(t.name, '') AS template_name, COALESCE(t.type::TEXT, '') AS template_type,
    (SELECT COUNT(*) FROM denma_automation_sends d WHERE d.automation_id = a.id AND d.status = 'sent') AS sent,
    (SELECT COUNT(*) FROM denma_automation_sends d WHERE d.automation_id = a.id AND d.status IN ('sent', 'done')) AS runs,
    (SELECT COUNT(*) FROM denma_automation_sends d WHERE d.automation_id = a.id AND d.status = 'failed') AS failed,
    (SELECT COUNT(*) FROM denma_automation_sends d WHERE d.automation_id = a.id AND d.status = 'skipped') AS skipped,
    (SELECT MAX(sent_at) FROM denma_automation_sends d WHERE d.automation_id = a.id AND d.status <> 'skipped') AS last_sent_at,
    0 AS waiting
FROM denma_automations a
LEFT JOIN templates t ON t.id = a.template_id
LEFT JOIN campaigns c ON c.id = a.trigger_campaign_id`

// denmaAutoForm is what the automation page saves.
type denmaAutoForm struct {
	Name              string   `json:"name"`
	TriggerType       string   `json:"trigger_type"`
	ListIDs           []int    `json:"list_ids"`
	TriggerTags       []string `json:"trigger_tags"`
	TriggerCampaignID int      `json:"trigger_campaign_id"`
	DelayMinutes      int      `json:"delay_minutes"`
	EachTime          bool     `json:"each_time"`
	IfTags            []string `json:"if_tags"`
	UnlessTags        []string `json:"unless_tags"`
	AddListIDs        []int    `json:"add_list_ids"`
	RemoveListIDs     []int    `json:"remove_list_ids"`
	AddTags           []string `json:"add_tags"`
	RemoveTags        []string `json:"remove_tags"`
	SendEmail         bool     `json:"send_email"`
	TemplateID        int      `json:"template_id"`
	Subject           string   `json:"subject"`
	FromEmail         string   `json:"from_email"`
	Active            bool     `json:"active"`

	// TestEmail is where "Send a test" sends it.
	TestEmail string `json:"test_email"`
}

// denmaAutoOption is a list, template or campaign in the automation page's
// selects.
type denmaAutoOption struct {
	ID      int    `db:"id" json:"id"`
	Name    string `db:"name" json:"name"`
	Type    string `db:"type" json:"type"`
	Optin   string `db:"optin" json:"optin"`
	Subject string `db:"subject" json:"subject"`
}

type denmaAutomationsView struct {
	adminView
	Automations []denmaAutomation
}

type denmaAutomationView struct {
	adminView
	Automation         denmaAutomation
	Lists              []denmaAutoOption
	Templates          []denmaAutoOption
	Campaigns          []denmaAutoOption
	FromEmail          string
	IndividualTracking bool
}

// denmaAutomationsOn reports whether the app has automations: a center, or
// listmonk without multi-center; not the hub.
func denmaAutomationsOn(a *App) bool {
	return !a.ko.Bool("denma.multi_center") || a.ko.String("denma.center") != ""
}

// denmaStartAutomations makes the app's automation tables if they're missing
// and adds the job that runs them to its cron. Called by buildApp.
func denmaStartAutomations(a *App) {
	if !denmaAutomationsOn(a) || a.crons == nil {
		return
	}
	var exists bool
	if err := a.db.Get(&exists, `SELECT to_regclass('denma_automations') IS NOT NULL`); err != nil {
		a.log.Printf("denma: error checking the automation tables: %v", err)
		return
	}
	if _, err := a.db.Exec(denmaAutoTables); err != nil {
		a.log.Printf("denma: error making the automation tables: %v", err)
		return
	}
	if !exists {
		// New permissions: the roles that can manage campaigns get them.
		if _, err := a.db.Exec(`UPDATE roles SET permissions = permissions || $1::TEXT[]
			WHERE type = 'user' AND 'campaigns:manage' = ANY(permissions) AND NOT ($2 = ANY(permissions))`,
			pq.Array([]string{denmaAutoGet, denmaAutoManage}), denmaAutoGet); err != nil {
			a.log.Printf("denma: error adding the automation permissions to roles: %v", err)
		}
	}

	var mu sync.Mutex
	if _, err := denmaEveryMinute(a, 0, "automations", func() {
		if !mu.TryLock() {
			return
		}
		defer mu.Unlock()
		a.runAutomations()
	}); err != nil {
		a.log.Printf("denma: error starting automations: %v", err)
		return
	}
	a.crons.Start()
}

// denmaDefaultAutomation gives a new center (db, its schema) its website
// signups' lists and the automation that moves people on from the first
// once they've confirmed. Called when the center is made.
func denmaDefaultAutomation(db *sqlx.DB) error {
	if _, err := db.Exec(denmaAutoTables); err != nil {
		return err
	}
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var hold, news int
	if err := tx.Get(&hold, `INSERT INTO lists (uuid, name, type, optin, tags, description)
		VALUES ($1, 'Website signups (awaiting confirmation)', 'private', 'double', '{}',
		'Sign-up forms and webhooks add people here. They are sent the confirmation e-mail, and an automation moves them to the newsletter once they confirm. Do not send campaigns to this list.')
		RETURNING id`, uuid.Must(uuid.NewV4()).String()); err != nil {
		return err
	}
	if err := tx.Get(&news, `INSERT INTO lists (uuid, name, type, optin, tags, description)
		VALUES ($1, 'Newsletter', 'private', 'single', '{}', 'Everyone who has signed up and confirmed.')
		RETURNING id`, uuid.Must(uuid.NewV4()).String()); err != nil {
		return err
	}
	if err := denmaInsertMoveAutomation(tx, hold, news, "Move confirmed website signups to the newsletter"); err != nil {
		return err
	}
	return tx.Commit()
}

// denmaInsertMoveAutomation adds an automation, on from now, that moves
// people who confirm the hold list to the target list, each time.
func denmaInsertMoveAutomation(tx *sqlx.Tx, hold, target int, name string) error {
	var id int
	if err := tx.Get(&id, `INSERT INTO denma_automations (uuid, name, subject, trigger_type, send_email, each_time, active, active_since)
		VALUES ($1, $2, '', 'confirms', false, true, true, NOW()) RETURNING id`, uuid.Must(uuid.NewV4()).String(), name); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO denma_automation_lists (automation_id, list_id, kind)
		VALUES ($1, $2, 'trigger'), ($1, $3, 'add'), ($1, $2, 'remove')`, id, hold, target)
	return err
}

// denmaMigrateSignupSetting turns the Website signups setting (a holding
// list and a target list), which the features' trigger used to do, into an
// automation, and removes it (features version 16).
func denmaMigrateSignupSetting(tx *sqlx.Tx) (bool, error) {
	if _, err := tx.Exec(denmaAutoTables); err != nil {
		return false, err
	}
	var hold, target int
	if err := tx.Get(&hold, `SELECT COALESCE((SELECT (value #>> '{}')::INT FROM settings WHERE key = 'denma.signup_holding_list'), 0)`); err != nil {
		return false, err
	}
	if err := tx.Get(&target, `SELECT COALESCE((SELECT (value #>> '{}')::INT FROM settings WHERE key = 'denma.signup_target_list'), 0)`); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`DELETE FROM settings WHERE key IN ('denma.signup_holding_list', 'denma.signup_target_list')`); err != nil {
		return false, err
	}
	var n int
	if err := tx.Get(&n, `SELECT COUNT(*) FROM lists WHERE id IN ($1, $2)`, hold, target); err != nil {
		return false, err
	}
	if hold == 0 || target == 0 || hold == target || n != 2 {
		return false, nil
	}
	return true, denmaInsertMoveAutomation(tx, hold, target, "Move confirmed website signups on")
}

// runAutomations runs what's due.
func (a *App) runAutomations() {
	var autos []denmaAutomation
	if err := a.db.Select(&autos, denmaAutoSelectSQL+` WHERE a.active AND (NOT a.send_email OR a.template_id IS NOT NULL) ORDER BY a.id`); err != nil {
		a.log.Printf("denma: error getting automations: %v", err)
		return
	}
	for _, auto := range autos {
		a.runAutomation(auto)
	}
}

// denmaAutoHeld is when each automation (center/ID) last logged that it's
// waiting for its sender's domain, to log it once an hour.
var denmaAutoHeld sync.Map

func (a *App) runAutomation(auto denmaAutomation) {
	limit := denmaAutoBatch
	claimed, pushed := false, 0 // its e-mails claimed against the daily limit, and pushed
	if auto.SendEmail {
		if a.emailMsgr == nil {
			return
		}
		// Nothing runs while SES has said it hasn't verified the sender's
		// domain (cmd/denma_domains.go): those due wait, and go once it has.
		from := auto.FromEmail
		if from == "" {
			from = a.cfg.FromEmail
		}
		if err := a.denmaCheckSenderReady(from); err != nil {
			key := fmt.Sprintf("%s/%d", a.ko.String("denma.center"), auto.ID)
			if t, ok := denmaAutoHeld.Load(key); !ok || time.Since(t.(time.Time)) > time.Hour {
				denmaAutoHeld.Store(key, time.Now())
				a.log.Printf("denma: automation %q waits: %v", auto.Name, err)
			}
			return
		}
		// The hub's daily limit (cmd/denma_daily.go): its share of what's
		// left, claimed before anything is sent, so that automations running
		// at the same time in other centers can't take the same room. What
		// isn't sent is given back; the rest are run later.
		got, ok := denmaDaily.Claim(limit)
		if got == 0 {
			return
		}
		limit, claimed = got, ok
		if claimed {
			defer func() { denmaDaily.Release(got - pushed) }()
		}
	}

	// Mark who's due as done first, so that no one is run twice for the same
	// thing.
	var ids []int
	if err := a.db.Select(&ids, `
		INSERT INTO denma_automation_sends AS d (automation_id, subscriber_id, trigger_at, status, sent_at)
		SELECT p.automation_id, p.subscriber_id, p.last_at, 'done', NOW() FROM (`+denmaAutoPendingSQL("a.id = $1")+`) p
		WHERE p.first_at + make_interval(mins => p.delay_minutes) <= NOW()
		ORDER BY p.first_at LIMIT $2
		ON CONFLICT (automation_id, subscriber_id) DO UPDATE SET trigger_at = EXCLUDED.trigger_at, status = 'done',
			error = '', did = '', runs = d.runs + 1, sent_at = NOW()
			WHERE d.trigger_at < EXCLUDED.trigger_at
		RETURNING subscriber_id`, auto.ID, limit); err != nil {
		a.log.Printf("denma: error getting automation %d's subscribers: %v", auto.ID, err)
		return
	}
	if len(ids) == 0 {
		return
	}

	// Its conditions, now that the wait is over.
	if len(auto.IfTags) > 0 || len(auto.UnlessTags) > 0 {
		var skip []int
		if err := a.db.Select(&skip, `SELECT id FROM subscribers WHERE id = ANY($1) AND NOT (
			(cardinality($2::TEXT[]) = 0 OR COALESCE(attribs->'tags' ?| $2::TEXT[], false))
			AND NOT COALESCE(attribs->'tags' ?| $3::TEXT[], false))`, pq.Array(ids), denmaTextArray(auto.IfTags), denmaTextArray(auto.UnlessTags)); err != nil {
			a.autoFailed(auto.ID, ids, err)
			return
		}
		if len(skip) > 0 {
			a.autoDid(auto.ID, skip, "skipped", "Skipped: "+auto.Conditions()+".")
			ids = slices.DeleteFunc(ids, func(id int) bool { return slices.Contains(skip, id) })
		}
	}
	if len(ids) == 0 {
		return
	}

	// Its lists and tags, together.
	did, notes, err := a.autoActions(auto, ids)
	if err != nil {
		a.autoFailed(auto.ID, ids, err)
		return
	}
	if !auto.SendEmail {
		a.autoDid(auto.ID, ids, "done", did)
		a.autoNotes(auto.ID, notes)
		a.log.Printf("denma: automation %q ran for %d subscribers", auto.Name, len(ids))
		return
	}
	defer a.autoNotes(auto.ID, notes)

	// Its e-mail, but not to imported subscribers.
	tpl, err := a.autoTemplate(int(auto.TemplateID.Int), true)
	var subs []models.Subscriber
	if err == nil {
		err = a.db.Select(&subs, `SELECT id, created_at, updated_at, uuid, email, name, attribs, status FROM subscribers WHERE id = ANY($1)`, pq.Array(ids))
	}
	if err != nil {
		a.autoFailed(auto.ID, ids, err)
		return
	}
	withDid := func(s string) string {
		if did == "" {
			return s
		}
		return did + "; " + s
	}
	var sent, imported []int
	for _, s := range subs {
		if _, ok := s.Attribs["imported_at"]; ok {
			imported = append(imported, s.ID)
			continue
		}
		msg, err := a.autoMessage(auto, tpl, s)
		if err == nil {
			if claimed {
				msg.Headers.Set(denmaClaimHeader, "1") // counted against its claim when it goes (cmd/denma_sending.go)
			}
			err = a.manager.PushMessage(msg)
		}
		if err != nil {
			a.autoFailed(auto.ID, []int{s.ID}, err)
			continue
		}
		pushed++
		sent = append(sent, s.ID)
	}
	a.autoDid(auto.ID, sent, "sent", withDid("sent the e-mail"))
	a.autoDid(auto.ID, imported, "done", withDid("no e-mail (imported)"))
	a.log.Printf("denma: automation %q ran for %d subscribers and sent %d e-mails", auto.Name, len(ids), len(sent))
}

// autoActions adds and removes an automation's lists and tags for
// subscribers, returning what it did, in words, and for those it didn't add
// to a list they'd unsubscribed from, saying so.
func (a *App) autoActions(auto denmaAutomation, ids []int) (string, map[int]string, error) {
	notes := map[int]string{}
	tx, err := a.db.Beginx()
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback()
	if len(auto.AddListIDs) > 0 {
		var unsub []struct {
			ID    int            `db:"subscriber_id"`
			Lists pq.StringArray `db:"lists"`
		}
		if err := tx.Select(&unsub, `SELECT sl.subscriber_id, ARRAY_AGG(l.name ORDER BY l.name) AS lists
			FROM subscriber_lists sl JOIN lists l ON l.id = sl.list_id
			WHERE sl.subscriber_id = ANY($1) AND sl.list_id = ANY($2) AND sl.status = 'unsubscribed'
			GROUP BY sl.subscriber_id`, pq.Array(ids), auto.AddListIDs); err != nil {
			return "", nil, err
		}
		for _, u := range unsub {
			notes[u.ID] = "not added to " + orList(u.Lists, "or") + ": they'd unsubscribed"
		}
		// Confirmed if they've confirmed a subscription here (so they can be
		// sent a double opt-in list's campaigns); never back on a list they
		// unsubscribed from.
		if _, err := tx.Exec(`INSERT INTO subscriber_lists (subscriber_id, list_id, status)
			SELECT s.id, l.id, (CASE WHEN EXISTS (SELECT 1 FROM subscriber_lists x WHERE x.subscriber_id = s.id AND x.status = 'confirmed')
				THEN 'confirmed' ELSE 'unconfirmed' END)::subscription_status
			FROM unnest($1::INT[]) s(id) CROSS JOIN unnest($2::INT[]) l(id)
			ON CONFLICT (subscriber_id, list_id) DO UPDATE SET status = 'confirmed', updated_at = NOW()
				WHERE subscriber_lists.status = 'unconfirmed' AND EXCLUDED.status = 'confirmed'`,
			pq.Array(ids), auto.AddListIDs); err != nil {
			return "", nil, err
		}
	}
	if len(auto.RemoveListIDs) > 0 {
		// Deleted, not unsubscribed: they haven't asked to leave. A list
		// they unsubscribed from keeps that.
		if _, err := tx.Exec(`DELETE FROM subscriber_lists WHERE subscriber_id = ANY($1) AND list_id = ANY($2) AND status <> 'unsubscribed'`,
			pq.Array(ids), auto.RemoveListIDs); err != nil {
			return "", nil, err
		}
	}
	if len(auto.AddTags) > 0 || len(auto.RemoveTags) > 0 {
		// The trigger tidies them (cmd/denma_tags.go).
		if _, err := tx.Exec(`UPDATE subscribers SET attribs = (CASE WHEN jsonb_typeof(attribs) = 'object' THEN attribs ELSE '{}'::JSONB END)
			|| jsonb_build_object('tags', (COALESCE(CASE WHEN jsonb_typeof(attribs->'tags') = 'array' THEN attribs->'tags' END, '[]'::JSONB)
				|| to_jsonb($2::TEXT[])) - $3::TEXT[]), updated_at = NOW()
			WHERE id = ANY($1)`, pq.Array(ids), denmaTextArray(auto.AddTags), denmaTextArray(auto.RemoveTags)); err != nil {
			return "", nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", nil, err
	}
	return strings.Join(auto.ActionList(false), "; "), notes, nil
}

// autoNotes adds a note to what an automation did for some subscribers.
func (a *App) autoNotes(id int, notes map[int]string) {
	for sub, note := range notes {
		if _, err := a.db.Exec(`UPDATE denma_automation_sends SET did = did || ' (' || $3 || ')'
			WHERE automation_id = $1 AND subscriber_id = $2 AND status <> 'failed'`, id, sub, note); err != nil {
			a.log.Printf("denma: error recording automation %d's run: %v", id, err)
		}
	}
}

// autoDid records what an automation did for subscribers.
func (a *App) autoDid(id int, subIDs []int, status, did string) {
	if len(subIDs) == 0 {
		return
	}
	if _, err := a.db.Exec(`UPDATE denma_automation_sends SET status = $3, did = $4
		WHERE automation_id = $1 AND subscriber_id = ANY($2)`, id, pq.Array(subIDs), status, did); err != nil {
		a.log.Printf("denma: error recording automation %d's run: %v", id, err)
	}
}

// autoFailed records that an automation didn't run for (or e-mail)
// subscribers.
func (a *App) autoFailed(id int, subIDs []int, err error) {
	a.log.Printf("denma: error running automation %d: %v", id, err)
	if _, e := a.db.Exec(`UPDATE denma_automation_sends SET status = 'failed', error = $3
		WHERE automation_id = $1 AND subscriber_id = ANY($2)`, id, pq.Array(subIDs), err.Error()); e != nil {
		a.log.Printf("denma: error recording automation %d's failure: %v", id, e)
	}
}

// denmaAutoRun is an automation's run for a subscriber, for their Activity
// tab.
type denmaAutoRun struct {
	ID     int       `db:"id"`
	Name   string    `db:"name"`
	Status string    `db:"status"`
	Did    string    `db:"did"`
	Error  string    `db:"error"`
	Runs   int       `db:"runs"`
	At     time.Time `db:"sent_at"`
}

// denmaAutoRuns returns the automations that ran for a subscriber, latest
// first.
func (a *App) denmaAutoRuns(subID int) []denmaAutoRun {
	if !denmaAutomationsOn(a) {
		return nil
	}
	var out []denmaAutoRun
	if err := a.db.Select(&out, `SELECT a.id, a.name, d.status, d.did, d.error, d.runs, d.sent_at
		FROM denma_automation_sends d JOIN denma_automations a ON a.id = d.automation_id
		WHERE d.subscriber_id = $1 ORDER BY d.sent_at DESC LIMIT 100`, subID); err != nil {
		a.log.Printf("denma: error getting subscriber %d's automations: %v", subID, err)
	}
	return out
}

// denmaAutoTemplate is an automation's template, ready to render.
type denmaAutoTemplate struct {
	Body        string
	Attachments []models.Attachment
}

// autoTemplate gets a template; for e-mail (inline), with its media inlined as
// listmonk does for transactional ones.
func (a *App) autoTemplate(id int, inline bool) (denmaAutoTemplate, error) {
	var t struct {
		Type string `db:"type"`
		Body string `db:"body"`
	}
	if err := a.db.Get(&t, `SELECT type, body FROM templates WHERE id = $1`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return denmaAutoTemplate{}, fmt.Errorf("template %d not found", id)
		}
		return denmaAutoTemplate{}, err
	}
	if t.Type != models.TemplateTypeTx && t.Type != models.TemplateTypeCampaignVisual {
		return denmaAutoTemplate{}, errors.New("Choose a transactional or visual template.")
	}
	if !inline {
		return denmaAutoTemplate{Body: t.Body}, nil
	}
	body, atts := a.manager.ApplyInlineImages(t.Body)
	return denmaAutoTemplate{Body: body, Attachments: atts}, nil
}

// denmaAutoData is what an automation's template is rendered with: what a
// transactional template (Subscriber, Tx) and a campaign's (Campaign, L) get.
type denmaAutoData struct {
	Subscriber models.Subscriber
	Campaign   *models.Campaign
	Tx         *models.TxMessage
	L          any
}

// reDenmaTrackLink is listmonk's https://...@TrackLink shorthand.
var reDenmaTrackLink = regexp.MustCompile(`(https?://[\p{L}\p{N}_\-\.~!#$&'()*+,/:;=?@\[\]%]*)@TrackLink`)

// autoMessage renders an automation's e-mail to a subscriber.
func (a *App) autoMessage(auto denmaAutomation, tpl denmaAutoTemplate, s models.Subscriber) (models.Message, error) {
	var (
		root      = strings.TrimSuffix(a.urlCfg.RootURL, "/")
		unsubURL  = fmt.Sprintf(a.urlCfg.UnsubURL, auto.UUID, s.UUID)
		optinURL  = fmt.Sprintf(a.urlCfg.OptinURL, s.UUID, "")
		viewURL   = fmt.Sprintf("%s/automation/%s/%s", root, auto.UUID, s.UUID)
		funcs     = template.FuncMap{}
		withURL   = func(u string) func(...any) string { return func(...any) string { return u } }
		emptyHTML = func(...any) template.HTML { return "" }
	)
	maps.Copy(funcs, a.manager.GenericTemplateFuncs())
	// Campaign templates' functions; there's no tracking.
	maps.Copy(funcs, template.FuncMap{
		"TrackLink":      func(u string, _ ...any) string { return u },
		"TrackView":      emptyHTML,
		"UnsubscribeURL": withURL(unsubURL),
		"ManageURL":      withURL(unsubURL + "?manage=true"),
		"OptinURL":       withURL(optinURL),
		"MessageURL":     withURL(viewURL),
		"ArchiveURL":     withURL(a.urlCfg.ArchiveURL),
		"RootURL":        withURL(a.urlCfg.RootURL),
	})

	data := denmaAutoData{
		Subscriber: s,
		Campaign:   &models.Campaign{UUID: auto.UUID, Name: auto.Name, Subject: auto.Subject},
		Tx:         &models.TxMessage{Data: map[string]any{}},
		L:          a.i18n,
	}

	var b bytes.Buffer
	subj, err := txttpl.New(models.BaseTpl).Funcs(txttpl.FuncMap(funcs)).Parse(auto.Subject)
	if err != nil {
		return models.Message{}, fmt.Errorf("error in the subject: %v", err)
	}
	if err := subj.Execute(&b, data); err != nil {
		return models.Message{}, fmt.Errorf("error in the subject: %v", err)
	}
	subject := strings.TrimSpace(b.String())
	b.Reset()

	body, err := template.New(models.BaseTpl).Funcs(funcs).Parse(reDenmaTrackLink.ReplaceAllString(tpl.Body, "$1"))
	if err != nil {
		return models.Message{}, fmt.Errorf("error in the template: %v", err)
	}
	if err := body.Execute(&b, data); err != nil {
		return models.Message{}, fmt.Errorf("error in the template: %v", err)
	}

	from := auto.FromEmail
	if from == "" {
		from = a.cfg.FromEmail
	}
	msg := models.Message{
		Subscriber:  s,
		To:          []string{s.Email},
		From:        from,
		Subject:     subject,
		ContentType: models.CampaignContentTypeHTML,
		Messenger:   emailMsgr,
		Body:        b.Bytes(),
		Attachments: tpl.Attachments,
		Headers:     textproto.MIMEHeader{},
	}
	msg.Headers.Set(models.EmailHeaderSubscriberUUID, s.UUID)
	if a.cfg.Privacy.UnsubHeader {
		msg.Headers.Set("List-Unsubscribe-Post", "List-Unsubscribe=One-Click")
		msg.Headers.Set("List-Unsubscribe", `<`+unsubURL+`>`)
	}
	return msg, nil
}

// initDenmaAutomationHandlers registers the automation pages.
func initDenmaAutomationHandlers(g *echo.Group, a *App) {
	g.GET(path.Join(uriAdmin, "/automations"), a.ViewDenmaAutomations)
	g.GET(path.Join(uriAdmin, "/automations/:id"), a.ViewDenmaAutomation)
}

func initDenmaAutomationAPIHandlers(g *echo.Group, a *App) {
	g.GET("/api/denma/automations", a.auth.Perm(a.DenmaGetAutomations, denmaAutoGet))
	g.GET("/api/denma/automations/schedule", a.auth.Perm(a.DenmaAutoSchedule, denmaAutoGet)) // cmd/denma_auto_schedule.go
	g.GET("/api/denma/automations/schedule/day", a.auth.Perm(a.DenmaAutoScheduleDay, denmaAutoGet))
	g.GET("/api/denma/automations/schedule/people", a.auth.Perm(a.DenmaAutoSchedulePeople, denmaAutoGet))
	g.POST("/api/denma/automations", a.auth.Perm(a.DenmaSaveAutomation, denmaAutoManage))
	g.POST("/api/denma/automations/test", a.auth.Perm(a.DenmaTestAutomation, denmaAutoManage))
	g.PUT("/api/denma/automations/:id", a.auth.Perm(hasID(a.DenmaSaveAutomation), denmaAutoManage))
	g.PUT("/api/denma/automations/:id/status", a.auth.Perm(hasID(a.DenmaSetAutomationStatus), denmaAutoManage))
	g.DELETE("/api/denma/automations/:id", a.auth.Perm(hasID(a.DenmaDeleteAutomation), denmaAutoManage))
}

// initDenmaPublicHandlers registers the public pages (handlers.go).
func initDenmaPublicHandlers(g *echo.Group, a *App) {
	initDenmaSignupPublicHandlers(g, a) // cmd/denma_signup.go
	if denmaAutomationsOn(a) {
		g.GET("/automation/:autoUUID/:subUUID", noIndex(a.hasUUID(a.ViewDenmaAutomationMessage, "autoUUID", "subUUID")))
	}
}

// automationsAllowed returns an error unless the app has automations and the
// user can see them.
func (a *App) automationsAllowed(v adminView) error {
	if !denmaAutomationsOn(a) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if !v.Can(denmaAutoGet) {
		return echo.NewHTTPError(http.StatusForbidden, a.i18n.Ts("globals.messages.permissionDenied", "name", denmaAutoGet))
	}
	return nil
}

// automations returns the center's automations, with how many are waiting
// for each.
func (a *App) automations() ([]denmaAutomation, error) {
	out := []denmaAutomation{}
	if err := a.db.Select(&out, denmaAutoSelectSQL+` ORDER BY a.created_at`); err != nil {
		return nil, err
	}
	var waiting []struct {
		ID int `db:"automation_id"`
		N  int `db:"n"`
	}
	if err := a.db.Select(&waiting, `SELECT automation_id, COUNT(*) AS n FROM (`+denmaAutoPendingSQL("TRUE")+`) p GROUP BY automation_id`); err != nil {
		return nil, err
	}
	for _, w := range waiting {
		for i := range out {
			if out[i].ID == w.ID {
				out[i].Waiting = w.N
			}
		}
	}
	return out, nil
}

// ViewDenmaAutomations renders the automations.
func (a *App) ViewDenmaAutomations(c echo.Context) error {
	v := newAdminView(c, "Automations", "", "denma.automations")
	if err := a.automationsAllowed(v); err != nil {
		return err
	}
	autos, err := a.automations()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.Render(http.StatusOK, "admin-denma-automations", denmaAutomationsView{adminView: v, Automations: autos})
}

// DenmaGetAutomations returns the center's automations.
func (a *App) DenmaGetAutomations(c echo.Context) error {
	if !denmaAutomationsOn(a) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	autos, err := a.automations()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{autos})
}

// ViewDenmaAutomation renders an automation's page, or a new one's ("new").
func (a *App) ViewDenmaAutomation(c echo.Context) error {
	v := newAdminView(c, "Automation", "", "denma.automations")
	if err := a.automationsAllowed(v); err != nil {
		return err
	}
	out := denmaAutomationView{adminView: v, FromEmail: a.cfg.FromEmail, IndividualTracking: a.cfg.Privacy.IndividualTracking}
	if c.Param("id") == "new" {
		out.Title = "New automation"
		out.Automation = denmaAutomation{TriggerType: denmaTrigConfirms, SendEmail: true}
	} else {
		id, _ := strconv.Atoi(c.Param("id"))
		if err := a.db.Get(&out.Automation, denmaAutoSelectSQL+` WHERE a.id = $1`, id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return echo.NewHTTPError(http.StatusNotFound, "That automation doesn't exist.")
			}
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		out.Title = out.Automation.Name
	}
	if err := a.db.Select(&out.Lists, `SELECT id, name, type::TEXT, optin::TEXT, '' AS subject FROM lists ORDER BY name`); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if err := a.db.Select(&out.Templates, `SELECT id, name, type::TEXT, '' AS optin, subject FROM templates
		WHERE type IN ('tx', 'campaign_visual') ORDER BY name`); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	// The latest campaigns (and the automation's, if it's older).
	if err := a.db.Select(&out.Campaigns, `SELECT id, name, status::TEXT AS type, '' AS optin, subject FROM campaigns
		WHERE id IN (SELECT id FROM campaigns ORDER BY created_at DESC LIMIT 200) OR id = $1
		ORDER BY created_at DESC`, out.Automation.TriggerCampaignID); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.Render(http.StatusOK, "admin-denma-automation", out)
}

// denmaAutoTagField tidies one of an automation's tag fields and checks that
// the center has them.
func (a *App) denmaAutoTagField(tags []string, what string) ([]string, error) {
	tags = denmaNormTags(tags)
	if len(tags) > denmaAutoMaxTags {
		return nil, echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Choose at most %d tags to %s.", denmaAutoMaxTags, what))
	}
	if err := a.denmaUnknownTags(tags); err != nil {
		return nil, err
	}
	return tags, nil
}

// denmaAutoListField checks one of an automation's list fields: lists that
// exist, that the user can manage; returned by ID.
func (a *App) denmaAutoListField(c echo.Context, ids []int) ([]int, error) {
	out := []int{}
	if len(ids) == 0 {
		return out, nil
	}
	if err := a.db.Select(&out, `SELECT id FROM lists WHERE id = ANY($1) ORDER BY id`, pq.Array(ids)); err != nil {
		return nil, echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if len(out) > 100 {
		return nil, echo.NewHTTPError(http.StatusBadRequest, "Choose at most 100 lists.")
	}
	if len(out) > 0 {
		u := auth.GetUser(c)
		if err := u.HasListPerm(auth.PermTypeManage, out...); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// validateAutomation checks and tidies a saved or tested automation.
func (a *App) validateAutomation(c echo.Context, f *denmaAutoForm) error {
	bad := func(msg string) error { return echo.NewHTTPError(http.StatusBadRequest, msg) }
	f.Name = strings.TrimSpace(f.Name)
	f.Subject = strings.TrimSpace(f.Subject)
	f.FromEmail = strings.TrimSpace(f.FromEmail)

	if !strHasLen(f.Name, 1, 200) {
		return bad("Enter the automation's name.")
	}
	var err error
	if f.ListIDs, err = a.denmaAutoListField(c, f.ListIDs); err != nil {
		return err
	}
	if f.AddListIDs, err = a.denmaAutoListField(c, f.AddListIDs); err != nil {
		return err
	}
	if f.RemoveListIDs, err = a.denmaAutoListField(c, f.RemoveListIDs); err != nil {
		return err
	}
	for _, t := range []struct {
		tags *[]string
		what string
	}{
		{&f.TriggerTags, "set it off"}, {&f.IfTags, "require"}, {&f.UnlessTags, "rule out"},
		{&f.AddTags, "add"}, {&f.RemoveTags, "remove"},
	} {
		if *t.tags, err = a.denmaAutoTagField(*t.tags, t.what); err != nil {
			return err
		}
	}

	// What sets it off.
	switch f.TriggerType {
	case denmaTrigJoins, denmaTrigConfirms:
		if len(f.ListIDs) == 0 {
			return bad("Choose the lists that set it off.")
		}
		want := "single"
		if f.TriggerType == denmaTrigConfirms {
			want = "double"
		}
		var other []string
		if err := a.db.Select(&other, `SELECT name FROM lists WHERE id = ANY($1) AND optin::TEXT <> $2 ORDER BY name`, pq.Array(f.ListIDs), want); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if len(other) > 0 && want == "single" {
			return bad(fmt.Sprintf("%s is double opt-in: choose \"someone confirms a double opt-in list\" for it, so that it runs once they've confirmed.", orList(other, "and")))
		}
		if len(other) > 0 {
			return bad(fmt.Sprintf("%s is single opt-in: there's nothing to confirm. Choose \"someone joins a single opt-in list\" for it.", orList(other, "and")))
		}
		f.TriggerTags, f.TriggerCampaignID = []string{}, 0
	case denmaTrigTagged:
		if len(f.TriggerTags) == 0 {
			return bad("Choose the tags that set it off.")
		}
		f.ListIDs, f.TriggerCampaignID = []int{}, 0
	case denmaTrigOpens, denmaTrigClicks:
		var n int
		if err := a.db.Get(&n, `SELECT COUNT(*) FROM campaigns WHERE id = $1`, f.TriggerCampaignID); err != nil || n == 0 {
			return bad("Choose the campaign.")
		}
		f.ListIDs, f.TriggerTags = []int{}, []string{}
	default:
		return bad("Choose what sets it off.")
	}
	if f.DelayMinutes < 0 || f.DelayMinutes > denmaAutoMaxDelay {
		return bad("The wait can be up to a year.")
	}
	for _, id := range f.AddListIDs {
		if slices.Contains(f.RemoveListIDs, id) {
			return bad("A list can't be both added and removed.")
		}
	}
	for _, t := range f.AddTags {
		if slices.Contains(f.RemoveTags, t) {
			return bad(fmt.Sprintf("The tag %s can't be both added and removed.", t))
		}
	}

	// Its e-mail.
	if !f.SendEmail {
		f.TemplateID, f.Subject, f.FromEmail = 0, "", ""
		if len(f.AddListIDs)+len(f.RemoveListIDs)+len(f.AddTags)+len(f.RemoveTags) == 0 {
			return bad("Choose something for it to do: lists or tags to add or remove, or an e-mail to send.")
		}
		return nil
	}
	if !strHasLen(f.Subject, 1, 500) {
		return bad("Enter the e-mail's subject.")
	}
	if f.FromEmail != "" {
		if _, err := mail.ParseAddress(f.FromEmail); err != nil {
			return bad(`The sender isn't a valid address. Use name@example.org or "Name" <name@example.org>.`)
		}
		if err := a.denmaCheckSender(f.FromEmail); err != nil { // cmd/denma_domains.go
			return denmaBadRequest(err)
		}
	}
	// The template, and that it renders.
	tpl, err := a.autoTemplate(f.TemplateID, false)
	if err != nil {
		return bad("Choose a template: a transactional or visual one.")
	}
	sample := models.Subscriber{UUID: dummyUUID, Email: "subscriber@example.org", Name: "Sample Subscriber", Attribs: models.JSON{}}
	if _, err := a.autoMessage(denmaAutomation{UUID: dummyUUID, Name: f.Name, Subject: f.Subject}, tpl, sample); err != nil {
		return bad(err.Error())
	}
	return nil
}

// denmaAutoNode is an automation in the loop check: what sets it off, and
// the lists and tags it adds.
type denmaAutoNode struct {
	ID       int
	Name     string
	Trigger  string
	Lists    []int64
	Tags     []string
	AddLists []int64
	AddTags  []string
}

// denmaAutoLoop returns the names of automations that would set each other
// off in a loop (the first back at the end), or nil: one sets off another
// by adding people to a list that sets it off, or giving them a tag that
// does.
func denmaAutoLoop(nodes []denmaAutoNode) []string {
	sets := func(x, y denmaAutoNode) bool {
		switch y.Trigger {
		case denmaTrigJoins, denmaTrigConfirms:
			return slices.ContainsFunc(x.AddLists, func(id int64) bool { return slices.Contains(y.Lists, id) })
		case denmaTrigTagged:
			return slices.ContainsFunc(x.AddTags, func(t string) bool { return slices.Contains(y.Tags, t) })
		}
		return false
	}
	const (
		unseen = iota
		open
		done
	)
	state := make([]int, len(nodes))
	var path []int
	var visit func(i int) []string
	visit = func(i int) []string {
		state[i] = open
		path = append(path, i)
		for j := range nodes {
			if !sets(nodes[i], nodes[j]) {
				continue
			}
			if state[j] == open {
				var names []string
				for k := slices.Index(path, j); k < len(path); k++ {
					names = append(names, nodes[path[k]].Name)
				}
				return append(names, nodes[j].Name)
			}
			if state[j] == unseen {
				if loop := visit(j); loop != nil {
					return loop
				}
			}
		}
		path = path[:len(path)-1]
		state[i] = done
		return nil
	}
	for i := range nodes {
		if state[i] == unseen {
			if loop := visit(i); loop != nil {
				return loop
			}
		}
	}
	return nil
}

// checkAutoLoop returns an error if the automation (id, 0 for a new one), on
// with this definition, would set off a loop with the other ones on.
func (a *App) checkAutoLoop(id int, self denmaAutoNode) error {
	var autos []denmaAutomation
	if err := a.db.Select(&autos, denmaAutoSelectSQL+` WHERE a.active AND a.id <> $1`, id); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	nodes := []denmaAutoNode{self}
	for _, x := range autos {
		nodes = append(nodes, denmaAutoNode{ID: x.ID, Name: x.Name, Trigger: x.TriggerType, Lists: x.ListIDs,
			Tags: x.TriggerTags, AddLists: x.AddListIDs, AddTags: x.AddTags})
	}
	if loop := denmaAutoLoop(nodes); loop != nil {
		if len(loop) == 2 {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("%s would set itself off again: it adds a list or tag that sets it off.", loop[0]))
		}
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("These automations would set each other off in a loop: %s. Change one, or turn one off.", strings.Join(loop, " → ")))
	}
	return nil
}

// denmaTextArray is a TEXT[] parameter, never NULL (as pq.Array of a nil
// slice is): NULL in jsonb || or - makes the whole value NULL.
func denmaTextArray(in []string) any {
	if in == nil {
		in = []string{}
	}
	return pq.Array(in)
}

func int64s(in []int) []int64 {
	out := make([]int64, len(in))
	for i, n := range in {
		out[i] = int64(n)
	}
	return out
}

// DenmaSaveAutomation creates (POST) or updates (PUT) an automation.
func (a *App) DenmaSaveAutomation(c echo.Context) error {
	if !denmaAutomationsOn(a) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	var f denmaAutoForm
	if err := c.Bind(&f); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := a.validateAutomation(c, &f); err != nil {
		return err
	}
	isNew := c.Request().Method == http.MethodPost
	id := 0
	if !isNew {
		id = getID(c)
	}
	// On (or turned on with it, if it's new): no loops.
	active := f.Active
	if !isNew {
		if err := a.db.Get(&active, `SELECT active FROM denma_automations WHERE id = $1`, id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return echo.NewHTTPError(http.StatusNotFound, "That automation doesn't exist.")
			}
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
	}
	if active {
		if err := a.checkAutoLoop(id, denmaAutoNode{ID: id, Name: f.Name, Trigger: f.TriggerType, Lists: int64s(f.ListIDs),
			Tags: f.TriggerTags, AddLists: int64s(f.AddListIDs), AddTags: f.AddTags}); err != nil {
			return err
		}
	}

	tx, err := a.db.Beginx()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	defer tx.Rollback()

	tplID := null.NewInt(f.TemplateID, f.TemplateID > 0)
	campID := null.NewInt(f.TriggerCampaignID, f.TriggerCampaignID > 0)
	if isNew {
		err := tx.Get(&id, `INSERT INTO denma_automations
			(uuid, name, template_id, subject, from_email, delay_minutes, active, active_since,
			 trigger_type, trigger_tags, trigger_campaign_id, if_tags, unless_tags, add_tags, remove_tags, send_email, each_time)
			VALUES ($1, $2, $3, $4, $5, $6, $7, CASE WHEN $7 THEN NOW() END, $8, $9, $10, $11, $12, $13, $14, $15, $16) RETURNING id`,
			uuid.Must(uuid.NewV4()).String(), f.Name, tplID, f.Subject, f.FromEmail, f.DelayMinutes, f.Active,
			f.TriggerType, denmaTextArray(f.TriggerTags), campID, denmaTextArray(f.IfTags), denmaTextArray(f.UnlessTags),
			denmaTextArray(f.AddTags), denmaTextArray(f.RemoveTags), f.SendEmail, f.EachTime)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
	} else {
		if _, err := tx.Exec(`UPDATE denma_automations SET name = $2, template_id = $3, subject = $4,
			from_email = $5, delay_minutes = $6, trigger_type = $7, trigger_tags = $8, trigger_campaign_id = $9,
			if_tags = $10, unless_tags = $11, add_tags = $12, remove_tags = $13, send_email = $14, each_time = $15,
			updated_at = NOW() WHERE id = $1`,
			id, f.Name, tplID, f.Subject, f.FromEmail, f.DelayMinutes, f.TriggerType, denmaTextArray(f.TriggerTags), campID,
			denmaTextArray(f.IfTags), denmaTextArray(f.UnlessTags), denmaTextArray(f.AddTags), denmaTextArray(f.RemoveTags), f.SendEmail, f.EachTime); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
	}

	// Its lists, of each kind.
	for kind, ids := range map[string][]int{"trigger": f.ListIDs, "add": f.AddListIDs, "remove": f.RemoveListIDs} {
		if _, err := tx.Exec(`DELETE FROM denma_automation_lists WHERE automation_id = $1 AND kind = $2 AND NOT (list_id = ANY($3))`,
			id, kind, pq.Array(ids)); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if _, err := tx.Exec(`INSERT INTO denma_automation_lists (automation_id, list_id, kind)
			SELECT $1, UNNEST($3::INT[]), $2 ON CONFLICT DO NOTHING`, id, kind, pq.Array(ids)); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
	}
	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{map[string]int{"id": id}})
}

// DenmaSetAutomationStatus turns an automation on or off. Turned on, it's for
// what happens from then on.
func (a *App) DenmaSetAutomationStatus(c echo.Context) error {
	if !denmaAutomationsOn(a) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	var req struct {
		Active bool `json:"active"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	var auto denmaAutomation
	if err := a.db.Get(&auto, denmaAutoSelectSQL+` WHERE a.id = $1`, getID(c)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, "That automation doesn't exist.")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if req.Active {
		if msg := auto.Broken(); msg != "" {
			return echo.NewHTTPError(http.StatusBadRequest, msg+" Edit it first.")
		}
		if err := a.checkAutoLoop(auto.ID, denmaAutoNode{ID: auto.ID, Name: auto.Name, Trigger: auto.TriggerType, Lists: auto.ListIDs,
			Tags: auto.TriggerTags, AddLists: auto.AddListIDs, AddTags: auto.AddTags}); err != nil {
			return err
		}
	}
	if _, err := a.db.Exec(`UPDATE denma_automations SET active = $2, updated_at = NOW(),
		active_since = CASE WHEN $2 AND NOT active THEN NOW() ELSE active_since END WHERE id = $1`, auto.ID, req.Active); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{true})
}

// DenmaDeleteAutomation deletes an automation, and the record of its runs.
func (a *App) DenmaDeleteAutomation(c echo.Context) error {
	if !denmaAutomationsOn(a) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if _, err := a.db.Exec(`DELETE FROM denma_automations WHERE id = $1`, getID(c)); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{true})
}

// DenmaTestAutomation sends the automation page's e-mail to an address: as
// to that subscriber, if there's one, or to a sample one. Its other actions
// aren't tried.
func (a *App) DenmaTestAutomation(c echo.Context) error {
	if !denmaAutomationsOn(a) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	var f denmaAutoForm
	if err := c.Bind(&f); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := a.validateAutomation(c, &f); err != nil {
		return err
	}
	if !f.SendEmail {
		return echo.NewHTTPError(http.StatusBadRequest, "It doesn't send an e-mail.")
	}
	email, err := a.importer.SanitizeEmail(f.TestEmail)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "Enter the address to send the test to.")
	}
	from := f.FromEmail
	if from == "" {
		from = a.cfg.FromEmail
	}
	if err := a.denmaCheckSenderReady(from); err != nil { // cmd/denma_domains.go
		return denmaBadRequest(err)
	}
	if a.emailMsgr == nil {
		return echo.NewHTTPError(http.StatusBadRequest, "There's no e-mail server set up to send it.")
	}

	sub := models.Subscriber{UUID: dummyUUID, Email: email, Name: "Sample Subscriber", Attribs: models.JSON{}}
	var found models.Subscriber
	if err := a.db.Get(&found, `SELECT id, created_at, updated_at, uuid, email, name, attribs, status FROM subscribers WHERE LOWER(email) = LOWER($1)`, email); err == nil {
		sub = found
	}
	tpl, err := a.autoTemplate(f.TemplateID, true)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	msg, err := a.autoMessage(denmaAutomation{UUID: dummyUUID, Name: f.Name, Subject: "[Test] " + f.Subject, FromEmail: f.FromEmail}, tpl, sub)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	msg.To = []string{email}
	if err := a.manager.PushMessage(msg); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{true})
}

// ViewDenmaAutomationMessage shows an automation's e-mail to a subscriber
// (its "view in browser" link).
func (a *App) ViewDenmaAutomationMessage(c echo.Context) error {
	notFound := func() error {
		return c.Render(http.StatusNotFound, tplMessage,
			makeMsgTpl(a.i18n.T("public.notFoundTitle"), "", a.i18n.T("public.campaignNotFound")))
	}
	var auto denmaAutomation
	if err := a.db.Get(&auto, denmaAutoSelectSQL+` WHERE a.uuid = $1`, c.Param("autoUUID")); err != nil || !auto.SendEmail || !auto.TemplateID.Valid {
		return notFound()
	}
	var sub models.Subscriber
	if err := a.db.Get(&sub, `SELECT id, created_at, updated_at, uuid, email, name, attribs, status FROM subscribers WHERE uuid = $1`, c.Param("subUUID")); err != nil {
		return notFound()
	}
	tpl, err := a.autoTemplate(int(auto.TemplateID.Int), false)
	if err != nil {
		return notFound()
	}
	msg, err := a.autoMessage(auto, tpl, sub)
	if err != nil {
		return notFound()
	}
	return c.HTMLBlob(http.StatusOK, msg.Body)
}

// orList is names in words: "A", "A or B", "A, B or C" (and, with "and").
func orList(names []string, or string) string {
	n := len(names)
	if n < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:n-1], ", ") + " " + or + " " + names[n-1]
}

// Broken says why the automation can't run, or "": what it needs was
// deleted.
func (d denmaAutomation) Broken() string {
	switch {
	case (d.TriggerType == denmaTrigJoins || d.TriggerType == denmaTrigConfirms) && len(d.ListNames) == 0:
		return "Its lists were deleted."
	case d.TriggerType == denmaTrigTagged && len(d.TriggerTags) == 0:
		return "Its tags were deleted."
	case (d.TriggerType == denmaTrigOpens || d.TriggerType == denmaTrigClicks) && !d.TriggerCampaignID.Valid:
		return "Its campaign was deleted."
	case d.SendEmail && !d.TemplateID.Valid:
		return "Its template was deleted."
	}
	return ""
}

// Trigger is what sets the automation off, in words: "When someone joins A".
func (d denmaAutomation) Trigger() string {
	switch d.TriggerType {
	case denmaTrigJoins:
		return "When someone joins " + orList(d.ListNames, "or")
	case denmaTrigConfirms:
		return "When someone confirms " + orList(d.ListNames, "or")
	case denmaTrigTagged:
		if len(d.TriggerTags) == 1 {
			return "When someone gets the tag " + d.TriggerTags[0]
		}
		return "When someone gets the tag " + orList(d.TriggerTags, "or")
	case denmaTrigOpens:
		return "When someone opens " + d.CampaignName
	case denmaTrigClicks:
		return "When someone clicks a link in " + d.CampaignName
	}
	return ""
}

// Wait is the automation's wait, in words: "right away", "after 3 days".
func (d denmaAutomation) Wait() string {
	n, unit := d.DelayMinutes, "minute"
	switch {
	case n == 0:
		return "right away"
	case n%1440 == 0:
		n, unit = n/1440, "day"
	case n%60 == 0:
		n, unit = n/60, "hour"
	}
	if n != 1 {
		unit += "s"
	}
	return fmt.Sprintf("after %d %s", n, unit)
}

// Conditions is the automation's conditions, in words, or "".
func (d denmaAutomation) Conditions() string {
	var out []string
	if len(d.IfTags) > 0 {
		out = append(out, "only if they have the tag "+orList(d.IfTags, "or"))
	}
	if len(d.UnlessTags) > 0 {
		out = append(out, "not if they have the tag "+orList(d.UnlessTags, "or"))
	}
	return strings.Join(out, ", ")
}

// ActionList is what the automation does, in words, each one a phrase: "add
// to A", "send the e-mail Welcome" (with mail, its e-mail too).
func (d denmaAutomation) ActionList(mail bool) []string {
	var out []string
	if len(d.AddListNames) > 0 {
		out = append(out, "added to "+orList(d.AddListNames, "and"))
	}
	if len(d.RemoveListNames) > 0 {
		out = append(out, "removed from "+orList(d.RemoveListNames, "and"))
	}
	if len(d.AddTags) > 0 {
		out = append(out, "tagged "+orList(d.AddTags, "and"))
	}
	if len(d.RemoveTags) > 0 {
		out = append(out, "untagged "+orList(d.RemoveTags, "and"))
	}
	if mail && d.SendEmail {
		out = append(out, "sent the e-mail "+d.TemplateName)
	}
	return out
}

// Summary is the automation in words: "When someone joins A, right away:
// added to B; sent the e-mail Welcome."
func (d denmaAutomation) Summary() string {
	s := d.Trigger() + ", " + d.Wait()
	if c := d.Conditions(); c != "" {
		s += ", " + c
	}
	if d.EachTime {
		s += " (each time)"
	}
	return s + ": " + strings.Join(d.ActionList(true), "; ") + "."
}
