package main

// denma: signing up again after unsubscribing. Unsubscribing blocklists
// someone in the center (cmd/denma_unsubscribe.go), and listmonk then leaves
// them out of every sign-up. Here, someone blocklisted who signs up again
// themselves, on the public form or API, or the website's form (the
// web-signup Lambda, through /api/denma/subscribers/resubscribe), is sent a
// confirmation e-mail, and stays blocklisted until they confirm. Confirming
// (listmonk's opt-in link and page) takes them off the blocklist and
// subscribes them to the lists they signed up for, not those they left.
//
// Those who complained, hard-bounced or failed the domain check
// (cmd/denma_emailcheck.go) aren't sent one. Someone who complained never
// comes back (cmd/denma_optouts.go); an admin can enable those who bounced.
// Everything else that adds someone (imports, admins' list changes, other API
// calls such as rg-sync's) leaves a blocklisted subscriber out, as listmonk
// does: under CASL, an e-mail asking for consent again has to be one they
// asked for. An admin can send the confirmation from the subscriber's page
// when they've asked for it (the same API as the web-signup Lambda's).
//
// An address is sent at most one such e-mail a day (and counts towards the
// opt-in limit, cmd/denma_optins.go); a sign-up in between adds its lists to
// the one sent. The link works for denmaResubTTL.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
	"net/url"
	"time"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/internal/notifs"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

const (
	denmaResubTTL   = 7 * 24 * time.Hour
	denmaResubEvery = 24 * time.Hour
)

// What became of a re-subscription.
const (
	denmaResubSent       = "sent"
	denmaResubAlready    = "already_sent" // one was sent in the last day
	denmaResubComplained = "complained"
	denmaResubBounced    = "bounced" // hard bounce, or the domain check
	denmaResubNotBlocked = "not_blocklisted"
)

// denmaResubscribeSQL makes the pending re-subscriptions' table, in a center
// (denmaFeaturesSQL).
const denmaResubscribeSQL = `
CREATE TABLE IF NOT EXISTS denma_resubscribes (
    subscriber_id INTEGER PRIMARY KEY REFERENCES subscribers(id) ON DELETE CASCADE,
    list_ids      INTEGER[] NOT NULL,
    attribs       JSONB NOT NULL DEFAULT '{}', -- added on confirming (a consent record)
    sent_at       TIMESTAMP WITH TIME ZONE NULL,
    created_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
`

// denmaResubscribe starts a blocklisted subscriber's re-subscription to
// listIDs, sending them the confirmation e-mail.
func (a *App) denmaResubscribe(sub models.Subscriber, listIDs []int, attribs map[string]any) (string, error) {
	if sub.Status != models.SubscriberStatusBlockListed {
		return denmaResubNotBlocked, nil
	}

	// Why they're blocklisted: a complaint or an address that can't receive
	// mail keeps them out.
	var why struct {
		Complained bool `db:"complained"`
		Bounced    bool `db:"bounced"`
	}
	if err := a.db.Get(&why, `SELECT
		EXISTS (SELECT 1 FROM bounces WHERE subscriber_id = $1 AND type = 'complaint')
			OR EXISTS (SELECT 1 FROM denma_optouts WHERE email_hash = denma_email_hash(s.email) AND kind = 'complained') AS complained,
		EXISTS (SELECT 1 FROM bounces WHERE subscriber_id = $1 AND type = 'hard')
			OR COALESCE(s.attribs ? 'auto_blocklist_reason', false) AS bounced
		FROM subscribers s WHERE s.id = $1`, sub.ID); err != nil {
		return "", err
	}
	if _, shared := denmaSharedBlock(a.db, sub.Email); shared {
		why.Bounced = true
	}
	switch {
	case why.Complained:
		a.log.Printf("denma: not re-subscribing subscriber %d: they complained", sub.ID)
		return denmaResubComplained, nil
	case why.Bounced:
		a.log.Printf("denma: not re-subscribing subscriber %d: their address bounced or can't receive mail", sub.ID)
		return denmaResubBounced, nil
	}

	// The lists, merged with a pending re-subscription's (unless it's expired).
	b, _ := json.Marshal(attribs)
	if attribs == nil {
		b = []byte("{}")
	}
	var row struct {
		ListIDs pq.Int64Array `db:"list_ids"`
		SentAt  sql.NullTime  `db:"sent_at"`
	}
	if err := a.db.Get(&row, `INSERT INTO denma_resubscribes AS r (subscriber_id, list_ids, attribs)
		SELECT $1, ARRAY(SELECT id FROM lists WHERE id = ANY($2::INT[]) ORDER BY id), $3
		ON CONFLICT (subscriber_id) DO UPDATE SET
			list_ids = CASE WHEN r.created_at < NOW() - $4 * INTERVAL '1 second' THEN EXCLUDED.list_ids
				ELSE ARRAY(SELECT DISTINCT x FROM unnest(r.list_ids || EXCLUDED.list_ids) x ORDER BY x) END,
			attribs = CASE WHEN r.created_at < NOW() - $4 * INTERVAL '1 second' THEN EXCLUDED.attribs ELSE r.attribs || EXCLUDED.attribs END,
			sent_at = CASE WHEN r.created_at < NOW() - $4 * INTERVAL '1 second' THEN NULL ELSE r.sent_at END,
			created_at = CASE WHEN r.created_at < NOW() - $4 * INTERVAL '1 second' THEN NOW() ELSE r.created_at END
		RETURNING list_ids, sent_at`,
		sub.ID, pq.Array(listIDs), string(b), int(denmaResubTTL.Seconds())); err != nil {
		return "", err
	}
	if len(row.ListIDs) == 0 {
		return "", echo.NewHTTPError(http.StatusBadRequest, "no lists")
	}
	if row.SentAt.Valid && time.Since(row.SentAt.Time) < denmaResubEvery {
		return denmaResubAlready, nil
	}

	lists, err := a.denmaListsByID(row.ListIDs)
	if err != nil {
		return "", err
	}
	// Through the opt-in checks: the shared blocklist, the domain check and
	// the opt-in limit.
	send := denmaCheckOptin(func(sub models.Subscriber, _ []int) (int, error) {
		return 1, a.denmaSendResub(sub, lists)
	}, a.db, a.ko)
	if _, err := send(sub, nil); err != nil {
		return "", err
	}
	if _, err := a.db.Exec(`UPDATE denma_resubscribes SET sent_at = NOW() WHERE subscriber_id = $1`, sub.ID); err != nil {
		return "", err
	}
	a.log.Printf("denma: sent subscriber %d a confirmation to re-subscribe", sub.ID)
	return denmaResubSent, nil
}

