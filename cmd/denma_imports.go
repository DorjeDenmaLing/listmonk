package main

// denma: an import's own options, on the import page (Subscribers ->
// Import), and the record of each import (denma_imports, kept for good).
//
//   - Run automations (run_automations): an import's joins and tags start
//     the center's automations only if it's ticked. Otherwise
//     denma_log_join and denma_log_tags (cmd/denma_automations.go) don't log
//     them. People imported are like anyone else afterwards: what happens
//     to them later starts automations, e-mails included.
//   - Why they're blocklisted (blocklist_reason, blocklist mode): an admin's
//     choice ("", which an admin can undo), "unsubscribed" (an opt-out, as
//     their own unsubscribe: only they can subscribe again;
//     cmd/denma_optouts.go), or "bounced" (marked so in this center only,
//     as a bounce is; an admin can still enable one on purpose). The file's
//     unsubscribed_at or bounced_at dates it, or else the import's day.
//     Someone the center has who consented here after the file's
//     unsubscribed_at is left as they are (denma_import_blocklist_status).
//   - Keep the consent dates in the file (keep_consent): a row whose
//     attributes give consent_at (a date that reads, not after the import)
//     and consent_source keeps them as the subscriber's consent record, with
//     consent_import, the import's number. A subscriber the center has gets
//     it only if they have none, and only with "Update names and attributes" (the
//     importer writes their attributes only then). Rows without a good one
//     are still imported, without it. Not in blocklist mode, nor through an
//     MCP key.
//
// Each batch of the import's rows is written with the import's number in
// denma.import_id (internal/subimporter/denma.go), which the triggers read
// (denma_import_opt). Without it, an import's rows get the defaults: no
// automations, no consent records.

