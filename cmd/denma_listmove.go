package main

// denma: moving subscribers between lists, one or many.

import (
	"net/http"
	"strings"
	"sync"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

func initDenmaListMoveHandlers(g *echo.Group, a *App) {
	g.PUT("/api/denma/subscribers/move", a.auth.Perm(a.DenmaMoveSubscribers, "subscribers:manage"))
}

// denmaMovedFrom is the subscription meta key naming the lists a pending opt-in was moved from.
const denmaMovedFrom = "moved_from"

// denmaMovedLists adds the lists their person was moved to from those an opt-in link names.
func (a *App) denmaMovedLists(subUUID string, listUUIDs *[]string) {
	if len(*listUUIDs) == 0 {
		return
	}
	var moved []string
	if err := a.db.Select(&moved, `SELECT l.uuid::TEXT FROM subscriber_lists sl
		JOIN subscribers s ON s.id = sl.subscriber_id AND s.uuid = $1::UUID
		JOIN lists l ON l.id = sl.list_id
		WHERE sl.status = 'unconfirmed' AND sl.meta->'`+denmaMovedFrom+`' ?| $2`, subUUID, pq.Array(*listUUIDs)); err != nil {
		a.log.Printf("denma: error finding the lists a pending opt-in was moved to: %v", err)
		return
	}
	*listUUIDs = append(*listUUIDs, moved...)
}

// denmaMoveSQL moves the subscribers subs gives from list from to list to.
func denmaMoveSQL(subs, from, to, pre, run string) string {
	return `WITH subs AS (` + subs + `),
quiet AS (
    SELECT set_config('denma.quiet_joins', CASE WHEN ` + run + `::BOOLEAN THEN '' ELSE 'on' END, true) AS v
),
added AS (
    INSERT INTO subscriber_lists (subscriber_id, list_id, status, meta)
    SELECT sl.subscriber_id, t.id, (CASE
        WHEN sl.status = 'unsubscribed' THEN 'unsubscribed'
        WHEN t.optin = 'double' AND ` + pre + `::BOOLEAN THEN 'confirmed'
        WHEN t.optin = 'double' AND NOT (f.optin = 'double' AND sl.status = 'confirmed') THEN 'unconfirmed'
        ELSE sl.status END)::subscription_status,
        (CASE WHEN f.optin = 'double' AND sl.status = 'unconfirmed' AND t.optin = 'double' AND NOT ` + pre + `::BOOLEAN
            THEN jsonb_build_object('` + denmaMovedFrom + `', COALESCE(sl.meta->'` + denmaMovedFrom + `', '[]'::JSONB) || to_jsonb(f.uuid::TEXT))
            ELSE '{}'::JSONB END)
    FROM subscriber_lists sl
    JOIN lists f ON f.id = sl.list_id
    JOIN lists t ON t.id = ` + to + `::INT
    WHERE sl.list_id = ` + from + `::INT AND sl.subscriber_id IN (SELECT id FROM subs)
        AND NOT (f.optin = 'double' AND sl.status = 'unconfirmed' AND t.optin <> 'double')
        AND (SELECT v FROM quiet) IS NOT NULL
    ON CONFLICT (subscriber_id, list_id) DO NOTHING
    RETURNING subscriber_id
)
DELETE FROM subscriber_lists sl
WHERE sl.list_id = ` + from + `::INT AND sl.subscriber_id IN (SELECT id FROM subs)
    AND (sl.subscriber_id IN (SELECT subscriber_id FROM added)
        OR EXISTS (SELECT 1 FROM subscriber_lists x WHERE x.list_id = ` + to + `::INT AND x.subscriber_id = sl.subscriber_id))`
}

// denmaQuietOff is logged once if listmonk's add-by-query SQL has no %query% placeholder.
var denmaQuietOff sync.Once

// denmaNoAPIList refuses an API campaign's list among listIDs.
func (a *App) denmaNoAPIList(listIDs []int) error {
	var n int
	if err := a.db.Get(&n, `SELECT COUNT(*) FROM lists WHERE id = ANY($1) AND '`+denmaAPIListTag+`' = ANY(COALESCE(tags, '{}'))`,
		pq.Array(listIDs)); err != nil {
		return denmaInternal(err)
	}
	if n > 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "An API campaign's list can't be used here: it's the campaign's own, and goes with it. Choose other lists.")
	}
	return nil
}

// denmaAddSubscriptions is listmonk's AddSubscriptions, without automations unless run is nil or true.
func (a *App) denmaAddSubscriptions(subIDs, listIDs []int, status string, run *bool) error {
	if err := a.denmaNoAPIList(listIDs); err != nil {
		return err
	}
	if run == nil || *run {
		return a.core.AddSubscriptions(subIDs, listIDs, status)
	}
	err := func() error {
		tx, err := a.db.Beginx()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.Exec(`SELECT set_config('denma.quiet_joins', 'on', true)`); err != nil {
			return err
		}
		if _, err := tx.Stmtx(a.queries.AddSubscribersToLists).Exec(pq.Array(subIDs), pq.Array(listIDs), status); err != nil {
			return err
		}
		return tx.Commit()
	}()
	if err != nil {
		a.log.Printf("denma: error adding subscriptions: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError,
			a.i18n.Ts("globals.messages.errorUpdating", "name", "{globals.terms.subscribers}", "error", err.Error()))
	}
	return nil
}

