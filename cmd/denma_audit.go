package main

// denma: the activity log, for accountability. Every change made in the admin
// or through the API, in the hub and in every center, is recorded in
// denma.audit_log: who (the user, and for a superadmin in a center, their hub
// username), when, from where (IP), what (a description, the route, the
// target and its name), whether it worked (the HTTP status), and the request
// (query and body, with passwords, secrets and tokens removed and long values
// cut). So are sign-ins (and failed ones), sign-outs, password resets, a
// superadmin opening a center, and data exports. Reading pages and searching
// aren't.
//
// A center's log is on its Activity page, for those who can use its Config
// page; the superadmins' hub has every center's, on its own Activity page
// (both views/denma-activity.html). Rows are never changed or deleted by the
// app.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/listmonk/internal/auth"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

const denmaAuditSQL = `
	CREATE SCHEMA IF NOT EXISTS denma;
	CREATE TABLE IF NOT EXISTS denma.audit_log (
		id          BIGSERIAL PRIMARY KEY,
		created_at  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
		center      TEXT NOT NULL DEFAULT '',  -- the center's slug; '' for the hub
		user_id     INT NULL,                  -- in that center (or the hub)
		username    TEXT NOT NULL DEFAULT '',  -- or the one tried, for a sign-in
		user_name   TEXT NOT NULL DEFAULT '',
		superadmin  TEXT NOT NULL DEFAULT '',  -- a superadmin's hub username, in a center
		ip          TEXT NOT NULL DEFAULT '',
		method      TEXT NOT NULL,
		path        TEXT NOT NULL,
		route       TEXT NOT NULL DEFAULT '',
		target      TEXT NOT NULL DEFAULT '',  -- the :id (or :slug, :key, ...)
		action      TEXT NOT NULL,             -- what happened, in words
		status      INT NOT NULL,
		details     JSONB NOT NULL DEFAULT '{}' -- query, body, files
	);
	CREATE INDEX IF NOT EXISTS audit_log_center ON denma.audit_log (center, id DESC);`

// denmaInitAudit creates the log (with the registry in multi-center mode).
func denmaInitAudit(db *sqlx.DB) error {
	_, err := db.Exec(denmaAuditSQL)
	return err
}

// Requests that change nothing, though they're not GETs.
var denmaAuditSkip = regexp.MustCompile(`/preview|^POST /api/campaigns/:id/(text|content)$|^POST /admin/subscribers`)

// GETs that are logged.
var denmaAuditGets = map[string]bool{
	"/admin/centers/:slug/open":   true,
	"/api/subscribers/export":     true,
	"/api/subscribers/:id/export": true,
}

// Changes on these routes aren't by a signed-in user (subscribers, mail
// servers): listmonk records them where they belong.
var denmaAuditPublic = regexp.MustCompile(`^/(subscription|webhooks|link|campaign|archive|api/public|auth/oidc)`)

var denmaAuditInit sync.Once

// denmaAudit is the middleware that records changes (initHTTPRouter).
func denmaAudit(a *App) echo.MiddlewareFunc {
	if !a.ko.Bool("denma.multi_center") {
		// One install: no registry to make the log with.
		denmaAuditInit.Do(func() {
			if err := denmaInitAudit(a.db); err != nil {
				a.log.Printf("denma: error creating the activity log: %v", err)
			}
		})
	}

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			req := c.Request()
			route := c.Path()
			key := req.Method + " " + route
			if req.Method == http.MethodGet || req.Method == http.MethodHead || req.Method == http.MethodOptions {
				if !denmaAuditGets[route] {
					return next(c)
				}
			}
			if denmaAuditSkip.MatchString(key) || denmaAuditPublic.MatchString(route) || route == "" {
				return next(c)
			}

			// The body, for the record (put back for the handler).
			var body []byte
			if strings.HasPrefix(req.Header.Get(echo.HeaderContentType), echo.MIMEApplicationJSON) && req.Body != nil {
				b, err := io.ReadAll(req.Body)
				if err != nil {
					return err
				}
				body = b
				req.Body = io.NopCloser(bytes.NewReader(b))
			}

			// The target's name before it changes (or goes).
			target, targetName := denmaAuditTarget(a, c, route)

			err := next(c)

			status := c.Response().Status
			if err != nil {
				status = http.StatusInternalServerError
				var he *echo.HTTPError
				if errors.As(err, &he) {
					status = he.Code
				}
			}
			a.denmaAuditWrite(c, route, target, targetName, body, status)
			return err
		}
	}
}

