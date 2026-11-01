package main

// denma: unsubscribing opts out of the center: blocklisted and off all its lists (cmd/denma_optouts.go).

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// The page for a link whose subscriber is no longer in the center (hasSub).
const (
	denmaNotSubscribedTitle = "You're not subscribed"
	denmaNotSubscribedText  = "You're not on this center's lists, so you won't get its e-mails."
)

// denmaUnsubscribe is listmonk's unsubscribe for any e-mail; the opt-out and blocklisting share a transaction.
func (a *App) denmaUnsubscribe(subUUID, campUUID string, _ bool) error {
	if !a.denmaHasOptOuts() {
		return a.core.UnsubscribeByCampaign(subUUID, campUUID, true)
	}
	fail := func(err error) error {
		a.log.Printf("denma: error unsubscribing: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, a.i18n.T("public.errorProcessingRequest"))
	}
	tx, err := a.db.Beginx()
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO denma_optouts (email_hash, kind, source)
		SELECT denma_email_hash(email), 'unsubscribed', 'unsubscribe' FROM subscribers WHERE uuid = $1
		ON CONFLICT (email_hash) DO NOTHING`, subUUID)
	if err != nil {
		return fail(err)
	}
	// A new opt-out counts for its campaign or automation (cmd/denma_campaign_stats.go).
	if n, _ := res.RowsAffected(); n > 0 && campUUID != "" {
		if _, err := tx.Exec(denmaCampaignUnsubSQL, campUUID); err != nil {
			return fail(err)
		}
		if denmaAutomationsOn(a) {
			if _, err := tx.Exec(denmaAutoUnsubSQL, campUUID, subUUID); err != nil {
				return fail(err)
			}
		}
	}
	// listmonk's unsubscribe-by-campaign, blocklisting: every list, and the address's variants (denma_canon_hash).
	if _, err := tx.Exec(`WITH sub AS (
			UPDATE subscribers SET status = 'blocklisted'
			WHERE denma_canon_hash(email) = (SELECT denma_canon_hash(email) FROM subscribers WHERE uuid = $1)
			RETURNING id
		)
		UPDATE subscriber_lists SET status = 'unsubscribed', updated_at = NOW()
		WHERE subscriber_id IN (SELECT id FROM sub) AND status != 'unsubscribed'`, subUUID); err != nil {
		return fail(err)
	}
	if err := tx.Commit(); err != nil {
		return fail(err)
	}
	return nil
}

// denmaWipe is the public page's "wipe my data": it records the opt-out, then deletes the subscriber.
func (a *App) denmaWipe(subUUID string) error {
	if err := a.denmaUnsubscribe(subUUID, "", false); err != nil {
		return err
	}
	return a.core.DeleteSubscribers(nil, []string{subUUID})
}
