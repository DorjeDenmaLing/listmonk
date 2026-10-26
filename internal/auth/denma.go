package auth

// denma: a user the request's context already carries: an MCP key's
// (cmd/denma_mcp.go), checked before the request reached the center, with
// only the key's permissions. Clients can't set it.

import (
	"context"

	"github.com/labstack/echo/v4"
)

type denmaUserKey struct{}

// DenmaWithUser is ctx carrying u, for Middleware.
func DenmaWithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, denmaUserKey{}, u)
}

// denmaSetUser sets the user the request carries, if it does.
func denmaSetUser(c echo.Context) bool {
	u, ok := c.Request().Context().Value(denmaUserKey{}).(User)
	if ok {
		c.Set(UserHTTPCtxKey, u)
	}
	return ok
}
