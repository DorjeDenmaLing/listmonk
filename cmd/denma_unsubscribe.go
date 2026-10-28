package main

// denma: unsubscribing opts out of the center. A center's lists are internal,
// so a subscriber never sees them: the unsubscribe page has one button, and
// it, and every other way to unsubscribe from an e-mail (a campaign's, an
// automation's or an opt-in's link, and the one-click List-Unsubscribe that
// Gmail and Yahoo send without the page), blocklists the subscriber in this
// center and unsubscribes them from all its lists. No campaign, tag or
// automation reaches them again. Signing up again sends them a confirmation,
// and only confirming brings them back (cmd/denma_resubscribe.go): no admin
// can (cmd/denma_optouts.go). Other centers aren't affected. The preferences page keeps the
// name and the privacy choices, without lists
// (static/public/templates/subscription.html).

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// denmaUnsubscribe is listmonk's unsubscribe (public.go), for any e-mail:
// campUUID may be a campaign's, an automation's or none (an opt-in's). In a
// center, their opt-out (cmd/denma_optouts.go) is recorded first and they're
// blocklisted in the same transaction: both, or neither (and they can try
// again), so nobody is left blocklisted with no opt-out to keep them so.
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
	// A new opt-out counts for the campaign it came from (cmd/denma_campaign_stats.go),
	// or the automation (cmd/denma_auto_reports.go).
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
	// listmonk's unsubscribe-by-campaign, blocklisting: every list. Their
	// other records at a variant of the address (denma_canon_hash: dots,
	// +tags), the same inbox, too.
	if _, err := tx.Exec(`WITH sub AS (
			UPDATE subscribers SET status = 'blocklisted' WHERE uuid = $1
				OR denma_canon_hash(email) = (SELECT denma_canon_hash(email) FROM subscribers WHERE uuid = $1)
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

// denmaWipe is the public page's "wipe my data": their opt-out is recorded
// first, as an unsubscribe's, so that it outlives them; then they're deleted.
func (a *App) denmaWipe(subUUID string) error {
	if err := a.denmaUnsubscribe(subUUID, "", false); err != nil {
		return err
	}
	return a.core.DeleteSubscribers(nil, []string{subUUID})
}
