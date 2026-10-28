package main

// denma: an import's own options, on the import page (Subscribers ->
// Import), and the record of each import (denma_imports, kept for good).
//
//   - Run automations (run_automations): an import's joins and tags start
//     the center's automations only if it's ticked. Otherwise
//     denma_log_join and denma_log_tags (cmd/denma_automations.go) don't log
//     them. People imported are like anyone else afterwards: what happens
//     to them later starts automations, e-mails included.
//
// Each batch of the import's rows is written with the import's number in
// denma.import_id (internal/subimporter/denma.go), which the triggers read
// (denma_import_opt). Without it, an import's rows get the defaults: no
// automations.

import (
	"net/http"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/internal/subimporter"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

// denmaImportsSQL makes the record of imports, and the function the triggers
// read an import's options with (in denmaFeaturesSQL).
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

-- An option of the import being written (denma.import_id); false outside
-- one.
CREATE OR REPLACE FUNCTION denma_import_opt(opt TEXT) RETURNS BOOLEAN
LANGUAGE sql STABLE SET search_path FROM CURRENT AS $$
    SELECT denma_is_import() AND COALESCE((
        SELECT CASE opt WHEN 'automations' THEN i.automations END
        FROM denma_imports i WHERE i.id = NULLIF(current_setting('denma.import_id', true), '')::INT), false);
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
		opt.DenmaAutomations = false
	}
	user := auth.GetUser(c)
	var uid *int
	if user.ID > 0 {
		uid = &user.ID
	}
	if err := a.db.Get(&opt.DenmaImportID, `INSERT INTO denma_imports (user_id, username, file, mode, automations)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`, uid, user.Username, opt.Filename, opt.Mode, opt.DenmaAutomations); err != nil {
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
