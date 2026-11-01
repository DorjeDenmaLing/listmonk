package main

import (
	"net/http"
	"strings"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/labstack/echo/v4"
)

// DenmaSharedUnblock takes an address off the shared blocklist (superadmins, in the hub).
func (a *App) DenmaSharedUnblock(c echo.Context) error {
	if denmaHub == nil || a.ko.String("denma.center") != "" || auth.GetUser(c).UserRole.ID != auth.SuperAdminRoleID {
		return echo.NewHTTPError(http.StatusForbidden, "Only a superadmin can do this, in the hub.")
	}
	var req struct {
		Email string `json:"email"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !strings.Contains(email, "@") {
		return echo.NewHTTPError(http.StatusBadRequest, "Give an e-mail address.")
	}
	res, err := a.db.Exec(`DELETE FROM denma.blocked_emails WHERE email = $1`, email)
	if err != nil {
		return denmaInternal(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return echo.NewHTTPError(http.StatusNotFound, "That address isn't on the shared blocklist.")
	}
	return c.JSON(http.StatusOK, okResp{true})
}
