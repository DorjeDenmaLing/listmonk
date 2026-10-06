package main

// denma: one sign-in page for everyone, at /login (and /login/forgot for a
// forgotten password), outside /admin: the web server keeps the hub's admin
// behind Cloudflare Access, for superadmins, while people sign in here.
//
// A superadmin (a hub user) signs in with listmonk's own sign-in, the hub's,
// and lands in the hub. Anyone else is a person (cmd/denma_people.go), who
// signs in with their e-mail address or username, their password and, if
// they've turned it on, a two-factor code (/login/twofa), and lands in their
// center. Someone who is a user of more than one center chooses one
// (/login/centers), unless the page they wanted was in one of them, and can
// switch between them later from the menu under their picture. A wrong
// password, or a name nobody has, gets listmonk's usual message.
//
// A forgotten password is e-mailed to the person (or the superadmin) with
// that address, as a link to /login/reset; the page says the same either way.
// The same page sets a new person's first password (addMember).
//
// These are the hub's pages (initDenmaLoginHandlers); the hub's server hands
// /login and what's under it to the hub's router before anything else
// (loginRoute). The old sign-in pages, the hub's and each center's
// (/c/<slug>/admin/login, where listmonk sends someone who isn't signed in),
// send people here, with the page they wanted.

import (
	"bytes"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/knadh/listmonk/internal/i18n"
	"github.com/knadh/listmonk/internal/notifs"
	"github.com/knadh/listmonk/internal/tmptokens"
	"github.com/knadh/listmonk/internal/utils"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/pquerna/otp/totp"
)

const (
	denmaLoginPath   = "/login"
	denmaForgotPath  = "/login/forgot"
	denmaTwofaPath   = "/login/twofa"
	denmaCentersPath = "/login/centers"
	denmaResetPath   = "/login/reset"

	// The signing-in-so-far cookie: after the password, for the two-factor
	// code; after that, for choosing a center.
	denmaLoginCookie = "denma_login"
	denmaLoginTTL    = 10 * time.Minute
)

// loginRoute hands the sign-in pages to the hub's router, before the hub's
// and the centers' routing (srv.Pre, cmd/denma_centers.go), reporting whether
// it did.
func (d *denmaCenters) loginRoute(c echo.Context, next echo.HandlerFunc) (bool, error) {
	req := c.Request()
	p := req.URL.Path
	get := req.Method == http.MethodGet || req.Method == http.MethodHead

	switch {
	case p == denmaLoginPath || strings.HasPrefix(p, denmaLoginPath+"/"):
		return true, d.toHub(c, next)
	case get && (p == "/admin/login" || p == "/admin/forgot"):
		return true, c.Redirect(http.StatusFound, denmaLoginRedirect(p, "", req.URL.Query()))
	case get && strings.HasPrefix(p, denmaCenterPath):
		slug, rest, _ := strings.Cut(strings.TrimPrefix(p, denmaCenterPath), "/")
		if rest == "admin/login" || rest == "admin/forgot" {
			return true, c.Redirect(http.StatusFound, denmaLoginRedirect("/"+rest, denmaCenterPath+slug, req.URL.Query()))
		}
	}
	return false, nil
}

// denmaLoginRedirect is where an old sign-in page (p, under prefix: a
// center's /c/<slug>, or the hub's "") sends people: the shared page, with the
// page they wanted under the same prefix. listmonk's own redirect to its
// sign-in page (cmd/handlers.go) gives the address as requested, which
// already has the prefix.
func denmaLoginRedirect(p, prefix string, q url.Values) string {
	if p == "/admin/forgot" {
		return denmaForgotPath
	}
	n := utils.SanitizeURI(q.Get("next"))
	switch {
	case n == "/" && prefix == "":
		return denmaLoginPath
	case n == "/":
		n = prefix + uriAdmin
	case prefix != "" && !strings.HasPrefix(n, denmaCenterPath):
		n = prefix + n
	}
	return denmaLoginPath + "?next=" + url.QueryEscape(n)
}

// toHub has the hub's router answer the request.
func (d *denmaCenters) toHub(c echo.Context, next echo.HandlerFunc) error {
	if r := d.baseRouter(); r != nil {
		r.ServeHTTP(c.Response(), c.Request())
		return nil
	}
	return next(c)
}

// initDenmaLoginHandlers registers the sign-in pages, in the hub (with
// centers).
func initDenmaLoginHandlers(g *echo.Group, a *App) {
	if !a.ko.Bool("denma.multi_center") || a.ko.String("denma.center") != "" {
		return
	}
	g.GET(denmaLoginPath, a.LoginPage) // listmonk's page, which posts here
	g.POST(denmaLoginPath, a.DenmaLogin)
	g.GET(denmaForgotPath, a.ForgotPage)
	g.POST(denmaForgotPath, a.DenmaForgot)
	g.GET(denmaTwofaPath, a.DenmaLoginTwofa)
	g.POST(denmaTwofaPath, a.DenmaLoginTwofa)
	g.GET(denmaCentersPath, a.DenmaLoginCenters)
	g.POST(denmaCentersPath, a.DenmaLoginCenters)
	g.GET(denmaResetPath, a.DenmaLoginReset)
	g.POST(denmaResetPath, a.DenmaLoginReset)
}

