package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/internal/auth"
	"github.com/labstack/echo/v4"
)

// denmaHubRoutes are the routes the hub serves (METHOD path): the ones
// registered that denmaHubPath lets through. A new route under /admin, /api/,
// /public/, /health or /webhooks/ is the hub's too unless denmaCenterContent
// refuses it, so each must be listed here on purpose.
var denmaHubRoutes = []string{
	"DELETE /api/denma/domains/:domain",
	"DELETE /api/denma/domains/:domain/centers/:slug",
	"DELETE /api/denma/mcp/connector",
	"DELETE /api/denma/media/folders/:id",
	"DELETE /api/denma/people/:id/twofa",
	"DELETE /api/maintenance/analytics/:type",
	"DELETE /api/maintenance/subscribers/:type",
	"DELETE /api/maintenance/subscriptions/unconfirmed",
	"DELETE /api/media/:id",
	"DELETE /api/roles/:id",
	"DELETE /api/users",
	"DELETE /api/users/:id",
	"DELETE /api/users/:id/twofa",
	"GET /admin",
	"GET /admin/activity",
	"GET /admin/analytics",
	"GET /admin/campaigns/media",
	"GET /admin/campaigns/media/fragment",
	"GET /admin/centers",
	"GET /admin/centers/:slug/open",
	"GET /admin/centers/new",
	"GET /admin/custom.css",
	"GET /admin/custom.js",
	"GET /admin/denma-twofa",
	"GET /admin/domains",
	"GET /admin/domains/:domain",
	"GET /admin/forgot",
	"GET /admin/hub",
	"GET /admin/login",
	"GET /admin/login/twofa",
	"GET /admin/mcp",
	"GET /admin/people",
	"GET /admin/reset",
	"GET /admin/settings",
	"GET /admin/settings/logs",
	"GET /admin/settings/maintenance",
	"GET /admin/settings/system",
	"GET /admin/switch/:slug",
	"GET /admin/system",
	"GET /admin/user/profile",
	"GET /admin/users",
	"GET /admin/users/:id",
	"GET /admin/users/new",
	"GET /admin/users/roles",
	"GET /admin/users/roles/:id",
	"GET /admin/users/roles/lists",
	"GET /admin/users/roles/lists/:id",
	"GET /admin/users/roles/lists/new",
	"GET /admin/users/roles/new",
	"GET /api/about",
	"GET /api/config",
	"GET /api/denma/audit",
	"GET /api/denma/hub/analytics",
	"GET /api/denma/hub/stats",
	"GET /api/denma/media/:id/uses",
	"GET /api/denma/media/folders",
	"GET /api/events",
	"GET /api/health",
	"GET /api/lang/:lang",
	"GET /api/logs",
	"GET /api/maintenance/analytics/:type/export",
	"GET /api/media",
	"GET /api/media/:id",
	"GET /api/profile",
	"GET /api/roles/lists",
	"GET /api/roles/users",
	"GET /api/settings",
	"GET /api/users",
	"GET /api/users/:id",
	"GET /api/users/:id/twofa/totp",
	"GET /health",
	"GET /health/centers",
	"GET /public/custom.css",
	"GET /public/custom.js",
	"POST /admin/denma-twofa/later",
	"POST /admin/forgot",
	"POST /admin/login",
	"POST /admin/login/twofa",
	"POST /admin/reset",
	"POST /api/admin/reload",
	"POST /api/denma/centers",
	"POST /api/denma/domains",
	"POST /api/denma/domains/:domain/centers",
	"POST /api/denma/domains/:domain/check",
	"POST /api/denma/domains/:domain/fix",
	"POST /api/denma/mcp/connector",
	"POST /api/denma/media/folders",
	"POST /api/logout",
	"POST /api/media",
	"POST /api/roles/lists",
	"POST /api/roles/users",
	"POST /api/settings/smtp/test",
	"POST /api/users",
	"POST /webhooks/service/:service",
	"PUT /api/denma/centers/:slug/status",
	"PUT /api/denma/mcp/connector",
	"PUT /api/denma/media/folder",
	"PUT /api/denma/media/folders/:id",
	"PUT /api/profile",
	"PUT /api/roles/lists/:id",
	"PUT /api/roles/users/:id",
	"PUT /api/settings",
	"PUT /api/settings/:key",
	"PUT /api/users/:id",
	"PUT /api/users/:id/twofa",
}

// Every route registered is either one the hub serves, listed above, or
// refused by the hub (denmaHubPath).
func TestDenmaHubRoutesClassified(t *testing.T) {
	ko := koanf.New(".")
	_ = ko.Set("denma.multi_center", true)
	cfg := &Config{EnablePublicArchive: true, BounceWebhooksEnabled: true, EnablePublicSubPage: true}
	cfg.Security.OIDC.Enabled = true
	a := &App{ko: ko, cfg: cfg, urlCfg: &UrlConfig{LoginURL: "/login", AdminPath: "/"}, auth: &auth.Auth{}}
	e := echo.New()
	initHTTPHandlers(e, a)

	var served, refused int
	for _, r := range e.Routes() {
		if strings.HasPrefix(r.Method, "echo_") { // the 404 pages
			continue
		}
		if !denmaHubPath(r.Path) {
			refused++
			continue
		}
		served++
		if !slices.Contains(denmaHubRoutes, r.Method+" "+r.Path) {
			t.Errorf("%s %s is served by the hub but isn't classified: list it in denmaHubRoutes, or refuse it in denmaCenterContent", r.Method, r.Path)
		}
	}
	if served < 50 || refused < 50 {
		t.Errorf("read %d served and %d refused routes: too few", served, refused)
	}
	// Center content isn't the hub's; nor is the admins' bounce webhook.
	for _, rt := range e.Routes() {
		if rt.Path == "/webhooks/bounce" {
			t.Errorf("%s /webhooks/bounce is registered", rt.Method)
		}
		if (rt.Path == "/api/lists" || rt.Path == "/api/subscribers" || rt.Path == "/api/tx") && denmaHubPath(rt.Path) {
			t.Errorf("%s %s is served by the hub", rt.Method, rt.Path)
		}
	}
}
