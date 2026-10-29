package main

import (
	"net/http"
	"testing"

	"github.com/knadh/listmonk/internal/auth"
)

// Only a hub superadmin without two-factor sign-in is asked, on a page they open.
func TestDenmaTwofaPromptDue(t *testing.T) {
	super := auth.User{Type: auth.UserTypeUser, PasswordLogin: true}
	super.UserRole.ID = auth.SuperAdminRoleID
	withTwofa := super
	withTwofa.TwofaType = "totp"
	notSuper := super
	notSuper.UserRole.ID = auth.SuperAdminRoleID + 1
	noPassword := super
	noPassword.PasswordLogin = false

	const html = "text/html,application/xhtml+xml"
	for name, c := range map[string]struct {
		u                 auth.User
		method, path, acc string
		later, want       bool
	}{
		"a page":           {super, http.MethodGet, "/admin", html, false, true},
		"another page":     {super, http.MethodGet, "/admin/campaigns", html, false, true},
		"later":            {super, http.MethodGet, "/admin", html, true, false},
		"two-factor on":    {withTwofa, http.MethodGet, "/admin", html, false, false},
		"not a superadmin": {notSuper, http.MethodGet, "/admin", html, false, false},
		"no password":      {noPassword, http.MethodGet, "/admin", html, false, false},
		"the profile":      {super, http.MethodGet, "/admin/user/profile", html, false, false},
		"the prompt":       {super, http.MethodGet, "/admin/denma-twofa", html, false, false},
		"a script":         {super, http.MethodGet, "/admin/custom.js", "*/*", false, false},
		"a post":           {super, http.MethodPost, "/admin/subscribers", html, false, false},
	} {
		if got := denmaTwofaPromptDue(c.u, c.method, c.path, c.acc, c.later); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}
