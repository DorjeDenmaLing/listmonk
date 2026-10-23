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
	"net/http"
	"time"

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
