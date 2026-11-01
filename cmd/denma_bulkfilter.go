package main

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

// denma: the Subscribers page's filters for bulk actions and export (status, subscribers on no list).

// denmaBulkSearch is denmaSearch for a bulk action's request, with its subscriber status and no-list filter.
func (a *App) denmaBulkSearch(user auth.User, req *subQueryReq) error {
	if err := a.denmaSearch(user, &req.Search, &req.Query); err != nil {
		return err
	}
	if req.SubscriptionStatus == "none" {
		req.Query = denmaAndSQL(req.Query, "NOT EXISTS (SELECT 1 FROM subscriber_lists WHERE subscriber_id = subscribers.id)")
		req.SubscriptionStatus = ""
	}
	return denmaWithSubStatus(req.SubscriberStatus, &req.Query)
}

// denmaStatusSubscribed is the status filter for those who get mail.
const denmaStatusSubscribed = "subscribed"

// denmaSubscribedSQL is those who get mail: enabled, and on a list as denmaActiveSQL counts it.
const denmaSubscribedSQL = `subscribers.status = 'enabled' AND EXISTS (SELECT 1 FROM subscriber_lists sl JOIN lists l ON l.id = sl.list_id
	WHERE sl.subscriber_id = subscribers.id AND sl.status <> 'unsubscribed' AND NOT (l.optin = 'double' AND sl.status = 'unconfirmed')
	AND ` + denmaHubListActive + `)`

// denmaStatusFilter moves the status filter for those who get mail into query.
func denmaStatusFilter(status, query string) (string, string) {
	if status == denmaStatusSubscribed {
		return "", denmaAndSQL(query, denmaSubscribedSQL)
	}
	return status, query
}

// denmaSubsNav is the sidebar item for the Contacts page's filter.
func denmaSubsNav(c echo.Context) string {
	switch c.QueryParam("status") {
	case denmaStatusSubscribed:
		return "subscribers.subscribed"
	case models.SubscriberStatusBlockListed:
		return "subscribers.blocklisted"
	}
	return "subscribers.all"
}

// denmaWithSubStatus adds a subscriber status to query.
func denmaWithSubStatus(status string, query *string) error {
	switch status {
	case "":
		return nil
	case denmaStatusSubscribed:
		*query = denmaAndSQL(*query, denmaSubscribedSQL)
		return nil
	case models.SubscriberStatusEnabled, models.SubscriberStatusDisabled, models.SubscriberStatusBlockListed:
	default:
		return echo.NewHTTPError(http.StatusBadRequest, "Unknown subscriber status.")
	}
	*query = denmaAndSQL(*query, "subscribers.status = '"+status+"'")
	return nil
}

// denmaAndSQL is the condition query AND cond.
func denmaAndSQL(query, cond string) string {
	if q := strings.TrimSpace(query); q != "" {
		return "(" + q + ") AND " + cond
	}
	return cond
}

// denmaNoBlocklistedDelete refuses a bulk delete that would take blocklisted subscribers (cmd/denma_optouts.go).
func (a *App) denmaNoBlocklistedDelete(ids []int, req *subQueryReq, listIDs []int) error {
	var n int
	if req == nil {
		if err := a.db.Get(&n, `SELECT COUNT(*) FROM subscribers WHERE id = ANY($1) AND status = 'blocklisted'`, pq.Array(ids)); err != nil {
			return denmaInternal(err)
		}
	} else {
		var err error
		if _, n, err = a.core.QuerySubscribers(req.Search, denmaAndSQL(req.Query, "subscribers.status = 'blocklisted'"),
			listIDs, req.SubscriptionStatus, "", "", "", 0, 1); err != nil {
			return err
		}
	}
	if n == 0 {
		return nil
	}
	are := "are"
	if n == 1 {
		are = "is"
	}
	return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("%d of them %s blocklisted. Blocklisted subscribers can't be deleted together: "+
		"they include everyone who unsubscribed, and deleting them would forget that they opted out. "+
		"Leave them out (Status: Enabled), or delete them one at a time.", n, are))
}
