package main

// denma: a center's admins send at most denmaConsentRequestsPerDay e-mails
// asking for consent a day: "Send opt-in e-mail" (SubscriberSendOptin) and
// "Send a confirmation to subscribe again" (DenmaResubscribe). Nobody is
// e-mailed to ask for consent unless they asked; each of these is an admin
// saying someone did, and a script (or a misled assistant) could otherwise
// send one to everyone who unsubscribed. Past the cap they're refused, and
// the center's admins and the superadmins are told once a day. Sign-ups'
// own opt-ins aren't counted (they have the per-address limit,
// cmd/denma_optins.go). MCP keys can't send either (cmd/denma_mcp.go).

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
)

const denmaConsentRequestsPerDay = 20

// denmaConsentCapSQL makes the count's table, in a center (denmaFeaturesSQL,
// version 44): one row per e-mail, and one per notice ('notice'). Kept two
// days (apiHousekeeping).
const denmaConsentCapSQL = `
CREATE TABLE IF NOT EXISTS denma_consent_requests (
    id         BIGSERIAL PRIMARY KEY,
    kind       TEXT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS denma_consent_requests_created ON denma_consent_requests (created_at);
`

// denmaConsentClaim takes one of the day's e-mails asking for consent
// (kind: optin or resubscribe), returning its row to give back if none is
// sent (denmaConsentUnclaim), or a 429 past the cap.
func (a *App) denmaConsentClaim(kind string) (int64, error) {
	tx, err := a.db.Beginx()
	if err != nil {
		return 0, echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext(current_schema() || '/denma_consent_requests'))`); err != nil {
		return 0, echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	var used int
	var told bool
	if err := tx.QueryRow(`SELECT COUNT(*) FILTER (WHERE kind <> 'notice'), COUNT(*) FILTER (WHERE kind = 'notice') > 0
		FROM denma_consent_requests WHERE created_at > NOW() - INTERVAL '24 hours'`).Scan(&used, &told); err != nil {
		return 0, echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if used >= denmaConsentRequestsPerDay {
		if !told {
			if _, err := tx.Exec(`INSERT INTO denma_consent_requests (kind) VALUES ('notice')`); err == nil && tx.Commit() == nil {
				a.denmaConsentNotify()
			}
		}
		return 0, echo.NewHTTPError(http.StatusTooManyRequests, fmt.Sprintf(
			"This center's admins have sent %d e-mails asking for consent in the last 24 hours, the most they can. Try again tomorrow.", denmaConsentRequestsPerDay))
	}
	var id int64
	if err := tx.Get(&id, `INSERT INTO denma_consent_requests (kind) VALUES ($1) RETURNING id`, kind); err != nil {
		return 0, echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return id, tx.Commit()
}

// denmaConsentUnclaim gives back a claim whose e-mail wasn't sent.
func (a *App) denmaConsentUnclaim(id int64) {
	if _, err := a.db.Exec(`DELETE FROM denma_consent_requests WHERE id = $1`, id); err != nil {
		a.log.Printf("denma: error giving back a consent e-mail's claim: %v", err)
	}
}

// denmaAdminOptin is SubscriberSendOptin's e-mail (fnOptinNotify), within
// the day's cap.
func (a *App) denmaAdminOptin(sub models.Subscriber) (int, error) {
	id, err := a.denmaConsentClaim("optin")
	if err != nil {
		return 0, err
	}
	n, err := a.fnOptinNotify(sub, nil)
	if err != nil || n == 0 {
		a.denmaConsentUnclaim(id)
	}
	return n, err
}

// denmaConsentNotify tells the center's admins and the superadmins that the
// cap was reached; no one is named.
func (a *App) denmaConsentNotify() {
	center := a.ko.String("app.site_name")
	a.log.Printf("denma: the center's admins reached their %d e-mails asking for consent a day", denmaConsentRequestsPerDay)
	data := map[string]any{
		"Center": center, "What": fmt.Sprintf("sent %d e-mails asking for consent in a day", denmaConsentRequestsPerDay),
		"Detail": fmt.Sprintf("The center's admins sent %d opt-in or re-subscription e-mails in the last 24 hours, the most they can; more are refused until tomorrow. "+
			"Each should go only to someone who asked. If nobody here sent that many, someone may be using an admin's account.", denmaConsentRequestsPerDay),
		"URL": strings.TrimSuffix(a.urlCfg.RootURL, "/") + "/admin/activity",
	}
	subject := fmt.Sprintf("E-mails asking for consent: the day's %d reached: %s", denmaConsentRequestsPerDay, center)
	if err := a.notifs.NotifySystem(subject, "denma-consent-alert", data, nil); err != nil {
		a.log.Printf("denma: error telling the center's admins about consent e-mails: %v", err)
	}
	if hub := denmaHub.current(); hub != nil && hub.notifs != nil {
		if err := hub.notifs.NotifySystem(subject, "denma-consent-alert", data, nil); err != nil {
			a.log.Printf("denma: error telling the superadmins about consent e-mails: %v", err)
		}
	}
}
