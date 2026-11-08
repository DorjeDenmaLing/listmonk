package main

// denma: one sign-in page for everyone at /login (and /login/forgot), outside /admin.

import (
	"bytes"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/internal/i18n"
	"github.com/knadh/listmonk/internal/notifs"
	"github.com/knadh/listmonk/internal/tmptokens"
	"github.com/knadh/listmonk/internal/utils"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
)

const (
	denmaLoginPath   = "/login"
	denmaForgotPath  = "/login/forgot"
	denmaTwofaPath   = "/login/twofa"
	denmaCentersPath = "/login/centers"
	denmaResetPath   = "/login/reset"
	denmaEmailPath   = "/login/email"

	// The signing-in-so-far cookie, for the two-factor code and choosing a center.
	denmaLoginCookie = "denma_login"
	denmaLoginTTL    = 10 * time.Minute

	// denmaBcryptCost is the bcrypt cost of new password hashes.
	denmaBcryptCost = "12"
	// denmaNoPasswordHash is checked against when there's no password to check, so every check takes as long.
	denmaNoPasswordHash = "$2a$12$FHT9nTCWk6htoCQ5SrkeG.c4xyc09C6iyRwB5CBton0y3A/SC1Q2W"

	// denmaForgotTime is how long /login/forgot takes to answer, at least.
	denmaForgotTime = 1500 * time.Millisecond
)

// loginRoute hands the sign-in pages to the hub's router, reporting whether it did.
func (d *denmaCenters) loginRoute(c echo.Context, next echo.HandlerFunc) (bool, error) {
	req := c.Request()
	p := req.URL.Path
	get := req.Method == http.MethodGet || req.Method == http.MethodHead

	slug, rest, _ := strings.Cut(strings.TrimPrefix(p, denmaCenterPath), "/")
	if !strings.HasPrefix(p, denmaCenterPath) {
		slug, rest = "", ""
	}

	switch {
	case p == denmaLoginPath || strings.HasPrefix(p, denmaLoginPath+"/"):
		return true, d.toHub(c, next)
	case req.Method == http.MethodPost && (p == "/admin/forgot" || rest == "admin/forgot"):
		// /login/forgot is throttled.
		req.URL.Path, req.URL.RawPath = denmaForgotPath, ""
		return true, d.toHub(c, next)
	case get && (p == "/admin/login" || p == "/admin/forgot"):
		return true, c.Redirect(http.StatusFound, denmaLoginRedirect(p, "", req.URL.Query()))
	case get && (rest == "admin/login" || rest == "admin/forgot"):
		return true, c.Redirect(http.StatusFound, denmaLoginRedirect("/"+rest, denmaCenterPath+slug, req.URL.Query()))
	}
	return false, nil
}

// denmaLoginRedirect is where an old sign-in page under prefix sends people: /login, keeping the page wanted.
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

// initDenmaLoginHandlers registers the sign-in pages in the hub.
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
	g.GET(denmaEmailPath, a.DenmaLoginEmail)
	g.POST(denmaEmailPath, a.DenmaLoginEmail)
}

// denmaLoginState is someone signing in, between pages: who, and the page they wanted.
type denmaLoginState struct {
	PersonID int
	Next     string
	Choosing bool // signed in, choosing a center; else, the two-factor code is next
}

func denmaLoginKey(token string) string { return "denma-login:" + token }

// setLoginState keeps st for the next page in a cookie of its own.
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
	v, err := tmptokens.Peek(denmaLoginKey(ck.Value))
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

