package main

// denma: MCP keys, for Claude through the listmonk MCP server (the mcp/
// folder beside the fork). A center's admins make them on its Config -> MCP
// page (views/denma-mcp.html), each for one of the center's users: the key
// acts as that user, so the activity log says who did what (and that it was
// through the key), and it can do only what both its own permissions and
// the user's role allow. Its requests go through the center's admin API, the
// same handlers and checks as the admin's own:
//
//   - GET /mcp/v1/centers: the centers the caller can use, with each key's
//     user and permissions;
//   - /mcp/v1/centers/<slug>/api/...: the center's /api/..., as the key's
//     user.
//
// The caller is a key (Authorization: Bearer lmm_...), for Claude Code and
// scripts; or the hub's connector secret (lmc_..., made on the hub's MCP
// page by superadmins) with X-Listmonk-Person: the e-mail address someone
// signed in to claude.ai with (the MCP server checks it with Google), who
// then uses their own key in the center, if it's ticked "usable from
// claude.ai".
//
// Keys and the secret are kept as SHA-256 and shown once. What hands out
// secrets or signs people in isn't reachable with a key (denmaMCPBlocked),
// nor exports of people's data unless the key may show personal data. Each
// key makes at most denmaMCPPerMinute requests a minute; requests with keys
// that don't work are limited by address.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/internal/utils"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

const (
	denmaMCPPath         = "/mcp/v1"
	denmaMCPKeyPrefix    = "lmm_"
	denmaMCPSecretPrefix = "lmc_"
	denmaMCPPersonHeader = "X-Listmonk-Person"
	denmaMCPMaxKeys      = 100 // keys a center can have
	denmaMCPPerMinute    = 600 // requests a key makes a minute

	// Key permissions for the lists a user's list role gives them.
	denmaMCPListGet    = "list:get"
	denmaMCPListManage = "list:manage"
)

// denmaMCPBadKeys limits requests with keys or secrets that don't work, by
// client address.
var denmaMCPBadKeys = newDenmaThrottle(20, 15*time.Minute)

const denmaMCPSQL = `
CREATE TABLE IF NOT EXISTS denma.mcp_keys (
    id            SERIAL PRIMARY KEY,
    center_id     INTEGER NOT NULL REFERENCES denma.centers(id) ON DELETE CASCADE,
    user_id       INTEGER NOT NULL,     -- the center's user it acts as
    name          TEXT NOT NULL,
    key_hash      TEXT NOT NULL UNIQUE, -- SHA-256 of the key, hex
    key_hint      TEXT NOT NULL,        -- its last 4 characters
    permissions   TEXT[] NOT NULL DEFAULT '{}',
    personal_data BOOLEAN NOT NULL DEFAULT FALSE, -- Claude may see people's addresses, names and attributes
    connector     BOOLEAN NOT NULL DEFAULT FALSE, -- usable from claude.ai, through the connector secret
    expires_at    TIMESTAMP WITH TIME ZONE NULL,
    uses          INTEGER NOT NULL DEFAULT 0,
    last_used_at  TIMESTAMP WITH TIME ZONE NULL,
    created_by    TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS mcp_keys_center ON denma.mcp_keys (center_id, user_id);
-- One key a user can use from claude.ai in each center.
CREATE UNIQUE INDEX IF NOT EXISTS mcp_keys_connector ON denma.mcp_keys (center_id, user_id) WHERE connector;
-- The hosted MCP server's secret, for people signed in to claude.ai.
CREATE TABLE IF NOT EXISTS denma.mcp_connector (
    id           INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    secret_hash  TEXT NOT NULL,
    secret_hint  TEXT NOT NULL,
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    uses         INTEGER NOT NULL DEFAULT 0,
    last_used_at TIMESTAMP WITH TIME ZONE NULL
);
ALTER TABLE denma.mcp_connector ADD COLUMN IF NOT EXISTS allowed_from TEXT[] NOT NULL DEFAULT '{}'; -- where it can be used from`

// denmaInitMCP makes the keys' tables (with the registry).
func denmaInitMCP(db interface {
	Exec(string, ...any) (sql.Result, error)
}) error {
	_, err := db.Exec(denmaMCPSQL)
	return err
}

// denmaMCPKey is a key, as the pages and the gateway see it.
type denmaMCPKey struct {
	ID           int            `db:"id" json:"id"`
	CenterID     int            `db:"center_id" json:"-"`
	Slug         string         `db:"slug" json:"-"`
	UserID       int            `db:"user_id" json:"user_id"`
	Username     string         `db:"username" json:"username"`   // "" if the user is gone
	UserName     string         `db:"user_name" json:"user_name"` // their name
	Name         string         `db:"name" json:"name"`
	KeyHint      string         `db:"key_hint" json:"key_hint"`
	Permissions  pq.StringArray `db:"permissions" json:"permissions"`
	PersonalData bool           `db:"personal_data" json:"personal_data"`
	Connector    bool           `db:"connector" json:"connector"`
	ExpiresAt    *time.Time     `db:"expires_at" json:"expires_at"`
	Uses         int            `db:"uses" json:"uses"`
	LastUsedAt   *time.Time     `db:"last_used_at" json:"last_used_at"`
	CreatedBy    string         `db:"created_by" json:"created_by"`
	CreatedAt    time.Time      `db:"created_at" json:"created_at"`
	Hub          bool           `db:"-" json:"hub"`      // a superadmin's account
	Editable     bool           `db:"-" json:"editable"` // by whoever's looking
	Lacks        []string       `db:"-" json:"lacks"`    // permissions the user's role doesn't give now

	Key string `db:"-" json:"key,omitempty"` // only when it's made
}

