package main

// denma: the superadmins' hub, Listmonk Shambhala, at / (multi-center mode).
// Its dashboard (partials/denma/hub.html) shows every center's sending
// figures, together or one at a time. Its Centers page
// (views/denma-centers.html) lists the centers; from there superadmins create
// centers, enable and disable them, and open any center without another
// login.
//
// Superadmins are the hub's users with the Super Admin role. In each center
// they open, they get a Super Admin account of their own (no password login),
// recorded in denma.center_superadmins so that it can't be confused with a
// center user of the same name. Centers have no Super Admins of their own
// (cmd/denma_hierarchy.go).

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/internal/notifs"
	"github.com/knadh/listmonk/internal/tmptokens"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/lib/pq"
	null "gopkg.in/volatiletech/null.v6"
)

// denmaInviteTTL is how long a new center admin's set-password link works.
// Like reset links, it lives in memory: a restart voids it (use Forgot
// password then).
const denmaInviteTTL = 7 * 24 * time.Hour

// initDenmaHubHandlers registers the hub's pages (on the admin group). The
// hub's home is its dashboard (views/dashboard.html).
func initDenmaHubHandlers(g *echo.Group, a *App) {
	g.GET(path.Join(uriAdmin, "/centers"), a.ViewDenmaCenters)
	g.GET(path.Join(uriAdmin, "/centers/new"), a.ViewDenmaNewCenter)
	g.GET(path.Join(uriAdmin, "/centers/:slug/open"), a.DenmaOpenCenter)
	g.GET(path.Join(uriAdmin, "/activity"), a.ViewDenmaActivity)
	g.GET(path.Join(uriAdmin, "/analytics"), a.ViewDenmaHubAnalytics)
}

// initDenmaAPIHandlers registers the hub's API (on the /api group).
func initDenmaAPIHandlers(g *echo.Group, a *App) {
	// Compressed: with hundreds of centers, these are hundreds of kilobytes.
	g.GET("/api/denma/hub/stats", a.DenmaHubStats, middleware.Gzip())
	g.GET("/api/denma/hub/analytics", a.DenmaHubAnalytics, middleware.Gzip())
	g.POST("/api/denma/centers", a.DenmaCreateCenter)
	g.PUT("/api/denma/centers/:slug/status", a.DenmaSetCenterStatus)
	initDenmaCenterAPIHandlers(g, a)
	initDenmaAutomationAPIHandlers(g, a)
	initDenmaSearchAPIHandlers(g, a)
	g.GET("/api/denma/audit", a.DenmaGetAudit)
}

