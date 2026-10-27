package main

// denma: what automations' e-mails led to, for Analytics -> Automations and
// Explore (views/denma-analytics.js): opens, clicks, unsubscribes, bounces
// and complaints, per automation and per e-mail of a series (its step).
//
//   - Opens: each automation e-mail has the tracking pixel ({{ TrackView }},
//     added if its template hasn't it, as campaigns', models.DenmaWithTrackView),
//     /automation/<uuid>/<subscriber>/px.png?step=N, recorded in
//     denma_automation_views (by whom only with individual tracking).
//   - Clicks: tracked links (cmd/denma_automations.go) carry ?step=N too;
//     clicks recorded before have no step and count only in an automation's
//     total.
//   - Unsubscribes: denmaUnsubscribe is given the automation's UUID, and a
//     new opt-out counts for it (denma_automation_unsubs), as for campaigns.
//   - Bounces and complaints: automation e-mails carry the automation's UUID
//     in X-Listmonk-Campaign, which bounce notifications give back as a
//     campaign's (no campaign has it, so listmonk records the bounce without
//     one); it counts for the automation (denma_automation_bounces).
//
// Unsubscribes and bounces keep only the automation, its e-mail and when
// that e-mail was sent (the person's latest from it), not who. Each was
// counted from when its table was made (denma_stat_starts).
//
// A report is about the e-mails sent in the period: how many, to how many
// people, and of those people, how many opened and clicked them (at any time
// since), unsubscribed or bounced. With individual tracking off, opens and
// clicks aren't tied to people: they're every open and click in the period.

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

// denmaAutoReportsSQL makes the tables (in denmaAutoTables).
const denmaAutoReportsSQL = `
CREATE TABLE IF NOT EXISTS denma_stat_starts (
    stat  TEXT PRIMARY KEY,
    since TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS denma_automation_views (
    id            BIGSERIAL PRIMARY KEY,
    automation_id INTEGER NOT NULL REFERENCES denma_automations(id) ON DELETE CASCADE,
    step          INTEGER NOT NULL,
    subscriber_id INTEGER NULL REFERENCES subscribers(id) ON DELETE SET NULL,
    created_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS denma_automation_views_auto ON denma_automation_views (automation_id, created_at);
CREATE INDEX IF NOT EXISTS denma_automation_views_sub ON denma_automation_views (automation_id, subscriber_id);
ALTER TABLE denma_automation_clicks ADD COLUMN IF NOT EXISTS step INTEGER NULL; -- NULL: before steps were recorded
CREATE INDEX IF NOT EXISTS denma_automation_clicks_at ON denma_automation_clicks (automation_id, created_at);
CREATE INDEX IF NOT EXISTS denma_automation_clicks_sub ON denma_automation_clicks (automation_id, subscriber_id);
CREATE TABLE IF NOT EXISTS denma_automation_unsubs (
    automation_id INTEGER NOT NULL REFERENCES denma_automations(id) ON DELETE CASCADE,
    step          INTEGER NULL,
    sent_at       TIMESTAMP WITH TIME ZONE NULL, -- the e-mail's; NULL if none was found
    created_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS denma_automation_unsubs_auto ON denma_automation_unsubs (automation_id);
CREATE TABLE IF NOT EXISTS denma_automation_bounces (
    automation_id INTEGER NOT NULL REFERENCES denma_automations(id) ON DELETE CASCADE,
    step          INTEGER NULL,
    type          TEXT NOT NULL, -- hard, soft, complaint
    sent_at       TIMESTAMP WITH TIME ZONE NULL,
    created_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS denma_automation_bounces_auto ON denma_automation_bounces (automation_id);
INSERT INTO denma_stat_starts (stat) VALUES ('automation_views'), ('automation_unsubs'), ('automation_bounces')
    ON CONFLICT (stat) DO NOTHING;`

// denmaAutoLastSentSQL is automation au's latest e-mail to the subscriber s
// who matches sub: its step and when it was sent, or NULLs.
func denmaAutoLastSentSQL(sub string) string {
	last := func(col string) string {
		return `(SELECT g.` + col + ` FROM denma_automation_log g JOIN subscribers s ON s.id = g.subscriber_id
        WHERE g.automation_id = au.id AND ` + sub + ` AND g.status IN ('sent', 'uncertain') ORDER BY g.at DESC LIMIT 1)`
	}
	return last("step") + " AS step, " + last("at") + " AS sent_at"
}

