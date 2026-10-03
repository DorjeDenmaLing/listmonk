package main

// denma: who can manage whom, among a center's users (and the hub's). listmonk
// lets anyone with users:manage or roles:manage give anyone any role, so a
// Center Admin could make themselves Super Admin, or give their role the
// settings permissions superadmins keep. Here, everyone but superadmins (the
// Super Admin role, 1):
//
//   - doesn't see Super Admin users or the Super Admin role;
//   - can only give out the permissions they have, and the lists they can get
//     or manage, in roles and list roles they create or edit, and to users;
//   - can only edit or delete users, roles and list roles with no more than
//     they have themselves (so their peers, on the same role, and those below).
//
// In a center, nobody sees the hub's superadmins' own accounts there (made
// when they open it, cmd/denma_hub.go): they aren't the center's users.
// Centers have their own admins (Center Admins) and no Super Admins of their
// own: the role is only the hub's accounts', and nobody sees it or gives it
// out there (ownAdmins).
//
// Called from listmonk's users and roles handlers (cmd/users.go,
// cmd/roles.go), at marked lines; the forms offer only what may be given
// (views/user.html, views/user-role.html, views/list-role.html).

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
)

// denmaBound reports whether u is held to the hierarchy: everyone but
// superadmins. (A loaded user's role is in UserRole.ID; listmonk zeroes
// UserRoleID.)
func denmaBound(u auth.User) bool {
	return u.UserRole.ID != auth.SuperAdminRoleID
}

// denmaInCenter reports whether a is a center's app (not the hub's, nor a
// single install's).
func (a *App) denmaInCenter() bool {
	return denmaHub != nil && a.ko.String("denma.center") != ""
}

// denmaHidesSuper reports whether c's user doesn't see the Super Admin role
// and its users: everyone in a center, and those who aren't superadmins.
func (a *App) denmaHidesSuper(c echo.Context) bool {
	return a.denmaInCenter() || denmaBound(auth.GetUser(c))
}

// ownAdmins keeps a center's Super Admins the hub's. The hub has an account
// of its own in each center (hub_user_id 0 in denma.center_superadmins,
// hidden like the superadmins'), as listmonk won't change or delete users
// unless an enabled Super Admin would remain, and a center starts with only
// its Center Admin. Anyone else with the role, such as an existing install's
// admins (DDL's), becomes a Center Admin. On every load (adopt).
func (d *denmaCenters) ownAdmins(c *denmaCenter, db *sqlx.DB) error {
	if _, err := d.hubAccount(c.ID, db, 0, "hub", "hub@hub.invalid", "hub@hub.invalid", "Listmonk Shambhala"); err != nil {
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

// denmaHasLists reports whether u can get every list given list:get on, and
// manage every list given list:manage on.
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

// denmaWithin reports whether a user has no more than u: not a superadmin
// (unless u is one), and with no permissions or lists u doesn't have.
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
	errDenmaNoUser = echo.NewHTTPError(http.StatusNotFound, "user not found")
	errDenmaNoRole = echo.NewHTTPError(http.StatusNotFound, "role not found")
)

// denmaHubAccounts returns the IDs of the hub's superadmins' accounts in this
// center (none in the hub, or without multi-center).
func (a *App) denmaHubAccounts() (map[int]bool, error) {
	slug := a.ko.String("denma.center")
	if denmaHub == nil || slug == "" {
		return nil, nil
	}
	var ids []int
	if err := a.db.Select(&ids, `SELECT s.center_user_id FROM denma.center_superadmins s
		JOIN denma.centers c ON c.id = s.center_id WHERE c.slug = $1`, slug); err != nil {
		return nil, echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	out := make(map[int]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// denmaVisibleUsers drops the users c's user doesn't see: the hub's accounts,
// and Super Admins (denmaHidesSuper).
func (a *App) denmaVisibleUsers(c echo.Context, users []auth.User) ([]auth.User, error) {
	hub, err := a.denmaHubAccounts()
	if err != nil {
		return nil, err
	}
	hide := a.denmaHidesSuper(c)
	out := users[:0]
	for _, x := range users {
		if !hub[x.ID] && !(hide && x.UserRole.ID == auth.SuperAdminRoleID) {
			out = append(out, x)
		}
	}
	return out, nil
}

// denmaSeeUser is "not found" for a user c's user doesn't see.
func (a *App) denmaSeeUser(c echo.Context, target auth.User) error {
	hub, err := a.denmaHubAccounts()
	if err != nil {
		return err
	}
	if hub[target.ID] || (a.denmaHidesSuper(c) && target.UserRole.ID == auth.SuperAdminRoleID) {
		return errDenmaNoUser
	}
	return nil
}

// denmaCheckUser checks that c's user may edit or delete the user with this
// ID: one they see, with no more than they have.
func (a *App) denmaCheckUser(c echo.Context, id int) error {
	target, err := a.core.GetUser(id, "", "")
	if err != nil {
		return err
	}
	if err := a.denmaSeeUser(c, target); err != nil {
		return err
	}
	if !denmaWithin(auth.GetUser(c), target) {
		return errDenmaNotYours
	}
	return nil
}

// denmaCheckAssign checks that c's user may give a user this role and list
// role (nil for none): ones with no more than they have, and never Super
// Admin in a center.
func (a *App) denmaCheckAssign(c echo.Context, roleID int, listRoleID *int) error {
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

// denmaAssignable keeps the roles c's user may give out (for the user form's
// selectors), and the ones the user being edited has now (cur, curList), so
// that their form shows them. Never Super Admin, unless a superadmin gives it
// in the hub.
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

// denmaCheckRole checks that c's user may save a user role with these
// permissions, replacing the role with this ID (0 for a new one): both with
// no more than they have.
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

// denmaCheckDeleteRole checks that c's user may delete the role with this ID,
// a user or list role: one with no more than they have.
func (a *App) denmaCheckDeleteRole(c echo.Context, id int) error {
	if !denmaBound(auth.GetUser(c)) {
		return nil
	}
	if _, err := a.denmaListRole(id); err == nil {
		return a.denmaCheckListRole(c, id, nil)
	}
	return a.denmaCheckRole(c, id, nil)
}

// denmaRoleForm fits the user role form to c's user: a role with more than
// they have is read-only (locked), and otherwise only their own permissions
// are offered.
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

// denmaListRoleForm fits the list role form to c's user: a list role with
// lists they can't get or manage is read-only (locked), and otherwise only
// the lists they can get are offered.
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