// hub returns the centers if this App is the hub and the user is a
// superadmin, or an HTTP error.
func (a *App) hub(c echo.Context) (*denmaCenters, error) {
	if denmaHub == nil || a.ko.String("denma.center") != "" {
		return nil, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if u, ok := c.Get(auth.UserHTTPCtxKey).(auth.User); !ok || u.UserRole.ID != auth.SuperAdminRoleID {
		return nil, echo.NewHTTPError(http.StatusForbidden, "superadmins only")
	}
	return denmaHub, nil
}

// denmaHubCenter is one center on the hub's dashboard: its state now, and its
// sending figures for a period and the one before it.
type denmaHubCenter struct {
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Path        string    `json:"path"`
	Status      string    `json:"status"` // enabled, disabled
	Error       string    `json:"error"`  // why it didn't load
	Loaded      bool      `json:"loaded"`
	Starting    bool      `json:"starting"` // still loading after a start
	Subscribers int       `json:"subscribers"`
	Lists       int       `json:"lists"`
	Users       int       `json:"users"` // the center's own (not superadmins)
	LastSent    null.Time `json:"last_sent"`
	Version     string    `json:"version"`

	Cur  *denmaPeriod `json:"cur"`
	Prev *denmaPeriod `json:"prev"`
}

// denmaPeriod is a center's sending in a period, defined as on the centers'
// dashboards (assets/js/denma-stats.js): campaigns count in the period they
// started sending; opens and clicks are unique people per campaign, from
// campaigns sent with individual tracking (those with no anonymous views);
// unsubscribes leave out bounce removals; bounces count by when they came.
type denmaPeriod struct {
	Campaigns        int `db:"campaigns" json:"campaigns"`
	Sends            int `db:"sends" json:"sends"`
	CampaignBounces  int `db:"camp_bounces" json:"camp_bounces"`
	TrackedDelivered int `db:"tracked_delivered" json:"tracked_delivered"`
	Opens            int `db:"opens" json:"opens"`
	Clicks           int `db:"clicks" json:"clicks"`
	Unsubscribes     int `db:"unsubs" json:"unsubs"`
	Hard             int `db:"hard" json:"hard"`
	Soft             int `db:"soft" json:"soft"`
	Complaint        int `db:"complaint" json:"complaint"`
	NewSubscribers   int `db:"new_subscribers" json:"new_subscribers"`
}

// S. is replaced with the center's schema; $1 and $2 are the period's start
// and end.
const denmaCenterInfoSQL = `SELECT
	(SELECT COUNT(*) FROM S.subscribers) AS subscribers,
	(SELECT COUNT(*) FROM S.lists) AS lists,
	(SELECT COUNT(*) FROM S.users WHERE type = 'user' AND user_role_id <> 1) AS users,
	(SELECT MAX(started_at) FROM S.campaigns WHERE sent > 0) AS last_sent,
	COALESCE((SELECT value->>-1 FROM S.settings WHERE key = 'migrations'), '') AS version`

const denmaPeriodSQL = `WITH camp AS (
	SELECT c.id, c.sent,
		LEAST((SELECT COUNT(*) FROM S.bounces b WHERE b.campaign_id = c.id), c.sent) AS bounced,
		NOT EXISTS (SELECT 1 FROM S.campaign_views v WHERE v.campaign_id = c.id AND v.subscriber_id IS NULL) AS tracked
	FROM S.campaigns c WHERE c.started_at >= $1 AND c.started_at < $2 AND c.sent > 0
)
SELECT
	(SELECT COUNT(*) FROM camp) AS campaigns,
	(SELECT COALESCE(SUM(sent), 0) FROM camp) AS sends,
	(SELECT COALESCE(SUM(bounced), 0) FROM camp) AS camp_bounces,
	(SELECT COALESCE(SUM(sent - bounced), 0) FROM camp WHERE tracked) AS tracked_delivered,
	(SELECT COUNT(*) FROM (SELECT DISTINCT v.campaign_id, v.subscriber_id FROM S.campaign_views v
		JOIN camp ON camp.id = v.campaign_id AND camp.tracked WHERE v.subscriber_id IS NOT NULL) x) AS opens,
	(SELECT COUNT(*) FROM (SELECT DISTINCT l.campaign_id, l.subscriber_id FROM S.link_clicks l
		JOIN camp ON camp.id = l.campaign_id AND camp.tracked WHERE l.subscriber_id IS NOT NULL) x) AS clicks,
	(SELECT COUNT(DISTINCT sl.subscriber_id) FROM S.subscriber_lists sl
		WHERE sl.status = 'unsubscribed' AND sl.updated_at >= $1 AND sl.updated_at < $2
		AND sl.subscriber_id NOT IN (SELECT subscriber_id FROM S.bounces
			WHERE subscriber_id IS NOT NULL AND created_at >= $1 AND created_at < $2)) AS unsubs,
	(SELECT COUNT(*) FROM S.bounces WHERE type = 'hard' AND created_at >= $1 AND created_at < $2) AS hard,
	(SELECT COUNT(*) FROM S.bounces WHERE type = 'soft' AND created_at >= $1 AND created_at < $2) AS soft,
	(SELECT COUNT(*) FROM S.bounces WHERE type = 'complaint' AND created_at >= $1 AND created_at < $2) AS complaint,
	(SELECT COUNT(*) FROM S.subscribers WHERE created_at >= $1 AND created_at < $2) AS new_subscribers`

func denmaInSchema(q, schema string) string {
	return strings.ReplaceAll(q, "S.", pq.QuoteIdentifier(schema)+".")
}

// denmaPeriods reads the from, to, prev_from and prev_to RFC 3339 times of a
// hub API request.
func denmaPeriods(c echo.Context) ([4]time.Time, error) {
	var t [4]time.Time
	for i, k := range []string{"from", "to", "prev_from", "prev_to"} {
		var err error
		if t[i], err = time.Parse(time.RFC3339, c.QueryParam(k)); err != nil {
			return t, echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid %s: %v", k, err))
		}
	}
	return t, nil
}

// DenmaHubStats returns every center with its figures for a period (from, to)
// and the previous one (prev_from, prev_to), all RFC 3339 times.
func (a *App) DenmaHubStats(c echo.Context) error {
	d, err := a.hub(c)
	if err != nil {
		return err
	}
	t, err := denmaPeriods(c)
	if err != nil {
		return err
	}

	reg, err := d.registered()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	type figures struct {
		Subscribers int       `db:"subscribers"`
		Lists       int       `db:"lists"`
		Users       int       `db:"users"`
		LastSent    null.Time `db:"last_sent"`
		Version     string    `db:"version"`
		cur, prev   *denmaPeriod
	}
	var (
		db  = d.current().db
		key = fmt.Sprintf("stats|%v", t)
		out = make([]denmaHubCenter, len(reg))
	)
	denmaEach(len(reg), func(i int) {
		x := reg[i]
		r := &out[i]
		*r = denmaHubCenter{Slug: x.Slug, Name: x.Name, Status: x.Status, Error: x.Error, Path: x.Path, Loaded: x.Loaded, Starting: x.Starting}

		f, err := denmaCached(x.Schema+"|"+key, func() (figures, error) {
			f := figures{cur: &denmaPeriod{}, prev: &denmaPeriod{}}
			err := db.Get(&f, denmaInSchema(denmaCenterInfoSQL, x.Schema))
			if err == nil {
				err = db.Get(f.cur, denmaInSchema(denmaPeriodSQL, x.Schema), t[0], t[1])
			}
			if err == nil {
				err = db.Get(f.prev, denmaInSchema(denmaPeriodSQL, x.Schema), t[2], t[3])
			}
			return f, err
		})
		if err != nil {
			if r.Error == "" {
				r.Error = err.Error()
			}
			return
		}
		r.Subscribers, r.Lists, r.Users, r.LastSent, r.Version = f.Subscribers, f.Lists, f.Users, f.LastSent, f.Version
		r.Cur, r.Prev = f.cur, f.prev
	})

	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return c.JSON(http.StatusOK, okResp{out})
}

// ViewDenmaCenters renders the hub's Centers page (views/denma-centers.html):
// every center, with its sending, to open, enable or disable.
func (a *App) ViewDenmaCenters(c echo.Context) error {
	if _, err := a.hub(c); err != nil {
		return err
	}
	return c.Render(http.StatusOK, "admin-denma-centers", newAdminView(c, "Centers", "", "denma.centers"))
}

type denmaNewCenterView struct {
	adminView
	CenterURL string // the address prefix, e.g. https://news.example.org/c/
	FromEmail string // the hub's sender, as the example
}

// ViewDenmaNewCenter renders the form for creating a center.
func (a *App) ViewDenmaNewCenter(c echo.Context) error {
	d, err := a.hub(c)
	if err != nil {
		return err
	}
	base := d.current()
	return c.Render(http.StatusOK, "admin-denma-center-new", denmaNewCenterView{
		adminView: newAdminView(c, "New center", "", "denma.centers"),
		CenterURL: strings.TrimSuffix(base.urlCfg.RootURL, "/") + denmaCenterPath,
		FromEmail: base.ko.String("app.from_email"),
	})
}

// DenmaOpenCenter signs the superadmin into a center and goes to its admin.
func (a *App) DenmaOpenCenter(c echo.Context) error {
	d, err := a.hub(c)
	if err != nil {
		return err
	}
	ctr := d.get(c.Param("slug"))
	if ctr == nil {
		return echo.NewHTTPError(http.StatusNotFound, "center not found or not running")
	}

	userID, err := d.superadminIn(ctr, auth.GetUser(c))
	if err != nil {
		a.log.Printf("denma: error signing into center %s: %v", ctr.Slug, err)
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	// The center's session cookie (its auth sets the center's cookie path).
	if err := ctr.app.auth.SaveSession(auth.User{Base: auth.Base{ID: userID}}, "", c); err != nil {
		return err
	}
	return c.Redirect(http.StatusFound, path.Join(ctr.app.urlCfg.RootPath, uriAdmin))
}

// superadminIn returns the superadmin's own account in a center, creating it
// the first time, and making sure it's still an enabled Super Admin without
// password login (a center admin may have changed it).
func (d *denmaCenters) superadminIn(ctr *denmaCenter, hu auth.User) (int, error) {
	alt := fmt.Sprintf("superadmin-%d@hub.invalid", hu.ID)
	email := hu.Email.String
	if email == "" {
		email = alt
	}
	return d.hubAccount(ctr.ID, ctr.app.db, hu.ID, hu.Username, email, alt, hu.Name)
}

// hubAccount returns an account of the hub's in a center (db), recorded in
// denma.center_superadmins under hubUserID (a superadmin's, or 0 for the
// hub's own, cmd/denma_hierarchy.go), creating it if there's none, and making
// sure it's still an enabled Super Admin without password login. A new one is
// named username, or username-hub1, -hub2… if that's taken in the center, with
// email, or alt if that's taken.
func (d *denmaCenters) hubAccount(centerID int, db *sqlx.DB, hubUserID int, username, email, alt, name string) (int, error) {
	var id int
	err := d.base.db.Get(&id, `SELECT center_user_id FROM denma.center_superadmins WHERE center_id = $1 AND hub_user_id = $2`, centerID, hubUserID)
	if err == nil {
		res, err := db.Exec(`UPDATE users SET user_role_id = $2, list_role_id = NULL, status = 'enabled', type = 'user',
			password_login = false, password = NULL WHERE id = $1`, id, auth.SuperAdminRoleID)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return id, nil
		}
		// The account was deleted in the center: make a new one.
	}

	// A username (and e-mail, both unique) not used in the center yet.
	var taken bool
	if err := db.Get(&taken, `SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)`, email); err != nil {
		return 0, err
	}
	if taken {
		email = alt
	}
	uname := username
	for i := 1; ; i++ {
		if err := db.Get(&taken, `SELECT EXISTS (SELECT 1 FROM users WHERE username = $1)`, uname); err != nil {
			return 0, err
		}
		if !taken {
			break
		}
		uname = fmt.Sprintf("%s-hub%d", username, i)
	}

	if err := db.Get(&id, `INSERT INTO users (username, password_login, password, email, name, type, user_role_id, status)
		VALUES ($1, false, NULL, $2, $3, 'user', $4, 'enabled') RETURNING id`,
		uname, email, name, auth.SuperAdminRoleID); err != nil {
		return 0, err
	}
	if _, err := d.base.db.Exec(`INSERT INTO denma.center_superadmins (center_id, hub_user_id, center_user_id) VALUES ($1, $2, $3)
		ON CONFLICT (center_id, hub_user_id) DO UPDATE SET center_user_id = EXCLUDED.center_user_id`, centerID, hubUserID, id); err != nil {
		return 0, err
	}
	return id, nil
}

type denmaNewCenter struct {
	Name          string `json:"name"`
	Slug          string `json:"slug"`
	FromEmail     string `json:"from_email"`
	AdminName     string `json:"admin_name"`
	AdminUsername string `json:"admin_username"`
	AdminEmail    string `json:"admin_email"`
}

// DenmaCreateCenter creates a center and its first admin, who is e-mailed a
// link to set their password (also returned, to pass on another way).
func (a *App) DenmaCreateCenter(c echo.Context) error {
	d, err := a.hub(c)
	if err != nil {
		return err
	}

	var req denmaNewCenter
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Slug = strings.TrimSpace(req.Slug)
	req.AdminName = strings.TrimSpace(req.AdminName)
	req.AdminUsername = strings.TrimSpace(req.AdminUsername)
	req.FromEmail = strings.TrimSpace(req.FromEmail)

	switch {
	case !strHasLen(req.Name, 1, 200):
		return echo.NewHTTPError(http.StatusBadRequest, "Enter the center's name.")
	case !reDenmaSlug.MatchString(req.Slug) || len(req.Slug) > 50:
		return echo.NewHTTPError(http.StatusBadRequest, "The address can have lowercase letters, digits and single hyphens.")
	case !strHasLen(req.AdminUsername, 3, 200) || !reUsername.MatchString(req.AdminUsername):
		return echo.NewHTTPError(http.StatusBadRequest, "The admin's username needs at least 3 letters, digits or _-.@")
	case !strHasLen(req.AdminName, 1, 200):
		return echo.NewHTTPError(http.StatusBadRequest, "Enter the admin's name.")
	}
	adminEmail, err := a.importer.SanitizeEmail(req.AdminEmail)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "The admin's e-mail address isn't valid.")
	}
	if req.FromEmail == "" {
		req.FromEmail = denmaFromEmail(req.Name, d.current().ko.String("app.from_email"))
	}

	ctr, err := denmaRegisterCenter(d.base.db, req.Slug, req.Name, "")
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if ctr == nil {
		return echo.NewHTTPError(http.StatusConflict, fmt.Sprintf("There's already a center at /c/%s/.", req.Slug))
	}
	ctr.initial = map[string]any{
		"app.from_email":    req.FromEmail,
		"app.notify_emails": []string{adminEmail},
	}

	lo.Printf("denma: creating center %s (%s)", ctr.Slug, ctr.Name)
	if err := d.load(ctr); err != nil {
		d.setFailed(ctr.Slug, err)
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("The center was registered but didn't start: %v", err))
	}
	d.set(ctr)

	invite, sent, err := d.addCenterAdmin(ctr, req.AdminUsername, req.AdminName, adminEmail)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("The center was created, but not its admin: %v", err))
	}

	return c.JSON(http.StatusOK, okResp{map[string]any{
		"slug":       ctr.Slug,
		"path":       path.Join(ctr.app.urlCfg.RootPath, uriAdmin),
		"invite_url": invite,
		"email_sent": sent,
	}})
}

