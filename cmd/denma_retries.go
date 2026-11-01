package main

// denma: campaign sends that failed, recorded (denma_send_failures) and tried again.

import (
	"net/http"
	"time"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
	null "gopkg.in/volatiletech/null.v6"
)

// denmaRetryWaits are the waits, in minutes, before each automatic retry of a temporary failure.
var denmaRetryWaits = []int{5, 30, 120}

// denmaRetriesSQL makes the failed sends' table and the campaigns' retry time, in a center and the hub.
const denmaRetriesSQL = `
CREATE TABLE IF NOT EXISTS denma_send_failures (
    campaign_id   INTEGER NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    subscriber_id INTEGER NOT NULL REFERENCES subscribers(id) ON DELETE CASCADE,
    reason        TEXT NOT NULL,
    temporary     BOOLEAN NOT NULL,
    retries       INTEGER NOT NULL DEFAULT 0,
    failed_at     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (campaign_id, subscriber_id)
);
CREATE INDEX IF NOT EXISTS denma_send_failures_sub ON denma_send_failures (subscriber_id);
ALTER TABLE denma_send_failures ADD COLUMN IF NOT EXISTS uncertain BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE campaigns ADD COLUMN IF NOT EXISTS denma_retry_at TIMESTAMP WITH TIME ZONE;
ALTER TABLE campaigns ADD COLUMN IF NOT EXISTS denma_sent_ahead INTEGER[] NOT NULL DEFAULT '{}';
`

// DenmaSendFailed records a failed send (or a retry's failure) for the campaign manager.
func (s *store) DenmaSendFailed(campID, subID int, reason string, temporary, uncertain bool) error {
	if r := []rune(reason); len(r) > 500 {
		reason = string(r[:500])
	}
	_, err := s.core.DenmaDB().Exec(`INSERT INTO denma_send_failures (campaign_id, subscriber_id, reason, temporary, uncertain) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (campaign_id, subscriber_id) DO UPDATE SET reason = EXCLUDED.reason, temporary = EXCLUDED.temporary,
		uncertain = EXCLUDED.uncertain, retries = denma_send_failures.retries + 1, failed_at = NOW()`, campID, subID, reason, temporary, uncertain)
	return err
}

// DenmaRetrySent forgets a failed send that a retry has now sent.
func (s *store) DenmaRetrySent(campID, subID int) error {
	_, err := s.core.DenmaDB().Exec(`DELETE FROM denma_send_failures WHERE campaign_id = $1 AND subscriber_id = $2`, campID, subID)
	return err
}

// denmaStillSentSQL is whether failed send f's subscriber s would still be sent its campaign.
const denmaStillSentSQL = `(EXISTS (SELECT 1 FROM campaign_lists cl JOIN lists l ON l.id = cl.list_id
		JOIN subscriber_lists sl ON sl.list_id = l.id AND sl.subscriber_id = s.id
		WHERE cl.campaign_id = f.campaign_id AND CASE WHEN l.optin = 'double' THEN sl.status = 'confirmed' ELSE sl.status != 'unsubscribed' END)
	OR (s.attribs->'tags' ?| (SELECT denma_tags FROM campaigns WHERE id = f.campaign_id) AND EXISTS (
		SELECT 1 FROM subscriber_lists dsl JOIN lists dl ON dl.id = dsl.list_id WHERE dsl.subscriber_id = s.id
		AND NOT ('` + denmaAPIListTag + `' = ANY(COALESCE(dl.tags, '{}'))) AND CASE WHEN dl.optin = 'double' THEN dsl.status = 'confirmed' ELSE dsl.status != 'unsubscribed' END)))`

// DenmaStillSent says whether a subscriber would still be sent a campaign.
func (s *store) DenmaStillSent(campID, subID int) (bool, error) {
	var still bool
	err := s.core.DenmaDB().Get(&still, `SELECT EXISTS (SELECT 1 FROM subscribers s, (SELECT $1::INT AS campaign_id) f
		WHERE s.id = $2 AND s.status <> 'blocklisted' AND `+denmaStillSentSQL+`)`, campID, subID)
	return still, err
}

