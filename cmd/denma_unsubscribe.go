package main

// denma: unsubscribing opts out of the center. A center's lists are internal,
// so a subscriber never sees them: the unsubscribe page has one button, and
// it, and every other way to unsubscribe from an e-mail (a campaign's, an
// automation's or an opt-in's link, and the one-click List-Unsubscribe that
// Gmail and Yahoo send without the page), blocklists the subscriber in this
// center and unsubscribes them from all its lists. No campaign, tag or
// automation reaches them again. Signing up again sends them a confirmation,
// and only confirming brings them back (cmd/denma_resubscribe.go); an admin
// can too. Other centers aren't affected. The preferences page keeps the
// name and the privacy choices, without lists
// (static/public/templates/subscription.html).

// denmaUnsubscribe is listmonk's unsubscribe (public.go), for any e-mail:
// campUUID may be a campaign's, an automation's or none (an opt-in's).
func (a *App) denmaUnsubscribe(subUUID, campUUID string, _ bool) error {
	return a.core.UnsubscribeByCampaign(subUUID, campUUID, true)
}
