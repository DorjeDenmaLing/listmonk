package main

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/labstack/echo/v4"
)

var reDenmaLogAddress = regexp.MustCompile(`[^\s@'"<>(),]+@[^\s@'"<>(),]+`)

// denmaInternal logs an unexpected error, without addresses, and gives a plain 500.
func denmaInternal(err error) error {
	lo.Printf("denma: internal error: %s", reDenmaLogAddress.ReplaceAllString(err.Error(), "[address]"))
	return echo.NewHTTPError(http.StatusInternalServerError, "Something went wrong on our side. Try again in a moment.")
}

var reDenmaPQCode = regexp.MustCompile(`\(([0-9A-Z]{5})\)\s*$`)

// denmaDBErrors keeps the database's wording out of answers: a value it can't take is a 400, a conflict a 409.
func denmaDBErrors(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		err := next(c)
		he, ok := err.(*echo.HTTPError)
		if !ok {
			return err
		}
		msg, ok := he.Message.(string)
		i := strings.Index(msg, "pq: ")
		if !ok || i < 0 {
			return err
		}
		code := ""
		if m := reDenmaPQCode.FindStringSubmatch(msg); m != nil {
			code = m[1]
		}
		lo.Printf("denma: database error %s: %s", code, reDenmaLogAddress.ReplaceAllString(msg, "[address]"))
		head := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(msg[:i]), ":"))
		if head == "" {
			head = "The request"
		}
		switch {
		case strings.HasPrefix(code, "22"):
			return echo.NewHTTPError(http.StatusBadRequest, head+": a value given isn't one it takes.")
		case strings.HasPrefix(code, "23"):
			return echo.NewHTTPError(http.StatusConflict, head+": it conflicts with what's there.")
		}
		return echo.NewHTTPError(he.Code, head+": the database couldn't do it. Try again in a moment.")
	}
}
