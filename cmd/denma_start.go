package main

// denma: what starting a campaign means, for the confirmation shown before
// it's started or scheduled (static/admin/partials/denma/start-confirm.html,
// assets/js/denma-start.js): how many it goes to, the center's default
// reply-to (cmd/denma_reply_to.go), its daily cap (cmd/denma_daily.go) and its
// sending hours (cmd/denma_sendwindow_next.go).
// A draft campaign that would go to no one can't be started or scheduled
// (denmaCheckRecipients).

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

func initDenmaStartHandlers(g *echo.Group, a *App) {
	pm := a.auth.Perm
	g.GET("/api/denma/campaigns/:id/start-check", pm(hasID(a.DenmaStartCheck), "campaigns:get_all", "campaigns:get"))
	g.GET("/api/denma/send-hours", pm(a.DenmaSendHours, "campaigns:get_all", "campaigns:get")) // cmd/denma_sendwindow_next.go
}

// denmaRecipientsSQL counts a campaign's recipients as next-campaigns
// (queries/campaigns.sql) does when it starts: the subscribers on its lists
// ($1) and the active ones with any of its tags ($2), once each, for its type
// ($3).
const denmaRecipientsSQL = `
SELECT COUNT(*) FROM (
    SELECT sl.subscriber_id FROM subscriber_lists sl
    JOIN lists l ON l.id = sl.list_id
    JOIN subscribers s ON s.id = sl.subscriber_id AND s.status != 'blocklisted'
    WHERE sl.list_id = ANY($1::INT[]) AND (
        CASE
            WHEN $3 = 'optin' THEN sl.status = 'unconfirmed' AND l.optin = 'double'
            WHEN l.optin = 'double' THEN sl.status = 'confirmed'
            ELSE sl.status != 'unsubscribed'
        END)
    UNION
    SELECT s.id FROM subscribers s
    WHERE $3 != 'optin' AND CARDINALITY($2::TEXT[]) > 0 AND s.attribs->'tags' ?| $2::TEXT[]
        AND s.status != 'blocklisted' AND EXISTS (
        SELECT 1 FROM subscriber_lists dsl JOIN lists dl ON dl.id = dsl.list_id WHERE dsl.subscriber_id = s.id
        AND NOT ('api-send' = ANY(COALESCE(dl.tags, '{}'))) AND (CASE WHEN dl.optin = 'double' THEN dsl.status = 'confirmed' ELSE dsl.status != 'unsubscribed' END))
) x`

// DenmaStartCheck answers how many a campaign would go to, with the lists
// (list_id) and tags (tag) given, which may not be saved yet; the lists the
// user can't see, and the tags if they can't send to tags, don't count.
func (a *App) DenmaStartCheck(c echo.Context) error {
	id := getID(c)
	if err := a.checkCampaignPerm(auth.PermTypeGet, id, c); err != nil {
		return err
	}
	camp, err := a.core.GetCampaign(id, "", "")
	if err != nil {
		return err
	}

	var listIDs []int
	for _, s := range c.QueryParams()["list_id"] {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			listIDs = append(listIDs, n)
		}
	}
	u := auth.GetUser(c)
	listIDs = u.FilterListsByPerm(auth.PermTypeGet|auth.PermTypeManage, listIDs)
	tags := []string{}
	if denmaAllLists(&u) {
		tags = denmaNormTags(c.QueryParams()["tag"])
	}

	count, err := a.denmaRecipients(id, listIDs, tags, camp.Type)
	if err != nil {
		return err
	}

	// When it goes: now, or the time it's scheduled for (at, RFC 3339).
	at := time.Now()
	if t, err := time.Parse(time.RFC3339, c.QueryParam("at")); err == nil && t.After(at) {
		at = t
	}

	return c.JSON(http.StatusOK, okResp{struct {
		Recipients int            `json:"recipients"`
		ReplyTo    string         `json:"reply_to"` // the center's default
		Sending    denmaCapStatus `json:"sending"`
		Hours      denmaHoursNote `json:"hours"` // the sending hours, and when it goes if outside them
	}{count, a.denmaReplyTo(), a.denmaCapStatus(), a.denmaHoursAt(at)}})
}

// denmaRecipients counts campaign id's recipients with the lists and tags
// given (denmaRecipientsSQL).
func (a *App) denmaRecipients(id int, listIDs []int, tags []string, typ string) (int, error) {
	var count int
	if err := a.db.Get(&count, denmaRecipientsSQL, pq.Array(listIDs), pq.Array(tags), typ); err != nil {
		a.log.Printf("denma: error counting campaign %d's recipients: %v", id, err)
		return 0, echo.NewHTTPError(http.StatusInternalServerError, "Error counting the campaign's recipients.")
	}
	return count, nil
}

// denmaCheckCompiles refuses to start or schedule a campaign whose e-mail
// can't be made, its plain-text version included (made from the body,
// cmd/denma_features.go): the campaign manager would try it every few
// seconds and never send it. Called by denmaCheckCampaignStart.
func (a *App) denmaCheckCompiles(id int) error {
	camp, err := a.core.GetCampaignForPreview(id, 0)
	if err != nil {
		return err
	}
	if err := camp.CompileTemplate(a.manager.TemplateFuncs(&camp)); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.Ts("templates.errorCompiling", "error", err.Error()))
	}
	return nil
}

// errDenmaNoRecipients refuses to start a campaign that would go to no one.
var errDenmaNoRecipients = errors.New("No one would get this campaign: no one subscribed is on its lists or has its tags. Add a list with subscribers first.")

// denmaCheckRecipients refuses to start or schedule a draft campaign that
// would go to no one, with its saved lists and tags. A paused one, which has
// sent to some already, can go on. Called by denmaCheckCampaignStart.
func (a *App) denmaCheckRecipients(id int) error {
	var c struct {
		Status string         `db:"status"`
		Type   string         `db:"type"`
		Tags   pq.StringArray `db:"denma_tags"`
		Lists  pq.Int64Array  `db:"lists"`
	}
	if err := a.db.Get(&c, `SELECT status, type, denma_tags,
		ARRAY(SELECT list_id FROM campaign_lists WHERE campaign_id = $1 AND list_id IS NOT NULL) AS lists
		FROM campaigns WHERE id = $1`, id); err != nil {
		a.log.Printf("denma: error reading campaign %d's lists: %v", id, err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error reading the campaign's lists.")
	}
	if c.Status != "draft" {
		return nil
	}
	listIDs := make([]int, len(c.Lists))
	for i, l := range c.Lists {
		listIDs[i] = int(l)
	}
	count, err := a.denmaRecipients(id, listIDs, c.Tags, c.Type)
	if err != nil {
		return err
	}
	if count == 0 {
		return denmaBadRequest(errDenmaNoRecipients)
	}
	return nil
}
