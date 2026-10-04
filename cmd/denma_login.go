package main

// denma: one sign-in page for everyone, at /login (and /login/forgot for a
// forgotten password), outside /admin: the web server keeps the hub's admin
// behind Cloudflare Access, for superadmins, while the centers' admins sign
// in with listmonk's own login. Someone signs in with their username and
// password and lands in the center that has them as a user, or the hub.
//
// A username or e-mail address belongs to one place only, the hub or one
// center (denmaCheckUserUnique, from listmonk's users handlers and the hub's
// New center form): a person is a user of one center. The superadmins'
// accounts in centers (cmd/denma_hub.go) don't count; they have no password.
//
// The page is the hub's listmonk sign-in page. On a post, the account is
// found in the hub and the running centers, its password checked there, and
// the post handed to that app's own sign-in (cmd/auth.go), which sets its
// session (or asks for the 2FA code) and records it in the activity log
// (cmd/denma_audit.go), as if the person had signed in at its own page. A
// wrong password, or a username nobody has, is the hub's to answer, with
// listmonk's usual message, so the page never tells which center a
// username belongs to. A forgotten password is e-mailed by the center the
// address belongs to; the page says the same either way.
//
// The old sign-in pages, the hub's and each center's (/c/<slug>/admin/login,
// where listmonk sends someone who isn't signed in), send people here, with
// the page they wanted.

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/knadh/listmonk/internal/utils"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

const (
	denmaLoginPath  = "/login"
	denmaForgotPath = "/login/forgot"
)

