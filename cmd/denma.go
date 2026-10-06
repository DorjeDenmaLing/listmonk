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

	"github.com/labstack/echo/v4"
)

// initDenmaAdminHandlers registers the denma admin pages on the authenticated admin group.
func initDenmaAdminHandlers(g *echo.Group, a *App) {
	g.GET(path.Join(uriAdmin, "/calendar"), a.ViewDenmaCalendar)
	g.GET(path.Join(uriAdmin, "/system"), a.ViewDenmaSystem) // cmd/denma_system.go
	// System's old address, under Settings.
	g.GET(path.Join(uriAdmin, "/settings/system"), func(c echo.Context) error {
		return c.Redirect(http.StatusMovedPermanently, path.Join(a.urlCfg.AdminPath, uriAdmin, "/system"))
	})
	initDenmaHubHandlers(g, a)
	initDenmaPeopleHandlers(g, a) // cmd/denma_people.go
	initDenmaCenterHandlers(g, a)
	initDenmaAutomationHandlers(g, a)
	initDenmaTagHandlers(g, a)
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
