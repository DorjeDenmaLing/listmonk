package main

// denma: a list an automation or a sign-up webhook uses can't be deleted
// (Lists: one, those picked, or every one matching the search); the refusal
// names what uses it, to be changed first. Server webhooks are changed by a
// superadmin.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

// denmaListsMatchSQL is the lists listmonk's delete-lists query deletes by a
// search ($1, spaces collapsed), type, opt-in, status and tags, of those the
// user may manage ($6 all, else $7).
const denmaListsMatchSQL = `SELECT id FROM lists
WHERE ($1 = '' OR to_tsvector(name) @@ plainto_tsquery($1) OR name ILIKE ('%' || $1 || '%'))
AND ($2 = '' OR type = $2::list_type)
AND ($3 = '' OR optin = $3::list_optin)
AND ($4 = '' OR status = $4::list_status)
AND (CARDINALITY($5::VARCHAR(100)[]) = 0 OR $5 <@ tags)
AND ($6 OR id = ANY($7::INT[]))`

// denmaListsInUseMatching is denmaListsInUse for the lists a delete by
// search would delete.
func (a *App) denmaListsInUseMatching(query, typ, optin, status string, tags []string, all bool, permitted []int) error {
	if tags == nil {
		tags = []string{}
	}
	var ids []int
	if err := a.db.Select(&ids, denmaListsMatchSQL, strings.Join(strings.Fields(query), " "), typ, optin, status,
		pq.StringArray(tags), all, pq.Array(permitted)); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return a.denmaListsInUse(ids)
}

// denmaListsInUse refuses to delete lists an automation or a sign-up webhook
// uses, naming them.
func (a *App) denmaListsInUse(ids []int) error {
	if len(ids) == 0 {
		return nil
	}
	var uses []struct {
		List string `db:"list"`
		What string `db:"what"`
		Name string `db:"name"`
	}
	err := a.db.Select(&uses, `SELECT l.name AS list, 'automation' AS what, au.name FROM denma_automation_lists al
		JOIN denma_automations au ON au.id = al.automation_id JOIN lists l ON l.id = al.list_id WHERE al.list_id = ANY($1)
		UNION
		SELECT l.name, CASE WHEN h.kind = 'server' THEN 'Server webhook' ELSE 'sign-up webhook' END, h.name FROM denma_signup_hook_lists hl
		JOIN denma_signup_hooks h ON h.id = hl.hook_id JOIN lists l ON l.id = hl.list_id WHERE hl.list_id = ANY($1)
		ORDER BY 1, 2, 3`, pq.Array(ids))
	if err != nil {
		var pe *pq.Error
		if errors.As(err, &pe) && pe.Code == "42P01" {
			return nil // no automations or webhooks here (undefined table)
		}
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if len(uses) == 0 {
		return nil
	}
	names := make([]string, 0, len(uses))
	for i, u := range uses {
		if i == 5 {
			names = append(names, fmt.Sprintf("%d more", len(uses)-5))
			break
		}
		names = append(names, fmt.Sprintf("%s by the %s %q", u.List, u.What, u.Name))
	}
	return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("A list in use can't be deleted: %s. Change or delete those first "+
		"(a superadmin changes Server webhooks).", orList(names, "and")))
}
