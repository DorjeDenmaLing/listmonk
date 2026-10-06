package main

// denma: one address for every center's admin. In multi-center mode the
// admin is at /admin (and its API at /api) whichever center it shows: the
// place cookie says which, a center's slug or none for the hub. Signing in
// to a center, switching to another or opening one from the hub sets it; a
// hub-only page (centers, people, settings...) clears it. Each center keeps
// its own session, in a cookie of its own name (session_<slug>) at /.
//
// A center's public pages (subscription, opt-in, archive, uploads, sign-up
// webhooks) and its API for API users stay under /c/<slug>/: they're in
// e-mails and on websites, and don't depend on who is signed in. An old
// admin address, /c/<slug>/admin/..., selects that center and goes to
// /admin/....

import (
	"net/http"
	"strings"
	"time"

	"github.com/knadh/koanf/v2"
	"github.com/labstack/echo/v4"
)

// denmaPlaceCookie holds the center the admin shows ("" or absent: the hub).
const denmaPlaceCookie = "denma_center"

// denmaAdminPath is an App's admin root path: "/" for the hub and every
// center in multi-center mode, else the root URL's path.
func denmaAdminPath(ko *koanf.Koanf, rootPath string) string {
	if ko.Bool("denma.multi_center") || ko.String("denma.center") != "" {
		return "/"
	}
	return rootPath
}

// denmaCookieName is a session cookie's name in an App: a center's has its
// slug, as all are at the same path.
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

// denmaHubOnlyPath reports whether an admin path is the hub's alone, which
// centers don't have: it's the hub's wherever the admin is.
func denmaHubOnlyPath(p string) bool {
	if denmaCenterSettingsPath(p) {
		return true
	}
	for _, pre := range []string{
		"/admin/hub", "/admin/centers", "/admin/people", "/admin/domains", "/admin/analytics", "/admin/system",
		"/api/denma/centers", "/api/denma/people", "/api/denma/domains", "/api/denma/hub",
	} {
		if p == pre || strings.HasPrefix(p, pre+"/") {
			return true
		}
	}
	return false
}

// placeCenter is the center that serves an admin request at /admin or
// /api, or nil for the hub.
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

// DenmaToHub is the top bar's way back to the hub, for superadmins in a
// center: the hub's dashboard (its being hub-only clears the place).
func (a *App) DenmaToHub(c echo.Context) error {
	return c.Redirect(http.StatusFound, uriAdmin)
}