// denmaAutoUnsubSQL counts an opt-out for the automation whose e-mail it
// came from, if $1 is an automation's UUID ($2: the subscriber's).
var denmaAutoUnsubSQL = `INSERT INTO denma_automation_unsubs (automation_id, step, sent_at)
    SELECT au.id, ` + denmaAutoLastSentSQL(`s.uuid::TEXT = $2`) + ` FROM denma_automations au WHERE au.uuid::TEXT = $1`

// denmaAutoStepOK is whether $step is one of automation au's e-mails.
func denmaAutoStepOK(step string) string {
	return `(` + step + ` = 1 OR EXISTS (SELECT 1 FROM denma_automation_steps st WHERE st.automation_id = au.id AND st.step = ` + step + `))`
}

// denmaAutoStepParam is the ?step= of an automation e-mail's link (1 if none).
func denmaAutoStepParam(c echo.Context) int {
	step, _ := strconv.Atoi(c.QueryParam("step"))
	return max(step, 1)
}

// denmaAutoTrackView is the tracking pixel in an automation's e-mail to a
// subscriber, as listmonk's TrackView makes campaigns'.
func (a *App) denmaAutoTrackView(autoUUID, subUUID string, step int) string {
	if a.cfg.Privacy.DisableTracking || autoUUID == dummyUUID || subUUID == dummyUUID {
		return "" // tracking off, a test or a sample
	}
	if !a.cfg.Privacy.IndividualTracking {
		subUUID = dummyUUID
	}
	u := fmt.Sprintf("%s/automation/%s/%s/px.png", strings.TrimSuffix(a.urlCfg.RootURL, "/"), autoUUID, subUUID)
	if step > 1 {
		u += "?step=" + strconv.Itoa(step)
	}
	return fmt.Sprintf(`<img src="%s" width="1" height="1" style="display:none;max-height:0;max-width:0;opacity:0" alt="">`, u)
}

// DenmaAutomationView records an open of an automation's e-mail (by whom
// only with individual tracking) and returns the pixel: an automation that
// sends e-mail, one of its e-mails, and, with individual tracking, a
// subscriber it ran for. Anything else isn't recorded.
func (a *App) DenmaAutomationView(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "no-cache")
	if a.cfg.Privacy.DisableTracking {
		return c.Blob(http.StatusOK, "image/png", pixelPNG)
	}
	var (
		sub     = c.Param("subUUID")
		anyone  = !a.cfg.Privacy.IndividualTracking
		stepArg = denmaAutoStepParam(c)
	)
	if !anyone && sub == dummyUUID {
		return c.Blob(http.StatusOK, "image/png", pixelPNG)
	}
	if _, err := a.db.Exec(`INSERT INTO denma_automation_views (automation_id, step, subscriber_id)
		SELECT au.id, $3, CASE WHEN $4 THEN NULL ELSE s.id END
		FROM denma_automations au LEFT JOIN subscribers s ON NOT $4 AND s.uuid::TEXT = $2
		WHERE au.uuid::TEXT = $1 AND au.send_email AND `+denmaAutoStepOK("$3")+`
			AND ($4 OR EXISTS (SELECT 1 FROM denma_automation_sends d WHERE d.automation_id = au.id AND d.subscriber_id = s.id))`,
		c.Param("autoUUID"), sub, stepArg, anyone); err != nil {
		a.log.Printf("denma: error recording an automation's open: %v", err)
	}
	return c.Blob(http.StatusOK, "image/png", pixelPNG)
}

// denmaAutoBounce is what a bounce counts for: the automation whose UUID it
// gives as its campaign's, and that automation's latest e-mail to the
// subscriber. ID 0 if it's no automation's.
type denmaAutoBounce struct {
	ID     int        `db:"id"`
	Step   *int       `db:"step"`
	SentAt *time.Time `db:"sent_at"`
}

