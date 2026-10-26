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

// denma: the Subscribers page's filters for its bulk actions (select all →
// delete, blocklist, manage lists, tags, move) and export as its list
// shows them: its status (?status=), which listmonk's by-query endpoints
// don't take, and subscribers on no list (?subscription_status=none).
//
// Bulk delete refuses blocklisted subscribers, as Maintenance does
// (DenmaGCSubscribers); one at a time is allowed.

// denmaBulkSearch is denmaSearch for a bulk action's request, with its
// subscriber status (subscriber_status) and subscribers on no list.
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

// denmaWithSubStatus adds a subscriber status to query, the condition
// denmaSearch made.
func denmaWithSubStatus(status string, query *string) error {
	switch status {
	case "":
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

// denmaNoBlocklistedDelete refuses a bulk delete, of ids or else of req's
// subscribers, if it would take blocklisted ones: they include everyone who
// unsubscribed, and deleting them would forget that they opted out.
func (a *App) denmaNoBlocklistedDelete(ids []int, req *subQueryReq, listIDs []int) error {
	var n int
	if req == nil {
		if err := a.db.Get(&n, `SELECT COUNT(*) FROM subscribers WHERE id = ANY($1) AND status = 'blocklisted'`, pq.Array(ids)); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
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