// DenmaLogin signs someone in: a superadmin with listmonk's sign-in, a person with theirs.
func (a *App) DenmaLogin(c echo.Context) error {
	var (
		start    = time.Now()
		username = strings.TrimSpace(c.FormValue("username"))
		password = c.FormValue("password")
		next     = utils.SanitizeURI(c.FormValue("next"))
	)
	// As long as listmonk's (doLogin), whoever it is.
	defer func() {
		if d := time.Since(start); d < 100*time.Millisecond {
			time.Sleep(100*time.Millisecond - d)
		}
	}()

	// Every answer clears the place, so none tells a superadmin from anyone else (cmd/denma_place.go).
	denmaSetPlace(c, "")

	// Not too many guesses (cmd/denma_throttle.go): past the limit the password isn't checked.
	account, personID := a.denmaPersonKey(username)
	if err := a.denmaPasswordWait(c, account); err != nil {
		return a.renderLoginPage(c, err)
	}
	invalid := func() error {
		a.denmaPasswordResult(c, account, false)
		return a.renderLoginPage(c, echo.NewHTTPError(http.StatusForbidden, a.i18n.T("users.invalidLogin")))
	}

	var hubUser bool
	if err := a.db.Get(&hubUser, `SELECT EXISTS (SELECT 1 FROM users WHERE LOWER(username) = LOWER($1) AND type = 'user')`, username); err != nil {
		return denmaInternal(err)
	}
	if hubUser {
		// listmonk's own sign-in; they land in the hub's admin.
		if next != uriAdmin && !strings.HasPrefix(next, uriAdmin+"/") {
			c.Request().Form.Set("next", uriAdmin) // read already, by FormValue
		}
		return a.LoginPage(c)
	}

	var p denmaPerson
	if personID == 0 {
		_, _ = a.db.Exec(`SELECT crypt($1, $2)`, password, denmaNoPasswordHash) // as long as a real check
		return invalid()
	}
	err := a.db.Get(&p, `SELECT `+denmaPersonCols+` FROM denma.people p
		WHERE p.id = $1 AND crypt($2, COALESCE(p.password, $3)) = p.password`, personID, password, denmaNoPasswordHash)
	if err != nil || username == "" || password == "" {
		return invalid()
	}
	a.denmaPasswordResult(c, account, true)
	a.denmaRehash(`denma.people`, p.ID, password)
	return a.denmaPasswordOK(c, &p, next)
}

// denmaPasswordOK carries on once a person's password is right: two-factor code, then center.
func (a *App) denmaPasswordOK(c echo.Context, p *denmaPerson, next string) error {
	if p.TwofaKey.Valid {
		if err := a.setLoginState(c, &denmaLoginState{PersonID: p.ID, Next: next}); err != nil {
			return err
		}
		return c.Redirect(http.StatusFound, denmaTwofaPath)
	}
	return a.denmaSignedIn(c, p.ID, next)
}