// loginRoute handles the sign-in paths before the hub's and the centers'
// routers (srv.Pre, cmd/denma_centers.go), reporting whether it did.
func (d *denmaCenters) loginRoute(c echo.Context, next echo.HandlerFunc) (bool, error) {
	req := c.Request()
	p := req.URL.Path
	get := req.Method == http.MethodGet || req.Method == http.MethodHead

	switch {
	case p == denmaLoginPath:
		return true, d.login(c, next)
	case p == denmaForgotPath:
		return true, d.forgot(c, next)
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

// login shows the sign-in page, and signs someone in where they're a user.
func (d *denmaCenters) login(c echo.Context, next echo.HandlerFunc) error {
	req := c.Request()
	if req.Method != http.MethodPost {
		return d.toHub(c, next, "/admin/login") // listmonk's page, which posts here
	}

	var (
		username = strings.TrimSpace(c.FormValue("username"))
		password = strings.TrimSpace(c.FormValue("password"))
		nextURI  = utils.SanitizeURI(c.FormValue("next"))
	)
	ctr, err := d.signInCenter(username, password)
	if err != nil {
		lo.Printf("denma: error finding where %q signs in: %v", username, err)
	}
	if ctr == nil {
		// The hub's account, or none (the hub's sign-in then fails, as for a
		// wrong password).
		if strings.HasPrefix(nextURI, denmaCenterPath) {
			nextURI = uriAdmin
		}
		denmaSetForm(req, "next", nextURI)
		return d.toHub(c, next, "/admin/login")
	}

	// The center's: the page wanted there, if it was one of its pages.
	prefix := denmaCenterPath + ctr.Slug
	if rest, ok := strings.CutPrefix(nextURI, prefix); ok && strings.HasPrefix(rest, "/") {
		nextURI = rest
	} else {
		nextURI = uriAdmin
	}
	denmaSetForm(req, "next", nextURI)
	d.toCenter(c, ctr, "/admin/login", c.Response())
	return nil
}

// signInCenter is the running center where username's password is right, or
// nil for the hub's account or none. A username is in one place only; before
// that was required, several could have it, and the first whose password is
// right is the one (the hub's first).
func (d *denmaCenters) signInCenter(username, password string) (*denmaCenter, error) {
	if username == "" || password == "" {
		return nil, nil
	}
	accs, err := d.accounts(`username = $1 AND password_login AND status = 'enabled'`, username, true)
	if err != nil {
		return nil, err
	}
	for _, acc := range accs {
		app := d.current()
		if acc.ctr != nil {
			app = acc.ctr.app
		}
		if _, err := app.core.LoginUser(username, password); err != nil {
			continue
		}
		return acc.ctr, nil
	}
	return nil, nil
}

// forgot shows the forgotten password page, and has the center (or hub) that
// the address belongs to e-mail a link to set a new one.
func (d *denmaCenters) forgot(c echo.Context, next echo.HandlerFunc) error {
	req := c.Request()
	if req.Method != http.MethodPost {
		return d.toHub(c, next, "/admin/forgot")
	}

	email := strings.ToLower(strings.TrimSpace(c.FormValue("email")))
	accs, err := d.accounts(`LOWER(email) = $1 AND password_login AND status = 'enabled'`, email, true)
	if err != nil {
		lo.Printf("denma: error finding the account for a forgotten password: %v", err)
	}
	hub := false
	for _, acc := range accs {
		if acc.ctr == nil {
			hub = true
			continue
		}
		// The center sends the e-mail (its page isn't shown).
		d.toCenter(c, acc.ctr, "/admin/forgot", &denmaDiscardWriter{h: http.Header{}})
	}
	if !hub {
		// The hub sends nothing, and shows the same message.
		denmaSetForm(req, "email", "")
	}
	return d.toHub(c, next, "/admin/forgot")
}

// toHub has the hub answer the request, as one for path.
func (d *denmaCenters) toHub(c echo.Context, next echo.HandlerFunc, path string) error {
	req := c.Request()
	req.URL.Path, req.URL.RawPath = path, ""
	if r := d.baseRouter(); r != nil {
		r.ServeHTTP(c.Response(), req)
		return nil
	}
	return next(c)
}

// toCenter has a center answer the request, as one for path, writing to w.
func (d *denmaCenters) toCenter(c echo.Context, ctr *denmaCenter, path string, w http.ResponseWriter) {
	r := c.Request().Clone(c.Request().Context()) // with the form, already read
	r.URL.Path, r.URL.RawPath = path, ""
	ctr.router.ServeHTTP(&denmaPrefixWriter{ResponseWriter: w, prefix: denmaCenterPath + ctr.Slug}, r)
}

// denmaSetForm changes a value of a request's form, already read.
func denmaSetForm(r *http.Request, key, value string) {
	if r.Form != nil {
		r.Form.Set(key, value)
	}
	if r.PostForm != nil {
		r.PostForm.Set(key, value)
	}
}

// denmaDiscardWriter is a response nobody sees.
type denmaDiscardWriter struct{ h http.Header }

func (w *denmaDiscardWriter) Header() http.Header         { return w.h }
func (w *denmaDiscardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *denmaDiscardWriter) WriteHeader(int)             {}

// denmaAccount is a user in the hub (ctr nil) or a center.
type denmaAccount struct {
	Slug string `db:"slug"` // "" for the hub
	ID   int    `db:"id"`
	ctr  *denmaCenter
}

// accounts finds the users matching cond ($1 is value) in the hub and every
// center (only the running ones, with running), the hub's first. The
// superadmins' accounts in centers aren't among them, nor API users.
func (d *denmaCenters) accounts(cond, value string, running bool) ([]denmaAccount, error) {
	var centers []struct {
		ID     int    `db:"id"`
		Slug   string `db:"slug"`
		Schema string `db:"schema_name"`
	}
	if err := d.current().db.Select(&centers, `SELECT c.id, c.slug, c.schema_name FROM denma.centers c
		WHERE EXISTS (SELECT 1 FROM information_schema.tables t WHERE t.table_schema = c.schema_name AND t.table_name = 'users')
		ORDER BY c.slug`); err != nil {
		return nil, err
	}

	parts := []string{fmt.Sprintf(`SELECT '' AS slug, id FROM %s.users WHERE type = 'user' AND (%s)`,
		pq.QuoteIdentifier(d.baseSchema), cond)}
	for _, ce := range centers {
		if running && d.get(ce.Slug) == nil {
			continue
		}
		parts = append(parts, fmt.Sprintf(`SELECT %s AS slug, id FROM %s.users WHERE type = 'user' AND (%s)
			AND id NOT IN (SELECT center_user_id FROM denma.center_superadmins WHERE center_id = %d)`,
			pq.QuoteLiteral(ce.Slug), pq.QuoteIdentifier(ce.Schema), cond, ce.ID))
	}

	var out []denmaAccount
	if err := d.current().db.Select(&out, strings.Join(parts, " UNION ALL "), value); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Slug != "" {
			out[i].ctr = d.get(out[i].Slug)
		}
	}
	return out, nil
}

// denmaCheckUserUnique refuses a username or e-mail address, of a user being
// created or changed in this app, that another center or the hub has: a
// person is a user of one place, where /login signs them in. listmonk checks
// for the same in the app itself.
func (a *App) denmaCheckUserUnique(username, email string) error {
	if denmaHub == nil {
		return nil
	}
	return denmaHub.checkUnique(a.ko.String("denma.center"), username, email) // "" in the hub
}

// checkUnique refuses a username or e-mail address that a user anywhere but
// own (a center's slug, or "" for the hub) has.
func (d *denmaCenters) checkUnique(own, username, email string) error {
	for _, v := range []struct{ what, cond, value string }{
		{"username", `LOWER(username) = LOWER($1)`, username},
		{"e-mail address", `LOWER(email) = LOWER($1)`, email},
	} {
		if v.value == "" {
			continue
		}
		accs, err := d.accounts(v.cond, v.value, false)
		if err != nil {
			lo.Printf("denma: error checking that a user is in one center only: %v", err)
			return echo.NewHTTPError(http.StatusInternalServerError, "Couldn't check the username and e-mail address. Try again.")
		}
		for _, acc := range accs {
			if acc.Slug == own {
				continue // this app's own, which listmonk checks
			}
			return echo.NewHTTPError(http.StatusBadRequest,
				fmt.Sprintf("That %s belongs to a user of another center (or the hub). A person can be a user of one center only.", v.what))
		}
	}
	return nil
}

// warnSharedUsers logs the usernames and e-mail addresses that more than one
// place has, from before each had to be in one place only: /login signs
// such a person in at the first where their password is right. Called after
// the centers load.
func (d *denmaCenters) warnSharedUsers() {
	var shared []string
	q := `SELECT LOWER(u) FROM (` + d.userNamesSQL() + `) t GROUP BY LOWER(u) HAVING COUNT(DISTINCT slug) > 1 ORDER BY 1 LIMIT 20`
	if err := d.current().db.Select(&shared, q); err != nil {
		lo.Printf("denma: error checking for users in more than one center: %v", err)
		return
	}
	if len(shared) > 0 {
		lo.Printf("denma: %d usernames or e-mail addresses are users of more than one center or the hub (/login signs them in at the first where the password is right; rename all but one): %s",
			len(shared), strings.Join(shared, ", "))
	}
}

// userNamesSQL lists every user's username and e-mail address (u) and where
// it is (slug), as accounts finds them.
func (d *denmaCenters) userNamesSQL() string {
	parts := []string{fmt.Sprintf(`SELECT '' AS slug, username AS u FROM %[1]s.users WHERE type = 'user'
		UNION ALL SELECT '', email FROM %[1]s.users WHERE type = 'user' AND email <> ''`, pq.QuoteIdentifier(d.baseSchema))}
	for _, ctr := range d.loaded() {
		s := pq.QuoteIdentifier(denmaSchemaName(ctr.Slug))
		not := fmt.Sprintf(`id NOT IN (SELECT center_user_id FROM denma.center_superadmins WHERE center_id = %d)`, ctr.ID)
		parts = append(parts, fmt.Sprintf(`SELECT %[1]s, username FROM %[2]s.users WHERE type = 'user' AND %[3]s
			UNION ALL SELECT %[1]s, email FROM %[2]s.users WHERE type = 'user' AND email <> '' AND %[3]s`,
			pq.QuoteLiteral(ctr.Slug), s, not))
	}
	return strings.Join(parts, " UNION ALL ")
}