// denmaLoginState is someone signing in, between pages: who, and the page
// they wanted.
type denmaLoginState struct {
	PersonID int
	Next     string
	Choosing bool // signed in, choosing a center; else, the two-factor code is next
}

func denmaLoginKey(token string) string { return "denma-login:" + token }

// setLoginState keeps st for the next page, in a cookie of its own (for
// /login's pages only).
func (a *App) setLoginState(c echo.Context, st *denmaLoginState) error {
	token, err := generateRandomString(tmpAuthTokenLen)
	if err != nil {
		return err
	}
	tmptokens.Set(denmaLoginKey(token), denmaLoginTTL, st)
	c.SetCookie(&http.Cookie{
		Name:     denmaLoginCookie,
		Value:    token,
		Path:     denmaLoginPath,
		MaxAge:   int(denmaLoginTTL.Seconds()),
		HttpOnly: true,
		Secure:   strings.HasPrefix(a.urlCfg.RootURL, "https://"),
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// loginState returns the signing-in-so-far, or nil.
func (a *App) loginState(c echo.Context) *denmaLoginState {
	ck, err := c.Cookie(denmaLoginCookie)
	if err != nil || ck.Value == "" {
		return nil
	}
	v, err := tmptokens.Check(denmaLoginKey(ck.Value))
	if err != nil {
		return nil
	}
	st, _ := v.(*denmaLoginState)
	return st
}

func (a *App) clearLoginState(c echo.Context) {
	if ck, err := c.Cookie(denmaLoginCookie); err == nil {
		tmptokens.Delete(denmaLoginKey(ck.Value))
	}
	c.SetCookie(&http.Cookie{Name: denmaLoginCookie, Path: denmaLoginPath, MaxAge: -1, HttpOnly: true})
}

// DenmaLogin signs someone in: a superadmin with listmonk's sign-in, a person
// with theirs.
func (a *App) DenmaLogin(c echo.Context) error {
	var (
		start    = time.Now()
		username = strings.TrimSpace(c.FormValue("username"))
		password = strings.TrimSpace(c.FormValue("password"))
		next     = utils.SanitizeURI(c.FormValue("next"))
	)
	// As long as listmonk's (doLogin), whoever it is.
	defer func() {
		if d := time.Since(start); d < 100*time.Millisecond {
			time.Sleep(100*time.Millisecond - d)
		}
	}()

	// Not too many guesses (cmd/denma_throttle.go): past the limit, the
	// password isn't checked.
	account := a.denmaPersonKey(username)
	if err := denmaPasswordWait(c, account); err != nil {
		return a.renderLoginPage(c, err)
	}
	invalid := func() error {
		denmaPasswordResult(c, account, false)
		return a.renderLoginPage(c, echo.NewHTTPError(http.StatusForbidden, a.i18n.T("users.invalidLogin")))
	}

	var hubUser bool
	if err := a.db.Get(&hubUser, `SELECT EXISTS (SELECT 1 FROM users WHERE LOWER(username) = LOWER($1) AND type = 'user')`, username); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if hubUser {
		// listmonk's own sign-in, which counts their guesses too (cmd/auth.go).
		// They land in the hub's admin: a center's page can't be theirs (they
		// open centers from the hub), and nothing else is at the root.
		if next != uriAdmin && !strings.HasPrefix(next, uriAdmin+"/") {
			c.Request().Form.Set("next", uriAdmin) // read already, by FormValue
		}
		return a.LoginPage(c)
	}

	var p denmaPerson
	err := a.db.Get(&p, `SELECT `+denmaPersonCols+` FROM denma.people p
		WHERE (LOWER(p.username) = LOWER($1) OR LOWER(p.email) = LOWER($1))
		AND p.password IS NOT NULL AND crypt($2, p.password) = p.password`, username, password)
	if err != nil || username == "" || password == "" {
		return invalid()
	}
	denmaPasswordResult(c, account, true)
	return a.denmaPasswordOK(c, &p, next)
}

// denmaPasswordOK carries on once a person's password is right: the
// two-factor code, if they have it on, then their center.
func (a *App) denmaPasswordOK(c echo.Context, p *denmaPerson, next string) error {
	if p.TwofaKey.Valid {
		if err := a.setLoginState(c, &denmaLoginState{PersonID: p.ID, Next: next}); err != nil {
			return err
		}
		return c.Redirect(http.StatusFound, denmaTwofaPath)
	}
	return a.denmaSignedIn(c, p.ID, next)
}

// denmaSignedIn takes a person who has signed in to their center: the one
// they're a user of, the one the page they wanted is in, or the one they
// choose.
func (a *App) denmaSignedIn(c echo.Context, personID int, next string) error {
	d := denmaHub
	if _, err := a.db.Exec(`UPDATE denma.people SET loggedin_at = NOW() WHERE id = $1`, personID); err != nil {
		a.log.Printf("denma: error recording a sign-in: %v", err)
	}
	ms, err := d.centersOf(personID)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if len(ms) == 0 {
		a.clearLoginState(c)
		return a.renderLoginPage(c, echo.NewHTTPError(http.StatusForbidden,
			"Your account isn't active in any center at the moment. Ask your center's admin."))
	}

	pick := -1
	for i, m := range ms {
		if strings.HasPrefix(next, denmaCenterPath+m.Slug+"/") {
			pick = i
		}
	}
	if pick < 0 && len(ms) == 1 {
		pick = 0
	}
	if pick < 0 {
		if err := a.setLoginState(c, &denmaLoginState{PersonID: personID, Choosing: true}); err != nil {
			return err
		}
		return c.Redirect(http.StatusFound, denmaCentersPath)
	}

	a.clearLoginState(c)
	ctr, err := d.enter(c, ms[pick])
	if err != nil {
		return a.renderLoginPage(c, err)
	}
	// The page they wanted, if it was one of this center's.
	root := strings.TrimSuffix(ctr.app.urlCfg.RootPath, "/")
	rest, ok := strings.CutPrefix(next, denmaCenterPath+ctr.Slug)
	if !ok || !strings.HasPrefix(rest, "/") {
		rest = uriAdmin
	}
	return c.Redirect(http.StatusFound, root+rest)
}

// denmaLoginPage is the data of the sign-in pages here.
type denmaLoginPage struct {
	Title       string
	Description string
	Error       string
	Centers     []denmaMembership // to choose from
	Token       string            // a set-password link's
	Email       string
}

// DenmaLoginTwofa asks a person for their two-factor code, once their
// password was right.
func (a *App) DenmaLoginTwofa(c echo.Context) error {
	st := a.loginState(c)
	if st == nil || st.Choosing {
		return c.Redirect(http.StatusFound, denmaLoginPath)
	}
	out := denmaLoginPage{Title: a.i18n.T("users.twoFA")}
	if c.Request().Method != http.MethodPost {
		return c.Render(http.StatusOK, "denma-login-twofa", out)
	}

	p, err := denmaHub.person(st.PersonID)
	if err != nil || p == nil || !p.TwofaKey.Valid {
		a.clearLoginState(c)
		return c.Redirect(http.StatusFound, denmaLoginPath)
	}
	code := strings.TrimSpace(c.FormValue("totp_code"))
	c.Request().Form.Set("username", p.Username) // who, for the activity log (cmd/denma_audit.go)

	// Wrong codes count for the person, whichever sign-in they came in
	// (cmd/denma_throttle.go): past the limit, no code is checked.
	key := strconv.Itoa(p.ID)
	if wait := denmaTwofaByPerson.wait(key); wait > 0 {
		a.clearLoginState(c)
		return a.renderLoginPage(c, denmaTooMany(wait))
	}
	if !strHasLen(code, 6, 6) || !totp.Validate(code, p.TwofaKey.String) {
		denmaTwofaByPerson.add(key)
		if wait := denmaTwofaByPerson.wait(key); wait > 0 {
			a.clearLoginState(c)
			return a.renderLoginPage(c, denmaTooMany(wait))
		}
		out.Error = a.i18n.T("globals.messages.invalidValue")
		return c.Render(http.StatusOK, "denma-login-twofa", out)
	}
	denmaTwofaByPerson.clear(key)
	return a.denmaSignedIn(c, p.ID, st.Next)
}

// DenmaLoginCenters lets a person who's a user of more than one center choose
// one, once they've signed in.
func (a *App) DenmaLoginCenters(c echo.Context) error {
	st := a.loginState(c)
	if st == nil || !st.Choosing {
		return c.Redirect(http.StatusFound, denmaLoginPath)
	}
	ms, err := denmaHub.centersOf(st.PersonID)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if c.Request().Method == http.MethodPost {
		slug := c.FormValue("center")
		if p, err := denmaHub.person(st.PersonID); err == nil && p != nil {
			c.Request().Form.Set("username", p.Username) // who, for the activity log (cmd/denma_audit.go)
		}
		for _, m := range ms {
			if m.Slug == slug {
				a.clearLoginState(c)
				ctr, err := denmaHub.enter(c, m)
				if err != nil {
					return a.renderLoginPage(c, err)
				}
				return c.Redirect(http.StatusFound, strings.TrimSuffix(ctr.app.urlCfg.RootPath, "/")+uriAdmin)
			}
		}
	}
	return c.Render(http.StatusOK, "denma-login-centers", denmaLoginPage{Title: "Choose a center", Centers: ms})
}

// DenmaForgot e-mails a person with this address a link to set a new
// password, and has the hub do the same for a superadmin's (listmonk's
// page, which says the same whether or not anyone has the address).
func (a *App) DenmaForgot(c echo.Context) error {
	email := strings.ToLower(strings.TrimSpace(c.FormValue("email")))
	// Not too many e-mails to one address, or requests from one client
	// (cmd/denma_throttle.go): the page says the same, and nothing is sent.
	if !denmaForgotAllowed(c, email) {
		return c.Render(http.StatusOK, tplMessage, makeMsgTpl(a.i18n.T("users.resetPassword"), "", a.i18n.T("users.resetLinkSent")))
	}
	if utils.ValidateEmail(email) {
		p, err := denmaHub.personByEmail(a.db, email)
		if err != nil {
			a.log.Printf("denma: error finding the person for a forgotten password: %v", err)
		}
		if p != nil {
			a.denmaSendReset(p)
		}
	}
	return a.ForgotPage(c)
}

// denmaSendReset e-mails a person a link to set a new password, as listmonk
// does (doForgotPassword). It's sent by one of their centers (the first, by
// name, they can sign in to), with its sender, look and language, as all
// their e-mails are; the hub sends it only if none is running.
func (a *App) denmaSendReset(p *denmaPerson) {
	token, err := denmaHub.newToken(p.ID, passwordResetTTL)
	if err != nil {
		a.log.Printf("denma: error making a reset link for %s: %v", p.Email, err)
		return
	}
	if ms, err := denmaHub.centersOf(p.ID); err != nil {
		a.log.Printf("denma: error finding the centers of %s for a reset link: %v", p.Email, err)
	} else if len(ms) > 0 {
		if ctr := denmaHub.get(ms[0].Slug); ctr != nil {
			a = ctr.app
		}
	}
	var msg bytes.Buffer
	if err := a.notifs.Tpls.ExecuteTemplate(&msg, notifs.TplForgotPassword, struct {
		ResetURL string
		L        *i18n.I18n
	}{denmaHub.resetURL(p, token), a.i18n}); err != nil {
		a.log.Printf("error compiling notification template '%s': %v", notifs.TplForgotPassword, err)
		return
	}
	subject, body := notifs.GetTplSubject(a.i18n.T("email.forgotPassword.subject"), msg.Bytes())
	m := models.Message{
		To:      []string{p.Email},
		Subject: subject,
		Body:    body,
	}
	a.denmaSetSystemSender(&m) // the hub's address until SES has verified the center's (cmd/denma_domains.go)
	if err := a.emailMsgr.Push(m); err != nil {
		a.log.Printf("error sending reset email: %s", err)
	}
}

// DenmaLoginReset sets a person's password from a link (a new person's, or
// a forgotten password's), then signs them in.
func (a *App) DenmaLoginReset(c echo.Context) error {
	var (
		token = strings.TrimSpace(c.QueryParam("token"))
		email = strings.ToLower(strings.TrimSpace(c.QueryParam("email")))
	)
	invalid := func() error {
		return c.Render(http.StatusBadRequest, tplMessage, makeMsgTpl(a.i18n.T("users.resetPassword"), "", a.i18n.T("users.invalidResetLink")))
	}
	p, err := denmaHub.tokenPerson(email, token)
	if err != nil || p == nil {
		return invalid()
	}

	out := denmaLoginPage{Title: a.i18n.T("users.resetPassword"), Token: token, Email: email}
	if c.Request().Method != http.MethodPost {
		return c.Render(http.StatusOK, "denma-login-reset", out)
	}
	password, password2 := c.FormValue("password"), c.FormValue("password2")
	switch {
	case !strHasLen(password, 8, stdInputMaxLen):
		out.Error = a.i18n.Ts("globals.messages.invalidFields", "name", "password")
	case password != password2:
		out.Error = a.i18n.T("users.passwordMismatch")
	}
	if out.Error != "" {
		return c.Render(http.StatusOK, "denma-login-reset", out)
	}

	// Once only: the link goes with the new password (setPassword).
	if err := denmaHub.setPassword(p.ID, password, ""); err != nil {
		a.log.Printf("denma: error setting %s's password: %v", p.Email, err)
		return echo.NewHTTPError(http.StatusInternalServerError, a.i18n.T("globals.messages.internalError"))
	}
	return a.denmaPasswordOK(c, p, "")
}