// denmaRecordBounce records a bounce with record (listmonk's), and counts it
// for its automation if it's one's: read first, as listmonk's may delete the
// subscriber; counted only once it's recorded, as a bounce that fails is
// tried again.
func denmaRecordBounce(db *sqlx.DB, b models.Bounce, record func(models.Bounce) error) error {
	var au denmaAutoBounce
	if b.CampaignUUID != "" && reUUID.MatchString(b.CampaignUUID) {
		// The subscriber by their UUID, or else their address (most providers give only that).
		sub := `(s.uuid::TEXT = $2 OR ($2 = '' AND LOWER(s.email) = LOWER($3)))`
		if err := db.Get(&au, `SELECT au.id, `+denmaAutoLastSentSQL(sub)+`
			FROM denma_automations au WHERE au.uuid::TEXT = $1`, b.CampaignUUID, b.SubscriberUUID, b.Email); err != nil && !errors.Is(err, sql.ErrNoRows) {
			lo.Printf("denma: error finding a bounce's automation: %v", err)
		}
	}
	if err := record(b); err != nil {
		return err
	}
	if au.ID > 0 {
		if _, err := db.Exec(`INSERT INTO denma_automation_bounces (automation_id, step, type, sent_at) VALUES ($1, $2, $3, $4)`,
			au.ID, au.Step, b.Type, au.SentAt); err != nil {
			lo.Printf("denma: error counting a bounce for its automation: %v", err)
		}
	}
	return nil
}

// denmaAutoReportRow is an automation's figures, or one of its e-mails'
// (Step > 0), for the e-mails sent in a period.
type denmaAutoReportRow struct {
	AutomationID int    `db:"automation_id" json:"automation_id"`
	Step         int    `db:"step" json:"step"` // 0: the automation's total
	Sent         int    `db:"sent" json:"sent"` // e-mails
	People       int    `db:"people" json:"people"`
	Opened       int    `db:"opened" json:"opened"`   // people (individual tracking)
	Clicked      int    `db:"clicked" json:"clicked"` // people (individual tracking)
	Opens        int    `db:"opens" json:"opens"`     // every open in the period
	Clicks       int    `db:"clicks" json:"clicks"`   // every click in the period
	Unsubs       int    `db:"unsubs" json:"unsubscribes"`
	Bounces      int    `db:"bounces" json:"bounces"` // hard and soft
	Complaints   int    `db:"complaints" json:"complaints"`
	Name         string `db:"-" json:"name"`
	Subject      string `db:"-" json:"subject"`
}

// denmaAutoReportSQL is each automation's figures and each of its e-mails',
// for e-mails sent from $1 to $2 (opens and clicks by anyone: in that time).
const denmaAutoReportSQL = `
WITH sent AS (
    SELECT automation_id, step, subscriber_id, COUNT(*) AS n, MIN(at) AS first_at
    FROM denma_automation_log WHERE status IN ('sent', 'uncertain') AND at >= $1 AND at < $2
    GROUP BY automation_id, step, subscriber_id
),
people AS (
    SELECT s.automation_id, s.step, s.subscriber_id, s.n,
        EXISTS (SELECT 1 FROM denma_automation_views v WHERE v.automation_id = s.automation_id AND v.subscriber_id = s.subscriber_id
            AND v.step = s.step AND v.created_at >= s.first_at) AS opened,
        EXISTS (SELECT 1 FROM denma_automation_clicks k WHERE k.automation_id = s.automation_id AND k.subscriber_id = s.subscriber_id
            AND k.step = s.step AND k.created_at >= s.first_at) AS clicked
    FROM sent s
),
ev AS (
    SELECT automation_id, step, subscriber_id, n, opened, clicked, 0 AS opens, 0 AS clicks, 0 AS unsubs, 0 AS bounces, 0 AS complaints FROM people
    UNION ALL SELECT automation_id, step, NULL, 0, false, false, 1, 0, 0, 0, 0 FROM denma_automation_views WHERE created_at >= $1 AND created_at < $2
    UNION ALL SELECT automation_id, step, NULL, 0, false, false, 0, 1, 0, 0, 0 FROM denma_automation_clicks WHERE created_at >= $1 AND created_at < $2
    UNION ALL SELECT automation_id, step, NULL, 0, false, false, 0, 0, 1, 0, 0 FROM denma_automation_unsubs
        WHERE COALESCE(sent_at, created_at) >= $1 AND COALESCE(sent_at, created_at) < $2
    UNION ALL SELECT automation_id, step, NULL, 0, false, false, 0, 0, 0, (type <> 'complaint')::INT, (type = 'complaint')::INT FROM denma_automation_bounces
        WHERE COALESCE(sent_at, created_at) >= $1 AND COALESCE(sent_at, created_at) < $2
)
SELECT automation_id, CASE WHEN GROUPING(step) = 1 THEN 0 ELSE COALESCE(step, -1) END AS step,
    SUM(n) AS sent, COUNT(DISTINCT subscriber_id) AS people,
    COUNT(DISTINCT subscriber_id) FILTER (WHERE opened) AS opened,
    COUNT(DISTINCT subscriber_id) FILTER (WHERE clicked) AS clicked,
    SUM(opens) AS opens, SUM(clicks) AS clicks, SUM(unsubs) AS unsubs, SUM(bounces) AS bounces, SUM(complaints) AS complaints
FROM ev
GROUP BY GROUPING SETS ((automation_id, step), (automation_id))
ORDER BY automation_id, step`

