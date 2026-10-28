package main

// denma: an import's own options, on the import page (Subscribers ->
// Import), and the record of each import (denma_imports, kept for good).
//
//   - Run automations (run_automations): an import's joins and tags start
//     the center's automations only if it's ticked. Otherwise
//     denma_log_join and denma_log_tags (cmd/denma_automations.go) don't log
//     them. People imported are like anyone else afterwards: what happens
//     to them later starts automations, e-mails included.
//   - Keep the consent dates in the file (keep_consent): a row whose
//     attributes give consent_at (a date that reads, not after the import)
//     and consent_source keeps them as the subscriber's consent record, with
//     consent_import, the import's number. A subscriber the center has gets
//     it only if they have none, and only with "Overwrite user info" (the
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
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/internal/subimporter"
	"github.com/knadh/listmonk/models"
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
    ADD COLUMN IF NOT EXISTS consents INTEGER NOT NULL DEFAULT 0;

-- An option of the import being written (denma.import_id); false outside
-- one.
CREATE OR REPLACE FUNCTION denma_import_opt(opt TEXT) RETURNS BOOLEAN
LANGUAGE sql STABLE SET search_path FROM CURRENT AS $$
    SELECT denma_is_import() AND COALESCE((
        SELECT CASE opt WHEN 'automations' THEN i.automations WHEN 'consent' THEN i.consent END
        FROM denma_imports i WHERE i.id = NULLIF(current_setting('denma.import_id', true), '')::INT), false);
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

// denmaImportStart records an import (opt, as POST /api/import/subscribers
// takes it) in denma_imports, and gives its session the number.
func (a *App) denmaImportStart(c echo.Context, opt *subimporter.SessionOpt) error {
	if opt.Mode != subimporter.ModeSubscribe {
		if opt.DenmaConsent {
			return echo.NewHTTPError(http.StatusBadRequest, "Consent records are kept only when subscribing.")
		}
		opt.DenmaAutomations = false
	}
	if _, ok := denmaMCPOf(c); ok && opt.DenmaConsent {
		return echo.NewHTTPError(http.StatusForbidden, "An MCP key can't import consent records.")
	}
	user := auth.GetUser(c)
	var uid *int
	if user.ID > 0 {
		uid = &user.ID
	}
	if err := a.db.Get(&opt.DenmaImportID, `INSERT INTO denma_imports (user_id, username, file, mode, automations, consent)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, uid, user.Username, opt.Filename, opt.Mode, opt.DenmaAutomations, opt.DenmaConsent); err != nil {
		a.log.Printf("denma: error recording an import: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error starting the import.")
	}
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
	denmaConsentNotOverwritten = `already in the center: only "Overwrite user info" gives them one`
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

// denmaConsentImport is where a subscriber's consent record came from, when
// an import gave it: their page shows it.
type denmaConsentImport struct {
	At         time.Time
	Source     string
	File       string    `db:"file"`
	Username   string    `db:"username"`
	ImportedAt time.Time `db:"created_at"`
}

// denmaConsentImportOf returns the import a subscriber's consent record
// (attribs) came from, or nil.
func (a *App) denmaConsentImportOf(attribs models.JSON) *denmaConsentImport {
	id, ok := attribs["consent_import"].(float64)
	if !ok {
		return nil
	}
	var c denmaConsentImport
	if err := a.db.Get(&c, `SELECT file, username, created_at FROM denma_imports WHERE id = $1`, int(id)); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			a.log.Printf("denma: error reading a consent record's import: %v", err)
		}
		return nil
	}
	c.At, _ = time.Parse(time.RFC3339, fmt.Sprint(attribs["consent_at"]))
	c.Source = fmt.Sprint(attribs["consent_source"])
	return &c
}
