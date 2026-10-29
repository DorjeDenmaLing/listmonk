package main

// denma: a superadmin who hasn't two-factor sign-in is taken, after signing in,
// to a page asking them to set it up (the Profile page's switch), with a
// "Later" that lasts that sign-in. Nothing is blocked. Center admins and
// people aren't asked.

import (
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/internal/utils"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
)

const (
	denmaTwofaPromptPath = "/denma-twofa"
	denmaTwofaLaterPath  = "/denma-twofa/later"
	denmaTwofaLater      = "denma_twofa_later" // the session's hash, once they chose Later
)

// denmaTwofaPromptDue reports whether a request is the first page of a hub
// superadmin without two-factor sign-in, who hasn't chosen Later: a page
// someone opens (GET, as HTML), not the Profile page or this one.
func denmaTwofaPromptDue(u auth.User, method, reqPath, accept string, later bool) bool {
	if u.Type != auth.UserTypeUser || u.UserRole.ID != auth.SuperAdminRoleID || !u.PasswordLogin || u.TwofaType == models.TwofaTypeTOTP || later {
		return false
	}
	if method != http.MethodGet || !strings.Contains(accept, "text/html") {
		return false
	}
	switch reqPath {
	case path.Join(uriAdmin, "/user/profile"), path.Join(uriAdmin, denmaTwofaPromptPath):
		return false
	}
	return true
}

// denmaTwofaPrompt sends the hub superadmin who's due to the prompt page,
// reporting whether it did.
func (a *App) denmaTwofaPrompt(c echo.Context) (bool, error) {
	if denmaHub == nil || a.denmaInCenter() {
		return false, nil
	}
	req := c.Request()
	if d := req.Header.Get("Sec-Fetch-Dest"); d != "" && d != "document" {
		return false, nil // a frame or a script's fetch, not a page
	}
	later := false
	if ck, err := c.Cookie(denmaTwofaLater); err == nil {
		later = ck.Value != "" && ck.Value == denmaAPIHash(auth.GetSessionID(c))
	}
	if !denmaTwofaPromptDue(auth.GetUser(c), req.Method, req.URL.Path, req.Header.Get("Accept"), later) {
		return false, nil
	}
	return true, c.Redirect(http.StatusFound, path.Join(uriAdmin, denmaTwofaPromptPath)+"?next="+url.QueryEscape(req.RequestURI))
}

// denmaTwofaPage is the prompt page's data.
type denmaTwofaPage struct {
	adminView
	Next string
}

// ViewDenmaTwofaPrompt asks a superadmin to set up two-factor sign-in.
func (a *App) ViewDenmaTwofaPrompt(c echo.Context) error {
	next := utils.SanitizeURI(c.QueryParam("next"))
	if next == "/" || strings.HasPrefix(next, path.Join(uriAdmin, denmaTwofaPromptPath)) {
		next = uriAdmin
	}
	return c.Render(http.StatusOK, "admin-denma-twofa-prompt", denmaTwofaPage{
		adminView: newAdminView(c, "Two-factor sign-in", "", "denma.twofa"),
		Next:      next,
	})
}

// DenmaTwofaLater stops the prompt for this sign-in, and goes on to the page
// they wanted.
func (a *App) DenmaTwofaLater(c echo.Context) error {
	c.SetCookie(&http.Cookie{
		Name:     denmaTwofaLater,
		Value:    denmaAPIHash(auth.GetSessionID(c)),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   c.Scheme() == "https" || c.Request().Header.Get("X-Forwarded-Proto") == "https",
	})
	next := utils.SanitizeURI(c.FormValue("next"))
	if next == "/" || strings.HasPrefix(next, path.Join(uriAdmin, denmaTwofaPromptPath)) {
		next = uriAdmin
	}
	return c.Redirect(http.StatusFound, next)
}

// initDenmaTwofaHandlers registers the prompt page.
func initDenmaTwofaHandlers(g *echo.Group, a *App) {
	g.GET(path.Join(uriAdmin, denmaTwofaPromptPath), a.ViewDenmaTwofaPrompt)
	g.POST(path.Join(uriAdmin, denmaTwofaLaterPath), a.DenmaTwofaLater)
}
