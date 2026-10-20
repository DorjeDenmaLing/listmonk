package main

// denma: sign-up answers reveal nothing about the address. listmonk's public
// subscription page (<center>/subscription/form, /api/public/subscription)
// and the Form webhooks (cmd/denma_signup.go) give everyone the same answer:
//
//   - the public page says "check your e-mail" whenever a list asked for is
//     double opt-in (has_optin), member or not, and "subscribed" otherwise;
//   - an address whose opt-ins for the day are used up (cmd/denma_optins.go,
//     counted across centers) gets that answer too, and no e-mail;
//   - an address whose domain can't receive mail is told so before anything
//     else, member or not: it says something about the domain, not them.

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

// errDenmaOptinLimited marks the opt-in limit's refusal (denmaCheckOptin),
// which sign-ups answer as if the e-mail had been sent.
var errDenmaOptinLimited = errors.New("the address's opt-ins for the day are used up")

// denmaSignupDomain refuses an address whose domain can't receive mail, for
// every sign-up alike (denmaCheckOptin only checks those sent an opt-in).
func (a *App) denmaSignupDomain(email string) error {
	if bad, reason := denmaEmailBad(email, denmaEmailSkip(a.ko)); bad {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("%s can't receive e-mail: %s. Check the address.", email, reason))
	}
	return nil
}

// denmaSubFormLists gives a public sign-up that names no lists the center's
// public lists, as the page doesn't show them (subscription-form.html).
func (a *App) denmaSubFormLists(uuids []string) []string {
	if len(uuids) > 0 {
		return uuids
	}
	lists, err := a.core.GetLists(models.ListTypePublic, models.ListStatusActive, true, nil)
	if err != nil {
		a.log.Printf("denma: error reading the public lists for a sign-up: %v", err)
		return nil
	}
	out := make([]string, 0, len(lists))
	for _, l := range lists {
		out = append(out, l.UUID)
	}
	return out
}

// denmaSubFormAnswer gives the public page's answer (processSubForm's
// results) from the lists asked for, not the address: the opt-in limit's
// refusal is no error, and hasOptin is whether a list is double opt-in.
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
