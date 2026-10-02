package main

// denma: the subscriber figures behind the dashboard and Analytics
// (assets/js/denma-stats.js, views/denma-analytics.js). They used to be SQL
// sent from the browser to the subscribers API; SQL queries are off now
// (cmd/denma_search.go), so each figure is named here and its SQL stays on
// the server. A `search` (cmd/denma_search.go) narrows any of them, such as
// to a mailbox provider's domains.

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

// Subscribed to a list, other than waiting on a double opt-in: a website
// signup on a double opt-in holding list isn't a subscriber until it confirms.
const denmaNotPendingSQL = `subscribers.id IN (SELECT sl.subscriber_id FROM subscriber_lists sl JOIN lists l ON l.id = sl.list_id
	WHERE NOT (l.optin = 'double' AND sl.status = 'unconfirmed'))`

// denmaStatSQL is a figure's SQL condition. from and to bound the period
// (for new, unsub and removed); campaign is the campaign (for received, opens
// and clicks).
func denmaStatSQL(metric string, from, to time.Time, campaign int) (string, error) {
	var (
		f       = pq.QuoteLiteral(from.UTC().Format(time.RFC3339Nano)) + "::TIMESTAMPTZ"
		t       = pq.QuoteLiteral(to.UTC().Format(time.RFC3339Nano)) + "::TIMESTAMPTZ"
		between = func(col string) string { return fmt.Sprintf("%s >= %s AND %s < %s", col, f, col, t) }
	)
	needPeriod := func() error {
		if from.IsZero() || to.IsZero() {
			return fmt.Errorf("%s needs from and to", metric)
		}
		return nil
	}
	needCampaign := func() error {
		if campaign < 1 {
			return fmt.Errorf("%s needs a campaign", metric)
		}
		return nil
	}

	switch metric {
	// Enabled, with at least one list subscription, now.
	case "active":
		return `subscribers.status = 'enabled' AND subscribers.id IN (SELECT sl.subscriber_id FROM subscriber_lists sl JOIN lists l ON l.id = sl.list_id
			WHERE sl.status <> 'unsubscribed' AND NOT (l.optin = 'double' AND sl.status = 'unconfirmed'))`, nil

	// Added in the period (and not still waiting on an opt-in).
	case "new":
		if err := needPeriod(); err != nil {
			return "", err
		}
		return between("subscribers.created_at") + " AND " + denmaNotPendingSQL, nil

	// Unsubscribed in the period, not counting bounce removals.
	case "unsub":
		if err := needPeriod(); err != nil {
			return "", err
		}
		return "subscribers.id IN (SELECT subscriber_id FROM subscriber_lists WHERE status = 'unsubscribed' AND " + between("updated_at") + ")" +
			" AND subscribers.id NOT IN (SELECT subscriber_id FROM bounces WHERE subscriber_id IS NOT NULL AND " + between("created_at") + ")", nil

	// Bounced in the period and, as a result, blocklisted or on no list.
	case "removed":
		if err := needPeriod(); err != nil {
			return "", err
		}
		return "subscribers.id IN (SELECT subscriber_id FROM bounces WHERE subscriber_id IS NOT NULL AND " + between("created_at") + ")" +
			" AND (subscribers.status = 'blocklisted' OR NOT EXISTS (SELECT 1 FROM subscriber_lists sl" +
			" WHERE sl.subscriber_id = subscribers.id AND sl.status <> 'unsubscribed'))", nil

	// Sent the campaign: on one of its lists when it started (confirmed, if
	// the list is double opt-in), and not blocklisted before then.
	case "received":
		if err := needCampaign(); err != nil {
			return "", err
		}
		st := fmt.Sprintf("(SELECT started_at FROM campaigns WHERE id = %d)", campaign)
		return fmt.Sprintf(`subscribers.id <= (SELECT max_subscriber_id FROM campaigns WHERE id = %[1]d)
			AND (subscribers.status <> 'blocklisted' OR subscribers.updated_at > %[2]s)
			AND subscribers.id IN (SELECT sl.subscriber_id FROM subscriber_lists sl JOIN lists l ON l.id = sl.list_id
				WHERE sl.list_id IN (SELECT list_id FROM campaign_lists WHERE campaign_id = %[1]d)
				AND sl.created_at <= %[2]s
				AND ((l.optin = 'double' AND sl.status = 'confirmed') OR (l.optin <> 'double' AND sl.status <> 'unsubscribed')
					OR (sl.status = 'unsubscribed' AND sl.updated_at > %[2]s)))`, campaign, st), nil

	case "opens":
		if err := needCampaign(); err != nil {
			return "", err
		}
		return fmt.Sprintf("subscribers.id IN (SELECT subscriber_id FROM campaign_views WHERE campaign_id = %d)", campaign), nil

	case "clicks":
		if err := needCampaign(); err != nil {
			return "", err
		}
		return fmt.Sprintf("subscribers.id IN (SELECT subscriber_id FROM link_clicks WHERE campaign_id = %d)", campaign), nil
	}
	return "", fmt.Errorf("unknown figure %q", metric)
}

// DenmaStatSubscribers returns the subscribers in a figure, like the
// subscribers API: { total, results } (per_page=1 for just the total).
//
//	GET /api/denma/stats/subscribers?metric=new&from=<RFC 3339>&to=...&search=...&per_page=all
//	GET /api/denma/stats/subscribers?metric=opens&campaign=12&search=domain:gmail.com
func (a *App) DenmaStatSubscribers(c echo.Context) error {
	var (
		user = auth.GetUser(c)
		qp   = c.QueryParams()
		from time.Time
		to   time.Time
	)
	for _, x := range []struct {
		name string
		t    *time.Time
	}{{"from", &from}, {"to", &to}} {
		if v := qp.Get(x.name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, x.name+" isn't an RFC 3339 time")
			}
			*x.t = t
		}
	}
	campaign, _ := strconv.Atoi(qp.Get("campaign"))

	cond, err := denmaStatSQL(qp.Get("metric"), from, to, campaign)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	search, err := a.denmaSearchSQL(denmaSubscribers, user, qp.Get("search"))
	if err != nil {
		return err
	}
	if search != "" {
		cond = "(" + cond + ") AND " + search
	}

	listIDs, err := a.filterListQueryByPerm("list_id", qp, user)
	if err != nil {
		return err
	}
	pg := a.pg.NewFromURL(qp)
	res, total, err := a.core.QuerySubscribers("", "("+cond+")", listIDs, "", "", "", "", pg.Offset, pg.Limit)
	if err != nil {
		return err
	}
	for i := range res {
		maskRestrictedSubLists(user, &res[i])
	}
	return c.JSON(http.StatusOK, okResp{models.PageResults{
		Results:   res,
		PageProps: models.PageProps{Total: total, Page: pg.Page, PerPage: pg.PerPage},
	}})
}
