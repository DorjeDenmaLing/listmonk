package main

// denma: who can manage whom among a center's users: non-superadmins see and give no more than they have.

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
)

// denmaBound reports whether u is held to the hierarchy: everyone but superadmins.
func denmaBound(u auth.User) bool {
	return u.UserRole.ID != auth.SuperAdminRoleID
}

// denmaInCenter reports whether a is a center's app (not the hub's, nor a single install's).
func (a *App) denmaInCenter() bool {
	return denmaHub != nil && a.ko.String("denma.center") != ""
}

// denmaHidesSuper reports whether c's user doesn't see the Super Admin role and its users.
func (a *App) denmaHidesSuper(c echo.Context) bool {
	return a.denmaInCenter() || denmaBound(auth.GetUser(c))
}

// ownAdmins keeps a center's Super Admins the hub's; others with the role become Center Admins.
func (d *denmaCenters) ownAdmins(c *denmaCenter, db *sqlx.DB) error {
	if _, err := d.hubAccount(c.ID, db, 0, "hub", "hub@hub.invalid", "hub@hub.invalid", d.base.ko.String("app.site_name")); err != nil {
		return fmt.Errorf("creating the hub's account: %v", err)
	}
	var names []string
	if err := db.Select(&names, `UPDATE users SET user_role_id = (SELECT id FROM roles WHERE name = $2 AND type = 'user'), updated_at = NOW()
		WHERE user_role_id = $3 AND id NOT IN (SELECT center_user_id FROM denma.center_superadmins WHERE center_id = $1)
		RETURNING username`, c.ID, denmaCenterAdminRole, auth.SuperAdminRoleID); err != nil {
		return fmt.Errorf("making the center's Super Admins %ss: %v", denmaCenterAdminRole, err)
	}
	if len(names) > 0 {
		lo.Printf("denma: center %s: %s now %s (centers have no Super Admins of their own)", c.Slug, strings.Join(names, ", "), denmaCenterAdminRole)
	}
	return nil
}

// denmaHasPerms reports whether u has every one of perms.
func denmaHasPerms(u auth.User, perms []string) bool {
	for _, p := range perms {
		if !u.HasPerm(p) {
			return false
		}
	}
	return true
}

// denmaHasLists reports whether u has every list permission given on lists.
func denmaHasLists(u auth.User, lists []auth.ListPermission) bool {
	for _, l := range lists {
		for _, p := range l.Permissions {
			t := auth.PermTypeGet
			if p == auth.PermListManage {
				t = auth.PermTypeManage
			}
			if u.HasListPerm(t, l.ID) != nil {
				return false
			}
		}
	}
	return true
}

// denmaWithin reports whether a user has no more than u.
func denmaWithin(u, target auth.User) bool {
	if !denmaBound(u) {
		return true
	}
	if target.UserRole.ID == auth.SuperAdminRoleID || !denmaHasPerms(u, target.UserRole.Permissions) {
		return false
	}
	return target.ListRole == nil || denmaHasLists(u, target.ListRole.Lists)
}

var (
	errDenmaNotYours = echo.NewHTTPError(http.StatusForbidden,
		"You can only give out permissions and lists you have yourself, and only manage users and roles with no more than you have.")
	errDenmaNoSuper = echo.NewHTTPError(http.StatusForbidden,
		"Centers have no Super Admins: their admins are Center Admins.")
	errDenmaAPIUser = echo.NewHTTPError(http.StatusForbidden,
		"There are no API users. Other systems use a center's sending API (Config -> API) or its sign-up webhooks (Config -> Webhooks).")
	errDenmaNoUser = echo.NewHTTPError(http.StatusNotFound, "user not found")
	errDenmaNoRole = echo.NewHTTPError(http.StatusNotFound, "role not found")
)