// denmaFromEmail is a center's default sender: its name, at the hub's
// address (the domain the mail servers send for).
func denmaFromEmail(name, base string) string {
	addr := base
	if i := strings.LastIndex(base, "<"); i >= 0 {
		addr = strings.TrimSuffix(strings.TrimSpace(base[i+1:]), ">")
	}
	return fmt.Sprintf("%q <%s>", name, addr)
}

// addCenterAdmin creates a center's admin, with the Center Admin role and an
// unguessable password, and e-mails them a link to set their own.
func (d *denmaCenters) addCenterAdmin(ctr *denmaCenter, username, name, email string) (string, bool, error) {
	app := ctr.app

	var roleID int
	if err := app.db.Get(&roleID, `SELECT id FROM roles WHERE name = $1 AND type = 'user'`, denmaCenterAdminRole); err != nil {
		return "", false, fmt.Errorf("finding the %s role: %v", denmaCenterAdminRole, err)
	}
	pw, err := generateRandomString(32)
	if err != nil {
		return "", false, err
	}
	if _, err := app.core.CreateUser(auth.User{
		Username:      username,
		Name:          name,
		Email:         null.String{String: email, Valid: true},
		PasswordLogin: true,
		Password:      null.String{String: pw, Valid: true},
		Type:          auth.UserTypeUser,
		UserRoleID:    roleID,
		Status:        auth.UserStatusEnabled,
	}); err != nil {
		return "", false, err
	}

	// A set-password link: the reset-password page, with a longer-lived token.
	token, err := generateRandomString(tmpAuthTokenLen)
	if err != nil {
		return "", false, err
	}
	tmptokens.Set(app.tmpKey(email), denmaInviteTTL, token)
	link := fmt.Sprintf("%s/admin/reset?token=%s&email=%s", app.urlCfg.RootURL, token, url.QueryEscape(email))

	// The e-mail, in the center's notification look (header and footer from
	// its e-mail templates, which --static-dir may replace wholesale, so the
	// invite itself is defined here).
	var body bytes.Buffer
	tpl, err := app.notifs.Tpls.Clone()
	if err == nil {
		_, err = tpl.New("denma-center-invite").Parse(denmaInviteTpl)
	}
	if err == nil {
		err = tpl.ExecuteTemplate(&body, "denma-center-invite", map[string]any{
			"ResetURL": link,
			"Site":     app.ko.String("app.site_name"),
			"Username": username,
			"Days":     int(denmaInviteTTL.Hours() / 24),
		})
	}
	if err != nil {
		lo.Printf("denma: error rendering the invite for %s: %v", email, err)
		return link, false, nil
	}
	subject, b := notifs.GetTplSubject(fmt.Sprintf("Your %s account", app.ko.String("app.site_name")), body.Bytes())
	if err := app.emailMsgr.Push(models.Message{
		From:    app.cfg.FromEmail,
		To:      []string{email},
		Subject: subject,
		Body:    b,
	}); err != nil {
		lo.Printf("denma: error sending the invite to %s: %v", email, err)
		return link, false, nil
	}
	return link, true, nil
}

