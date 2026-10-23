package main

import (
	"net/http"
	"path"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/labstack/echo/v4"
)

// denmaViewPerms is the permissions each admin page needs, by its route:
// one of each group. Pages that check their own aren't here.
var denmaViewPerms = map[string][][]string{}

func init() {
	subsGet := []string{"subscribers:get_all", "subscribers:get"}
	campsGet := []string{"campaigns:get_all", "campaigns:get"}
	for p, perms := range map[string][][]string{
		"/lists/new":                {{"lists:manage_all"}},
		"/campaigns":                {campsGet},
		"/campaigns/new":            {{"campaigns:manage_all", "campaigns:manage"}},
		"/campaigns/:id":            {campsGet},
		"/campaigns/:id/:tab":       {campsGet},
		"/campaigns/analytics":      {{"campaigns:get_analytics"}},
		"/campaigns/media":          {{"media:get"}},
		"/campaigns/media/fragment": {{"media:get"}},
		"/templates":                {{"templates:get"}},
		"/templates/new":            {{"templates:manage"}},
		"/templates/:id":            {{"templates:get"}},
		"/subscribers":              {subsGet},
		"/subscribers/lists/:id":    {subsGet},
		"/subscribers/new":          {{"subscribers:manage"}},
		"/subscribers/import":       {{"subscribers:import"}},
		"/subscribers/bounces":      {{"bounces:get"}},
		"/subscribers/:id":          {subsGet},
		"/subscribers/:id/lists":    {subsGet},
		"/subscribers/:id/activity": {subsGet},
		"/subscribers/:id/bounces":  {subsGet, {"bounces:get"}},
	} {
		denmaViewPerms[path.Join(uriAdmin, p)] = perms
	}
}

// denmaViewPerm refuses an admin page the user hasn't the permissions for.
func denmaViewPerm(c echo.Context) error {
	perms, ok := denmaViewPerms[c.Path()]
	if !ok {
		return nil
	}
	u := auth.GetUser(c)
	for _, group := range perms {
		has := false
		for _, p := range group {
			has = has || u.HasPerm(p)
		}
		if !has {
			return echo.NewHTTPError(http.StatusForbidden, "permission denied: "+group[0])
		}
	}
	return nil
}
