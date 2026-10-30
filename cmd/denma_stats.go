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
	"strings"
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

// denmaOwnUnsubSQL is an opt-out (denma_optouts) that's someone's own
// unsubscribe, not an import's or a complaint.
const denmaOwnUnsubSQL = `kind = 'unsubscribed' AND source = 'unsubscribe'`

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

	// Unsubscribed themselves in the period: their opt-out
	// (cmd/denma_optouts.go), not a list row an admin's add or a move made.
	case "unsub":
		if err := needPeriod(); err != nil {
			return "", err
		}
		return "denma_email_hash(subscribers.email) IN (SELECT email_hash FROM denma_optouts WHERE " + denmaOwnUnsubSQL + " AND " + between("created_at") + ")", nil

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

// denmaProvidersSQL is, by provider, who received, opened and clicked each
// campaign in $1, as the received, opens and clicks figures count them, of the
// subscribers on a list in $2 (all if empty).
const denmaProvidersSQL = `WITH camp AS (
	SELECT id, started_at, max_subscriber_id FROM campaigns WHERE id = ANY($1::INT[])
), prov AS MATERIALIZED (
	SELECT s.id, s.status, s.updated_at, P(s.email) AS provider FROM subscribers s
	WHERE COALESCE(CARDINALITY($2::INT[]), 0) = 0 OR EXISTS (SELECT 1 FROM subscriber_lists x WHERE x.subscriber_id = s.id AND x.list_id = ANY($2::INT[]))
), got AS (
	` + denmaGotSQL + `
)
SELECT 'received' AS kind, got.campaign_id, p.provider, COUNT(*) AS n FROM got JOIN prov p ON p.id = got.subscriber_id
	WHERE p.status <> 'blocklisted' OR p.updated_at > got.started_at GROUP BY 2, 3
UNION ALL
SELECT 'opens', v.campaign_id, p.provider, COUNT(DISTINCT v.subscriber_id) FROM campaign_views v JOIN prov p ON p.id = v.subscriber_id
	WHERE v.campaign_id = ANY($1::INT[]) GROUP BY 2, 3
UNION ALL
SELECT 'clicks', k.campaign_id, p.provider, COUNT(DISTINCT k.subscriber_id) FROM link_clicks k JOIN prov p ON p.id = k.subscriber_id
	WHERE k.campaign_id = ANY($1::INT[]) GROUP BY 2, 3
UNION ALL
SELECT 'active', 0, p.provider, COUNT(*) FROM subscribers JOIN prov p ON p.id = subscribers.id WHERE ACTIVE GROUP BY 3
UNION ALL
SELECT 'unsub', 0, p.provider, COUNT(*) FROM subscribers JOIN prov p ON p.id = subscribers.id WHERE UNSUB GROUP BY 3`

// DenmaStatProviders returns the Mailbox providers figures in one request:
// active subscribers and unsubscribes in the period, by provider, and each
// campaign's received, opens and clicks by provider.
//
//	GET /api/denma/stats/providers?id=12&id=13&from=<RFC 3339>&to=...
//	{ "active": { "gmail": 120, ..., "other": 80 }, "unsub": {...},
//	  "campaigns": { "12": { "received": {...}, "opens": {...}, "clicks": {...} } } }
func (a *App) DenmaStatProviders(c echo.Context) error {
	var (
		user = auth.GetUser(c)
		qp   = c.QueryParams()
		t    [2]time.Time
	)
	for i, k := range []string{"from", "to"} {
		var err error
		if t[i], err = time.Parse(time.RFC3339, qp.Get(k)); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, k+" isn't an RFC 3339 time")
		}
	}
	ids, err := getQueryInts("id", qp)
	if err != nil || len(ids) > 100 {
		return echo.NewHTTPError(http.StatusBadRequest, "id: up to 100 campaign IDs")
	}
	active, _ := denmaStatSQL("active", t[0], t[1], 0)
	unsub, _ := denmaStatSQL("unsub", t[0], t[1], 0)
	listIDs, err := a.filterListQueryByPerm("list_id", qp, user)
	if err != nil {
		return err
	}

	q := strings.NewReplacer("S.", "", "P(s.email)", denmaProviderSQL("s.email"), "ACTIVE", "("+active+")", "UNSUB", "("+unsub+")").Replace(denmaProvidersSQL)
	var rows []struct {
		Kind     string `db:"kind"`
		Campaign int    `db:"campaign_id"`
		Provider string `db:"provider"`
		N        int    `db:"n"`
	}
	if err := a.db.Select(&rows, q, pq.Array(ids), pq.Array(listIDs)); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	out := map[string]any{"active": map[string]int{}, "unsub": map[string]int{}}
	camps := map[int]map[string]map[string]int{}
	for _, id := range ids {
		camps[id] = map[string]map[string]int{"received": {}, "opens": {}, "clicks": {}}
	}
	for _, r := range rows {
		if r.Campaign == 0 {
			out[r.Kind].(map[string]int)[r.Provider] = r.N
		} else if m := camps[r.Campaign]; m != nil {
			m[r.Kind][r.Provider] = r.N
		}
	}
	out["campaigns"] = camps
	return c.JSON(http.StatusOK, okResp{out})
}

// DenmaUntrackedCampaigns returns the IDs of the campaigns whose opens are
// counted only in total, not per subscriber: all their views are without one
// (sent while individual tracking was off, as before production turned it
// on), or they started before Maintenance deleted the center's analytics. A
// deleted subscriber's views are anonymous too, but leave a campaign with
// others counted. Open and click rates leave them out (denma-stats.js
// isTracked), as the hub's figures do (denmaTrackedCampSQL).
//
//	GET /api/denma/stats/untracked
func (a *App) DenmaUntrackedCampaigns(c echo.Context) error {
	ids := []int{}
	if err := a.db.Select(&ids, `SELECT id FROM (
		SELECT campaign_id AS id FROM campaign_views GROUP BY 1 HAVING COUNT(subscriber_id) = 0
		UNION SELECT id FROM campaigns WHERE started_at < (SELECT since FROM denma_stat_starts WHERE stat = '`+denmaAnalyticsDeleted+`')
	) u ORDER BY 1`); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{ids})
}