import (
	"database/sql"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/internal/subimporter"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

// denmaImportsSQL makes the record of imports, and the functions the
// triggers read an import's options and consent records with (in
// denmaFeaturesSQL, before the triggers).
const denmaImportsSQL = `
CREATE TABLE IF NOT EXISTS denma_imports (
    id          SERIAL PRIMARY KEY,
    created_at  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    user_id     INTEGER NULL,
    username    TEXT NOT NULL DEFAULT '',
    file        TEXT NOT NULL DEFAULT '',
    mode        TEXT NOT NULL DEFAULT '',
    automations BOOLEAN NOT NULL DEFAULT false
);
ALTER TABLE denma_imports ADD COLUMN IF NOT EXISTS consent BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS consents INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS blocklist_reason TEXT NOT NULL DEFAULT '';

-- An option of the import being written (denma.import_id); false outside
-- one.
CREATE OR REPLACE FUNCTION denma_import_opt(opt TEXT) RETURNS BOOLEAN
LANGUAGE sql STABLE SET search_path FROM CURRENT AS $$
    SELECT denma_is_import() AND COALESCE((
        SELECT CASE opt WHEN 'automations' THEN i.automations WHEN 'consent' THEN i.consent END
        FROM denma_imports i WHERE i.id = NULLIF(current_setting('denma.import_id', true), '')::INT), false);
$$;

-- Why the import being written blocklists them: '' (an admin's choice),
-- 'unsubscribed' or 'bounced'.
CREATE OR REPLACE FUNCTION denma_import_reason() RETURNS TEXT
LANGUAGE sql STABLE SET search_path FROM CURRENT AS $$
    SELECT CASE WHEN denma_is_import() THEN COALESCE((SELECT i.blocklist_reason FROM denma_imports i
        WHERE i.id = NULLIF(current_setting('denma.import_id', true), '')::INT), '') ELSE '' END;
$$;

-- A date as an import's file gives it (2019-03-04, 2019-03-04 15:22:10,
-- with or without a zone; without, UTC), or NULL.
CREATE OR REPLACE FUNCTION denma_import_date(v TEXT) RETURNS TIMESTAMPTZ
LANGUAGE plpgsql STABLE SET TimeZone = 'UTC' AS $$
BEGIN
    IF v !~ '^\s*\d{4}-\d{2}-\d{2}([ T]\d{2}:\d{2}(:\d{2}(\.\d+)?)?)?\s*(Z|[+-]\d{2}(:?\d{2})?)?\s*$' THEN
        RETURN NULL;
    END IF;
    RETURN btrim(v)::TIMESTAMPTZ;
EXCEPTION WHEN others THEN
    RETURN NULL;
END;
$$;

-- An imported consent date as kept, or NULL if it doesn't read, is after
-- now or before 1990.
CREATE OR REPLACE FUNCTION denma_import_consent_at(v TEXT) RETURNS TEXT
LANGUAGE sql STABLE SET search_path FROM CURRENT AS $$
    SELECT CASE WHEN t IS NULL OR t > NOW() OR t < '1990-01-01'::TIMESTAMPTZ THEN NULL
        ELSE to_char(t AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') END
    FROM denma_import_date(v) t;
$$;

-- The consent record an imported row's attributes give, with the import's
-- number, or {} unless both its date and its source are good.
CREATE OR REPLACE FUNCTION denma_import_consent(a JSONB) RETURNS JSONB
LANGUAGE sql STABLE SET search_path FROM CURRENT AS $$
    SELECT CASE WHEN jsonb_typeof(a) = 'object' AND jsonb_typeof(a->'consent_at') = 'string'
            AND jsonb_typeof(a->'consent_source') = 'string' AND length(btrim(a->>'consent_source')) BETWEEN 1 AND 200
            AND denma_import_consent_at(a->>'consent_at') IS NOT NULL
            AND NULLIF(current_setting('denma.import_id', true), '') IS NOT NULL
        THEN jsonb_build_object('consent_at', denma_import_consent_at(a->>'consent_at'),
            'consent_source', btrim(a->>'consent_source'),
            'consent_import', current_setting('denma.import_id', true)::INT)
        ELSE '{}'::JSONB END;
$$;

-- The attributes recording a bounce, for an import of addresses that
-- bounced, dated by the file's bounced_at (attributes a) if it's good; {}
-- for another import.
CREATE OR REPLACE FUNCTION denma_import_bounced(a JSONB) RETURNS JSONB
LANGUAGE sql STABLE SET search_path FROM CURRENT AS $$
    SELECT CASE WHEN denma_import_reason() = 'bounced' THEN jsonb_build_object(
            'auto_blocklist_reason', 'bounced before it was imported',
            'auto_blocklist_time', COALESCE(CASE WHEN jsonb_typeof(a) = 'object' THEN denma_import_consent_at(a->>'bounced_at') END,
                to_char(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')))
        ELSE '{}'::JSONB END;
$$;

-- The latest consent a subscriber's attributes record, or NULL.
CREATE OR REPLACE FUNCTION denma_latest_consent(a JSONB) RETURNS TIMESTAMPTZ
LANGUAGE sql STABLE SET search_path FROM CURRENT AS $$
    SELECT MAX(t) FROM (
        SELECT denma_import_date(a->>k) t FROM unnest(ARRAY['consent_at', 'consent_confirmed_at', 'resubscribed_at']) k
        UNION ALL
        SELECT denma_import_date(c->>'at') FROM jsonb_array_elements(
            CASE WHEN jsonb_typeof(a->'consents') = 'array' THEN a->'consents' ELSE '[]'::JSONB END) c
    ) d WHERE jsonb_typeof(a) = 'object';
$$;

-- The file's date for the reason, of the subscriber a blocklist import is
-- updating (denma.import_file, from upsert-blocklist-subscriber,
-- cmd/denma_queries.go), as kept, or NULL.
CREATE OR REPLACE FUNCTION denma_import_file_date() RETURNS TEXT
LANGUAGE sql STABLE SET search_path FROM CURRENT AS $$
    SELECT denma_import_consent_at(f->>CASE denma_import_reason()
            WHEN 'unsubscribed' THEN 'unsubscribed_at' WHEN 'bounced' THEN 'bounced_at' END)
    FROM (SELECT NULLIF(current_setting('denma.import_file', true), '')::JSONB f) x
    WHERE jsonb_typeof(f) = 'object';
$$;
`

// denmaImportBlocklistSQL records why a blocklist import blocklists them (at
// the end of denmaFeaturesSQL, after the opt-outs): a subscriber the center
// had is marked bounced, if it isn't already (one it adds is, by
// denma_mark_imported_subscriber); and someone who unsubscribed gets their
// opt-out (cmd/denma_optouts.go), dated by the file's unsubscribed_at if
// it's good, unless they have one. A subscriber the center had is dated by
// denma_import_file_date, and left as they are if the file says they
// unsubscribed and they consented here since.
const denmaImportBlocklistSQL = `
CREATE OR REPLACE FUNCTION denma_import_blocklist_status() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    IF denma_import_reason() = 'unsubscribed'
        AND COALESCE(denma_latest_consent(OLD.attribs) > COALESCE(denma_import_file_date()::TIMESTAMPTZ, NOW()), false) THEN
        RETURN NULL;
    END IF;
    IF denma_import_reason() = 'bounced' AND NOT (jsonb_typeof(NEW.attribs) = 'object' AND NEW.attribs ? 'auto_blocklist_reason') THEN
        NEW.attribs := (CASE WHEN jsonb_typeof(NEW.attribs) = 'object' THEN NEW.attribs ELSE '{}'::JSONB END)
            || denma_import_bounced(jsonb_build_object('bounced_at', denma_import_file_date()));
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS denma_import_blocklist_status ON subscribers;
CREATE TRIGGER denma_import_blocklist_status
    BEFORE UPDATE OF status ON subscribers
    FOR EACH ROW WHEN (NEW.status = 'blocklisted') EXECUTE FUNCTION denma_import_blocklist_status();

CREATE OR REPLACE FUNCTION denma_import_optout() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    IF denma_import_reason() = 'unsubscribed' THEN
        INSERT INTO denma_optouts (email_hash, canon_hash, kind, source, created_at)
        VALUES (denma_email_hash(NEW.email), denma_canon_hash(NEW.email), 'unsubscribed', 'import',
            COALESCE(CASE WHEN TG_OP = 'INSERT' AND jsonb_typeof(NEW.attribs) = 'object'
                THEN denma_import_consent_at(NEW.attribs->>'unsubscribed_at')
                WHEN TG_OP = 'UPDATE' THEN denma_import_file_date() END::TIMESTAMPTZ, NOW()))
        ON CONFLICT (email_hash) DO NOTHING;
    END IF;
    RETURN NULL;
END;
$$;
DROP TRIGGER IF EXISTS denma_import_optout ON subscribers;
CREATE TRIGGER denma_import_optout
    AFTER INSERT OR UPDATE OF status ON subscribers
    FOR EACH ROW WHEN (NEW.status = 'blocklisted') EXECUTE FUNCTION denma_import_optout();
`

// denmaImportEventsSQL leaves out, once (features version 51), the joins and
// tags imports logged before they could start automations only if asked:
// those logged in the second the subscriber was marked imported, by the
// same statement.
const denmaImportEventsSQL = `
DO $$ BEGIN
    IF to_regclass('denma_list_joins') IS NOT NULL THEN
        DELETE FROM denma_list_joins j USING subscribers s
        WHERE s.id = j.subscriber_id AND denma_is_imported(s.attribs)
            AND date_trunc('second', j.joined_at) = CASE WHEN s.attribs->>'imported_at' ~ '^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$'
                THEN (s.attribs->>'imported_at')::TIMESTAMPTZ END;
    END IF;
    IF to_regclass('denma_tag_log') IS NOT NULL THEN
        DELETE FROM denma_tag_log t USING subscribers s
        WHERE s.id = t.subscriber_id AND denma_is_imported(s.attribs)
            AND date_trunc('second', t.added_at) = CASE WHEN s.attribs->>'imported_at' ~ '^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$'
                THEN (s.attribs->>'imported_at')::TIMESTAMPTZ END;
    END IF;
END $$;`

// denmaImportBounced is a blocklist import's reason for addresses that
// bounced.
const denmaImportBounced = "bounced"

// denmaImportStart records an import (opt, as POST /api/import/subscribers
// takes it) in denma_imports, and gives its session the number.
func (a *App) denmaImportStart(c echo.Context, opt *subimporter.SessionOpt) error {
	if opt.Mode != subimporter.ModeSubscribe {
		if opt.DenmaConsent {
			return echo.NewHTTPError(http.StatusBadRequest, "Consent records are kept only when subscribing.")
		}
		opt.DenmaAutomations = false
	} else if opt.DenmaBlockReason != "" {
		return echo.NewHTTPError(http.StatusBadRequest, "Why they're blocklisted is for a blocklist import.")
	}
	if !slices.Contains([]string{"", denmaOptOutUnsubscribed, denmaImportBounced}, opt.DenmaBlockReason) {
		return echo.NewHTTPError(http.StatusBadRequest, "Unknown reason for blocklisting.")
	}
	if _, ok := denmaMCPOf(c); ok && opt.DenmaConsent {
		return echo.NewHTTPError(http.StatusForbidden, "An MCP key can't import consent records.")
	}
	user := auth.GetUser(c)
	var uid *int
	if user.ID > 0 {
		uid = &user.ID
	}
	if err := a.db.Get(&opt.DenmaImportID, `INSERT INTO denma_imports (user_id, username, file, mode, automations, consent, blocklist_reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`, uid, user.Username, opt.Filename, opt.Mode, opt.DenmaAutomations, opt.DenmaConsent,
		opt.DenmaBlockReason); err != nil {
		a.log.Printf("denma: error recording an import: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error starting the import.")
	}
	denmaKept.drop(denmaKeptKey(a.ko.String("denma.center"), user.ID)) // forget its preview's kept file (cmd/denma_importkeep.go)
	return nil
}

// denmaImportAutomations are the names of the active automations an import
// would start: those joining its lists (listIDs, with the subscription
// status it gives) or getting its tags start.
func (a *App) denmaImportAutomations(listIDs []int, status string, tags []string) ([]string, error) {
	names := []string{}
	var ok bool
	if err := a.db.Get(&ok, `SELECT to_regclass('denma_automations') IS NOT NULL`); err != nil || !ok {
		return names, err
	}
	err := a.db.Select(&names, `SELECT a.name FROM denma_automations a
		WHERE a.active AND (
			(a.trigger_type IN ('joins', 'confirms') AND EXISTS (SELECT 1 FROM denma_automation_lists al JOIN lists l ON l.id = al.list_id
				WHERE al.automation_id = a.id AND al.kind = 'trigger' AND al.list_id = ANY($1) AND (l.optin <> 'double' OR $2 = 'confirmed')))
			OR (a.trigger_type = 'tagged' AND a.trigger_tags && $3::TEXT[]))
		ORDER BY a.name`, pq.Array(listIDs), status, pq.Array(tags))
	return names, err
}

// denmaImportConsentGiven is the consent record a row's attributes give.
type denmaImportConsentGiven struct {
	at, source string
	hasAt      bool // consent_at given, as text
	hasSource  bool // consent_source given, as text that isn't blank
}

func denmaImportConsentOf(attribs map[string]any) denmaImportConsentGiven {
	var c denmaImportConsentGiven
	if v, ok := attribs["consent_at"].(string); ok && strings.TrimSpace(v) != "" {
		c.at, c.hasAt = v, true
	}
	if v, ok := attribs["consent_source"].(string); ok && strings.Trim(v, " ") != "" {
		c.source, c.hasSource = strings.Trim(v, " "), true
	}
	return c
}

// denmaImportConsents is what an import keeping the file's consent records
// does with them: how many people keep theirs, and why the rest don't.
type denmaImportConsents struct {
	Kept    int                  `json:"kept"`
	LeftOut []denmaImportLeftOut `json:"left_out"`
}

type denmaImportLeftOut struct {
	Why  string `json:"why"`
	Rows int    `json:"rows"`
}

// Why a row's consent record isn't kept, as the preview says it.
const (
	denmaConsentNoDate         = "no consent_at"
	denmaConsentNoSource       = "no consent_source"
	denmaConsentLongSource     = "a consent_source over 200 characters"
	denmaConsentBadDate        = "a consent_at that isn't a date like 2019-03-04 or 2019-03-04 15:22:10"
	denmaConsentFuture         = "a consent_at after today"
	denmaConsentEarly          = "a consent_at before 1990"
	denmaConsentHasOne         = "already in the center with a consent record, which they keep"
	denmaConsentNotOverwritten = `already in the center: only "Update names and attributes" gives them one`
)

// denmaImportConsentsPreview fills in the consent records the import would
// keep (p.Consent), and each sample row's, as denma_import_consent and
// denma_mark_imported_subscriber (cmd/denma_features.go) would.
func (a *App) denmaImportConsentsPreview(p *denmaImportPreview, addrs []string, entries map[string]*denmaImportEntry, overwrite bool) error {
	dates := []string{}
	for _, e := range entries {
		if e.consent.hasAt {
			dates = append(dates, e.consent.at)
		}
	}
	for _, r := range p.Sample {
		if r.consent.hasAt {
			dates = append(dates, r.consent.at)
		}
	}
	var read []struct {
		V      string         `db:"v"`
		At     sql.NullString `db:"at"`
		Unread bool           `db:"unread"`
		Future bool           `db:"future"`
	}
	if err := a.db.Select(&read, `SELECT v, denma_import_consent_at(v) AS at, denma_import_date(v) IS NULL AS unread,
		COALESCE(denma_import_date(v) > NOW(), false) AS future FROM unnest($1::TEXT[]) v`, pq.Array(dates)); err != nil {
		return err
	}
	kept := map[string]string{}
	whyDate := map[string]string{}
	for _, d := range read {
		switch {
		case d.At.Valid:
			kept[d.V] = d.At.String
		case d.Unread:
			whyDate[d.V] = denmaConsentBadDate
		case d.Future:
			whyDate[d.V] = denmaConsentFuture
		default:
			whyDate[d.V] = denmaConsentEarly
		}
	}
	// what the record kept is, or why there's none ("" for a row without one).
	what := func(c denmaImportConsentGiven, existing, hasConsent bool) (string, string) {
		switch {
		case !c.hasAt && !c.hasSource:
			return "", ""
		case !c.hasAt:
			return "", denmaConsentNoDate
		case !c.hasSource:
			return "", denmaConsentNoSource
		case utf8.RuneCountInString(c.source) > 200:
			return "", denmaConsentLongSource
		case whyDate[c.at] != "":
			return "", whyDate[c.at]
		case existing && hasConsent:
			return "", denmaConsentHasOne
		case existing && !overwrite:
			return "", denmaConsentNotOverwritten
		}
		return kept[c.at] + " · " + c.source, ""
	}

	out := &denmaImportConsents{LeftOut: []denmaImportLeftOut{}}
	left := map[string]int{}
	for _, addr := range addrs {
		e := entries[addr]
		rec, why := what(e.consent, e.existing, e.hasConsent)
		if rec != "" {
			out.Kept++
		} else if why != "" {
			left[why]++
		}
	}
	for _, why := range []string{denmaConsentNoDate, denmaConsentNoSource, denmaConsentLongSource, denmaConsentBadDate,
		denmaConsentFuture, denmaConsentEarly, denmaConsentHasOne, denmaConsentNotOverwritten} {
		if left[why] > 0 {
			out.LeftOut = append(out.LeftOut, denmaImportLeftOut{Why: why, Rows: left[why]})
		}
	}
	p.Consent = out

	// The sample shows each row's, and the two attributes aren't left out.
	for i := range p.Sample {
		r := &p.Sample[i]
		e := entries[r.Email]
		rec, why := what(r.consent, r.Existing, e != nil && e.hasConsent)
		if why != "" {
			rec = "Not kept: " + why
		}
		r.Consent = rec
		r.Dropped = slices.DeleteFunc(r.Dropped, denmaIsImportConsentKey)
	}
	for i := range p.Attribs {
		if denmaIsImportConsentKey(p.Attribs[i].Key) {
			p.Attribs[i].Dropped = false
		}
	}
	return nil
}

func denmaIsImportConsentKey(k string) bool {
	return k == "consent_at" || k == "consent_source"
}

// denmaImportDates is how a blocklist import with a reason dates them: by
// the file's date (Key), or the import's day.
type denmaImportDates struct {
	Key     string `json:"key"`
	Dated   int    `json:"dated" db:"dated"`
	Undated int    `json:"undated" db:"undated"` // no date, or one that isn't good
	Kept    int    `json:"kept" db:"kept"`       // the center's, who consented here since: left as they are
}

// denmaImportDateKeys are the attributes dating a blocklist import's
// reasons.
var denmaImportDateKeys = map[string]string{denmaOptOutUnsubscribed: "unsubscribed_at", denmaImportBounced: "bounced_at"}

func denmaImportDatesOf(attribs map[string]any) map[string]string {
	out := map[string]string{}
	for _, k := range denmaImportDateKeys {
		if v, ok := attribs[k].(string); ok {
			out[k] = v
		}
	}
	return out
}

// denmaImportDatesPreview fills in p.BlockDates, as
// denma_import_blocklist_status and denma_import_optout would date them.
func (a *App) denmaImportDatesPreview(p *denmaImportPreview, addrs []string, entries map[string]*denmaImportEntry, reason string) error {
	key := denmaImportDateKeys[reason]
	vals := make([]string, len(addrs))
	for i, addr := range addrs {
		vals[i] = entries[addr].dates[key]
	}
	d := &denmaImportDates{Key: key}
	if err := a.db.Get(d, `SELECT COUNT(*) FILTER (WHERE kept) AS kept,
		COUNT(*) FILTER (WHERE NOT kept AND at IS NOT NULL) AS dated,
		COUNT(*) FILTER (WHERE NOT kept AND at IS NULL) AS undated
		FROM (SELECT f.at, $3 = 'unsubscribed' AND COALESCE(denma_latest_consent(s.attribs) > COALESCE(f.at, NOW()), false) AS kept
			FROM (SELECT email, denma_import_consent_at(v)::TIMESTAMPTZ AS at FROM unnest($1::TEXT[], $2::TEXT[]) u(email, v)) f
			LEFT JOIN subscribers s ON LOWER(s.email) = f.email) x`,
		pq.Array(addrs), pq.Array(vals), reason); err != nil {
		return err
	}
	p.BlockDates = d
	return nil
}
