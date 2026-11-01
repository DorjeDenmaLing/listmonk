package main

// denma: there are no listmonk API users; other systems use the sending API or sign-up webhooks.

import (
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/listmonk/internal/auth"
)

// denmaDeleteAPIUsersSQL deletes the schema's API users, returning their usernames.
const denmaDeleteAPIUsersSQL = `DELETE FROM users WHERE type = 'api' RETURNING username`

// denmaDeleteAPIUsers deletes the API users in q's schema (the hub or a center), logging their usernames.
func denmaDeleteAPIUsers(q sqlx.Queryer, where string) error {
	var gone []string
	if err := sqlx.Select(q, &gone, denmaDeleteAPIUsersSQL); err != nil {
		return err
	}
	if len(gone) > 0 {
		lo.Printf("denma: %s: deleted its API users (%s): there are none now; other systems use the sending API or sign-up webhooks",
			where, strings.Join(gone, ", "))
	}
	return nil
}

// denmaLegacyAPIUser ignores the TOML's legacy admin_username and admin_password.
func denmaLegacyAPIUser(auth.User) {
	lo.Println("denma: app.admin_username and app.admin_password are ignored: there are no API users")
}

// denmaInstallAPIUser is false: an install makes no API user.
func denmaInstallAPIUser() bool {
	lo.Println("denma: LISTMONK_ADMIN_API_USER is ignored: there are no API users")
	return false
}
