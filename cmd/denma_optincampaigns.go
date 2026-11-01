package main

// denma: no opt-in campaigns; any existing one is cancelled when its center loads.

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

// denmaCheckOptinStart refuses starting or scheduling an opt-in campaign.
func (a *App) denmaCheckOptinStart(id int) error {
	var typ string
	if err := a.db.Get(&typ, `SELECT type::TEXT FROM campaigns WHERE id = $1`, id); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return denmaInternal(err)
	}
	return denmaNoOptinCampaign(typ)
}

// denmaCancelOptinCampaignsSQL cancels the opt-in campaigns that would still send.
const denmaCancelOptinCampaignsSQL = `UPDATE campaigns SET status = 'cancelled', updated_at = NOW()
	WHERE type = 'optin' AND status IN ('scheduled', 'running', 'paused')`