// DenmaAutomationReports returns each automation's figures, with each of its
// e-mails', for the e-mails sent in a period (from, to: RFC 3339 times),
// and since when opens, unsubscribes and bounces are counted.
//
//	GET /api/denma/stats/automations?from=...&to=...
func (a *App) DenmaAutomationReports(c echo.Context) error {
	if !denmaAutomationsOn(a) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	var t [2]time.Time
	for i, k := range []string{"from", "to"} {
		var err error
		if t[i], err = time.Parse(time.RFC3339, c.QueryParam(k)); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid %s: %v", k, err))
		}
	}
	out := struct {
		Individual bool                 `json:"individual_tracking"`
		Disabled   bool                 `json:"tracking_disabled"`
		Since      map[string]time.Time `json:"since"`
		Results    []denmaAutoReportRow `json:"results"`
	}{Individual: a.cfg.Privacy.IndividualTracking, Disabled: a.cfg.Privacy.DisableTracking, Since: map[string]time.Time{}}

	var since []struct {
		Stat  string    `db:"stat"`
		Since time.Time `db:"since"`
	}
	if err := a.db.Select(&since, `SELECT stat, since FROM denma_stat_starts WHERE stat LIKE 'automation_%'`); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	for _, s := range since {
		out.Since[strings.TrimPrefix(s.Stat, "automation_")] = s.Since
	}

	var rows []denmaAutoReportRow
	if err := a.db.Select(&rows, denmaAutoReportSQL, t[0], t[1]); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	autos, err := a.automations()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	byID := map[int]denmaAutomation{}
	for _, au := range autos {
		byID[au.ID] = au
	}
	out.Results = []denmaAutoReportRow{}
	for _, r := range rows {
		au, ok := byID[r.AutomationID]
		if !ok || r.Step < 0 {
			continue // deleted since, or clicks before steps were recorded (in the total)
		}
		r.Name = au.Name
		if r.Step > 0 {
			if _, subj, ok := au.stepEmail(r.Step); ok {
				r.Subject = subj
			}
		}
		out.Results = append(out.Results, r)
	}
	return c.JSON(http.StatusOK, okResp{out})
}

// denmaExploreAuto is an automation chosen on Explore.
type denmaExploreAuto struct {
	ID   int    `db:"id" json:"id"`
	Name string `db:"name" json:"name"`
}

// denmaExploreCount is an automation's opens, clicks or bounces in a bucket
// (an hour or a day), as listmonk's campaign counts.
type denmaExploreCount struct {
	AutomationID int       `db:"automation_id" json:"automation_id"`
	Count        int       `db:"count" json:"count"`
	Timestamp    time.Time `db:"timestamp" json:"timestamp"`
}

// denmaExplore is what Explore shows of the automations chosen.
type denmaExplore struct {
	Can         bool                `json:"can"` // the user can pick automations
	Automations []denmaExploreAuto  `json:"automations"`
	Views       []denmaExploreCount `json:"views"`
	Clicks      []denmaExploreCount `json:"clicks"`
	Bounces     []denmaExploreCount `json:"bounces"`
}

