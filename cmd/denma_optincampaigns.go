package main

// denma: no opt-in campaigns. listmonk's (Lists -> "Send opt-in campaign")
// e-mails everyone unconfirmed on a double opt-in list, as a campaign, past
// the opt-in limit (cmd/denma_optins.go): people imported unconfirmed and
// people an admin added, who never asked, as well as those who signed up and
// were sent their opt-in already. Nobody is e-mailed to ask for consent
// unless they asked, so they can't be made, saved or started; one left from
// before is cancelled when its center loads (features version 43). An
// admin's "Send opt-in e-mail" to one person stays.

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
)

const denmaNoOptinCampaignMsg = "Opt-in campaigns can't be sent: they'd ask people for consent who never asked. " +
	"People who sign up are sent their opt-in e-mail; to send one to someone who asked, use \"Send opt-in e-mail\" on their page."

// denmaNoOptinCampaign refuses an opt-in campaign's type.
func denmaNoOptinCampaign(typ string) error {
	if typ == models.CampaignTypeOptin {
		return echo.NewHTTPError(http.StatusBadRequest, denmaNoOptinCampaignMsg)
	}
	return nil
}

// denmaCheckOptinStart refuses starting or scheduling an opt-in campaign made
// before they were refused.
func (a *App) denmaCheckOptinStart(id int) error {
	var typ string
	if err := a.db.Get(&typ, `SELECT type::TEXT FROM campaigns WHERE id = $1`, id); errors.Is(err, sql.ErrNoRows) {
		return nil // listmonk's own handling answers for a campaign that isn't there
	} else if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return denmaNoOptinCampaign(typ)
}

// denmaCancelOptinCampaignsSQL cancels the opt-in campaigns that would still
// send (features version 43).
const denmaCancelOptinCampaignsSQL = `UPDATE campaigns SET status = 'cancelled', updated_at = NOW()
	WHERE type = 'optin' AND status IN ('scheduled', 'running', 'paused')`
