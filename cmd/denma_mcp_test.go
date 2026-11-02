package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
	null "gopkg.in/volatiletech/null.v6"
)

// mcpTestUser is a user with a role's permissions and a list role.
func mcpTestUser(role int, perms []string, lists []auth.ListPermission) auth.User {
	u := auth.User{}
	u.ID = 7
	u.UserRole.ID = role
	u.UserRole.Permissions = perms
	u.PermissionsMap = map[string]struct{}{}
	for _, p := range perms {
		u.PermissionsMap[p] = struct{}{}
	}
	if lists != nil {
		u.ListRole = &auth.ListRolePermissions{ID: 9, Lists: lists}
	}
	return u
}

func TestDenmaMCPUser(t *testing.T) {
	known := map[string]struct{}{}
	for _, p := range []string{"lists:get_all", "lists:manage_all", "campaigns:get_all", "campaigns:send", "subscribers:sql_query"} {
		known[p] = struct{}{}
	}

	// Only what both the key and the role give.
	u := mcpTestUser(5, []string{"lists:get_all", "campaigns:get_all"}, nil)
	eff := denmaMCPUser(u, []string{"lists:get_all", "lists:manage_all", "campaigns:send"}, known)
	if got := strings.Join(eff.UserRole.Permissions, ","); got != "lists:get_all" {
		t.Fatalf("permissions %q, want lists:get_all", got)
	}
	if eff.HasPerm("lists:manage_all") || eff.HasPerm("campaigns:get_all") || !eff.HasPerm("lists:get_all") {
		t.Fatalf("HasPerm doesn't follow the key and the role: %v", eff.PermissionsMap)
	}

	// A superadmin's account: what the key gives, and not a Super Admin.
	s := mcpTestUser(auth.SuperAdminRoleID, nil, nil)
	eff = denmaMCPUser(s, []string{"campaigns:get_all", "subscribers:sql_query", "nonsense"}, known)
	if eff.UserRole.ID == auth.SuperAdminRoleID {
		t.Fatal("a superadmin's key is a Super Admin's")
	}
	if got := strings.Join(eff.UserRole.Permissions, ","); got != "campaigns:get_all" {
		t.Fatalf("superadmin's key permissions %q, want campaigns:get_all", got)
	}
	if !denmaBound(eff) {
		t.Fatal("a superadmin's key isn't bound by the hierarchy")
	}

	// Lists: the list role's, as far as the key's list:get / list:manage go.
	l := mcpTestUser(5, nil, []auth.ListPermission{
		{ID: 1, Permissions: pq.StringArray{"list:get", "list:manage"}},
		{ID: 2, Permissions: pq.StringArray{"list:manage"}},
	})
	eff = denmaMCPUser(l, []string{"list:get"}, known)
	if eff.HasListPerm(auth.PermTypeGet, 1) != nil {
		t.Fatal("list:get on list 1 lost")
	}
	if eff.HasListPerm(auth.PermTypeManage, 1) == nil || eff.HasListPerm(auth.PermTypeGet, 2) == nil {
		t.Fatal("list permissions beyond the key's kept")
	}
	if len(eff.GetListIDs) != 1 || len(eff.ManageListIDs) != 0 || len(eff.ListRole.Lists) != 1 {
		t.Fatalf("list IDs get %v manage %v lists %v", eff.GetListIDs, eff.ManageListIDs, eff.ListRole.Lists)
	}
	if len(l.ListRole.Lists) != 2 {
		t.Fatal("the user's own list role was changed")
	}
	eff = denmaMCPUser(l, []string{"lists:get_all"}, known)
	if len(eff.ListPermissionsMap) != 0 || eff.HasListPerm(auth.PermTypeGet, 1) == nil {
		t.Fatal("a key without list:get kept the list role's lists")
	}
}

func TestDenmaMCPAllowed(t *testing.T) {
	for _, p := range []string{
		"/api/denma/center/api-keys", "/api/denma/center/api-keys/3/key", "/api/denma/center/mcp-keys",
		"/api/denma/center/signup-hooks/1/key", "/api/denma/center/api/resume", "/api/settings", "/api/settings/smtp/test",
		"/api/admin/reload", "/api/logs", "/api/about", "/api/events", "/api/profile", "/api/logout", "/api/users/4/twofa",
		"/api/users/4/twofa/totp", "/api/denma/audit", "/api/subscribers/12/optin", "/api/denma/subscribers/resubscribe",
		"/api/users", "/api/users/4", "/api/roles/users", "/api/roles/4", "/api/maintenance/subscribers/blocklisted",
		"/api/maintenance/analytics/views/export",
	} {
		for _, m := range []string{"GET", "POST"} {
			if denmaMCPAllowed(p, m, true) == "" {
				t.Errorf("%s %s is allowed", m, p)
			}
		}
	}
	// The center's details can be read, not changed.
	for _, p := range []string{"/api/denma/center", "/api/denma/center/"} {
		if denmaMCPAllowed(p, "GET", true) != "" {
			t.Errorf("GET %s is refused", p)
		}
		for _, m := range []string{"POST", "PUT", "DELETE"} {
			if denmaMCPAllowed(p, m, true) == "" {
				t.Errorf("%s %s is allowed", m, p)
			}
		}
	}
	for _, p := range []string{"/api/subscribers/export", "/api/subscribers/12/export"} {
		if denmaMCPAllowed(p, "GET", false) == "" {
			t.Errorf("%s is allowed without personal data", p)
		}
		if denmaMCPAllowed(p, "GET", true) != "" {
			t.Errorf("%s is refused with personal data", p)
		}
	}
	for _, p := range []string{
		"/api/lists", "/api/subscribers", "/api/campaigns/3/status", "/api/denma/center/apix", "/api/settingsx",
		"/api/subscribers/12", "/api/subscribers/12/bounces", "/api/denma/subscribers", "/api/denma/auditx", "/api/usersx",
	} {
		for _, m := range []string{"GET", "POST", "PUT"} {
			if why := denmaMCPAllowed(p, m, false); why != "" && !strings.Contains(why, "exports") {
				t.Errorf("%s %s is refused: %s", m, p, why)
			}
		}
	}
}