func (a *App) denmaListsByID(ids pq.Int64Array) ([]models.List, error) {
	var lists []models.List
	err := a.db.Select(&lists, `SELECT * FROM lists WHERE id = ANY($1) ORDER BY name`, ids)
	return lists, err
}

// denmaSendResub sends the confirmation: listmonk's opt-in e-mail, with the
// lists signed up for.
func (a *App) denmaSendResub(sub models.Subscriber, lists []models.List) error {
	q := url.Values{}
	for _, l := range lists {
		q.Add("l", l.UUID)
	}
	out := subOptin{Subscriber: sub, Lists: lists,
		OptinURL: fmt.Sprintf(a.urlCfg.OptinURL, sub.UUID, q.Encode()),
		UnsubURL: fmt.Sprintf(a.urlCfg.UnsubURL, dummyUUID, sub.UUID)}
	hdr := textproto.MIMEHeader{}
	hdr.Set(models.EmailHeaderSubscriberUUID, sub.UUID)
	return a.notifs.Notify([]string{sub.Email}, a.i18n.T("subscribers.optinSubject"), notifs.TplSubscriberOptin, out, hdr)
}

// denmaResubPending is a blocklisted subscriber's pending re-subscription's
// lists, if they have one that hasn't expired.
func (a *App) denmaResubPending(subID int) ([]models.List, models.JSON, error) {
	var row struct {
		ListIDs pq.Int64Array `db:"list_ids"`
		Attribs models.JSON   `db:"attribs"`
	}
	err := a.db.Get(&row, `SELECT list_ids, attribs FROM denma_resubscribes
		WHERE subscriber_id = $1 AND sent_at IS NOT NULL AND created_at > NOW() - $2 * INTERVAL '1 second'`,
		subID, int(denmaResubTTL.Seconds()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	lists, err := a.denmaListsByID(row.ListIDs)
	return lists, row.Attribs, err
}

// denmaResubOptin is listmonk's opt-in page (OptinPage) for a blocklisted
// subscriber with a pending re-subscription: its lists to confirm, and
// confirming. done is false for anyone else, whom listmonk's page handles.
func (a *App) denmaResubOptin(c echo.Context, subUUID string, confirm bool) (bool, error) {
	if !reUUID.MatchString(subUUID) {
		return false, nil
	}
	sub, err := a.core.GetSubscriber(0, subUUID, "")
	if err != nil || sub.Status != models.SubscriberStatusBlockListed {
		return false, nil
	}
	lists, attribs, err := a.denmaResubPending(sub.ID)
	if err != nil {
		a.log.Printf("denma: error reading subscriber %d's re-subscription: %v", sub.ID, err)
		return false, nil
	}
	if len(lists) == 0 {
		return false, nil
	}

	if !confirm && a.cfg.ShowOptinPage {
		var out optinTpl
		out.Lists = lists
		out.SubUUID = subUUID
		out.Title = a.i18n.T("public.confirmOptinSubTitle")
		return true, c.Render(http.StatusOK, "optin", out)
	}

	meta := models.JSON{"resubscribed": true}
	if a.cfg.Privacy.RecordOptinIP {
		meta["optin_ip"] = c.RealIP()
	}
	if err := a.denmaResubConfirm(sub.ID, lists, attribs, meta); err != nil {
		a.log.Printf("denma: error re-subscribing subscriber %d: %v", sub.ID, err)
		return true, c.Render(http.StatusInternalServerError, tplMessage,
			makeMsgTpl(a.i18n.T("public.errorTitle"), "", a.i18n.Ts("public.errorProcessingRequest")))
	}
	a.log.Printf("denma: subscriber %d confirmed and is re-subscribed to %d list(s)", sub.ID, len(lists))
	return true, c.Render(http.StatusOK, tplMessage,
		makeMsgTpl(a.i18n.T("public.subConfirmedTitle"), "", a.i18n.Ts("public.subConfirmed")))
}

// denmaResubConfirm takes a subscriber off the blocklist and confirms their
// subscriptions to lists: each is made unconfirmed, then confirmed, as an
// opt-in is, so that the website signups' holding list moves them on
// (denma_confirm_signup, cmd/denma_features.go).
func (a *App) denmaResubConfirm(subID int, lists []models.List, attribs, meta models.JSON) error {
	ids := make([]int, len(lists))
	for i, l := range lists {
		ids[i] = l.ID
	}
	// The holding list's target too: the move doesn't undo the unsubscribe
	// from it, which this confirmation does.
	if hold, target := a.ko.Int("denma.signup_holding_list"), a.ko.Int("denma.signup_target_list"); hold > 0 && target > 0 {
		for _, id := range ids {
			if id == hold {
				ids = append(ids, target)
				break
			}
		}
	}
	if attribs == nil {
		attribs = models.JSON{}
	}
	attribs["resubscribed_at"] = time.Now().UTC().Format(time.RFC3339)
	ab, _ := json.Marshal(attribs)
	mb, _ := json.Marshal(meta)

	tx, err := a.db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Their opt-out goes (cmd/denma_optouts.go): confirming is their consent.
	// A complaint's stays, and enabling them is refused.
	if _, err := tx.Exec(`DELETE FROM denma_optouts WHERE kind = 'unsubscribed'
		AND email_hash = (SELECT denma_email_hash(email) FROM subscribers WHERE id = $1)`, subID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE subscribers SET status = 'enabled', updated_at = NOW(),
		attribs = (CASE WHEN jsonb_typeof(attribs) = 'object' THEN attribs ELSE '{}'::JSONB END) || $2::JSONB
		WHERE id = $1`, subID, string(ab)); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO subscriber_lists (subscriber_id, list_id, status)
		SELECT $1, UNNEST($2::INT[]), 'unconfirmed'
		ON CONFLICT (subscriber_id, list_id) DO UPDATE SET status = 'unconfirmed', updated_at = NOW()`,
		subID, pq.Array(ids)); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE subscriber_lists SET status = 'confirmed', updated_at = NOW(),
		meta = COALESCE(meta, '{}'::JSONB) || $3::JSONB
		WHERE subscriber_id = $1 AND list_id = ANY($2::INT[])`, subID, pq.Array(ids), string(mb)); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM denma_resubscribes WHERE subscriber_id = $1`, subID); err != nil {
		return err
	}
	return tx.Commit()
}

// denmaResubForm is the public form's (and API's) sign-up of an existing
// subscriber who's blocklisted (processSubForm), to the lists listUUIDs.
// Whatever became of it, they're told to check their e-mail: whoever typed
// the address isn't told why someone was blocklisted.
func (a *App) denmaResubForm(sub models.Subscriber, listUUIDs []string) (bool, error) {
	var ids []int
	if err := a.db.Select(&ids, `SELECT id FROM lists WHERE uuid = ANY($1::UUID[])`, pq.Array(listUUIDs)); err != nil {
		return false, err
	}
	if _, err := a.denmaResubscribe(sub, ids, nil); err != nil {
		var he *echo.HTTPError
		if errors.As(err, &he) {
			return false, he // the opt-in limit, an address that can't receive mail
		}
		a.log.Printf("denma: error re-subscribing subscriber %d: %v", sub.ID, err)
		return false, echo.NewHTTPError(http.StatusInternalServerError, a.i18n.T("public.errorProcessingRequest"))
	}
	return true, nil
}

func initDenmaResubscribeHandlers(g *echo.Group, a *App) {
	g.POST("/api/denma/subscribers/resubscribe", a.auth.Perm(a.DenmaResubscribe, "subscribers:manage"))
}

// DenmaResubscribe is a sign-up the person made on another site (the
// website's form, through the web-signup Lambda) for someone who's
// blocklisted: {email, list_ids, attribs}. It says what became of it
// (status: sent, already_sent, complained, bounced or not_blocklisted).
func (a *App) DenmaResubscribe(c echo.Context) error {
	var req struct {
		Email   string         `json:"email"`
		ListIDs []int          `json:"list_ids"`
		Attribs map[string]any `json:"attribs"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if len(req.ListIDs) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "Choose at least one list.")
	}
	u := auth.GetUser(c)
	if err := u.HasListPerm(auth.PermTypeManage, req.ListIDs...); err != nil {
		return err
	}
	sub, err := a.core.GetSubscriber(0, "", req.Email)
	if err != nil {
		return err
	}
	status, err := a.denmaResubscribe(sub, req.ListIDs, req.Attribs)
	if err != nil {
		var he *echo.HTTPError
		if errors.As(err, &he) {
			return he
		}
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{map[string]string{"status": status}})
}