// denmaAuditTarget is the route's :id (or other parameter) and, for an
// :id, the thing's name.
func denmaAuditTarget(a *App, c echo.Context, route string) (string, string) {
	names := c.ParamNames()
	if len(names) == 0 {
		return "", ""
	}
	target := c.Param(names[0])
	if names[0] != "id" {
		return target, ""
	}
	q := map[string]string{
		"campaigns":   `SELECT name FROM campaigns WHERE id = $1`,
		"lists":       `SELECT name FROM lists WHERE id = $1`,
		"templates":   `SELECT name FROM templates WHERE id = $1`,
		"subscribers": `SELECT email FROM subscribers WHERE id = $1`,
		"users":       `SELECT username FROM users WHERE id = $1`,
		"media":       `SELECT filename FROM media WHERE id = $1`,
		"roles":       `SELECT name FROM roles WHERE id = $1`,
		"automations": `SELECT name FROM denma_automations WHERE id = $1`,
	}[denmaAuditResource(route)]
	id, err := strconv.Atoi(target)
	if q == "" || err != nil {
		return target, ""
	}
	var name string
	_ = a.db.Get(&name, q, id)
	return target, name
}

// denmaAuditResource is the thing a route is about: campaigns in
// /api/campaigns/:id/status, automations in /api/denma/automations/:id.
func denmaAuditResource(route string) string {
	parts := strings.Split(strings.Trim(route, "/"), "/")
	if len(parts) > 1 && parts[0] == "api" {
		parts = parts[1:]
	}
	if len(parts) > 1 && parts[0] == "denma" {
		parts = parts[1:]
	}
	return parts[0]
}

func (a *App) denmaAuditWrite(c echo.Context, route, target, targetName string, body []byte, status int) {
	req := c.Request()

	var (
		uid            *int
		username, name string
		role           int
	)
	if u, ok := c.Get(auth.UserHTTPCtxKey).(auth.User); ok && u.ID > 0 {
		uid, username, name, role = &u.ID, u.Username, u.Name, u.UserRole.ID
	} else {
		// Signing in, or a password reset: who it was for.
		username = strings.TrimSpace(c.FormValue("username"))
		if username == "" {
			username = strings.TrimSpace(c.FormValue("email"))
		}
	}

	details := map[string]any{}
	if q := req.URL.Query(); len(q) > 0 {
		details["query"] = denmaRedact(map[string][]string(q))
	}
	if len(body) > 0 {
		var v any
		if json.Unmarshal(body, &v) == nil {
			details["body"] = denmaRedact(v)
		}
	} else if req.MultipartForm != nil {
		files := []string{}
		for _, fs := range req.MultipartForm.File {
			for _, f := range fs {
				files = append(files, fmt.Sprintf("%s (%d KB)", f.Filename, f.Size/1024))
				if targetName == "" {
					targetName = f.Filename
				}
			}
		}
		details["files"] = files
		if len(req.MultipartForm.Value) > 0 {
			details["form"] = denmaRedact(req.MultipartForm.Value)
		}
	} else if req.PostForm != nil && len(req.PostForm) > 0 {
		details["form"] = denmaRedact(map[string][]string(req.PostForm))
	}
	if loc := c.Response().Header().Get(echo.HeaderLocation); loc != "" {
		details["redirect"] = loc
	}
	dj, err := json.Marshal(details)
	if err != nil || len(dj) > 64*1024 {
		dj = []byte(`{"note": "too large to record"}`)
	}

	action := denmaAuditAction(req.Method, route, target, targetName, body, status, c.Response().Header().Get(echo.HeaderLocation))

	center := a.ko.String("denma.center")
	supSQL := `''`
	if denmaHub != nil && center != "" && role == auth.SuperAdminRoleID && uid != nil {
		supSQL = `COALESCE((SELECT hu.username FROM denma.center_superadmins s JOIN denma.centers ce ON ce.id = s.center_id
			JOIN ` + pq.QuoteIdentifier(denmaHub.baseSchema) + `.users hu ON hu.id = s.hub_user_id
			WHERE ce.slug = $1 AND s.center_user_id = $2), '')`
	}
	if _, err := a.db.Exec(`INSERT INTO denma.audit_log (center, user_id, username, user_name, superadmin, ip, method, path, route, target, action, status, details)
		VALUES ($1, $2, $3, $4, `+supSQL+`, $5, $6, $7, $8, $9, $10, $11, $12)`,
		center, uid, username, name, c.RealIP(), req.Method, req.URL.Path, route, target, action, status, string(dj)); err != nil {
		a.log.Printf("denma: error recording %s %s in the activity log: %v", req.Method, req.URL.Path, err)
	}
}

