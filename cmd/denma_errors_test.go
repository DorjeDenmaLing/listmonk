package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestDenmaDBErrors(t *testing.T) {
	e := echo.New()
	run := func(err error) error {
		c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
		return denmaDBErrors(func(echo.Context) error { return err })(c)
	}
	for _, tc := range []struct {
		in   error
		code int
		msg  string
	}{
		{echo.NewHTTPError(500, `Error fetching Lists: pq: invalid input value for enum list_status: "x" (22P02)`), 400, "Error fetching Lists: a value given isn't one it takes."},
		{echo.NewHTTPError(500, `Error creating List: pq: duplicate key value violates unique constraint "x" (23505)`), 409, "Error creating List: it conflicts with what's there."},
		{echo.NewHTTPError(500, `pq: canceling statement due to statement timeout (57014)`), 500, "The request: the database couldn't do it. Try again in a moment."},
		{echo.NewHTTPError(400, "Invalid ID"), 400, "Invalid ID"},
	} {
		var he *echo.HTTPError
		if !errors.As(run(tc.in), &he) || he.Code != tc.code || he.Message != tc.msg {
			t.Errorf("%v: got %v", tc.in, he)
		}
	}
	if err := run(errors.New("pq: plain")); err == nil || err.Error() != "pq: plain" {
		t.Errorf("a plain error is left to the error handler, got %v", err)
	}
}
