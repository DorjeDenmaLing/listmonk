package main

// denma: a center's Analytics, a group in the sidebar
// (partials/denma/sidebar.html) of four pages, each with its own dates
// (partials/denma/analytics.html, views/denma-analytics.js):
//   - Audience: Audience growth and Mailbox providers.
//   - Campaigns: Campaign reports.
//   - Automations: Automation reports (cmd/denma_auto_reports.go).
//   - Explore: listmonk's Campaigns -> Analytics, where automations can be
//     picked as well as campaigns. It keeps its route, so reports and the
//     dashboard still link to it.
// Each needs Analytics' permission (campaigns:get_analytics) and those its
// figures do. The hub's Analytics is its own (cmd/denma_hub_analytics.go).

import (
	"net/http"
	"path"

	"github.com/labstack/echo/v4"
)

// denmaAnalyticsPages are the pages, with their titles.
var denmaAnalyticsPages = map[string]string{
	"audience":    "Audience",
	"campaigns":   "Campaigns",
	"automations": "Automations",
}

// denmaAnalyticsView is an Analytics page (views/denma-analytics.html).
type denmaAnalyticsView struct {
	adminView
	Page               string
	IndividualTracking bool
	DisableTracking    bool
}

// Shows reports whether the page has anything the user can see.
func (v denmaAnalyticsView) Shows() bool {
	camps := v.Can("campaigns:get", "campaigns:get_all")
	subs := v.Can("subscribers:get", "subscribers:get_all")
	switch v.Page {
	case "audience":
		return subs && (camps || v.Can("bounces:get"))
	case "campaigns":
		return camps
	case "automations":
		return v.Can(denmaAutoGet)
	}
	return false
}

func init() {
	for p := range denmaAnalyticsPages {
		denmaViewPerms[path.Join(uriAdmin, "/analytics", p)] = [][]string{{"campaigns:get_analytics"}}
	}
}

// initDenmaAnalyticsHandlers registers the pages (a center's; the hub has no
// center content, cmd/denma_centers.go).
func initDenmaAnalyticsHandlers(g *echo.Group, a *App) {
	for p := range denmaAnalyticsPages {
		g.GET(path.Join(uriAdmin, "/analytics", p), func(c echo.Context) error { return a.ViewDenmaAnalytics(c, p) })
	}
}

// ViewDenmaAnalytics renders an Analytics page.
func (a *App) ViewDenmaAnalytics(c echo.Context, page string) error {
	v := denmaAnalyticsView{
		adminView:          newAdminView(c, denmaAnalyticsPages[page], "", "analytics."+page),
		Page:               page,
		IndividualTracking: a.cfg.Privacy.IndividualTracking,
		DisableTracking:    a.cfg.Privacy.DisableTracking,
	}
	if page == "automations" && !denmaAutomationsOn(a) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if !v.Shows() {
		return echo.NewHTTPError(http.StatusForbidden, a.i18n.Ts("globals.messages.permissionDenied", "name", "analytics"))
	}
	return c.Render(http.StatusOK, "admin-denma-analytics", v)
}
