package main

// denma: sign-up answers reveal nothing about the address (the same answer for everyone).

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

// errDenmaOptinLimited marks the opt-in limit's refusal, which sign-ups answer as if sent.
var errDenmaOptinLimited = errors.New("the address's opt-ins for the day are used up")

// denmaSignupDomain refuses an address whose domain can't receive mail, for every sign-up.
func (a *App) denmaSignupDomain(email string) error {
	if bad, reason := denmaEmailBad(email, denmaEmailSkip(a.ko)); bad {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("%s can't receive e-mail: %s. Check the address.", email, reason))
	}
	return nil
}

// denmaSubFormLists is a public sign-up's lists: double opt-in ones it names, else public ones.
func (a *App) denmaSubFormLists(uuids []string) []string {
	out := []string{}
	if err := a.db.Select(&out, `SELECT uuid::TEXT FROM lists WHERE optin = 'double' AND CASE
		WHEN CARDINALITY($1::TEXT[]) > 0 THEN uuid::TEXT IN (SELECT LOWER(u) FROM UNNEST($1::TEXT[]) u)
		ELSE type = 'public' AND status = 'active' END ORDER BY id`, pq.StringArray(uuids)); err != nil {
		a.log.Printf("denma: error reading the lists for a public sign-up: %v", err)
		return nil
	}
	return out
}

// denmaDoubleOptin is the double opt-in lists of lists, which a public sign-up joins.
func denmaDoubleOptin(lists []models.List) []models.List {
	out := lists[:0]
	for _, l := range lists {
		if l.Optin == models.ListOptinDouble {
			out = append(out, l)
		}
	}
	return out
}

// denmaSubFormAnswer gives the public page's answer from the lists asked for, not the address.
func (a *App) denmaSubFormAnswer(listUUIDs []string, hasOptin *bool, err *error) {
	if *err != nil && !errors.Is(*err, errDenmaOptinLimited) {
		return
	}
	if *err != nil {
		a.log.Printf("denma: public sign-up: %v, no e-mail sent", errDenmaOptinLimited)
	}
	var double bool
	if e := a.db.Get(&double, `SELECT EXISTS (SELECT 1 FROM lists WHERE uuid = ANY($1::UUID[]) AND optin = 'double')`, pq.Array(listUUIDs)); e != nil {
		a.log.Printf("denma: error reading the public sign-up's lists: %v", e)
		*hasOptin, *err = false, echo.NewHTTPError(http.StatusInternalServerError, a.i18n.T("public.errorProcessingRequest"))
		return
	}
	*hasOptin, *err = double, nil
}