// denmaExploreCountSQL counts a table's rows per automation and bucket, as
// get-campaign-view-counts does (people once per bucket, with individual
// tracking, for opens and clicks).
func denmaExploreCountSQL(table string, distinct bool) string {
	count := "COUNT(*)"
	if distinct {
		count = "COUNT(DISTINCT subscriber_id)"
	}
	return `SELECT automation_id, ` + count + ` AS "count",
        DATE_TRUNC(CASE WHEN (EXTRACT (EPOCH FROM ($3::TIMESTAMP - $2::TIMESTAMP)) / 86400) >= 7 THEN 'day' ELSE 'hour' END, created_at) AS "timestamp"
    FROM ` + table + ` WHERE automation_id = ANY($1) AND created_at >= $2 AND created_at <= $3
    GROUP BY automation_id, "timestamp" ORDER BY "timestamp"`
}

// denmaExploreAutomations adds the automations chosen on Explore (?auto=) to
// its figures and charts (cmd/campaign_analytics.go), for users who can see
// automations; with any, its links table is the campaigns' and theirs.
func (a *App) denmaExploreAutomations(c echo.Context, v *campaignAnalyticsView, from, to time.Time) error {
	v.Denma = denmaExplore{Automations: []denmaExploreAuto{}}
	if !denmaAutomationsOn(a) || !v.Can(denmaAutoGet) {
		return nil
	}
	v.Denma.Can = true
	ids, err := getQueryInts("auto", c.QueryParams())
	if err != nil || len(ids) == 0 {
		return nil
	}
	if err := a.db.Select(&v.Denma.Automations, `SELECT id, name FROM denma_automations WHERE id = ANY($1) ORDER BY name`, pq.Array(ids)); err != nil {
		return err
	}
	if len(v.Denma.Automations) == 0 {
		return nil
	}
	ids = ids[:0]
	for _, au := range v.Denma.Automations {
		ids = append(ids, au.ID)
	}
	var (
		fromStr = from.Format("2006-01-02 15:04:05")
		toStr   = to.Format("2006-01-02 15:04:05")
		ind     = a.cfg.Privacy.IndividualTracking
		d       = &v.Denma
	)
	for _, q := range []struct {
		dst   *[]denmaExploreCount
		sql   string
		count *int
	}{
		{&d.Views, denmaExploreCountSQL("denma_automation_views", ind), &v.Counts.Views},
		{&d.Clicks, denmaExploreCountSQL("denma_automation_clicks", ind), &v.Counts.Clicks},
		{&d.Bounces, denmaExploreCountSQL("denma_automation_bounces", false), &v.Counts.Bounces},
	} {
		*q.dst = []denmaExploreCount{}
		if err := a.db.Select(q.dst, q.sql, pq.Array(ids), fromStr, toStr); err != nil {
			return err
		}
		for _, r := range *q.dst {
			*q.count += r.Count
		}
	}

	// The links, the campaigns' and the automations' together (people once
	// with individual tracking, as get-campaign-link-counts).
	count := "COUNT(*)"
	if ind {
		count = "COUNT(DISTINCT subscriber_id)"
	}
	campIDs := make([]int, len(v.Campaigns))
	for i, cm := range v.Campaigns {
		campIDs[i] = cm.ID
	}
	links := []models.CampaignAnalyticsLink{}
	if err := a.db.Select(&links, `SELECT `+count+` AS "count", l.url FROM (
			SELECT link_id, subscriber_id FROM link_clicks WHERE campaign_id = ANY($1) AND created_at >= $3 AND created_at <= $4
			UNION ALL SELECT link_id, subscriber_id FROM denma_automation_clicks WHERE automation_id = ANY($2) AND created_at >= $3 AND created_at <= $4
		) k JOIN links l ON l.id = k.link_id GROUP BY l.url ORDER BY "count" DESC LIMIT 50`,
		pq.Array(campIDs), pq.Array(ids), fromStr, toStr); err != nil {
		return err
	}
	v.HasData = true
	v.Data.Links = links
	return nil
}