// denmaSignedIn takes a person who has signed in to their center, or has them choose one.
func (a *App) denmaSignedIn(c echo.Context, personID int, next string) error {
	d := denmaHub
	if _, err := a.db.Exec(`UPDATE denma.people SET loggedin_at = NOW() WHERE id = $1`, personID); err != nil {
		a.log.Printf("denma: error recording a sign-in: %v", err)
	}
	ms, err := d.centersOf(personID)
	if err != nil {
		return denmaInternal(err)
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
	// The page they wanted, if it was one of this center's (cmd/denma_place.go).
	rest, ok := strings.CutPrefix(next, denmaCenterPath+ctr.Slug)
	if !ok {
		rest = next
	}
	if rest != uriAdmin && !strings.HasPrefix(rest, uriAdmin+"/") {
		rest = uriAdmin
	}
	return c.Redirect(http.StatusFound, rest)
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

// DenmaLoginTwofa asks a person for their two-factor code.
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

	// Wrong codes count for the person (cmd/denma_throttle.go): past the limit no code is checked.
	key := strconv.Itoa(p.ID)
	if wait := denmaTwofaByPerson.wait(key); wait > 0 {
		a.clearLoginState(c)
		return a.renderLoginPage(c, denmaTooMany(wait))
	}
	if !strHasLen(code, 6, 6) || !denmaTOTPOnce("person:"+key, code, p.TwofaKey.String) {
		denmaTwofaByPerson.add(key)
		if wait := denmaTwofaByPerson.wait(key); wait > 0 {
			a.clearLoginState(c)
			return a.renderLoginPage(c, denmaTooMany(wait))
		}
		out.Error = "That code didn't work. Enter the current 6-digit code from your authenticator app."
		return c.Render(http.StatusOK, "denma-login-twofa", out)
	}
	denmaTwofaByPerson.clear(key)
	return a.denmaSignedIn(c, p.ID, st.Next)
}

// DenmaLoginCenters lets a person with several centers choose one.
func (a *App) DenmaLoginCenters(c echo.Context) error {
	st := a.loginState(c)
	if st == nil || !st.Choosing {
		return c.Redirect(http.StatusFound, denmaLoginPath)
	}
	ms, err := denmaHub.centersOf(st.PersonID)
	if err != nil {
		return denmaInternal(err)
	}
	if c.Request().Method == http.MethodPost {
		slug := c.FormValue("center")
		if p, err := denmaHub.person(st.PersonID); err == nil && p != nil {
			c.Request().Form.Set("username", p.Username) // who, for the activity log (cmd/denma_audit.go)
		}
		for _, m := range ms {
			if m.Slug == slug {
				a.clearLoginState(c)
				if _, err := denmaHub.enter(c, m); err != nil {
					return a.renderLoginPage(c, err)
				}
				return c.Redirect(http.StatusFound, uriAdmin) // now that center's (cmd/denma_place.go)
			}
		}
	}
	return c.Render(http.StatusOK, "denma-login-centers", denmaLoginPage{Title: "Choose a center", Centers: ms})
}

// DenmaForgot e-mails a person a link to set a new password, and has the hub do so for a superadmin.
func (a *App) DenmaForgot(c echo.Context) error {
	start := time.Now()
	email := strings.ToLower(strings.TrimSpace(c.FormValue("email")))
	// As long whoever it is: a superadmin's e-mail is sent before the answer (doForgotPassword).
	defer func() {
		if d := time.Since(start); d < denmaForgotTime {
			time.Sleep(denmaForgotTime - d)
		}
	}()
	// Limited by address and client (cmd/denma_throttle.go); the page says the same.
	if !denmaForgotAllowed(c, email) {
		return c.Render(http.StatusOK, tplMessage, makeMsgTpl(a.i18n.T("users.resetPassword"), "", a.i18n.T("users.resetLinkSent")))
	}
	if utils.ValidateEmail(email) {
		p, err := denmaHub.personByEmail(a.db, email)
		if err != nil {
			a.log.Printf("denma: error finding the person for a forgotten password: %v", err)
		}
		if p != nil {
			go a.denmaSendReset(p)
		}
	}
	return a.ForgotPage(c)
}

// denmaSendReset e-mails a person a password link, sent by their first center (the hub if none runs).
func (a *App) denmaSendReset(p *denmaPerson) {
	token, err := denmaHub.newToken(p.ID, passwordResetTTL)
	if err != nil {
		a.log.Printf("denma: error making a reset link for person %d: %v", p.ID, err)
		return
	}
	if ms, err := denmaHub.centersOf(p.ID); err != nil {
		a.log.Printf("denma: error finding the centers of person %d for a reset link: %v", p.ID, err)
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
	subject, body := notifs.GetTplSubject(a.i18n.T("email.forgotPassword.subject")+": "+a.ko.String("app.site_name"), msg.Bytes())
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

// DenmaLoginReset sets a person's password from a link, then signs them in.
func (a *App) DenmaLoginReset(c echo.Context) error {
	var (
		token = strings.TrimSpace(c.QueryParam("token"))
		email = strings.ToLower(strings.TrimSpace(c.QueryParam("email")))
	)
	invalid := func() error {
		msg := makeMsgTpl(a.i18n.T("users.resetPassword"), "", a.i18n.T("users.invalidResetLink"))
		msg.DenmaLinkURL, msg.DenmaLinkText = denmaForgotPath, "Forgot password"
		return c.Render(http.StatusBadRequest, tplMessage, msg)
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
		out.Error = "Use at least 8 characters."
	case password != password2:
		out.Error = a.i18n.T("users.passwordMismatch")
	}
	if out.Error != "" {
		return c.Render(http.StatusOK, "denma-login-reset", out)
	}

	// The link goes with the new password (setPassword).
	if err := denmaHub.setPassword(p.ID, password, ""); err != nil {
		a.log.Printf("denma: error setting person %d's password: %v", p.ID, err)
		return echo.NewHTTPError(http.StatusInternalServerError, a.i18n.T("globals.messages.internalError"))
	}
	return a.denmaPasswordOK(c, p, "")
}

// DenmaLoginEmail makes a person's new e-mail address theirs: its page asks, a POST does it.
func (a *App) DenmaLoginEmail(c echo.Context) error {
	const title = "Your new e-mail address"
	token := strings.TrimSpace(c.QueryParam("token"))
	msg := func(code int, text string) error {
		return c.Render(code, tplMessage, makeMsgTpl(title, "", text))
	}
	var ch struct {
		PersonID int    `db:"person_id"`
		Email    string `db:"email"`
	}
	if err := a.db.Get(&ch, `SELECT person_id, email FROM denma.person_email_changes WHERE token_hash = $1 AND expires_at > NOW()`,
		denmaTokenHash(token)); err != nil || len(token) < tmpAuthTokenLen {
		return msg(http.StatusBadRequest, "This link has expired or has been used. Change your address on your Profile page again.")
	}
	if c.Request().Method != http.MethodPost {
		return c.Render(http.StatusOK, "denma-login-email", denmaLoginPage{Title: title, Token: token, Email: ch.Email})
	}

	err := func() error {
		tx, err := a.db.Beginx()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.Exec(`UPDATE denma.people SET email = $2, updated_at = NOW() WHERE id = $1`, ch.PersonID, ch.Email); err != nil {
			return err
		}
		if err := denmaHub.eachAccount(tx, ch.PersonID, `UPDATE S.users SET email = $2, updated_at = NOW() WHERE id = $1`, ch.Email); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM denma.person_email_changes WHERE person_id = $1`, ch.PersonID); err != nil {
			return err
		}
		return tx.Commit()
	}()
	if denmaUniqueViolation(err) {
		return msg(http.StatusBadRequest, "This address is already someone else's, so it can't be yours too.")
	}
	if err != nil {
		a.log.Printf("denma: error changing person %d's e-mail address: %v", ch.PersonID, err)
		return echo.NewHTTPError(http.StatusInternalServerError, a.i18n.T("globals.messages.internalError"))
	}
	return msg(http.StatusOK, "Done: you now sign in and get your e-mails at "+ch.Email+".")
}

// denmaLogoutAll ends every session this browser has, the hub's and each center's.
func (a *App) denmaLogoutAll(c echo.Context) {
	d := denmaHub
	if d == nil {
		return
	}
	secure := c.Scheme() == "https" || c.Request().Header.Get("X-Forwarded-Proto") == "https"
	for _, ck := range c.Cookies() {
		schema := ""
		if ck.Name == "session" {
			schema = d.baseSchema
		} else if slug, ok := strings.CutPrefix(ck.Name, "session_"); ok {
			if ctr := d.get(slug); ctr != nil {
				schema = ctr.Schema
			}
		}
		if schema == "" {
			continue
		}
		if _, err := d.base.db.Exec(denmaInSchema(`DELETE FROM S.sessions WHERE id = $1`, schema), ck.Value); err != nil {
			a.log.Printf("denma: error ending a session at sign-out: %v", err)
		}
		c.SetCookie(&http.Cookie{Name: ck.Name, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure})
	}
	denmaSetPlace(c, "")
}

// denmaEndCenterSessions signs a superadmin out of their accounts in the centers.
func (a *App) denmaEndCenterSessions(hubUserID int) {
	if denmaHub == nil || a.ko.String("denma.center") != "" {
		return
	}
	var accts []struct {
		Schema string `db:"schema_name"`
		UserID int    `db:"center_user_id"`
	}
	if err := denmaHub.base.db.Select(&accts, `SELECT c.schema_name, s.center_user_id FROM denma.center_superadmins s
		JOIN denma.centers c ON c.id = s.center_id WHERE s.hub_user_id = $1`, hubUserID); err != nil {
		a.log.Printf("denma: error finding a superadmin's accounts to sign out: %v", err)
		return
	}
	for _, ac := range accts {
		if _, err := denmaHub.base.db.Exec(denmaInSchema(`DELETE FROM S.sessions WHERE data->>'user_id' = $1::TEXT`, ac.Schema), ac.UserID); err != nil {
			a.log.Printf("denma: error signing a superadmin out of a center: %v", err)
		}
	}
}

// denmaNoOIDC keeps OIDC sign-in off in multi-center mode.
func denmaNoOIDC(ko *koanf.Koanf) {
	if ko.Bool("denma.multi_center") || ko.String("denma.center") != "" {
		_ = ko.Set("security.oidc.enabled", false)
	}
}

// denmaCheckNoOIDC refuses settings that turn OIDC sign-in on (denmaNoOIDC).
func (a *App) denmaCheckNoOIDC(set *models.Settings) error {
	if set.OIDC.Enabled && (a.ko.Bool("denma.multi_center") || a.ko.String("denma.center") != "") {
		return echo.NewHTTPError(http.StatusBadRequest, "OIDC sign-in can't be turned on: everyone signs in at /login.")
	}
	return nil
}

// denmaRehash stores a password just checked with the current bcrypt cost, if its hash has a lower one.
func (a *App) denmaRehash(table string, id int, password string) {
	if _, err := a.db.Exec(`UPDATE `+table+` SET password = crypt($2, gen_salt('bf', `+denmaBcryptCost+`))
		WHERE id = $1 AND password NOT LIKE '$2a$`+denmaBcryptCost+`$%'`, id, password); err != nil {
		a.log.Printf("denma: error updating a password's hash: %v", err)
	}
}