// DenmaRetrySubscribers returns the next batch (after afterID) of a campaign's failed sends to retry.
func (s *store) DenmaRetrySubscribers(campID, afterID, limit int, all bool) ([]models.Subscriber, error) {
	var out []models.Subscriber
	err := s.core.DenmaDB().Select(&out, `SELECT s.* FROM denma_send_failures f JOIN subscribers s ON s.id = f.subscriber_id
		WHERE f.campaign_id = $1 AND s.id > $2 AND ($4 OR f.temporary) AND NOT f.uncertain AND f.retries < $5 AND s.status != 'blocklisted'
		AND `+denmaStillSentSQL+`
		ORDER BY s.id LIMIT $3`, campID, afterID, limit, all, len(denmaRetryWaits))
	if err == nil {
		err = denmaAPIRecipientData(s.core.DenmaDB(), campID, out) // an API campaign's data (cmd/denma_api_send.go)
	}
	return out, err
}

// DenmaRetryLater sets when a campaign next retries its temporary failures; it clears the time if none.
func (s *store) DenmaRetryLater(campID int) (bool, error) {
	var at null.Time
	err := s.core.DenmaDB().Get(&at, `UPDATE campaigns SET denma_retry_at = (
		SELECT MIN(f.failed_at + make_interval(mins => ($2::INT[])[f.retries + 1]))
		FROM denma_send_failures f JOIN subscribers s ON s.id = f.subscriber_id
		WHERE f.campaign_id = $1 AND f.temporary AND f.retries < $3 AND s.status != 'blocklisted' AND `+denmaStillSentSQL+`
	) WHERE id = $1 RETURNING denma_retry_at`, campID, pq.Array(denmaRetryWaits), len(denmaRetryWaits))
	return at.Valid, err
}

// DenmaSentAhead returns those a campaign's stopped pipes sent past its checkpoint.
func (s *store) DenmaSentAhead(campID int) ([]int, error) {
	var ids pq.Int64Array
	if err := s.core.DenmaDB().Get(&ids, `SELECT denma_sent_ahead FROM campaigns WHERE id = $1`, campID); err != nil {
		return nil, err
	}
	out := make([]int, len(ids))
	for i, id := range ids {
		out[i] = int(id)
	}
	return out, nil
}

// DenmaSaveSentAhead adds ids to those sent past the campaign's checkpoint.
func (s *store) DenmaSaveSentAhead(campID int, ids []int) error {
	_, err := s.core.DenmaDB().Exec(`UPDATE campaigns c SET denma_sent_ahead = ARRAY(
		SELECT DISTINCT x FROM unnest(c.denma_sent_ahead || $2::INT[]) x WHERE x > c.last_subscriber_id ORDER BY x)
		WHERE c.id = $1 AND (c.denma_sent_ahead <> '{}' OR CARDINALITY($2::INT[]) > 0)`, campID, pq.Array(ids))
	return err
}

// denmaSendFailure is a failed send, for the campaign's page.
type denmaSendFailure struct {
	SubscriberID int       `db:"subscriber_id" json:"subscriber_id"`
	Email        string    `db:"email" json:"email"`
	Name         string    `db:"name" json:"name"`
	Reason       string    `db:"reason" json:"reason"`
	Temporary    bool      `db:"temporary" json:"temporary"`
	Uncertain    bool      `db:"uncertain" json:"uncertain"`
	Tries        int       `db:"tries" json:"tries"`
	FailedAt     time.Time `db:"failed_at" json:"failed_at"`
}

// denmaSendFailures is a campaign's failed sends: how many, common reasons, latest ones, next try.
type denmaSendFailures struct {
	Total   int `db:"total" json:"total"`
	Reasons []struct {
		Reason string `db:"reason" json:"reason"`
		N      int    `db:"n" json:"n"`
	} `db:"-" json:"reasons"`
	Items   []denmaSendFailure `db:"-" json:"items"`
	RetryAt null.Time          `db:"retry_at" json:"retry_at"`
	// Pending is how many will be tried again automatically; Waiting, whether that's later (RetryAt).
	Pending int  `db:"pending" json:"pending"`
	Waiting bool `db:"-" json:"-"`
}

// denmaSendFailuresShown is how many failed sends the campaign's page lists.
const denmaSendFailuresShown = 100