const denmaInviteTpl = `{{ template "header" . }}
<h2>Your {{ .Site }} account</h2>
<p>An account has been made for you to manage {{ .Site }}'s mailing lists. Your username is <strong>{{ .Username }}</strong>.</p>
<p><a href="{{ .ResetURL }}" class="button">Set your password</a></p>
<p style="color: #666; font-size: 12px;">This link works for {{ .Days }} days. After that, use "Forgot password" on the login page.</p>
{{ template "footer" }}`

// DenmaSetCenterStatus enables or disables a center.
func (a *App) DenmaSetCenterStatus(c echo.Context) error {
	d, err := a.hub(c)
	if err != nil {
		return err
	}
	var req struct {
		Status string `json:"status"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	slug := c.Param("slug")
	switch req.Status {
	case "enabled":
		err = d.enable(slug)
	case "disabled":
		err = d.disable(slug)
	default:
		return echo.NewHTTPError(http.StatusBadRequest, "status must be enabled or disabled")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{true})
}

// denmaTplFuncs adds the hub's template functions (for the sidebar) to an
// App's template functions; u is that App's URLs.
func denmaTplFuncs(funcs template.FuncMap, u *UrlConfig) {
	// DenmaHubURL is the hub's dashboard, or "" without multi-center.
	funcs["DenmaHubURL"] = func() string {
		if denmaHub == nil {
			return ""
		}
		return path.Join(denmaHub.current().urlCfg.RootPath, uriAdmin)
	}
	// DenmaIsHub reports whether the page is the hub's.
	funcs["DenmaIsHub"] = func() bool {
		return denmaHub != nil && u.RootPath == denmaHub.current().urlCfg.RootPath
	}
}