// denmaMCPBlocked is the admin API a key can't reach: what hands out keys or
// secrets, signs in or out, changes a profile or two-factor sign-in, the
// hub's settings, the server's logs, its event stream and its about page
// (database size, hostname), the center's activity log, the e-mails that
// ask someone for consent, people and roles, and maintenance. "{}" is any
// one path segment.
var denmaMCPBlocked = []string{
	"/api/denma/center/api-keys", "/api/denma/center/api", "/api/denma/center/signup-hooks", "/api/denma/center/mcp-keys",
	"/api/settings", "/api/admin", "/api/logs", "/api/about", "/api/events", "/api/profile", "/api/logout", "/api/login",
	"/api/denma/audit", "/api/subscribers/{}/optin", "/api/denma/subscribers/resubscribe",
	"/api/users", "/api/roles", "/api/maintenance",
}

// denmaMCPWriteBlockedExact is what a key can read but not change: the
// center's own details (sender, reply-to, postal address).
var denmaMCPWriteBlockedExact = []string{"/api/denma/center"}

// denmaMCPExports are the admin API's downloads of people's data, for keys
// that may show personal data only.
var denmaMCPExports = []string{"/api/subscribers/export"}

// denmaMCPMatch is whether p is pattern or under it.
func denmaMCPMatch(p, pattern string) bool {
	ps, pp := strings.Split(p, "/"), strings.Split(pattern, "/")
	if len(ps) < len(pp) {
		return false
	}
	for i, seg := range pp {
		if seg != "{}" && seg != ps[i] {
			return false
		}
	}
	return true
}

// denmaMCPAllowed is why a key can't make a request (p: an /api path), or "".
func denmaMCPAllowed(p, method string, personal bool) string {
	refused := fmt.Sprintf("%s isn't available through the MCP: it's done in the admin.", p)
	for _, pre := range denmaMCPBlocked {
		if denmaMCPMatch(p, pre) {
			return refused
		}
	}
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions &&
		denmaHasString(denmaMCPWriteBlockedExact, strings.TrimRight(p, "/")) {
		return refused
	}
	if !personal {
		for _, pre := range denmaMCPExports {
			if denmaMCPMatch(p, pre) {
				return fmt.Sprintf("%s exports people's data, which this key doesn't show Claude.", p)
			}
		}
		if strings.HasPrefix(p, "/api/subscribers/") && strings.HasSuffix(p, "/export") {
			return fmt.Sprintf("%s exports people's data, which this key doesn't show Claude.", p)
		}
	}
	return ""
}

// denmaMCPNoKeyPerms are permissions a key can't have: raw SQL is off for
// everyone, bounce webhooks were API users', settings are the hub's, and
// people and roles are the admin's (a key could add an admin who keeps its
// permissions and none of its limits).
var denmaMCPNoKeyPerms = map[string]bool{
	"subscribers:sql_query": true, "webhooks:post_bounce": true,
	"settings:get": true, "settings:manage": true, "settings:maintain": true,
	"users:get": true, "users:manage": true, "roles:get": true, "roles:manage": true,
}

// denmaMCPPermGroup is a group of permissions a key can have.
type denmaMCPPermGroup struct {
	Group       string   `json:"group"`
	Permissions []string `json:"permissions"`
}

// mcpPermGroups is what keys can be given, by group: the app's permissions
// (with ours) but denmaMCPNoKeyPerms, and the lists of a user's list role.
func (a *App) mcpPermGroups() []denmaMCPPermGroup {
	var groups []denmaMCPPermGroup
	if err := json.Unmarshal(a.cfg.PermissionsRaw, &groups); err != nil {
		return nil
	}
	out := []denmaMCPPermGroup{}
	for _, g := range groups {
		keep := []string{}
		for _, p := range g.Permissions {
			if !denmaMCPNoKeyPerms[p] {
				keep = append(keep, p)
			}
		}
		if g.Group == "lists" {
			keep = append(keep, denmaMCPListGet, denmaMCPListManage)
		}
		if len(keep) > 0 {
			out = append(out, denmaMCPPermGroup{Group: g.Group, Permissions: keep})
		}
	}
	return out
}

// denmaMCPUser is the user a key's request runs as: theirs, with only the
// permissions (and lists) both the key and their role give. A superadmin's
// account isn't a Super Admin through a key: it has what the key gives.
func denmaMCPUser(u auth.User, keyPerms []string, known map[string]struct{}) auth.User {
	super := u.UserRole.ID == auth.SuperAdminRoleID
	eff := map[string]struct{}{}
	perms := []string{}
	for _, p := range keyPerms {
		if p == denmaMCPListGet || p == denmaMCPListManage || denmaMCPNoKeyPerms[p] {
			continue
		}
		_, has := u.PermissionsMap[p]
		if _, ok := known[p]; super && ok {
			has = true
		}
		if _, dup := eff[p]; has && !dup {
			eff[p] = struct{}{}
			perms = append(perms, p)
		}
	}
	sort.Strings(perms)
	u.PermissionsMap = eff
	u.UserRole.Permissions = perms
	u.UserRolePerms = perms
	if super {
		u.UserRole.ID = 0
	}

	// The lists their list role gives, as far as the key does.
	lists := map[int]map[string]struct{}{}
	u.GetListIDs, u.ManageListIDs = nil, nil
	if u.ListRole != nil {
		lr := *u.ListRole
		kept := []auth.ListPermission{}
		for _, l := range lr.Lists {
			lp := pq.StringArray{}
			for _, p := range l.Permissions {
				if denmaHasString(keyPerms, p) {
					lp = append(lp, p)
				}
			}
			if len(lp) == 0 {
				continue
			}
			kept = append(kept, auth.ListPermission{ID: l.ID, Name: l.Name, Permissions: lp})
			lists[l.ID] = map[string]struct{}{}
			for _, p := range lp {
				lists[l.ID][p] = struct{}{}
				switch p {
				case auth.PermListGet:
					u.GetListIDs = append(u.GetListIDs, l.ID)
				case auth.PermListManage:
					u.ManageListIDs = append(u.ManageListIDs, l.ID)
				}
			}
		}
		lr.Lists = kept
		u.ListRole = &lr
	}
	u.ListPermissionsMap = lists
	return u
}

