package main

import (
	"net/http"
	"strings"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
)

// denma: the Subscribers page's filters for its bulk actions (select all →
// delete, blocklist, manage lists, tags, move) and export as its list
// shows them: its status (?status=), which listmonk's by-query endpoints
// don't take, and subscribers on no list (?subscription_status=none).

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