// Keys whose values aren't recorded.
var denmaSecretKey = regexp.MustCompile(`(?i)pass|secret|token|otp|totp|api_?key|access_?key|private|^code$|^key$|cookie|session|nonce`)

// denmaRedact is a request's values without secrets, with long text cut
// (campaign bodies) and long lists shortened (thousands of subscriber IDs).
func denmaRedact(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			if denmaSecretKey.MatchString(k) {
				out[k] = "(not recorded)"
				continue
			}
			out[k] = denmaRedact(x)
		}
		return out
	case map[string][]string:
		out := make(map[string]any, len(t))
		for k, x := range t {
			if denmaSecretKey.MatchString(k) {
				out[k] = "(not recorded)"
				continue
			}
			if len(x) == 1 {
				out[k] = denmaRedact(x[0])
			} else {
				out[k] = denmaRedact(toAnySlice(x))
			}
		}
		return out
	case []any:
		if len(t) > 100 {
			return append(denmaRedact(t[:100]).([]any), fmt.Sprintf("... %d more", len(t)-100))
		}
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = denmaRedact(x)
		}
		return out
	case string:
		if r := []rune(t); len(r) > 300 {
			return string(r[:300]) + fmt.Sprintf("... (%d characters)", len(r))
		}
	}
	return v
}

func toAnySlice(s []string) []any {
	out := make([]any, len(s))
	for i, x := range s {
		out[i] = x
	}
	return out
}

// Nouns for the routes' things.
var denmaAuditNouns = map[string]string{
	"campaigns": "campaign", "lists": "list", "subscribers": "subscriber", "templates": "template",
	"media": "media file", "bounces": "bounce", "users": "user", "roles": "role", "settings": "settings",
	"import": "import", "tx": "transactional e-mail", "automations": "automation", "centers": "center",
	"center": "center details", "profile": "own profile", "maintenance": "maintenance",
}