// denmaMCPReq is what a request through a key carries to the center (in its
// context), for the activity log.
type denmaMCPReq struct {
	KeyID   int
	KeyName string
	Super   bool   // the user is a superadmin's account
	Person  string // the person signed in to claude.ai, through the connector
}

type denmaMCPCtxKey struct{}

// denmaMCPOf is the key a request came through, if it did.
func denmaMCPOf(c echo.Context) (denmaMCPReq, bool) {
	m, ok := c.Request().Context().Value(denmaMCPCtxKey{}).(denmaMCPReq)
	return m, ok
}

// denmaMCPCaller is who's calling: a key, or the connector with a person.
type denmaMCPCaller struct {
	key    *denmaMCPKey
	person string // the connector's: the person's e-mail address, lowercase
}

// denmaMCPInternal is a plain answer to a failure the caller needn't know
// about, which is logged.
func denmaMCPInternal(err error) error {
	lo.Printf("denma: MCP gateway: %v", err)
	return echo.NewHTTPError(http.StatusInternalServerError, "Something went wrong. Try again in a minute.")
}

// mcpRoute serves /mcp/v1/... (done: it was one).
func (d *denmaCenters) mcpRoute(c echo.Context) (bool, error) {
	p := c.Request().URL.Path
	if p != denmaMCPPath && !strings.HasPrefix(p, denmaMCPPath+"/") {
		return false, nil
	}
	return true, d.mcpServe(c, strings.TrimPrefix(p, denmaMCPPath))
}

func (d *denmaCenters) mcpServe(c echo.Context, rest string) error {
	// Failures are counted by the connector secret when it's right (the hosted
	// server's one address must not lock everyone out for what a stranger
	// sends), else by address.
	ip := c.RealIP()
	secret, secretOK := d.mcpConnectorSecret(c, ip)
	bad := ip
	if secretOK {
		bad = "secret:" + secret
	}
	if wait := denmaMCPBadKeys.wait(bad); wait > 0 {
		return denmaTooMany(wait)
	}
	who, err := d.mcpCaller(c, secret, secretOK)
	if err != nil {
		denmaMCPBadKeys.add(bad)
		if who.key != nil { // a key that's known, but expired
			d.mcpRefused(c, *who.key, nil, http.StatusUnauthorized, "the key has expired")
		}
		c.Response().Header().Set("WWW-Authenticate", `Bearer realm="listmonk"`)
		return err
	}

	if rest == "/centers" || rest == "/centers/" {
		if c.Request().Method != http.MethodGet {
			return echo.NewHTTPError(http.StatusMethodNotAllowed, "method not allowed")
		}
		keys, err := d.mcpKeysOf(who, "")
		if err != nil {
			return denmaMCPInternal(err)
		}
		out := []map[string]any{}
		for _, k := range keys {
			ctr := d.get(k.Slug)
			if ctr == nil {
				continue
			}
			u, err := ctr.app.core.GetUser(k.UserID, "", "")
			if u, err = denmaSessionUser(ctr.app.ko, u, err); err != nil || u.Type != auth.UserTypeUser {
				continue
			}
			eff := denmaMCPUser(u, k.Permissions, ctr.app.cfg.Permissions)
			perms := append([]string{}, eff.UserRole.Permissions...)
			for _, p := range []string{denmaMCPListGet, denmaMCPListManage} {
				if denmaHasString(k.Permissions, p) && u.ListRole != nil {
					perms = append(perms, p)
				}
			}
			out = append(out, map[string]any{
				"center": map[string]string{"slug": k.Slug, "name": ctr.Name},
				"user":   map[string]any{"id": u.ID, "username": u.Username, "name": u.Name},
				"key": map[string]any{"id": k.ID, "name": k.Name, "hint": k.KeyHint, "permissions": perms,
					"personal_data": k.PersonalData, "connector": k.Connector, "expires_at": k.ExpiresAt},
			})
		}
		return c.JSON(http.StatusOK, okResp{out})
	}

	slug, sub, _ := strings.Cut(strings.TrimPrefix(rest, "/centers/"), "/")
	if !strings.HasPrefix(rest, "/centers/") || slug == "" || (sub != "api" && !strings.HasPrefix(sub, "api/")) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	keys, err := d.mcpKeysOf(who, slug)
	if err != nil {
		return denmaMCPInternal(err)
	}
	if len(keys) == 0 {
		if who.key != nil {
			d.mcpRefused(c, *who.key, nil, http.StatusForbidden, "used for another center")
		}
		// The same answer whether the center exists or not.
		return echo.NewHTTPError(http.StatusForbidden, "There's no key for this center.")
	}
	k := keys[0]
	ctr := d.get(slug)
	if ctr == nil {
		if d.isStarting(slug) {
			return echo.NewHTTPError(http.StatusServiceUnavailable, "The center is starting. Try again in a minute.")
		}
		return echo.NewHTTPError(http.StatusForbidden, "There's no key for this center.")
	}
	p := "/" + sub
	// Counted first: refusals are requests too, each in the activity log.
	if n := denmaMCPRateOK(k.ID); n > denmaMCPPerMinute {
		if n == denmaMCPPerMinute+1 { // once a minute, not every request
			d.mcpRefused(c, k, ctr, http.StatusTooManyRequests, "over the requests a minute")
		}
		return echo.NewHTTPError(http.StatusTooManyRequests, fmt.Sprintf("At most %d requests a minute. Try again in a minute.", denmaMCPPerMinute))
	}
	if why := denmaMCPAllowed(p, c.Request().Method, k.PersonalData); why != "" {
		d.mcpRefused(c, k, ctr, http.StatusForbidden, "not available through the MCP")
		return echo.NewHTTPError(http.StatusForbidden, why)
	}
	// A superadmin's account goes with their hub user, as when signed in.
	u, err := ctr.app.core.GetUser(k.UserID, "", "")
	u, err = denmaSessionUser(ctr.app.ko, u, err)
	if err != nil || u.Type != auth.UserTypeUser {
		d.mcpRefused(c, k, ctr, http.StatusUnauthorized, "the key's user is gone or disabled")
		return echo.NewHTTPError(http.StatusUnauthorized, "The key's user is no longer in the center, or is disabled.")
	}
	if _, err := d.base.db.Exec(`UPDATE denma.mcp_keys SET uses = uses + 1, last_used_at = NOW() WHERE id = $1`, k.ID); err != nil {
		lo.Printf("denma: error counting MCP key %d's use: %v", k.ID, err)
	}

	// The center answers as for its own admin, as the key's user.
	m := denmaMCPReq{KeyID: k.ID, KeyName: k.Name, Super: u.UserRole.ID == auth.SuperAdminRoleID, Person: who.person}
	ctx := context.WithValue(c.Request().Context(), denmaMCPCtxKey{}, m)
	ctx = auth.DenmaWithUser(ctx, denmaMCPUser(u, k.Permissions, ctr.app.cfg.Permissions))
	r := c.Request().Clone(ctx)
	r.URL.Path = p
	r.URL.RawPath = ""
	r.RequestURI = r.URL.RequestURI()
	for _, h := range []string{echo.HeaderAuthorization, denmaMCPPersonHeader, "Cookie", "Origin", "Referer", denmaTabHeader} {
		r.Header.Del(h)
	}
	denmaSandbox(ctr.app, c.Response().Header(), p)
	ctr.router.ServeHTTP(c.Response(), r)
	return nil
}