// denmaCampaignFailures returns a campaign's failed sends (nil if none).
func (a *App) denmaCampaignFailures(campID int) (*denmaSendFailures, error) {
	out := &denmaSendFailures{}
	if err := a.db.Get(out, `SELECT COUNT(*) AS total,
		COUNT(*) FILTER (WHERE f.temporary AND f.retries < $2 AND s.status != 'blocklisted') AS pending,
		(SELECT denma_retry_at FROM campaigns WHERE id = $1) AS retry_at
		FROM denma_send_failures f JOIN subscribers s ON s.id = f.subscriber_id WHERE f.campaign_id = $1`,
		campID, len(denmaRetryWaits)); err != nil {
		return nil, err
	}
	if out.Total == 0 {
		return nil, nil
	}
	out.Waiting = out.RetryAt.Valid && out.RetryAt.Time.After(time.Now())
	if err := a.db.Select(&out.Reasons, `SELECT reason, COUNT(*) AS n FROM denma_send_failures WHERE campaign_id = $1
		GROUP BY reason ORDER BY n DESC, reason LIMIT 5`, campID); err != nil {
		return nil, err
	}
	if err := a.db.Select(&out.Items, `SELECT f.subscriber_id, s.email, s.name, f.reason, f.temporary, f.uncertain, f.retries + 1 AS tries, f.failed_at
		FROM denma_send_failures f JOIN subscribers s ON s.id = f.subscriber_id WHERE f.campaign_id = $1
		ORDER BY f.failed_at DESC, f.subscriber_id LIMIT $2`, campID, denmaSendFailuresShown); err != nil {
		return nil, err
	}
	return out, nil
}

// denmaCampaignFailuresView is denmaCampaignFailures for the campaign's page.
func (a *App) denmaCampaignFailuresView(campID int) *denmaSendFailures {
	out, err := a.denmaCampaignFailures(campID)
	if err != nil {
		a.log.Printf("denma: error reading campaign %d's failed sends: %v", campID, err)
	}
	return out
}

func initDenmaRetryHandlers(g *echo.Group, a *App) {
	pm := a.auth.Perm
	g.GET("/api/denma/campaigns/:id/failures", pm(hasID(a.DenmaGetSendFailures), "campaigns:get_all", "campaigns:get"))
	g.POST("/api/denma/campaigns/:id/resend", pm(hasID(a.DenmaResendFailures), "campaigns:send"))
}

// DenmaGetSendFailures returns a campaign's failed sends.
func (a *App) DenmaGetSendFailures(c echo.Context) error {
	id := getID(c)
	if err := a.checkCampaignPerm(auth.PermTypeGet, id, c); err != nil {
		return err
	}
	out, err := a.denmaCampaignFailures(id)
	if err != nil {
		a.log.Printf("denma: error reading campaign %d's failed sends: %v", id, err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error reading the failed sends.")
	}
	if out == nil {
		out = &denmaSendFailures{Items: []denmaSendFailure{}}
	}
	return c.JSON(http.StatusOK, okResp{out})
}

// DenmaResendFailures sends a finished campaign again to those it couldn't send to.
func (a *App) DenmaResendFailures(c echo.Context) error {
	id := getID(c)
	if err := a.checkCampaignPerm(auth.PermTypeManage, id, c); err != nil {
		return err
	}
	if err := a.denmaCheckCampaignStart(id, models.CampaignStatusRunning); err != nil { // its sender, as starting it (cmd/denma_domains.go)
		return err
	}
	tx, err := a.db.Beginx()
	if err != nil {
		return a.denmaResendErr(id, err)
	}
	defer tx.Rollback()

	var status string
	if err := tx.Get(&status, `SELECT status FROM campaigns WHERE id = $1 FOR UPDATE`, id); err != nil {
		return echo.NewHTTPError(http.StatusNotFound, a.i18n.Ts("globals.messages.notFound", "name", "{globals.terms.campaign}"))
	}
	if status != models.CampaignStatusFinished {
		return echo.NewHTTPError(http.StatusBadRequest, "Only a finished campaign can be resent to those it couldn't send to; resuming a paused one tries them again.")
	}
	res, err := tx.Exec(`UPDATE denma_send_failures f SET retries = 0, temporary = true, uncertain = false FROM subscribers s
		WHERE f.campaign_id = $1 AND s.id = f.subscriber_id AND s.status != 'blocklisted' AND `+denmaStillSentSQL, id)
	if err != nil {
		return a.denmaResendErr(id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "There's no one to resend to: everyone it couldn't send to has since unsubscribed, been blocklisted or left its lists.")
	}
	// Running, as a retry run (denma_retry_at set): the campaign manager sends only to them.
	if _, err := tx.Exec(`UPDATE campaigns SET status = 'running', denma_retry_at = NOW(), updated_at = NOW() WHERE id = $1`, id); err != nil {
		return a.denmaResendErr(id, err)
	}
	if err := tx.Commit(); err != nil {
		return a.denmaResendErr(id, err)
	}
	return c.JSON(http.StatusOK, okResp{map[string]int64{"subscribers": n}})
}

func (a *App) denmaResendErr(id int, err error) error {
	a.log.Printf("denma: error resending campaign %d: %v", id, err)
	return echo.NewHTTPError(http.StatusInternalServerError, "Error resending the campaign.")
}
