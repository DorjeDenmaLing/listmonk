package main

// denma: one admin address (/admin, /api) for every center, chosen by the place cookie.

import (
	"net/http"
	"strings"
	"time"

	"github.com/knadh/koanf/v2"
	"github.com/labstack/echo/v4"
)

// denmaPlaceCookie holds the center the admin shows ("" or absent: the hub).
const denmaPlaceCookie = "denma_center"

// denmaAdminPath is an App's admin root path: "/" in multi-center mode, else the root URL's path.
func denmaAdminPath(ko *koanf.Koanf, rootPath string) string {
	if ko.Bool("denma.multi_center") || ko.String("denma.center") != "" {
		return "/"
	}
	return rootPath
}

// denmaCookieName is a session cookie's name in an App: a center's has its slug.
func denmaCookieName(ko *koanf.Koanf, name string) string {
	if slug := ko.String("denma.center"); slug != "" {
		return name + "_" + slug
	}
	return name
}

// denmaSetPlace makes the admin show a center (slug), or the hub ("").
func denmaSetPlace(c echo.Context, slug string) {
	ck := &http.Cookie{
		Name:     denmaPlaceCookie,
		Value:    slug,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   c.Scheme() == "https" || c.Request().Header.Get("X-Forwarded-Proto") == "https",
		MaxAge:   int((7 * 24 * time.Hour).Seconds()),
	}
	if slug == "" {
		ck.MaxAge = -1
	}
	c.SetCookie(ck)
}

// denmaPlace is the center the admin shows, or "" for the hub.
func denmaPlace(c echo.Context) string {
	ck, err := c.Cookie(denmaPlaceCookie)
	if err != nil {
		return ""
	}
	return ck.Value
}

// denmaAdminRequest reports whether a path is the admin's or its API's.
func denmaAdminRequest(p string) bool {
	return p == uriAdmin || strings.HasPrefix(p, uriAdmin+"/") || p == "/api" || strings.HasPrefix(p, "/api/")
}

// denmaHubOnlyPath reports whether an admin path is the hub's alone.
func denmaHubOnlyPath(p string) bool {
	// The hub's Analytics; a center's pages are under it (cmd/denma_analytics.go).
	if denmaCenterSettingsPath(p) || p == "/admin/analytics" {
		return true
	}
	for _, pre := range []string{
		"/admin/hub", "/admin/centers", "/admin/people", "/admin/domains", "/admin/system", "/admin/mcp",
		"/api/denma/centers", "/api/denma/people", "/api/denma/domains", "/api/denma/hub", "/api/denma/mcp",
	} {
		if p == pre || strings.HasPrefix(p, pre+"/") {
			return true
		}
	}
	return false
}

// placeCenter is the center that serves an admin request at /admin or /api, or nil for the hub.
func (d *denmaCenters) placeCenter(c echo.Context, p string) *denmaCenter {
	if !denmaAdminRequest(p) || denmaHubOnlyPath(p) {
		return nil
	}
	slug := denmaPlace(c)
	if slug == "" {
		return nil
	}
	return d.get(slug)
}

// denmaTabHeader is the center ("" for the hub) the admin page was loaded for (denma-place.js).
const denmaTabHeader = "X-Denma-Center"

// denmaCheckTab refuses an admin API request from a page loaded for another center than the place's.
func denmaCheckTab(c echo.Context, p string) error {
	if (p != "/api" && !strings.HasPrefix(p, "/api/")) || denmaHubOnlyPath(p) {
		return nil
	}
	h, ok := c.Request().Header[denmaTabHeader]
	if !ok || len(h) == 0 || h[0] == denmaPlace(c) {
		return nil
	}
	return echo.NewHTTPError(http.StatusConflict, "Another center was opened in another tab or window since this page was loaded. Reload the page to work there.")
}

// DenmaToHub is the top bar's way back to the hub, for superadmins in a center.
func (a *App) DenmaToHub(c echo.Context) error {
	return c.Redirect(http.StatusFound, uriAdmin)
}