// mcpConnectorSecret is the connector secret a request brings, hashed, and
// whether it's the right one, from where it may be used.
func (d *denmaCenters) mcpConnectorSecret(c echo.Context, ip string) (string, bool) {
	secret, ok := denmaBearer(c.Request().Header.Get(echo.HeaderAuthorization), denmaMCPSecretPrefix)
	if !ok {
		return "", false
	}
	hash := denmaAPIHash(secret)
	var from pq.StringArray
	if err := d.base.db.Get(&from, `SELECT allowed_from FROM denma.mcp_connector WHERE secret_hash = $1`, hash); err != nil {
		return hash, false
	}
	return hash, denmaFromAllowed(ip, from)
}

// mcpCaller checks a request's key, or the connector secret (secretOK) and
// the person it's for. A key that's known but expired comes back with the
// error.
func (d *denmaCenters) mcpCaller(c echo.Context, secret string, secretOK bool) (denmaMCPCaller, error) {
	unknown := echo.NewHTTPError(http.StatusUnauthorized, "unrecognized key")
	hdr := c.Request().Header.Get(echo.HeaderAuthorization)
	if key, ok := denmaBearer(hdr, denmaMCPKeyPrefix); ok {
		var k denmaMCPKey
		err := d.base.db.Get(&k, `SELECT k.id, k.center_id, c.slug, k.user_id, k.name, k.key_hint, k.permissions, k.personal_data,
			k.connector, k.expires_at, k.uses, k.last_used_at, k.created_by, k.created_at, '' AS username, '' AS user_name
			FROM denma.mcp_keys k JOIN denma.centers c ON c.id = k.center_id AND c.status = 'enabled'
			WHERE k.key_hash = $1`, denmaAPIHash(key))
		if errors.Is(err, sql.ErrNoRows) {
			return denmaMCPCaller{}, unknown
		}
		if err != nil {
			return denmaMCPCaller{}, denmaMCPInternal(err)
		}
		if k.ExpiresAt != nil && !k.ExpiresAt.After(time.Now()) {
			return denmaMCPCaller{key: &k}, echo.NewHTTPError(http.StatusUnauthorized, fmt.Sprintf(
				"This key expired on %s. The center's admins can give it a later date on its Config -> MCP page.", k.ExpiresAt.UTC().Format("2006-01-02")))
		}
		return denmaMCPCaller{key: &k}, nil
	}
	if _, ok := denmaBearer(hdr, denmaMCPSecretPrefix); ok {
		if !secretOK {
			return denmaMCPCaller{}, unknown
		}
		if _, err := d.base.db.Exec(`UPDATE denma.mcp_connector SET uses = uses + 1, last_used_at = NOW() WHERE secret_hash = $1`, secret); err != nil {
			return denmaMCPCaller{}, denmaMCPInternal(err)
		}
		person := strings.ToLower(strings.TrimSpace(c.Request().Header.Get(denmaMCPPersonHeader)))
		if a, err := mail.ParseAddress(person); err != nil || a.Address != person {
			return denmaMCPCaller{}, echo.NewHTTPError(http.StatusBadRequest, "Send the person's e-mail address in "+denmaMCPPersonHeader+".")
		}
		return denmaMCPCaller{person: person}, nil
	}
	return denmaMCPCaller{}, unknown
}

// mcpRefused records, in the key's center's activity log, a request through
// the key that was refused: the route, not the query or body (ctr: the
// center, if it's known).
func (d *denmaCenters) mcpRefused(c echo.Context, k denmaMCPKey, ctr *denmaCenter, status int, why string) {
	var username, name string
	if ctr == nil {
		ctr = d.get(k.Slug)
	}
	if ctr != nil {
		if u, err := ctr.app.core.GetUser(k.UserID, "", ""); err == nil {
			username, name = u.Username, u.Name
		}
	}
	req := c.Request()
	details, _ := json.Marshal(map[string]any{"via": fmt.Sprintf("MCP key %q", k.Name), "mcp_key_id": k.ID, "refused": why})
	if _, err := d.base.db.Exec(`INSERT INTO denma.audit_log (center, user_id, username, user_name, superadmin, ip, method, path, route, target, action, status, details)
		VALUES ($1, $2, $3, $4, '', $5, $6, $7, $8, '', $9, $10, $11)`,
		k.Slug, k.UserID, username, name, c.RealIP(), req.Method, req.URL.Path, denmaMCPPath+"/centers/:slug/api/*",
		"Refused a request through an MCP key: "+why, status, string(details)); err != nil {
		lo.Printf("denma: error recording a refused MCP request in the activity log: %v", err)
	}
}

