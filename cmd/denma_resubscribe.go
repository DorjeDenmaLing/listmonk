package main

// denma: re-subscribing someone who unsubscribed, once they confirm by e-mail.

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

// denmaResubscribeSQL makes the pending re-subscriptions' table, in a center (denmaFeaturesSQL).
const denmaResubscribeSQL = `
CREATE TABLE IF NOT EXISTS denma_resubscribes (
    subscriber_id INTEGER PRIMARY KEY REFERENCES subscribers(id) ON DELETE CASCADE,
    list_ids      INTEGER[] NOT NULL,
    attribs       JSONB NOT NULL DEFAULT '{}', -- added on confirming (a consent record)
    sent_at       TIMESTAMP WITH TIME ZONE NULL,
    created_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
-- When an address was last sent the e-mail, by address.
CREATE TABLE IF NOT EXISTS denma_resub_sent (
    email_hash TEXT PRIMARY KEY,
    sent_at    TIMESTAMP WITH TIME ZONE NOT NULL
);
-- When each subscriber was last blocklisted.
CREATE TABLE IF NOT EXISTS denma_blocklisted_at (
    subscriber_id INTEGER PRIMARY KEY REFERENCES subscribers(id) ON DELETE CASCADE,
    at            TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE OR REPLACE FUNCTION denma_note_blocklisted() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    INSERT INTO denma_blocklisted_at (subscriber_id) VALUES (NEW.id)
        ON CONFLICT (subscriber_id) DO UPDATE SET at = NOW();
    RETURN NULL;
END $$;
DROP TRIGGER IF EXISTS denma_note_blocklisted ON subscribers;
CREATE TRIGGER denma_note_blocklisted AFTER INSERT ON subscribers
    FOR EACH ROW WHEN (NEW.status = 'blocklisted') EXECUTE FUNCTION denma_note_blocklisted();
DROP TRIGGER IF EXISTS denma_note_blocklisted_update ON subscribers;
CREATE TRIGGER denma_note_blocklisted_update AFTER UPDATE OF status ON subscribers
    FOR EACH ROW WHEN (NEW.status = 'blocklisted' AND OLD.status IS DISTINCT FROM NEW.status) EXECUTE FUNCTION denma_note_blocklisted();
`