// denmaAddSubscriptionsByQuery is listmonk's AddSubscriptionsByQuery, with the same run rule.
func (a *App) denmaAddSubscriptionsByQuery(search, query string, sourceListIDs, targetListIDs []int, status, subStatus string, run *bool) error {
	if err := a.denmaNoAPIList(targetListIDs); err != nil {
		return err
	}
	tpl := a.queries.AddSubscribersToListsByQuery
	if run == nil || *run || !strings.Contains(tpl, "%query%") {
		if run != nil && !*run {
			denmaQuietOff.Do(func() {
				a.log.Printf("denma: add-subscribers-to-lists-by-query has no %%query%%: adds run automations")
			})
		}
		return a.core.AddSubscriptionsByQuery(search, query, sourceListIDs, targetListIDs, status, subStatus)
	}
	tpl = strings.Replace(tpl, "%query%", `SELECT q.* FROM (%query%) q WHERE (SELECT set_config('denma.quiet_joins', 'on', true)) IS NOT NULL`, 1)
	if sourceListIDs == nil {
		sourceListIDs = []int{}
	}
	if err := a.queries.ExecSubQueryTpl(search, query, tpl, sourceListIDs, a.db, subStatus, pq.Array(targetListIDs), status); err != nil {
		a.log.Printf("denma: error adding subscriptions by query: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError,
			a.i18n.Ts("globals.messages.errorUpdating", "name", "{globals.terms.subscribers}", "error", err.Error()))
	}
	return nil
}

// DenmaMoveSubscribers moves the subscribers picked, or every one matching a search.
func (a *App) DenmaMoveSubscribers(c echo.Context) error {
	user := auth.GetUser(c)
	var req struct {
		subQueryReq
		FromListID  int  `json:"from_list_id"`
		ToListID    int  `json:"to_list_id"`
		Preconfirm  bool `json:"preconfirm"`
		Automations bool `json:"run_automations"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	if req.FromListID < 1 || req.ToListID < 1 || req.FromListID > 1<<31-1 || req.ToListID > 1<<31-1 {
		return echo.NewHTTPError(http.StatusBadRequest, "Pick the list to move from and the list to move to.")
	}
	if req.FromListID == req.ToListID {
		return echo.NewHTTPError(http.StatusBadRequest, "Pick a different list to move to.")
	}
	if len(user.FilterListsByPerm(auth.PermTypeManage, []int{req.FromListID, req.ToListID})) != 2 {
		return echo.NewHTTPError(http.StatusForbidden, a.i18n.Ts("globals.messages.permissionDenied", "name", "lists"))
	}
	var n int
	if err := a.db.Get(&n, `SELECT COUNT(*) FROM lists WHERE id IN ($1, $2)`, req.FromListID, req.ToListID); err != nil {
		a.log.Printf("denma: error moving subscribers: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error moving the subscribers.")
	} else if n != 2 {
		return echo.NewHTTPError(http.StatusBadRequest, "That list doesn't exist.")
	}
	if err := a.denmaNoAPIList([]int{req.FromListID, req.ToListID}); err != nil {
		return err
	}

	if len(req.SubscriberIDs) > 0 {
		if err := a.hasSubPerm(user, auth.PermTypeManage, req.SubscriberIDs); err != nil {
			return err
		}
		res, err := a.db.Exec(denmaMoveSQL(`SELECT UNNEST($1::INT[]) AS id`, "$2", "$3", "$4", "$5"),
			pq.Array(req.SubscriberIDs), req.FromListID, req.ToListID, req.Preconfirm, req.Automations)
		if err != nil {
			a.log.Printf("denma: error moving subscribers: %v", err)
			return echo.NewHTTPError(http.StatusInternalServerError, "Error moving the subscribers.")
		}
		moved, _ := res.RowsAffected()
		return c.JSON(http.StatusOK, okResp{map[string]int64{"moved": moved}})
	}

	// Every subscriber matching the search, as DenmaTagSubscribers.
	req.Search = strings.TrimSpace(req.Search)
	req.Query = formatSQLExp(req.Query)
	if req.All {
		req.Search, req.Query = "", ""
	} else if req.Search == "" && req.Query == "" {
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.Ts("globals.messages.invalidFields", "name", "query"))
	}
	if err := a.denmaBulkSearch(user, &req.subQueryReq); err != nil {
		return err
	}
	listIDs := user.GetPermittedListIDs(auth.PermTypeManage, req.ListIDs)
	if all, _ := user.GetPermittedLists(auth.PermTypeManage); !all && len(listIDs) == 0 {
		return echo.NewHTTPError(http.StatusForbidden, a.i18n.Ts("globals.messages.permissionDenied", "name", "lists"))
	}
	if err := a.queries.ExecSubQueryTpl(req.Search, sanitizeDenmaSQL(req.Query), denmaMoveSQL("%query%", "$5", "$6", "$7", "$8"),
		listIDs, a.db, req.SubscriptionStatus, req.FromListID, req.ToListID, req.Preconfirm, req.Automations); err != nil {
		a.log.Printf("denma: error moving subscribers: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error moving the subscribers.")
	}
	return c.JSON(http.StatusOK, okResp{true})
}