// mcpKeysOf is the caller's keys: a key's own, or the person's keys usable
// from claude.ai, in a center (slug) or all, in enabled centers.
func (d *denmaCenters) mcpKeysOf(who denmaMCPCaller, slug string) ([]denmaMCPKey, error) {
	if who.key != nil {
		if slug != "" && who.key.Slug != slug {
			return nil, nil
		}
		return []denmaMCPKey{*who.key}, nil
	}
	out := []denmaMCPKey{}
	// The person's accounts: as a person (memberships), or as a superadmin.
	err := d.base.db.Select(&out, `SELECT k.id, k.center_id, c.slug, k.user_id, k.name, k.key_hint, k.permissions, k.personal_data,
		k.connector, k.expires_at, k.uses, k.last_used_at, k.created_by, k.created_at, '' AS username, '' AS user_name
		FROM denma.mcp_keys k JOIN denma.centers c ON c.id = k.center_id AND c.status = 'enabled'
		WHERE k.connector AND (k.expires_at IS NULL OR k.expires_at > NOW()) AND ($2 = '' OR c.slug = $2)
		AND (EXISTS (SELECT 1 FROM denma.memberships m JOIN denma.people p ON p.id = m.person_id
				WHERE m.center_id = k.center_id AND m.center_user_id = k.user_id AND LOWER(p.email) = $1)
			OR EXISTS (SELECT 1 FROM denma.center_superadmins s JOIN `+pq.QuoteIdentifier(d.baseSchema)+`.users hu ON hu.id = s.hub_user_id
				WHERE s.center_id = k.center_id AND s.center_user_id = k.user_id AND LOWER(hu.email) = $1))
		ORDER BY c.slug, k.id`, who.person, slug)
	return out, err
}

// denmaMCPRate is each key's requests this minute, for denmaMCPPerMinute.
var denmaMCPRate = struct {
	sync.Mutex
	m map[int]*denmaSignupWindow
}{m: map[int]*denmaSignupWindow{}}

// denmaMCPRateOK counts a key's request, returning how many it made this
// minute.
func denmaMCPRateOK(id int) int {
	now := time.Now()
	denmaMCPRate.Lock()
	defer denmaMCPRate.Unlock()
	if len(denmaMCPRate.m) > 1000 { // the keys that have been quiet a minute go
		for k, w := range denmaMCPRate.m {
			if now.Sub(w.start) >= time.Minute {
				delete(denmaMCPRate.m, k)
			}
		}
	}
	w := denmaMCPRate.m[id]
	if w == nil || now.Sub(w.start) >= time.Minute {
		w = &denmaSignupWindow{start: now}
		denmaMCPRate.m[id] = w
	}
	w.n++
	return w.n
}

// ---- Config -> MCP: a center's keys ----

func initDenmaMCPAdminHandlers(g *echo.Group, a *App) {
	g.GET(path.Join(uriAdmin, "/center/mcp"), a.ViewDenmaMCP)
	g.GET(path.Join(uriAdmin, "/mcp"), a.ViewDenmaHubMCP)
}

func initDenmaMCPKeyHandlers(g *echo.Group, a *App) {
	pm := func(h echo.HandlerFunc) echo.HandlerFunc { return a.auth.Perm(h, denmaCenterPerm) }
	g.GET("/api/denma/center/mcp-keys", pm(a.DenmaGetMCPKeys))
	g.POST("/api/denma/center/mcp-keys", pm(a.DenmaCreateMCPKey))
	g.PUT("/api/denma/center/mcp-keys/:id", pm(a.DenmaUpdateMCPKey))
	g.POST("/api/denma/center/mcp-keys/:id/key", pm(a.DenmaNewMCPKey))
	g.DELETE("/api/denma/center/mcp-keys/:id", pm(a.DenmaDeleteMCPKey))
	g.POST("/api/denma/mcp/connector", a.DenmaNewMCPConnector)
	g.PUT("/api/denma/mcp/connector", a.DenmaUpdateMCPConnector)
	g.DELETE("/api/denma/mcp/connector", a.DenmaDeleteMCPConnector)
}

