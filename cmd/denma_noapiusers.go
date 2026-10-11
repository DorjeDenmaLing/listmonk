package main

// denma: there are no listmonk API users, in the hub or any center. listmonk's
// API users use the admin API with a role's permissions, outside the center's
// consent rules (cmd/denma_optouts.go, cmd/denma_noconsent.go), the sending
// limits and the activity log's redaction. Other systems use what's made for
// them: the sending API (cmd/denma_api.go) to send e-mails and campaigns, and
// sign-up webhooks (cmd/denma_signup.go) to subscribe people.
//
//   - Nobody can make one, or make a user one, superadmins included
//     (denmaCheckAssign, cmd/denma_hierarchy.go; views/user.html doesn't offer
//     the type).
//   - None is cached for signing in (cacheUsers, cmd/users.go), so an API
//     token is refused; so are the TOML's legacy admin_username and
//     admin_password (denmaLegacyAPIUser), and an install's
//     LISTMONK_ADMIN_API_USER (denmaInstallAPIUser).
//   - Those there were are deleted: the hub's when it starts (denmaHubColumns,
//     cmd/denma_tags.go), a center's with features version 23
//     (cmd/denma_features.go).
//
// listmonk's /webhooks/bounce, which only an API user could call, goes with
// them: bounces come from SES through /webhooks/service/ses
// (cmd/denma_bounces.go).

import (
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/listmonk/internal/auth"
)

// denmaDeleteAPIUsersSQL deletes the schema's API users, returning their
// usernames.
const denmaDeleteAPIUsersSQL = `DELETE FROM users WHERE type = 'api' RETURNING username`

// denmaDeleteAPIUsers deletes the API users in q's schema (where: the hub, or
// a center), logging their usernames (a system's, never a person's).
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

// denmaLegacyAPIUser ignores the TOML's legacy admin_username and
// admin_password, which listmonk would take as an API user's.
func denmaLegacyAPIUser(auth.User) {
	lo.Println("denma: app.admin_username and app.admin_password are ignored: there are no API users")
}

// denmaInstallAPIUser is false: an install makes no API user, even with
// LISTMONK_ADMIN_API_USER.
func denmaInstallAPIUser() bool {
	lo.Println("denma: LISTMONK_ADMIN_API_USER is ignored: there are no API users")
	return false
}
