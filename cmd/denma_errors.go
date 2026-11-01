package main

import (
	"net/http"
	"regexp"

	"github.com/labstack/echo/v4"
)

var reDenmaLogAddress = regexp.MustCompile(`[^\s@'"<>(),]+@[^\s@'"<>(),]+`)

// denmaInternal logs an unexpected error, without addresses, and gives a plain 500.
func denmaInternal(err error) error {
	lo.Printf("denma: internal error: %s", reDenmaLogAddress.ReplaceAllString(err.Error(), "[address]"))
	return echo.NewHTTPError(http.StatusInternalServerError, "Something went wrong on our side. Try again in a moment.")
}
