package main

// denma: campaign sends that failed, and trying them again. listmonk logs a
// failed send and moves on: the campaign still finishes, and no one knows who
// didn't get it. Here each one is recorded (denma_send_failures: who, why,
// whether it's worth trying again), with those whose message couldn't be made
// from the campaign's template.
//
//   - A temporary failure (the mail server unreachable or saying "try later",
//     such as SES's sending quota) is tried again automatically when the
//     campaign reaches the end of its list: the campaign stays running, and
//     is picked up again after a wait (campaigns.denma_retry_at) to send only
//     those, up to len(denmaRetryWaits) more times. One the server refused for
//     good (a 5xx reply) isn't.
//   - A run of failures in a row (internal/manager/denma.go) pauses the
//     campaign, so a mail server that's down doesn't fail everyone left.
//   - Resuming a paused campaign tries its failed sends again first (every
//     one, as whatever failed may have been fixed), then carries on.
//   - The campaign's page lists them with the reasons, and a finished
//     campaign's "Resend to them" sends only those again.
//
// A retry run only sends to those failed sends: it doesn't recount the
// campaign's lists (queries/campaigns.sql, next-campaigns), so people who
// joined since don't get it.

import (
	"net/http"
	"time"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
	null "gopkg.in/volatiletech/null.v6"
)

// denmaRetryWaits are the waits, in minutes, before each automatic retry of a
// temporary failure, from when it last failed.
var denmaRetryWaits = []int{5, 30, 120}

// denmaRetriesSQL makes the failed sends' table and the campaigns' retry
// time, in a center (denmaFeaturesSQL) and in the hub (denmaHubColumns), whose
// campaign manager runs the same queries.
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
ALTER TABLE campaigns ADD COLUMN IF NOT EXISTS denma_retry_at TIMESTAMP WITH TIME ZONE;
`

// DenmaSendFailed records a failed send (or a retry's failure) for the
// campaign manager.
func (s *store) DenmaSendFailed(campID, subID int, reason string, temporary bool) error {
	if r := []rune(reason); len(r) > 500 {
		reason = string(r[:500])
	}
	_, err := s.core.DenmaDB().Exec(`INSERT INTO denma_send_failures (campaign_id, subscriber_id, reason, temporary) VALUES ($1, $2, $3, $4)
		ON CONFLICT (campaign_id, subscriber_id) DO UPDATE SET reason = EXCLUDED.reason, temporary = EXCLUDED.temporary,
		retries = denma_send_failures.retries + 1, failed_at = NOW()`, campID, subID, reason, temporary)
	return err
}

// DenmaRetrySent forgets a failed send that a retry has now sent.
func (s *store) DenmaRetrySent(campID, subID int) error {
	_, err := s.core.DenmaDB().Exec(`DELETE FROM denma_send_failures WHERE campaign_id = $1 AND subscriber_id = $2`, campID, subID)
	return err
}

// DenmaRetrySubscribers returns the next batch (after afterID) of a
// campaign's failed sends to try again: temporary ones, or all of them, that
// haven't been retried as often as they may be, to subscribers who haven't
// since been blocklisted (which unsubscribing does).
func (s *store) DenmaRetrySubscribers(campID, afterID, limit int, all bool) ([]models.Subscriber, error) {
	var out []models.Subscriber
	err := s.core.DenmaDB().Select(&out, `SELECT s.* FROM denma_send_failures f JOIN subscribers s ON s.id = f.subscriber_id
		WHERE f.campaign_id = $1 AND s.id > $2 AND ($4 OR f.temporary) AND f.retries < $5 AND s.status != 'blocklisted'
		ORDER BY s.id LIMIT $3`, campID, afterID, limit, all, len(denmaRetryWaits))
	return out, err
}

// DenmaRetryLater sets when a campaign that reached the end of its list next
// tries its temporary failures again, and reports whether it has any;
// without any, it clears the time and the campaign can finish.
func (s *store) DenmaRetryLater(campID int) (bool, error) {
	var at null.Time
	err := s.core.DenmaDB().Get(&at, `UPDATE campaigns SET denma_retry_at = (
		SELECT MIN(f.failed_at + make_interval(mins => ($2::INT[])[f.retries + 1]))
		FROM denma_send_failures f JOIN subscribers s ON s.id = f.subscriber_id
		WHERE f.campaign_id = $1 AND f.temporary AND f.retries < $3 AND s.status != 'blocklisted'
	) WHERE id = $1 RETURNING denma_retry_at`, campID, pq.Array(denmaRetryWaits), len(denmaRetryWaits))
	return at.Valid, err
}

// denmaSendFailure is a failed send, for the campaign's page.
type denmaSendFailure struct {
	SubscriberID int       `db:"subscriber_id" json:"subscriber_id"`
	Email        string    `db:"email" json:"email"`
	Name         string    `db:"name" json:"name"`
	Reason       string    `db:"reason" json:"reason"`
	Temporary    bool      `db:"temporary" json:"temporary"`
	Tries        int       `db:"tries" json:"tries"`
	FailedAt     time.Time `db:"failed_at" json:"failed_at"`
}

// denmaSendFailures is a campaign's failed sends: how many, the commonest
// reasons, the latest ones, and when they're next tried.
type denmaSendFailures struct {
	Total   int `db:"total" json:"total"`
	Reasons []struct {
		Reason string `db:"reason" json:"reason"`
		N      int    `db:"n" json:"n"`
	} `db:"-" json:"reasons"`
	Items   []denmaSendFailure `db:"-" json:"items"`
	RetryAt null.Time          `db:"retry_at" json:"retry_at"`
	// Pending is how many will be tried again automatically; Waiting, whether
	// that's later (RetryAt), rather than now.
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
	if err := a.db.Select(&out.Items, `SELECT f.subscriber_id, s.email, s.name, f.reason, f.temporary, f.retries + 1 AS tries, f.failed_at
		FROM denma_send_failures f JOIN subscribers s ON s.id = f.subscriber_id WHERE f.campaign_id = $1
		ORDER BY f.failed_at DESC, f.subscriber_id LIMIT $2`, campID, denmaSendFailuresShown); err != nil {
		return nil, err
	}
	return out, nil
}

// denmaCampaignFailuresView is denmaCampaignFailures for the campaign's page,
// which shows nothing if they can't be read.
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

// DenmaResendFailures sends a finished campaign again to those it couldn't
// send to, and to no one else: every failed send gets as many tries again.
func (a *App) DenmaResendFailures(c echo.Context) error {
	id := getID(c)
	if err := a.checkCampaignPerm(auth.PermTypeManage, id, c); err != nil {
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
	res, err := tx.Exec(`UPDATE denma_send_failures f SET retries = 0, temporary = true FROM subscribers s
		WHERE f.campaign_id = $1 AND s.id = f.subscriber_id AND s.status != 'blocklisted'`, id)
	if err != nil {
		return a.denmaResendErr(id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "There's no one to resend to: everyone it couldn't send to has since unsubscribed or been blocklisted.")
	}
	// Running, as a retry run (denma_retry_at set): the campaign manager
	// picks it up within seconds and sends only to them.
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