// mcpCenterID is the center's ID in the registry.
func (a *App) mcpCenterID() (int, error) {
	if err := a.inCenter(); err != nil {
		return 0, err
	}
	var id int
	if err := a.db.Get(&id, `SELECT id FROM denma.centers WHERE slug = $1`, a.ko.String("denma.center")); err != nil {
		return 0, echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return id, nil
}

const denmaMCPKeySQL = `SELECT k.id, k.center_id, '' AS slug, k.user_id, k.name, k.key_hint, k.permissions, k.personal_data, k.connector,
	k.expires_at, k.uses, k.last_used_at, k.created_by, k.created_at,
	COALESCE(u.username, '') AS username, COALESCE(u.name, '') AS user_name
	FROM denma.mcp_keys k LEFT JOIN users u ON u.id = k.user_id`

// mcpKeys is the center's keys, as c's user sees them: those of the hub's
// accounts only for superadmins, editable if they can manage the user.
func (a *App) mcpKeys(c echo.Context, id int) ([]denmaMCPKey, error) {
	cid, err := a.mcpCenterID()
	if err != nil {
		return nil, err
	}
	out := []denmaMCPKey{}
	q := denmaMCPKeySQL + ` WHERE k.center_id = $1 AND ($2 = 0 OR k.id = $2) ORDER BY k.id`
	if err := a.db.Select(&out, q, cid, id); err != nil {
		return nil, echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	hub, err := a.denmaHubAccounts()
	if err != nil {
		return nil, err
	}
	me := auth.GetUser(c)
	shown := out[:0]
	for _, k := range out {
		k.Hub = hub[k.UserID]
		if k.Hub && denmaBound(me) {
			continue
		}
		if k.Permissions == nil {
			k.Permissions = pq.StringArray{}
		}
		k.Lacks = []string{}
		if u, err := a.core.GetUser(k.UserID, "", ""); err == nil {
			k.Editable = a.mcpMayKey(me, u, hub)
			if u.UserRole.ID != auth.SuperAdminRoleID {
				for _, p := range k.Permissions {
					if p == denmaMCPListGet || p == denmaMCPListManage {
						if u.ListRole == nil {
							k.Lacks = append(k.Lacks, p)
						}
					} else if !u.HasPerm(p) {
						k.Lacks = append(k.Lacks, p)
					}
				}
			}
		} else {
			k.Editable = !denmaBound(me) || !k.Hub
		}
		shown = append(shown, k)
	}
	return shown, nil
}

// mcpKey is one of the center's keys that c's user can change.
func (a *App) mcpKey(c echo.Context) (denmaMCPKey, error) {
	id, _ := strconv.Atoi(c.Param("id"))
	keys, err := a.mcpKeys(c, id)
	if err != nil {
		return denmaMCPKey{}, err
	}
	if id < 1 || len(keys) == 0 {
		return denmaMCPKey{}, echo.NewHTTPError(http.StatusNotFound, "That key doesn't exist (any more).")
	}
	if !keys[0].Editable {
		return keys[0], echo.NewHTTPError(http.StatusForbidden, "You can only manage keys of users with no more than you have.")
	}
	return keys[0], nil
}

// mcpMayKey is whether me can make or change a key for u: their own, or a
// user they see with no more than they have.
func (a *App) mcpMayKey(me, u auth.User, hub map[int]bool) bool {
	if u.ID == me.ID {
		return true
	}
	if hub[u.ID] || u.Type != auth.UserTypeUser {
		return false
	}
	return denmaWithin(me, u)
}

// denmaMCPUserOption is a user a key can be made for.
type denmaMCPUserOption struct {
	ID          int      `json:"id"`
	Username    string   `json:"username"`
	Name        string   `json:"name"`
	Role        string   `json:"role"`
	Permissions []string `json:"permissions"` // their role's ("*": all)
	Lists       bool     `json:"lists"`       // they have a list role
}

// mcpUserOptions is the users c's user can make keys for.
func (a *App) mcpUserOptions(c echo.Context) ([]denmaMCPUserOption, error) {
	users, err := a.core.GetUsers(auth.UserTypeUser, auth.UserStatusEnabled, 0, 0)
	if err != nil {
		return nil, err
	}
	hub, err := a.denmaHubAccounts()
	if err != nil {
		return nil, err
	}
	me := auth.GetUser(c)
	out := []denmaMCPUserOption{}
	for _, u := range users {
		if !a.mcpMayKey(me, u, hub) {
			continue
		}
		perms := u.UserRole.Permissions
		if u.UserRole.ID == auth.SuperAdminRoleID {
			perms = []string{"*"}
		}
		if perms == nil {
			perms = []string{}
		}
		out = append(out, denmaMCPUserOption{ID: u.ID, Username: u.Username, Name: u.Name, Role: u.UserRole.Name,
			Permissions: perms, Lists: u.ListRole != nil})
	}
	return out, nil
}

// denmaMCPView is Config -> MCP.
type denmaMCPView struct {
	adminView
	Keys       []denmaMCPKey
	Users      []denmaMCPUserOption
	Groups     []denmaMCPPermGroup
	Mine       []string // the permissions c's user can give ("*": all)
	MCPURL     string   // where the gateway is
	Connector  bool     // the hub has a connector secret
	PerMinute  int
	CenterSlug string
}

// ViewDenmaMCP renders Config -> MCP.
func (a *App) ViewDenmaMCP(c echo.Context) error {
	v, err := a.centerView(c, "MCP", "config.mcp")
	if err != nil {
		return err
	}
	view := denmaMCPView{adminView: v, Groups: a.mcpPermGroups(), PerMinute: denmaMCPPerMinute,
		CenterSlug: a.ko.String("denma.center"), MCPURL: strings.TrimSuffix(denmaHub.current().urlCfg.RootURL, "/") + denmaMCPPath}
	if view.Keys, err = a.mcpKeys(c, 0); err != nil {
		return err
	}
	if view.Users, err = a.mcpUserOptions(c); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	view.Mine = a.mcpGivable(auth.GetUser(c))
	if err := a.db.Get(&view.Connector, `SELECT EXISTS (SELECT 1 FROM denma.mcp_connector)`); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.Render(http.StatusOK, "admin-denma-mcp", view)
}

// mcpGivable is the permissions me can give a key: those they have ("*" for
// a superadmin), as with roles (cmd/denma_hierarchy.go).
func (a *App) mcpGivable(me auth.User) []string {
	if !denmaBound(me) {
		return []string{"*"}
	}
	out := []string{denmaMCPListGet, denmaMCPListManage}
	for _, g := range a.mcpPermGroups() {
		for _, p := range g.Permissions {
			if p != denmaMCPListGet && p != denmaMCPListManage && me.HasPerm(p) {
				out = append(out, p)
			}
		}
	}
	return out
}

// DenmaGetMCPKeys lists the center's keys.
func (a *App) DenmaGetMCPKeys(c echo.Context) error {
	out, err := a.mcpKeys(c, 0)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, okResp{out})
}

// denmaMCPKeyForm is a key as Config -> MCP sends it.
type denmaMCPKeyForm struct {
	Name         string   `json:"name"`
	UserID       int      `json:"user_id"` // when it's made
	Permissions  []string `json:"permissions"`
	PersonalData bool     `json:"personal_data"`
	Connector    bool     `json:"connector"`
	ExpiresOn    string   `json:"expires_on"` // YYYY-MM-DD (UTC), or "" for never

	expiresAt *time.Time
}

// checkMCPKeyForm trims and checks a key's settings.
func (a *App) checkMCPKeyForm(c echo.Context) (denmaMCPKeyForm, error) {
	var f denmaMCPKeyForm
	if err := c.Bind(&f); err != nil {
		return f, echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	bad := func(msg string) error { return echo.NewHTTPError(http.StatusBadRequest, msg) }
	if f.Name = strings.TrimSpace(f.Name); !strHasLen(f.Name, 1, 200) {
		return f, bad("Give the key a name, such as whose Claude uses it.")
	}
	known := map[string]bool{}
	for _, g := range a.mcpPermGroups() {
		for _, p := range g.Permissions {
			known[p] = true
		}
	}
	givable := a.mcpGivable(auth.GetUser(c))
	perms := []string{}
	for _, p := range f.Permissions {
		if !known[p] {
			return f, bad(fmt.Sprintf("A key can't have the permission %q.", p))
		}
		if !denmaHasString(givable, "*") && !denmaHasString(givable, p) {
			return f, errDenmaNotYours
		}
		if !denmaHasString(perms, p) {
			perms = append(perms, p)
		}
	}
	if len(perms) == 0 {
		return f, bad("Give the key at least one permission.")
	}
	f.Permissions = perms
	if f.ExpiresOn = strings.TrimSpace(f.ExpiresOn); f.ExpiresOn != "" {
		d, err := time.Parse("2006-01-02", f.ExpiresOn)
		if err != nil {
			return f, bad("The expiry date isn't a date.")
		}
		t := d.Add(24*time.Hour - time.Second)
		if !t.After(time.Now()) {
			return f, bad("The expiry date has to be in the future.")
		}
		f.expiresAt = &t
	}
	return f, nil
}

// mcpConnectorTaken is a connector error, if the user already has a key
// usable from claude.ai (but key id) in the center.
func (a *App) mcpConnectorTaken(cid, userID, id int) error {
	var taken bool
	if err := a.db.Get(&taken, `SELECT EXISTS (SELECT 1 FROM denma.mcp_keys WHERE center_id = $1 AND user_id = $2 AND connector AND id <> $3)`,
		cid, userID, id); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if taken {
		return echo.NewHTTPError(http.StatusBadRequest, "The user already has a key usable from claude.ai here: untick it on that one first.")
	}
	return nil
}

// DenmaCreateMCPKey makes a key for a user; it's shown this once.
func (a *App) DenmaCreateMCPKey(c echo.Context) error {
	cid, err := a.mcpCenterID()
	if err != nil {
		return err
	}
	f, err := a.checkMCPKeyForm(c)
	if err != nil {
		return err
	}
	u, err := a.core.GetUser(f.UserID, "", "")
	if err != nil || u.Status != auth.UserStatusEnabled {
		return echo.NewHTTPError(http.StatusBadRequest, "Choose an enabled user for the key.")
	}
	hub, err := a.denmaHubAccounts()
	if err != nil {
		return err
	}
	if !a.mcpMayKey(auth.GetUser(c), u, hub) {
		return errDenmaNotYours
	}
	var n int
	if err := a.db.Get(&n, `SELECT COUNT(*) FROM denma.mcp_keys WHERE center_id = $1`, cid); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if n >= denmaMCPMaxKeys {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("A center can have at most %d MCP keys.", denmaMCPMaxKeys))
	}
	if f.Connector {
		if err := a.mcpConnectorTaken(cid, u.ID, 0); err != nil {
			return err
		}
	}
	key, hash, hint, err := denmaNewMCPSecret(denmaMCPKeyPrefix)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	var id int
	if err := a.db.Get(&id, `INSERT INTO denma.mcp_keys (center_id, user_id, name, key_hash, key_hint, permissions, personal_data, connector,
		expires_at, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
		cid, u.ID, f.Name, hash, hint, pq.Array(f.Permissions), f.PersonalData, f.Connector, f.expiresAt, denmaAPIWho(c)); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	keys, err := a.mcpKeys(c, id)
	if err != nil || len(keys) == 0 {
		return echo.NewHTTPError(http.StatusInternalServerError, "The key was made but couldn't be read back.")
	}
	keys[0].Key = key
	return c.JSON(http.StatusOK, okResp{keys[0]})
}

// DenmaUpdateMCPKey changes a key's name, permissions, whether it shows
// personal data, whether it's usable from claude.ai, or its expiry date. Its
// user stays the same.
func (a *App) DenmaUpdateMCPKey(c echo.Context) error {
	k, err := a.mcpKey(c)
	if err != nil {
		return err
	}
	f, err := a.checkMCPKeyForm(c)
	if err != nil {
		return err
	}
	if f.Connector {
		if err := a.mcpConnectorTaken(k.CenterID, k.UserID, k.ID); err != nil {
			return err
		}
	}
	if _, err := a.db.Exec(`UPDATE denma.mcp_keys SET name = $2, permissions = $3, personal_data = $4, connector = $5, expires_at = $6,
		updated_at = NOW() WHERE id = $1`, k.ID, f.Name, pq.Array(f.Permissions), f.PersonalData, f.Connector, f.expiresAt); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	keys, err := a.mcpKeys(c, k.ID)
	if err != nil || len(keys) == 0 {
		return echo.NewHTTPError(http.StatusInternalServerError, "The key was saved but couldn't be read back.")
	}
	return c.JSON(http.StatusOK, okResp{keys[0]})
}

// DenmaNewMCPKey replaces a key; the old one stops working at once.
func (a *App) DenmaNewMCPKey(c echo.Context) error {
	k, err := a.mcpKey(c)
	if err != nil {
		return err
	}
	key, hash, hint, err := denmaNewMCPSecret(denmaMCPKeyPrefix)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if _, err := a.db.Exec(`UPDATE denma.mcp_keys SET key_hash = $2, key_hint = $3, updated_at = NOW() WHERE id = $1`, k.ID, hash, hint); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	keys, err := a.mcpKeys(c, k.ID)
	if err != nil || len(keys) == 0 {
		return echo.NewHTTPError(http.StatusInternalServerError, "The key was replaced but couldn't be read back.")
	}
	keys[0].Key = key
	return c.JSON(http.StatusOK, okResp{keys[0]})
}

// DenmaDeleteMCPKey deletes a key; it stops working at once.
func (a *App) DenmaDeleteMCPKey(c echo.Context) error {
	k, err := a.mcpKey(c)
	if err != nil {
		return err
	}
	if _, err := a.db.Exec(`DELETE FROM denma.mcp_keys WHERE id = $1`, k.ID); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{true})
}

// denmaNewMCPSecret makes a key or secret with a prefix, returning it, its
// hash and its hint.
func denmaNewMCPSecret(prefix string) (string, string, string, error) {
	r, err := utils.GenerateRandomString(40)
	if err != nil {
		return "", "", "", err
	}
	s := prefix + r
	return s, denmaAPIHash(s), s[len(s)-4:], nil
}

// ---- The hub's MCP page: the connector secret, and every center's keys ----

// denmaMCPHubKey is a key on the hub's MCP page.
type denmaMCPHubKey struct {
	Center       string         `db:"center" json:"center"`
	Slug         string         `db:"slug" json:"slug"`
	Name         string         `db:"name" json:"name"`
	UserID       int            `db:"user_id" json:"user_id"`
	User         string         `db:"-" json:"user"` // their username in the center
	Permissions  pq.StringArray `db:"permissions" json:"permissions"`
	PersonalData bool           `db:"personal_data" json:"personal_data"`
	Connector    bool           `db:"connector" json:"connector"`
	ExpiresAt    *time.Time     `db:"expires_at" json:"expires_at"`
	Uses         int            `db:"uses" json:"uses"`
	LastUsedAt   *time.Time     `db:"last_used_at" json:"last_used_at"`
	CreatedBy    string         `db:"created_by" json:"created_by"`
}

// denmaMCPConnector is the connector secret, without it.
type denmaMCPConnector struct {
	SecretHint  string         `db:"secret_hint" json:"secret_hint"`
	CreatedBy   string         `db:"created_by" json:"created_by"`
	CreatedAt   time.Time      `db:"created_at" json:"created_at"`
	Uses        int            `db:"uses" json:"uses"`
	LastUsedAt  *time.Time     `db:"last_used_at" json:"last_used_at"`
	AllowedFrom pq.StringArray `db:"allowed_from" json:"allowed_from"` // where it can be used from; none: anywhere
	Secret      string         `db:"-" json:"secret,omitempty"`        // only when it's made
}

// denmaHubMCPView is the hub's MCP page.
type denmaHubMCPView struct {
	adminView
	Connector *denmaMCPConnector
	Keys      []denmaMCPHubKey
	MCPURL    string
}

// ViewDenmaHubMCP renders the hub's MCP page, for superadmins.
func (a *App) ViewDenmaHubMCP(c echo.Context) error {
	d, err := a.hub(c)
	if err != nil {
		return err
	}
	view := denmaHubMCPView{adminView: newAdminView(c, "MCP", "", "denma.mcp"), Keys: []denmaMCPHubKey{},
		MCPURL: strings.TrimSuffix(a.urlCfg.RootURL, "/") + denmaMCPPath}
	var cn denmaMCPConnector
	switch err := d.base.db.Get(&cn, `SELECT secret_hint, created_by, created_at, uses, last_used_at, allowed_from FROM denma.mcp_connector`); {
	case err == nil:
		view.Connector = &cn
	case !errors.Is(err, sql.ErrNoRows):
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if err := d.base.db.Select(&view.Keys, `SELECT c.name AS center, c.slug, k.name, k.user_id, k.permissions, k.personal_data, k.connector,
		k.expires_at, k.uses, k.last_used_at, k.created_by
		FROM denma.mcp_keys k JOIN denma.centers c ON c.id = k.center_id ORDER BY c.name, k.name`); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	for i, k := range view.Keys {
		if ctr := d.get(k.Slug); ctr != nil {
			if u, err := ctr.app.core.GetUser(k.UserID, "", ""); err == nil {
				view.Keys[i].User = u.Username
			}
		}
	}
	return c.Render(http.StatusOK, "admin-denma-hub-mcp", view)
}

// DenmaNewMCPConnector makes (or replaces) the connector secret, shown once.
func (a *App) DenmaNewMCPConnector(c echo.Context) error {
	d, err := a.hub(c)
	if err != nil {
		return err
	}
	secret, hash, hint, err := denmaNewMCPSecret(denmaMCPSecretPrefix)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	// A new secret keeps where the old one could be used from.
	var cn denmaMCPConnector
	if err := d.base.db.Get(&cn, `INSERT INTO denma.mcp_connector (id, secret_hash, secret_hint, created_by) VALUES (1, $1, $2, $3)
		ON CONFLICT (id) DO UPDATE SET secret_hash = $1, secret_hint = $2, created_by = $3, created_at = NOW(), uses = 0, last_used_at = NULL
		RETURNING secret_hint, created_by, created_at, uses, last_used_at, allowed_from`, hash, hint, denmaAPIWho(c)); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	cn.Secret = secret
	return c.JSON(http.StatusOK, okResp{cn})
}

// DenmaUpdateMCPConnector sets where the connector secret can be used from
// (addresses, networks or host names; none: anywhere).
func (a *App) DenmaUpdateMCPConnector(c echo.Context) error {
	d, err := a.hub(c)
	if err != nil {
		return err
	}
	var f struct {
		AllowedFrom []string `json:"allowed_from"`
	}
	if err := c.Bind(&f); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	from := []string{}
	for _, s := range f.AllowedFrom {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if err := denmaCheckAllowedFrom(s); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		from = append(from, s)
	}
	if len(from) > 50 {
		return echo.NewHTTPError(http.StatusBadRequest, "At most 50 addresses.")
	}
	var cn denmaMCPConnector
	if err := d.base.db.Get(&cn, `UPDATE denma.mcp_connector SET allowed_from = $1
		RETURNING secret_hint, created_by, created_at, uses, last_used_at, allowed_from`, pq.Array(from)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusBadRequest, "Make the connector secret first.")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{cn})
}

// DenmaDeleteMCPConnector deletes the connector secret: claude.ai's
// connector stops working, keys don't.
func (a *App) DenmaDeleteMCPConnector(c echo.Context) error {
	d, err := a.hub(c)
	if err != nil {
		return err
	}
	if _, err := d.base.db.Exec(`DELETE FROM denma.mcp_connector`); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{true})
}
