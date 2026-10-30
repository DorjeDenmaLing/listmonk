package main

// denma: what each campaign led to, for Campaign reports on Campaigns ->
// Analytics (views/denma-analytics.js): unsubscribes, which listmonk doesn't
// keep by campaign, and bounces apart from spam complaints.
//
// An unsubscribe from a campaign's e-mail counts for that campaign
// (denma_campaign_unsubs, from denmaUnsubscribe) when it opts the person out,
// not again for someone already opted out. Only the campaign and the time
// are kept, not who. Counting started when the table was made
// (denma_stat_starts), so earlier campaigns have none recorded.

import (
	"fmt"
	"net/http"
	"time"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

// denmaCampaignStatsSQL makes the tables, in a center (denmaFeaturesSQL).
const denmaCampaignStatsSQL = `
CREATE TABLE IF NOT EXISTS denma_stat_starts (
    stat  TEXT PRIMARY KEY,
    since TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS denma_campaign_unsubs (
    campaign_id INTEGER NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE ON UPDATE CASCADE,
    created_at  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS denma_campaign_unsubs_campaign ON denma_campaign_unsubs (campaign_id);
INSERT INTO denma_stat_starts (stat) VALUES ('campaign_unsubs') ON CONFLICT (stat) DO NOTHING;
CREATE INDEX IF NOT EXISTS denma_views_camp_sub ON campaign_views (campaign_id, subscriber_id, created_at);
`

// denmaCampaignUnsub counts an opt-out for the campaign whose e-mail it came
// from, if campUUID is a campaign's.
const denmaCampaignUnsubSQL = `INSERT INTO denma_campaign_unsubs (campaign_id)
	SELECT id FROM campaigns WHERE uuid::TEXT = $1`

type denmaCampaignOutcome struct {
	CampaignID   int `db:"campaign_id" json:"campaign_id"`
	Bounces      int `db:"bounces" json:"bounces"` // hard and soft
	Complaints   int `db:"complaints" json:"complaints"`
	Unsubscribes int `db:"unsubscribes" json:"unsubscribes"`
}

// DenmaCampaignOutcomes returns campaigns' bounces, complaints and
// unsubscribes, and since when unsubscribes are counted.
//
//	GET /api/denma/stats/campaigns?id=1&id=2
func (a *App) DenmaCampaignOutcomes(c echo.Context) error {
	ids, err := parseStringIDs(c.QueryParams()["id"])
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.Ts("globals.messages.errorInvalidIDs", "error", err.Error()))
	}
	out := struct {
		UnsubsSince *time.Time             `json:"unsubscribes_since"`
		Results     []denmaCampaignOutcome `json:"results"`
	}{Results: []denmaCampaignOutcome{}}
	if err := a.db.Get(&out.UnsubsSince, `SELECT (SELECT since FROM denma_stat_starts WHERE stat = 'campaign_unsubs')`); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if err := a.db.Select(&out.Results, `SELECT c.id AS campaign_id,
		(SELECT COUNT(*) FROM bounces b WHERE b.campaign_id = c.id AND b.type <> 'complaint') AS bounces,
		(SELECT COUNT(*) FROM bounces b WHERE b.campaign_id = c.id AND b.type = 'complaint') AS complaints,
		(SELECT COUNT(*) FROM denma_campaign_unsubs u WHERE u.campaign_id = c.id) AS unsubscribes
		FROM campaigns c WHERE c.id = ANY($1) ORDER BY c.id`, pq.Array(ids)); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{out})
}

// DenmaCampaignPeopleStats returns, for each campaign, the people who opened and
// clicked it from from to to, as the analytics' views and clicks count them
// added up over the period (every view and click without individual
// tracking).
//
//	GET /api/denma/stats/people?id=1&id=2&from=2026-01-01&to=2026-12-31
func (a *App) DenmaCampaignPeopleStats(c echo.Context) error {
	ids, err := parseStringIDs(c.QueryParams()["id"])
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.Ts("globals.messages.errorInvalidIDs", "error", err.Error()))
	}
	for _, id := range ids {
		if err := a.checkCampaignPerm(auth.PermTypeGet, id, c); err != nil {
			return err
		}
	}
	from, to := c.QueryParam("from"), c.QueryParam("to")
	if !strHasLen(from, 10, 30) || !strHasLen(to, 10, 30) {
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.T("analytics.invalidDates"))
	}
	count := "COUNT(DISTINCT subscriber_id)"
	if !a.cfg.Privacy.IndividualTracking {
		count = "COUNT(*)"
	}
	out := []struct {
		CampaignID int `db:"campaign_id" json:"campaign_id"`
		Opened     int `db:"opened" json:"opened"`
		Clicked    int `db:"clicked" json:"clicked"`
	}{}
	if err := a.db.Select(&out, fmt.Sprintf(`SELECT c.id AS campaign_id, COALESCE(v.n, 0) AS opened, COALESCE(k.n, 0) AS clicked
		FROM campaigns c
		LEFT JOIN (SELECT campaign_id, %[1]s AS n FROM campaign_views
			WHERE campaign_id = ANY($1) AND created_at >= $2::TIMESTAMP AND created_at <= $3::TIMESTAMP GROUP BY campaign_id) v ON v.campaign_id = c.id
		LEFT JOIN (SELECT campaign_id, %[1]s AS n FROM link_clicks
			WHERE campaign_id = ANY($1) AND created_at >= $2::TIMESTAMP AND created_at <= $3::TIMESTAMP GROUP BY campaign_id) k ON k.campaign_id = c.id
		WHERE c.id = ANY($1) ORDER BY c.id`, count), pq.Array(ids), from, to); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{out})
}
