package main

// denma: Dorje Denma Ling additions to the admin, in denma* files; upstream files carry one-line hooks.

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
	initDenmaAnalyticsHandlers(g, a) // cmd/denma_analytics.go
	initDenmaTagHandlers(g, a)
	initDenmaTwofaHandlers(g, a) // cmd/denma_twofa_prompt.go
}

// ViewDenmaCalendar renders the campaign calendar; the page loads campaigns from /api/campaigns.
func (a *App) ViewDenmaCalendar(c echo.Context) error {
	v := newAdminView(c, "Calendar", "", "denma.calendar")
	if !v.Can("campaigns:get", "campaigns:get_all") {
		return echo.NewHTTPError(http.StatusForbidden, a.i18n.Ts("globals.messages.permissionDenied", "name", "campaigns:get"))
	}

	return c.Render(http.StatusOK, "admin-denma-calendar", v)
}
