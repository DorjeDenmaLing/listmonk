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
	"fmt"
	"html/template"
	"net/http"
	"net/mail"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/internal/denmadaily"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/lib/pq"
	null "gopkg.in/volatiletech/null.v6"
)

// initDenmaHubHandlers registers the hub's pages (on the admin group). The
// hub's home is its dashboard (views/dashboard.html).
func initDenmaHubHandlers(g *echo.Group, a *App) {
	g.GET(path.Join(uriAdmin, "/centers"), a.ViewDenmaCenters)
	g.GET(path.Join(uriAdmin, "/centers/new"), a.ViewDenmaNewCenter)
	g.GET(path.Join(uriAdmin, "/centers/:slug/open"), a.DenmaOpenCenter)
	g.GET(path.Join(uriAdmin, "/activity"), a.ViewDenmaActivity) // and a center's
	g.GET(path.Join(uriAdmin, "/analytics"), a.ViewDenmaHubAnalytics)
	initDenmaDomainHandlers(g, a) // cmd/denma_domains.go
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
	initDenmaTagAPIHandlers(g, a) // cmd/denma_tags.go
	initDenmaRetryHandlers(g, a)  // cmd/denma_retries.go
	initDenmaPeopleAPIHandlers(g, a)
	initDenmaDomainAPIHandlers(g, a)   // cmd/denma_domains.go
	initDenmaResubscribeHandlers(g, a) // cmd/denma_resubscribe.go
	g.GET("/api/denma/audit", a.DenmaGetAudit)
	initDenmaMaintenanceHandlers(g, a) // cmd/denma_maintenance.go
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
	if _, err := mail.ParseAddress(req.FromEmail); err != nil || len(req.FromEmail) > 300 {
		return echo.NewHTTPError(http.StatusBadRequest, `Enter the center's sender, as name@example.org or "Name" <name@example.org>.`)
	}
	// Its domain is added for it (cmd/denma_domains.go).
	domain := denmaSenderDomain(req.FromEmail)
	if !reDenmaDomain.MatchString(domain) {
		return echo.NewHTTPError(http.StatusBadRequest, "The sender's domain isn't one that can be set up for sending.")
	}
	adminEmail, err := a.importer.SanitizeEmail(req.AdminEmail)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "The admin's e-mail address isn't valid.")
	}
	// A new person's username must be free (an existing person, by e-mail
	// address, keeps theirs).
	if p, err := d.personByEmail(d.db(), adminEmail); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	} else if p == nil {
		if taken, err := d.usernameTaken(d.db(), req.AdminUsername, 0); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		} else if taken {
			return errDenmaUsernameTaken
		}
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

	// Its admin: a person (cmd/denma_people.go), new or not.
	var roleID int
	if err := ctr.app.db.Get(&roleID, `SELECT id FROM roles WHERE name = $1 AND type = 'user'`, denmaCenterAdminRole); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("The center was created, but not its admin: finding the %s role: %v", denmaCenterAdminRole, err))
	}
	_, inv, err := d.addMember(ctr, auth.User{
		Username:   req.AdminUsername,
		Name:       req.AdminName,
		Email:      null.StringFrom(adminEmail),
		UserRoleID: roleID,
		Status:     auth.UserStatusEnabled,
	})
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("The center was created, but not its admin: %v", err))
	}

	// The sender's domain, for the center: set up in SES, if it isn't.
	out := map[string]any{
		"slug":       ctr.Slug,
		"path":       path.Join(ctr.app.urlCfg.RootPath, uriAdmin),
		"invite_url": inv.URL, // "" for someone who has an account already
		"email_sent": inv.Sent,
		"login_url":  d.loginURL(),
	}
	if denmaDomainsOn() {
		st, err := d.addDomain(domain, "bounce."+domain, []int{ctr.ID})
		out["domain"], out["domain_url"], out["domain_can_send"] = domain, path.Join(a.urlCfg.RootPath, uriAdmin, "domains", domain), st.CanSend
		if err != nil {
			out["domain_warning"] = err.Error()
		}
	}
	return c.JSON(http.StatusOK, okResp{out})
}

// denmaHubSender is the hub's sender (app.from_email) from its settings
// form: the address entered there, as "Superadmin e-mails from"
// (partials/settings/general.html), with the hub's name. Only superadmins'
// own e-mails use it (password resets, the hub's notices); every center has
// its own sender, on its Config page.
func (a *App) denmaHubSender(set *models.Settings) error {
	if denmaHub == nil || a.denmaInCenter() {
		return nil
	}
	addr, err := mail.ParseAddress(strings.TrimSpace(set.AppFromEmail))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "Superadmin e-mails from isn't an e-mail address.")
	}
	if err := a.denmaCheckHubSender(addr.Address); err != nil { // cmd/denma_domains.go
		return denmaBadRequest(err)
	}
	set.AppFromEmail = fmt.Sprintf("%q <%s>", strings.TrimSpace(set.AppSiteName), addr.Address)
	return nil
}

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
	// DenmaDailySending is the e-mails sent in the last 24 hours against the
	// daily limit (cmd/denma_daily.go).
	funcs["DenmaDailySending"] = func() denmadaily.Status {
		return denmaDaily.Status()
	}
	// DenmaIsHub reports whether the page is the hub's.
	funcs["DenmaIsHub"] = func() bool {
		return denmaHub != nil && u.RootPath == denmaHub.current().urlCfg.RootPath
	}
	// DenmaInCenter reports whether the page is a center's, whose users are
	// people (cmd/denma_people.go).
	funcs["DenmaInCenter"] = func() bool {
		return denmaHub != nil && u.RootPath != denmaHub.current().urlCfg.RootPath
	}
	// DenmaOtherCenters is a person's other centers, for the menu under their
	// picture (partials/denma/topnav.html); userID is their account here.
	funcs["DenmaOtherCenters"] = func(userID int) []denmaOtherCenter {
		if denmaHub == nil || u.RootPath == denmaHub.current().urlCfg.RootPath {
			return nil
		}
		return denmaOtherCenters(path.Base(u.RootPath), userID)
	}
	// DenmaLoginURL and DenmaForgotURL are the sign-in and forgotten password
	// forms' addresses (public/templates): with centers, everyone's
	// (cmd/denma_login.go); else listmonk's, under root.
	funcs["DenmaLoginURL"] = func(root string) string {
		if denmaHub == nil {
			return root + "/admin/login"
		}
		return denmaLoginPath
	}
	funcs["DenmaForgotURL"] = func(root string) string {
		if denmaHub == nil {
			return root + "/admin/forgot"
		}
		return denmaForgotPath
	}
	// DenmaSourceURL is the fork's source (cmd/denma_system.go).
	funcs["DenmaSourceURL"] = func() string {
		return denmaSourceURL
	}
	// DenmaAdminLogo and DenmaAdminIcon are the top bar's logo and its icon on
	// narrow screens (partials/denma/topnav.html), from the hub's settings
	// (Settings -> General), in the hub and every center; "" for listmonk's.
	// Read from the hub as each page renders, so saving them shows everywhere
	// without reloading the centers.
	funcs["DenmaAdminLogo"] = func() string {
		if denmaHub == nil {
			return ""
		}
		return denmaHub.current().ko.String("denma.admin_logo_url")
	}
	funcs["DenmaAdminIcon"] = func() string {
		if denmaHub == nil {
			return ""
		}
		return denmaHub.current().ko.String("denma.admin_icon_url")
	}
}
