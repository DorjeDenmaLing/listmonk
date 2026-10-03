package main

// denma: Dorje Denma Ling additions to the admin.
//
// They live in their own files so that upstream merges stay clean:
//   cmd/denma*.go                               routes and handlers
//   static/admin/views/denma-*.html             new admin pages
//   static/admin/partials/denma/*.html          sections included in upstream pages
//   static/admin/assets/js/views/denma-*.js     page scripts (built automatically)
//   static/admin/assets/static/denma.css        styles (served at /admin/static/denma.css)
// Upstream files only carry one-line hooks into these; search for "denma" to find them.

import (
	"net/http"
	"path"

	"github.com/jmoiron/sqlx/types"
	"github.com/labstack/echo/v4"
)

// initDenmaAdminHandlers registers the denma admin pages on the authenticated admin group.
func initDenmaAdminHandlers(g *echo.Group, a *App) {
	g.GET(path.Join(uriAdmin, "/calendar"), a.ViewDenmaCalendar)
	g.GET(path.Join(uriAdmin, "/settings/system"), a.ViewDenmaSystem)
	initDenmaHubHandlers(g, a)
	initDenmaCenterHandlers(g, a)
	initDenmaAutomationHandlers(g, a)
	initDenmaTagHandlers(g, a)
}

type denmaSystemView struct {
	adminView
	System systemStats
	About  about
	DB     struct {
		Version string  `json:"version"`
		SizeMB  float64 `json:"size_mb"`
	}
}

// ViewDenmaSystem renders Settings -> System: the server stats that upstream
// shows on the dashboard (getSystemStats in dashboard.go), plus what listmonk
// collects for /api/about: version, build, database and host.
func (a *App) ViewDenmaSystem(c echo.Context) error {
	v := newAdminView(c, a.i18n.T("dashboard.system"), "", "settings.system")
	if !v.Can("settings:get") {
		return echo.NewHTTPError(http.StatusForbidden, a.i18n.Ts("globals.messages.permissionDenied", "name", "settings:get"))
	}

	out := denmaSystemView{adminView: v, System: getSystemStats(), About: a.about}

	// The database's version and size, now (a.about has them from startup).
	var info types.JSONText
	if err := a.db.QueryRow(a.queries.GetDBInfo).Scan(&info); err != nil {
		a.log.Printf("error getting database info: %v", err)
	} else if err := info.Unmarshal(&out.DB); err != nil {
		a.log.Printf("error reading database info: %v", err)
	}

	return c.Render(http.StatusOK, "admin-denma-system", out)
}

// ViewDenmaCalendar renders the campaign calendar. Campaigns are loaded by the page
// from /api/campaigns, which limits them to the lists the user can see.
func (a *App) ViewDenmaCalendar(c echo.Context) error {
	v := newAdminView(c, "Calendar", "", "denma.calendar")
	if !v.Can("campaigns:get", "campaigns:get_all") {
		return echo.NewHTTPError(http.StatusForbidden, a.i18n.Ts("globals.messages.permissionDenied", "name", "campaigns:get"))
	}

	return c.Render(http.StatusOK, "admin-denma-calendar", v)
}