// denmaResubscribe starts a blocklisted subscriber's re-subscription to listIDs, sending the e-mail.
func (a *App) denmaResubscribe(sub models.Subscriber, listIDs []int, attribs map[string]any) (string, error) {
	if sub.Status != models.SubscriberStatusBlockListed {
		return denmaResubNotBlocked, nil
	}
	if why, err := a.denmaResubRefused(sub); err != nil || why != "" {
		return why, err
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
	// Claims the address's e-mail for the day, given back if it isn't sent.
	claimed, prev, err := a.denmaResubClaim(sub.Email)
	if err != nil {
		return "", err
	}
	if !claimed {
		return denmaResubAlready, nil
	}
	sent := false
	defer func() {
		if !sent {
			a.denmaResubUnclaim(sub.Email, prev)
		}
	}()

	lists, err := a.denmaListsByID(row.ListIDs)
	if err != nil {
		return "", err
	}
	// Through the opt-in checks: shared blocklist, domain check and opt-in limit.
	send := denmaCheckOptin(func(sub models.Subscriber, _ []int) (int, error) {
		return 1, a.denmaSendResub(sub, lists)
	}, a.db, a.ko)
	if n, err := send(sub, nil); err != nil {
		return "", err
	} else if n == 0 {
		return denmaResubBounced, nil
	}
	sent = true
	if _, err := a.db.Exec(`UPDATE denma_resubscribes SET sent_at = NOW() WHERE subscriber_id = $1`, sub.ID); err != nil {
		return "", err
	}
	a.log.Printf("denma: sent subscriber %d a confirmation to re-subscribe", sub.ID)
	return denmaResubSent, nil
}

// denmaResubClaim takes an address's e-mail for the day, returning whether it did and the last send.
func (a *App) denmaResubClaim(email string) (bool, sql.NullTime, error) {
	var out []struct {
		Prev sql.NullTime `db:"prev"`
	}
	err := a.db.Select(&out, `WITH gone AS (DELETE FROM denma_resub_sent WHERE sent_at < NOW() - $2 * INTERVAL '1 second' AND email_hash <> denma_email_hash($1)),
			prev AS (SELECT sent_at FROM denma_resub_sent WHERE email_hash = denma_email_hash($1))
		INSERT INTO denma_resub_sent AS r (email_hash, sent_at) VALUES (denma_email_hash($1), NOW())
		ON CONFLICT (email_hash) DO UPDATE SET sent_at = NOW() WHERE r.sent_at < NOW() - $2 * INTERVAL '1 second'
		RETURNING (SELECT sent_at FROM prev) AS prev`, email, int(denmaResubEvery.Seconds()))
	if err != nil || len(out) == 0 {
		return false, sql.NullTime{}, err
	}
	return true, out[0].Prev, nil
}

// denmaResubUnclaim gives back a claim whose e-mail wasn't sent.
func (a *App) denmaResubUnclaim(email string, prev sql.NullTime) {
	var err error
	if prev.Valid {
		_, err = a.db.Exec(`UPDATE denma_resub_sent SET sent_at = $2 WHERE email_hash = denma_email_hash($1)`, email, prev.Time)
	} else {
		_, err = a.db.Exec(`DELETE FROM denma_resub_sent WHERE email_hash = denma_email_hash($1)`, email)
	}
	if err != nil {
		a.log.Printf("denma: error giving back a re-subscription e-mail's claim: %v", err)
	}
}

// denmaResubRefused says why a blocklisted subscriber can't subscribe again, or "" if they can.
func (a *App) denmaResubRefused(sub models.Subscriber) (string, error) {
	var why struct {
		Complained bool `db:"complained"`
		Bounced    bool `db:"bounced"`
	}
	if err := a.db.Get(&why, `SELECT
		EXISTS (SELECT 1 FROM bounces WHERE subscriber_id = $1 AND type = 'complaint')
			OR COALESCE(denma_optout_kind(s.email) = 'complained', false) AS complained,
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
	return "", nil
}

func (a *App) denmaListsByID(ids pq.Int64Array) ([]models.List, error) {
	var lists []models.List
	err := a.db.Select(&lists, `SELECT * FROM lists WHERE id = ANY($1) ORDER BY name`, ids)
	return lists, err
}

// denmaSendResub sends the confirmation: listmonk's opt-in e-mail, with the lists signed up for.
func (a *App) denmaSendResub(sub models.Subscriber, lists []models.List) error {
	q := url.Values{}
	for _, l := range lists {
		q.Add("l", l.UUID)
	}
	out := subOptin{Subscriber: sub, Lists: lists,
		OptinURL: denmaOptinTokenURL(fmt.Sprintf(a.urlCfg.OptinURL, sub.UUID, q.Encode()), sub.UUID),
		UnsubURL: fmt.Sprintf(a.urlCfg.UnsubURL, dummyUUID, sub.UUID),
		LinkDays: int(denmaResubTTL.Hours() / 24)}
	hdr := textproto.MIMEHeader{}
	hdr.Set(models.EmailHeaderSubscriberUUID, sub.UUID)
	return a.notifs.Notify([]string{sub.Email}, a.i18n.T("subscribers.optinSubject")+": "+a.ko.String("app.site_name"), notifs.TplSubscriberOptin, out, hdr)
}

// denmaResubPending is a blocklisted subscriber's unexpired pending re-subscription lists.
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

// denmaResubOptin is listmonk's opt-in page for a blocklisted subscriber's pending re-subscription.
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
	// Only with the e-mail's own link: admins see the UUID.
	if len(lists) == 0 || !denmaOptinTokenOK(a.db, sub.ID, c.FormValue("t")) {
		return true, c.Render(http.StatusOK, tplMessage, makeMsgTpl("This link has expired", "",
			"This link has expired or has been used. To get a new one, sign up again where you first signed up."))
	}

	if !confirm {
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
	if err := a.denmaResubConfirm(sub.ID, lists, attribs, meta, true); err != nil {
		a.log.Printf("denma: error re-subscribing subscriber %d: %v", sub.ID, err)
		return true, c.Render(http.StatusInternalServerError, tplMessage,
			makeMsgTpl(a.i18n.T("public.errorTitle"), "", a.i18n.Ts("public.errorProcessingRequest")))
	}
	a.log.Printf("denma: subscriber %d confirmed and is re-subscribed to %d list(s)", sub.ID, len(lists))
	return true, c.Render(http.StatusOK, tplMessage,
		makeMsgTpl(a.i18n.T("public.subConfirmedTitle"), "", a.i18n.Ts("public.subConfirmed")))
}

// errDenmaResubStale is a consent dated before an opt-out made while it was being acted on.
var errDenmaResubStale = errors.New("they unsubscribed after that consent")

// denmaListIDs is lists' IDs.
func denmaListIDs(lists []models.List) []int {
	ids := make([]int, len(lists))
	for i, l := range lists {
		ids[i] = l.ID
	}
	return ids
}

// denmaResubMore is the extra lists confirming a re-subscription also subscribes them to.
func (a *App) denmaResubMore(subID int, ids []int) ([]int, error) {
	more := []int{}
	if !denmaAutomationsOn(a) {
		return more, nil
	}
	err := a.db.Select(&more, `SELECT DISTINCT add.list_id FROM denma_automations au
		JOIN denma_automation_lists t ON t.automation_id = au.id AND t.kind = 'trigger'
		JOIN denma_automation_lists add ON add.automation_id = au.id AND add.kind = 'add'
		JOIN subscriber_lists sl ON sl.subscriber_id = $1 AND sl.list_id = add.list_id AND sl.status = 'unsubscribed'
		WHERE au.active AND au.trigger_type IN ('joins', 'confirms') AND t.list_id = ANY($2) AND NOT (add.list_id = ANY($2))`,
		subID, pq.Array(ids))
	return more, err
}

// denmaResubConfirm unblocklists a subscriber and confirms lists; own: they confirmed it themselves.
func (a *App) denmaResubConfirm(subID int, lists []models.List, attribs, meta models.JSON, own bool, consentAt ...time.Time) error {
	ids := denmaListIDs(lists)
	more, err := a.denmaResubMore(subID, ids)
	if err != nil {
		return err
	}
	ids = append(ids, more...)
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
	if own {
		if _, err := tx.Exec(`SELECT set_config('denma.own_confirm', 'on', true)`); err != nil {
			return err
		}
	}
	// Refuses a consent dated before an opt-out made since it was read.
	if len(consentAt) > 0 {
		var newer bool
		if err := tx.Get(&newer, `WITH s AS (SELECT email FROM subscribers WHERE id = $1 FOR UPDATE)
			SELECT EXISTS (SELECT 1 FROM denma_optouts o, s WHERE (o.email_hash = denma_email_hash(s.email) OR o.canon_hash = denma_canon_hash(s.email)) AND o.created_at > $2)`, subID, consentAt[0]); err != nil {
			return err
		}
		if newer {
			return errDenmaResubStale
		}
	}
	// Their opt-out goes (cmd/denma_optouts.go), and their address variants'; a complaint's stays.
	if _, err := tx.Exec(`DELETE FROM denma_optouts o USING subscribers s WHERE s.id = $1 AND o.kind = 'unsubscribed'
		AND (o.email_hash = denma_email_hash(s.email) OR o.canon_hash = denma_canon_hash(s.email))`, subID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM denma_no_consent
		WHERE email_hash = (SELECT denma_email_hash(email) FROM subscribers WHERE id = $1)`, subID); err != nil {
		return err
	}
	// Their first consent is kept if they have one; resubscribed_at records this one.
	if _, err := tx.Exec(`UPDATE subscribers s SET status = 'enabled', updated_at = NOW(),
		attribs = x.b || $2::JSONB || jsonb_strip_nulls(jsonb_build_object('consent_at', x.b->'consent_at', 'consent_source', x.b->'consent_source'))
		FROM (SELECT CASE WHEN jsonb_typeof(attribs) = 'object' THEN attribs ELSE '{}'::JSONB END AS b FROM subscribers WHERE id = $1) x
		WHERE s.id = $1`, subID, string(ab)); err != nil {
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
	if _, err := tx.Exec(`DELETE FROM denma_optin_tokens WHERE subscriber_id = $1`, subID); err != nil {
		return err
	}
	return tx.Commit()
}

// denmaResubForm is the public form's sign-up of a blocklisted subscriber to lists listUUIDs.
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

// DenmaResubscribe sends the re-subscription confirmation {email, list_ids} and says what became of it.
func (a *App) DenmaResubscribe(c echo.Context) error {
	var req struct {
		Email   string `json:"email"`
		ListIDs []int  `json:"list_ids"`
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
	if err := a.denmaNoAPIList(req.ListIDs); err != nil { // cmd/denma_listmove.go
		return err
	}
	// Someone they may not manage is answered as someone not there.
	sub, err := a.core.GetSubscriber(0, "", req.Email)
	if err == nil {
		err = a.hasSubPerm(u, auth.PermTypeManage, []int{sub.ID})
	}
	if err != nil {
		var he *echo.HTTPError
		if errors.As(err, &he) && he.Code < 500 { // listmonk's not found is a 400
			return echo.NewHTTPError(http.StatusNotFound, "There's no contact with that address on your lists.")
		}
		return err
	}
	// Within the admins' daily cap (cmd/denma_consentcap.go), given back if nothing is sent.
	claim, err := a.denmaConsentClaim("resubscribe")
	if err != nil {
		return err
	}
	status, err := a.denmaResubscribe(sub, req.ListIDs, nil)
	if err != nil || status != denmaResubSent {
		a.denmaConsentUnclaim(claim)
	}
	if err != nil {
		var he *echo.HTTPError
		if errors.As(err, &he) {
			return he
		}
		return denmaInternal(err)
	}
	return c.JSON(http.StatusOK, okResp{map[string]string{"status": status}})
}