// denmaAuditAction describes a request in words.
func denmaAuditAction(method, route, target, targetName string, body []byte, status int, location string) string {
	var b map[string]any
	_ = json.Unmarshal(body, &b)
	str := func(k string) string {
		if s, ok := b[k].(string); ok {
			return s
		}
		return ""
	}

	// The thing: campaign 12 "October newsletter".
	label := func(noun string) string {
		s := noun
		if target != "" {
			s += " " + target
		}
		n := targetName
		if n == "" {
			for _, k := range []string{"name", "subject", "email", "username"} {
				if n = str(k); n != "" {
					break
				}
			}
		}
		if n != "" {
			s += fmt.Sprintf(" %q", n)
		}
		return s
	}

	switch method + " " + route {
	case "POST /admin/login":
		if status == http.StatusFound && strings.Contains(location, "/login/twofa") {
			return "Signed in with a password (2FA next)"
		}
		if status == http.StatusFound {
			return "Signed in"
		}
		return "Failed to sign in"
	case "POST /admin/login/twofa":
		if status == http.StatusFound && !strings.Contains(location, "/login") {
			return "Signed in (2FA)"
		}
		return "Failed two-factor sign-in"
	case "POST /admin/forgot":
		return "Asked for a password reset"
	case "POST /admin/reset":
		return "Reset a password"
	case "POST /api/logout":
		return "Signed out"
	case "GET /admin/centers/:slug/open":
		return "Opened center " + target
	case "GET /api/subscribers/export":
		return "Exported subscribers"
	case "GET /api/subscribers/:id/export":
		return "Downloaded the data of " + label("subscriber")
	case "POST /api/campaigns/:id/test":
		return "Sent a test of " + label("campaign")
	case "PUT /api/campaigns/:id/status":
		return fmt.Sprintf("Set %s to %s", label("campaign"), str("status"))
	case "PUT /api/campaigns/:id/archive":
		return "Changed the archive settings of " + label("campaign")
	case "POST /api/media":
		return "Uploaded " + label("media file")
	case "POST /api/subscribers/:id/optin":
		return "Sent an opt-in e-mail to " + label("subscriber")
	case "PUT /api/subscribers/:id/blocklist":
		return "Blocklisted " + label("subscriber")
	case "PUT /api/subscribers/blocklist":
		return "Blocklisted subscribers"
	case "PUT /api/subscribers/query/blocklist":
		return "Blocklisted subscribers by search"
	case "POST /api/subscribers/query/delete":
		return "Deleted subscribers by search"
	case "PUT /api/subscribers/lists", "PUT /api/subscribers/lists/:id", "PUT /api/subscribers/query/lists":
		return fmt.Sprintf("Changed list subscriptions (%s)", str("action"))
	case "DELETE /api/subscribers":
		return "Deleted subscribers"
	case "DELETE /api/subscribers/:id/bounces":
		return "Deleted the bounces of " + label("subscriber")
	case "PUT /api/bounces/blocklist":
		return "Blocklisted bounced subscribers"
	case "POST /api/import/subscribers":
		return "Started a subscriber import"
	case "DELETE /api/import/subscribers":
		return "Stopped or cleared a subscriber import"
	case "PUT /api/templates/:id/default":
		return "Made " + label("template") + " the default"
	case "PUT /api/settings":
		return "Changed the settings"
	case "PUT /api/settings/:key":
		return "Changed the setting " + target
	case "POST /api/settings/smtp/test":
		return "Tested the SMTP settings"
	case "POST /api/admin/reload":
		return "Restarted"
	case "POST /api/tx":
		return "Sent a transactional e-mail"
	case "PUT /api/profile":
		return "Changed their own profile"
	case "PUT /api/users/:id/twofa":
		return "Turned on two-factor sign-in for " + label("user")
	case "DELETE /api/users/:id/twofa":
		return "Turned off two-factor sign-in for " + label("user")
	case "POST /api/denma/automations/test":
		return "Sent a test of " + label("automation")
	case "PUT /api/denma/automations/:id/status":
		if on, _ := b["active"].(bool); on {
			return "Turned on " + label("automation")
		}
		return "Turned off " + label("automation")
	case "POST /api/denma/centers":
		return fmt.Sprintf("Created center %s %q", str("slug"), str("name"))
	case "PUT /api/denma/centers/:slug/status":
		return fmt.Sprintf("Set center %s to %s", target, str("status"))
	case "PUT /api/denma/center":
		return "Changed the center's details"
	case "POST /api/denma/campaigns/:id/resend":
		return "Resent " + label("campaign") + " to those it couldn't send to"
	case "POST /api/denma/tags":
		return fmt.Sprintf("Added the tag %q", str("tag"))
	case "PUT /api/denma/tags":
		if str("action") == "rename" {
			return fmt.Sprintf("Renamed the tag %q to %q", str("tag"), str("to"))
		}
		return fmt.Sprintf("Deleted the tag %q", str("tag"))
	case "PUT /api/denma/tags/subscribers":
		tags := []string{}
		if ts, ok := b["tags"].([]any); ok {
			for _, t := range ts {
				tags = append(tags, fmt.Sprint(t))
			}
		}
		verb := "Added the tags %s to"
		if str("action") == "remove" {
			verb = "Removed the tags %s from"
		}
		who := "the subscribers found by a search"
		if ids, ok := b["ids"].([]any); ok && len(ids) > 0 {
			who = fmt.Sprintf("%d subscribers", len(ids))
		}
		return fmt.Sprintf(verb+" %s", strings.Join(tags, ", "), who)
	}
	if strings.HasPrefix(route, "/api/maintenance/") {
		return "Ran maintenance: " + strings.TrimPrefix(route, "/api/maintenance/") + " " + target
	}

	// Anything else: created, changed or deleted, the thing.
	noun := denmaAuditNouns[denmaAuditResource(route)]
	if noun == "" {
		noun = strings.TrimPrefix(path.Clean(route), "/")
	}
	verb := map[string]string{"POST": "Created", "PUT": "Changed", "PATCH": "Changed", "DELETE": "Deleted"}[method]
	if verb == "" {
		verb = method
	}
	if target == "" && method == "DELETE" {
		return verb + " " + noun + "s"
	}
	return verb + " " + label(noun)
}

// ---- Reading it ----

