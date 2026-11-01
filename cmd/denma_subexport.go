package main

// denma: exporting a subscriber's data for one that doesn't exist is a 404.

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// denmaSubscriberFound is nil if the subscriber (by id, or else UUID) is there, else a 404.
func (a *App) denmaSubscriberFound(id int, uuid string) error {
	var found bool
	if err := a.db.Get(&found, `SELECT EXISTS (SELECT 1 FROM subscribers WHERE CASE WHEN $1 > 0 THEN id = $1 ELSE uuid::TEXT = $2 END)`, id, uuid); err != nil {
		a.log.Printf("denma: error finding a subscriber to export: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, a.i18n.T("public.errorProcessingRequest"))
	}
	if !found {
		return echo.NewHTTPError(http.StatusNotFound, a.i18n.Ts("globals.messages.notFound", "name", "{globals.terms.subscriber}"))
	}
	return nil
}