func TestDenmaMCPNoKeyPerms(t *testing.T) {
	for _, p := range []string{"users:get", "users:manage", "roles:get", "roles:manage", "settings:manage", "subscribers:sql_query"} {
		if !denmaMCPNoKeyPerms[p] {
			t.Errorf("a key can have %s", p)
		}
	}
}

func TestDenmaMCPNoTx(t *testing.T) {
	if !denmaMCPNoKeyPerms["tx:send"] {
		t.Error("a key can have tx:send")
	}
	for _, m := range []string{"POST", "GET"} {
		if denmaMCPAllowed("/api/tx", m, true) == "" {
			t.Errorf("%s /api/tx is allowed", m)
		}
	}

	// Not offered on the keys' page.
	raw, _ := json.Marshal([]denmaMCPPermGroup{
		{Group: "campaigns", Permissions: []string{"campaigns:get_all"}},
		{Group: "transactional", Permissions: []string{"tx:send"}},
	})
	a := &App{cfg: &Config{PermissionsRaw: raw}}
	for _, g := range a.mcpPermGroups() {
		for _, p := range g.Permissions {
			if p == "tx:send" {
				t.Errorf("group %s offers tx:send", g.Group)
			}
		}
		if g.Group == "transactional" {
			t.Error("the transactional group is offered, empty")
		}
	}

	// Not effective for a key that has it.
	known := map[string]struct{}{"tx:send": {}, "campaigns:get_all": {}}
	for _, u := range []auth.User{
		mcpTestUser(5, []string{"tx:send", "campaigns:get_all"}, nil),
		mcpTestUser(auth.SuperAdminRoleID, nil, nil),
	} {
		eff := denmaMCPUser(u, []string{"tx:send", "campaigns:get_all"}, known)
		if eff.HasPerm("tx:send") || !eff.HasPerm("campaigns:get_all") {
			t.Errorf("a key's effective permissions %v", eff.PermissionsMap)
		}
	}
}

func TestDenmaMCPTestTo(t *testing.T) {
	a := &App{}
	ctx := func(viaKey bool) echo.Context {
		r := httptest.NewRequest(http.MethodPost, "/api/campaigns/1/test", nil)
		if viaKey {
			r = r.WithContext(context.WithValue(r.Context(), denmaMCPCtxKey{}, denmaMCPReq{KeyID: 1}))
		}
		c := echo.New().NewContext(r, httptest.NewRecorder())
		u := mcpTestUser(5, nil, nil)
		u.Email = null.StringFrom("Me@Example.org")
		c.Set(auth.UserHTTPCtxKey, u)
		return c
	}
	for _, emails := range [][]string{{"me@example.org"}, {"  ME@example.ORG "}} {
		if err := a.denmaMCPTestTo(ctx(true), emails); err != nil {
			t.Errorf("%q through a key: %v", emails, err)
		}
	}
	for _, emails := range [][]string{{"other@example.org"}, {"me@example.org", "other@example.org"}, {"me@example.org", "me@example.org"}, {}, nil, {""}} {
		err := a.denmaMCPTestTo(ctx(true), emails)
		he, ok := err.(*echo.HTTPError)
		if !ok || he.Code != http.StatusBadRequest || !strings.Contains(he.Message.(string), "me@example.org") {
			t.Errorf("%q through a key: %v", emails, err)
		}
	}
	for _, emails := range [][]string{{"other@example.org"}, {"a@example.org", "b@example.org"}, {}, nil} {
		if err := a.denmaMCPTestTo(ctx(false), emails); err != nil {
			t.Errorf("%q not through a key: %v", emails, err)
		}
	}

	// A user with no address can't be sent to.
	c := ctx(true)
	c.Set(auth.UserHTTPCtxKey, mcpTestUser(5, nil, nil))
	if err := a.denmaMCPTestTo(c, []string{""}); err == nil {
		t.Error("a user with no address got a test")
	}
}

func TestDenmaMCPRateOK(t *testing.T) {
	for i := 1; i <= denmaMCPPerMinute+2; i++ {
		if n := denmaMCPRateOK(987654); n != i {
			t.Fatalf("request %d counted as %d", i, n)
		}
	}
}