type denmaAuditRow struct {
	ID         int64           `db:"id" json:"id"`
	CreatedAt  string          `db:"created_at" json:"created_at"`
	Center     string          `db:"center" json:"center"`
	CenterName string          `db:"center_name" json:"center_name"`
	Username   string          `db:"username" json:"username"`
	UserName   string          `db:"user_name" json:"user_name"`
	Superadmin string          `db:"superadmin" json:"superadmin"`
	IP         string          `db:"ip" json:"ip"`
	Method     string          `db:"method" json:"method"`
	Path       string          `db:"path" json:"path"`
	Action     string          `db:"action" json:"action"`
	Status     int             `db:"status" json:"status"`
	Details    json.RawMessage `db:"details" json:"details"`
}

// DenmaGetAudit returns a page of the log, newest first: a center's own (on
// its Activity page), or, in the hub, every center's (?center= a slug, or hub
// for the hub's own). ?before= an ID pages back; ?q= matches the user or the
// action.
func (a *App) DenmaGetAudit(c echo.Context) error {
	center := a.ko.String("denma.center")
	if center == "" && denmaHub != nil {
		// The hub: superadmins only, every center ("*"), one, or the hub's own ("").
		if _, err := a.hub(c); err != nil {
			return err
		}
		switch center = c.QueryParam("center"); center {
		case "":
			center = "*"
		case "hub":
			center = ""
		}
	} else if u := auth.GetUser(c); !u.HasPerm(denmaCenterPerm) {
		return echo.NewHTTPError(http.StatusForbidden, a.i18n.Ts("globals.messages.permissionDenied", "name", denmaCenterPerm))
	}

	before, _ := strconv.ParseInt(c.QueryParam("before"), 10, 64)
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	if limit < 1 || limit > 200 {
		limit = 50
	}
	out := []denmaAuditRow{}
	if err := a.db.Select(&out, `SELECT l.id, TO_CHAR(l.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS created_at,
			l.center, COALESCE(ce.name, '') AS center_name, l.username, l.user_name, l.superadmin, l.ip, l.method, l.path,
			l.action, l.status, l.details
		FROM denma.audit_log l LEFT JOIN denma.centers ce ON ce.slug = l.center
		WHERE ($1 = '*' OR l.center = $1)
		AND ($2 = 0 OR l.id < $2)
		AND ($3 = '' OR l.username ILIKE $3 OR l.user_name ILIKE $3 OR l.superadmin ILIKE $3 OR l.action ILIKE $3 OR l.ip ILIKE $3)
		ORDER BY l.id DESC LIMIT $4`,
		center, before, denmaAuditLike(c.QueryParam("q")), limit); err != nil {
		a.log.Printf("denma: error reading the activity log: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error reading the activity log.")
	}
	return c.JSON(http.StatusOK, okResp{out})
}

func denmaAuditLike(q string) string {
	if q = strings.TrimSpace(q); q == "" {
		return ""
	}
	return "%" + denmaLikeEscape(q) + "%"
}

// denmaActivityView is an Activity page: in the hub, every center's log; in a
// center, its own.
type denmaActivityView struct {
	adminView
	Hub     bool
	Centers []denmaNamedSlug
}

type denmaNamedSlug struct {
	Slug string `db:"slug" json:"slug"`
	Name string `db:"name" json:"name"`
}

// ViewDenmaActivity renders the Activity page: the hub's (superadmins), or a
// center's (center:manage).
func (a *App) ViewDenmaActivity(c echo.Context) error {
	if a.inCenter() == nil {
		v := newAdminView(c, "Activity", "", "denma.activity")
		if !v.Can(denmaCenterPerm) {
			return echo.NewHTTPError(http.StatusForbidden, a.i18n.Ts("globals.messages.permissionDenied", "name", denmaCenterPerm))
		}
		return c.Render(http.StatusOK, "admin-denma-activity", denmaActivityView{adminView: v})
	}
	if _, err := a.hub(c); err != nil {
		return err
	}
	centers := []denmaNamedSlug{}
	if err := a.db.Select(&centers, `SELECT slug, name FROM denma.centers ORDER BY name`); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.Render(http.StatusOK, "admin-denma-activity", denmaActivityView{
		adminView: newAdminView(c, "Activity", "", "denma.activity"),
		Hub:       true,
		Centers:   centers,
	})
}