// denmaHubAccounts returns the IDs of the hub's superadmins' accounts in this center.
func (a *App) denmaHubAccounts() (map[int]bool, error) {
	slug := a.ko.String("denma.center")
	if denmaHub == nil || slug == "" {
		return nil, nil
	}
	var ids []int
	if err := a.db.Select(&ids, `SELECT s.center_user_id FROM denma.center_superadmins s
		JOIN denma.centers c ON c.id = s.center_id WHERE c.slug = $1`, slug); err != nil {
		return nil, denmaInternal(err)
	}
	out := make(map[int]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// denmaVisibleUsers drops the users c's user doesn't see.
func (a *App) denmaVisibleUsers(c echo.Context, users []auth.User) ([]auth.User, error) {
	hub, err := a.denmaHubAccounts()
	if err != nil {
		return nil, err
	}
	hide := a.denmaHidesSuper(c)
	out := users[:0]
	for _, x := range users {
		if !hub[x.ID] && !(hide && x.UserRole.ID == auth.SuperAdminRoleID) {
			a.denmaHideTwofa(c, &x)
			out = append(out, x)
		}
	}
	return out, nil
}

// denmaSeeUser is "not found" for a user c's user doesn't see.
func (a *App) denmaSeeUser(c echo.Context, target *auth.User) error {
	hub, err := a.denmaHubAccounts()
	if err != nil {
		return err
	}
	if hub[target.ID] || (a.denmaHidesSuper(c) && target.UserRole.ID == auth.SuperAdminRoleID) {
		return errDenmaNoUser
	}
	a.denmaHideTwofa(c, target)
	return nil
}

// denmaHideTwofa leaves out whether a center's user has two-factor sign-in, except for c's own.
func (a *App) denmaHideTwofa(c echo.Context, u *auth.User) {
	if a.denmaInCenter() && u.ID != auth.GetUser(c).ID {
		u.TwofaType = models.TwofaTypeNone
	}
}

// denmaCheckUser checks that c's user may edit or delete the user with this ID.
func (a *App) denmaCheckUser(c echo.Context, id int) error {
	target, err := a.core.GetUser(id, "", "")
	if err != nil {
		return err
	}
	if err := a.denmaSeeUser(c, &target); err != nil {
		return err
	}
	if !denmaWithin(auth.GetUser(c), target) {
		return errDenmaNotYours
	}
	return nil
}

// denmaCheckAssign checks that c's user may give a user of a type this role and list role (nil for none).
func (a *App) denmaCheckAssign(c echo.Context, typ string, roleID int, listRoleID *int) error {
	if typ == auth.UserTypeAPI {
		return errDenmaAPIUser
	}
	if roleID == auth.SuperAdminRoleID && a.denmaInCenter() {
		return errDenmaNoSuper
	}
	u := auth.GetUser(c)
	if !denmaBound(u) {
		return nil
	}
	if roleID == auth.SuperAdminRoleID {
		return errDenmaNotYours
	}
	role, err := a.core.GetRole(roleID)
	if err != nil {
		return err
	}
	if !denmaHasPerms(u, role.Permissions) {
		return errDenmaNotYours
	}
	if listRoleID != nil && *listRoleID > 0 {
		lr, err := a.denmaListRole(*listRoleID)
		if err != nil {
			return err
		}
		if !denmaHasLists(u, lr.Lists) {
			return errDenmaNotYours
		}
	}
	return nil
}

// denmaListRole returns a list role by ID (listmonk has no getter for one).
func (a *App) denmaListRole(id int) (auth.ListRole, error) {
	roles, err := a.core.GetListRoles()
	if err != nil {
		return auth.ListRole{}, err
	}
	for _, r := range roles {
		if r.ID == id {
			return r, nil
		}
	}
	return auth.ListRole{}, errDenmaNoRole
}

// denmaAssignable keeps the roles c's user may give out, and the ones the user being edited has now.
func (a *App) denmaAssignable(c echo.Context, roles []auth.Role, lists []auth.ListRole, cur int, curList *int) ([]auth.Role, []auth.ListRole) {
	if !a.denmaHidesSuper(c) {
		return roles, lists
	}
	u := auth.GetUser(c)
	bound := denmaBound(u)
	outR := []auth.Role{}
	for _, r := range roles {
		if r.ID != auth.SuperAdminRoleID && (!bound || r.ID == cur || denmaHasPerms(u, r.Permissions)) {
			outR = append(outR, r)
		}
	}
	outL := []auth.ListRole{}
	for _, r := range lists {
		if !bound || (curList != nil && r.ID == *curList) || denmaHasLists(u, r.Lists) {
			outL = append(outL, r)
		}
	}
	return outR, outL
}

// denmaVisibleRoles drops the Super Admin role (denmaHidesSuper).
func (a *App) denmaVisibleRoles(c echo.Context, roles []auth.Role) []auth.Role {
	if !a.denmaHidesSuper(c) {
		return roles
	}
	out := roles[:0]
	for _, r := range roles {
		if r.ID != auth.SuperAdminRoleID {
			out = append(out, r)
		}
	}
	return out
}

// denmaCheckRole checks that c's user may save a user role with these permissions (id 0 for new).
func (a *App) denmaCheckRole(c echo.Context, id int, perms []string) error {
	u := auth.GetUser(c)
	if !denmaBound(u) {
		return nil
	}
	if !denmaHasPerms(u, perms) {
		return errDenmaNotYours
	}
	if id > 0 {
		cur, err := a.core.GetRole(id)
		if err != nil {
			return err
		}
		if !denmaHasPerms(u, cur.Permissions) {
			return errDenmaNotYours
		}
	}
	return nil
}

// denmaCheckListRole is denmaCheckRole for a list role and its lists.
func (a *App) denmaCheckListRole(c echo.Context, id int, lists []auth.ListPermission) error {
	u := auth.GetUser(c)
	if !denmaBound(u) {
		return nil
	}
	if !denmaHasLists(u, lists) {
		return errDenmaNotYours
	}
	if id > 0 {
		cur, err := a.denmaListRole(id)
		if err != nil {
			return err
		}
		if !denmaHasLists(u, cur.Lists) {
			return errDenmaNotYours
		}
	}
	return nil
}

// denmaCheckDeleteRole checks that c's user may delete the role with this ID (user or list role).
func (a *App) denmaCheckDeleteRole(c echo.Context, id int) error {
	if denmaBound(auth.GetUser(c)) {
		var err error
		if _, lerr := a.denmaListRole(id); lerr == nil {
			err = a.denmaCheckListRole(c, id, nil)
		} else {
			err = a.denmaCheckRole(c, id, nil)
		}
		if err != nil {
			return err
		}
	}
	var held int
	if err := a.db.Get(&held, `SELECT COUNT(*) FROM users WHERE list_role_id = $1`, id); err != nil {
		return denmaInternal(err)
	}
	if held > 0 {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf(
			"%d users have this list role. Give them another, or none, before deleting it.", held))
	}
	return nil
}

// denmaRoleForm fits the user role form to c's user: read-only if the role has more than they do.
func denmaRoleForm(c echo.Context, groups []permGroup, role auth.Role, isNew bool) ([]permGroup, bool) {
	u := auth.GetUser(c)
	if !denmaBound(u) {
		return groups, false
	}
	if !isNew && !denmaHasPerms(u, role.Permissions) {
		return groups, true
	}
	out := make([]permGroup, 0, len(groups))
	for _, g := range groups {
		var ps []string
		for _, p := range g.Permissions {
			if u.HasPerm(p) {
				ps = append(ps, p)
			}
		}
		if len(ps) > 0 {
			out = append(out, permGroup{Group: g.Group, Permissions: ps})
		}
	}
	return out, false
}

// denmaListRoleForm fits the list role form to c's user: read-only if it has lists they can't reach.
func denmaListRoleForm(c echo.Context, lists []models.List, role auth.ListRole, isNew bool) ([]models.List, bool) {
	u := auth.GetUser(c)
	if !denmaBound(u) {
		return lists, false
	}
	if !isNew && !denmaHasLists(u, role.Lists) {
		return lists, true
	}
	out := make([]models.List, 0, len(lists))
	for _, l := range lists {
		if u.HasListPerm(auth.PermTypeGet, l.ID) == nil {
			out = append(out, l)
		}
	}
	return out, false
}
