package main

// denma: automations (sidebar, in a center; not the hub): e-mails that send
// themselves, as Mailchimp's do. When someone joins one of an automation's
// lists (any of them), after an optional
// wait, they're sent a template once: a transactional one (HTML) or a visual
// one (made with the drag and drop editor), with the automation's subject.
//
// Only people who join while an automation is on get it (not those already
// on its lists). On a double opt-in list, they're sent it once they confirm.
// A cron job (every minute, per center) sends what's due through the
// center's e-mail messenger; denma_automation_sends records who was sent
// what, so no one gets an automation twice.
//
// Its e-mails' unsubscribe links take the automation's UUID in place of a
// campaign's (public.go calls denmaUnsubscribe), and unsubscribe from its
// lists. Their "view in browser" link is /automation/<uuid>/<subscriber>.

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"maps"
	"net/http"
	"net/mail"
	"net/textproto"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	txttpl "text/template"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
	null "gopkg.in/volatiletech/null.v6"
)

const (
	denmaAutoGet    = "automations:get"
	denmaAutoManage = "automations:manage"

	// denmaAutoBatch is the most e-mails one automation sends a minute.
	denmaAutoBatch = 500
	// denmaAutoMaxDelay is the longest wait, in minutes: a year.
	denmaAutoMaxDelay = 365 * 24 * 60
)

// denmaAutoTables are made in each center's schema when it starts.
const denmaAutoTables = `
CREATE TABLE IF NOT EXISTS denma_automations (
    id            SERIAL PRIMARY KEY,
    uuid          UUID NOT NULL UNIQUE,
    name          TEXT NOT NULL,
    template_id   INTEGER NULL REFERENCES templates(id) ON DELETE SET NULL,
    subject       TEXT NOT NULL,
    from_email    TEXT NOT NULL DEFAULT '',
    delay_minutes INTEGER NOT NULL DEFAULT 0,
    active        BOOLEAN NOT NULL DEFAULT false,
    active_since  TIMESTAMP WITH TIME ZONE NULL,
    created_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS denma_automation_lists (
    automation_id INTEGER NOT NULL REFERENCES denma_automations(id) ON DELETE CASCADE,
    list_id       INTEGER NOT NULL REFERENCES lists(id) ON DELETE CASCADE,
    PRIMARY KEY (automation_id, list_id)
);
CREATE TABLE IF NOT EXISTS denma_automation_sends (
    automation_id INTEGER NOT NULL REFERENCES denma_automations(id) ON DELETE CASCADE,
    subscriber_id INTEGER NOT NULL REFERENCES subscribers(id) ON DELETE CASCADE,
    status        TEXT NOT NULL DEFAULT 'sent',
    error         TEXT NOT NULL DEFAULT '',
    sent_at       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (automation_id, subscriber_id)
);`

// denmaAutoWaitingSQL: the subscriptions (sl) to an automation's (a) lists
// made while it was on, still on, of subscribers it hasn't sent to yet; one
// row per subscription, so a subscriber can be in more than one. Imported
// subscribers (attribs.imported_at, cmd/denma_features.go) never get
// automations.
// denmaAutoDueSQL narrows them to those whose wait is over.
const (
	denmaAutoWaitingSQL = `
FROM denma_automations a
JOIN denma_automation_lists al ON al.automation_id = a.id
JOIN subscriber_lists sl ON sl.list_id = al.list_id
JOIN subscribers s ON s.id = sl.subscriber_id
JOIN lists l ON l.id = sl.list_id
WHERE a.active AND a.template_id IS NOT NULL
    AND s.status <> 'blocklisted' AND sl.status <> 'unsubscribed'
    AND NOT (jsonb_typeof(s.attribs) = 'object' AND s.attribs ? 'imported_at')
    AND (l.optin <> 'double' OR sl.status = 'confirmed')
    AND sl.created_at >= a.active_since
    AND NOT EXISTS (SELECT 1 FROM denma_automation_sends d WHERE d.automation_id = a.id AND d.subscriber_id = sl.subscriber_id)`

	denmaAutoDueSQL = denmaAutoWaitingSQL + `
    AND (CASE WHEN l.optin = 'double' THEN GREATEST(sl.created_at, sl.updated_at) ELSE sl.created_at END)
        + make_interval(mins => a.delay_minutes) <= NOW()`
)

// denmaAutomation is an automation, with its lists and its template's name.
type denmaAutomation struct {
	ID           int       `db:"id" json:"id"`
	UUID         string    `db:"uuid" json:"uuid"`
	Name         string    `db:"name" json:"name"`
	TemplateID   null.Int  `db:"template_id" json:"template_id"`
	Subject      string    `db:"subject" json:"subject"`
	FromEmail    string    `db:"from_email" json:"from_email"`
	DelayMinutes int       `db:"delay_minutes" json:"delay_minutes"`
	Active       bool      `db:"active" json:"active"`
	ActiveSince  null.Time `db:"active_since" json:"active_since"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
	UpdatedAt    time.Time `db:"updated_at" json:"updated_at"`

	ListIDs      pq.Int64Array  `db:"list_ids" json:"list_ids"`
	ListNames    pq.StringArray `db:"list_names" json:"list_names"`
	TemplateName string         `db:"template_name" json:"template_name"`
	TemplateType string         `db:"template_type" json:"template_type"`
	Sent         int            `db:"sent" json:"sent"`
	Failed       int            `db:"failed" json:"failed"`
	LastSentAt   null.Time      `db:"last_sent_at" json:"last_sent_at"`
	Waiting      int            `db:"waiting" json:"waiting"`
}

const denmaAutoSelectSQL = `
SELECT a.*,
    ARRAY(SELECT l.id FROM denma_automation_lists al JOIN lists l ON l.id = al.list_id WHERE al.automation_id = a.id ORDER BY l.name) AS list_ids,
    ARRAY(SELECT l.name FROM denma_automation_lists al JOIN lists l ON l.id = al.list_id WHERE al.automation_id = a.id ORDER BY l.name) AS list_names,
    COALESCE(t.name, '') AS template_name, COALESCE(t.type::TEXT, '') AS template_type,
    (SELECT COUNT(*) FROM denma_automation_sends d WHERE d.automation_id = a.id AND d.status = 'sent') AS sent,
    (SELECT COUNT(*) FROM denma_automation_sends d WHERE d.automation_id = a.id AND d.status = 'failed') AS failed,
    (SELECT MAX(sent_at) FROM denma_automation_sends d WHERE d.automation_id = a.id) AS last_sent_at,
    0 AS waiting
FROM denma_automations a
LEFT JOIN templates t ON t.id = a.template_id`

// denmaAutoForm is what the automation page saves.
type denmaAutoForm struct {
	Name         string `json:"name"`
	ListIDs      []int  `json:"list_ids"`
	TemplateID   int    `json:"template_id"`
	Subject      string `json:"subject"`
	FromEmail    string `json:"from_email"`
	DelayMinutes int    `json:"delay_minutes"`
	Active       bool   `json:"active"`

	// TestEmail is where "Send a test" sends it.
	TestEmail string `json:"test_email"`
}

// denmaAutoOption is a list or template in the automation page's selects.
type denmaAutoOption struct {
	ID      int    `db:"id" json:"id"`
	Name    string `db:"name" json:"name"`
	Type    string `db:"type" json:"type"`
	Optin   string `db:"optin" json:"optin"`
	Subject string `db:"subject" json:"subject"`
}

type denmaAutomationsView struct {
	adminView
	Automations []denmaAutomation
}

type denmaAutomationView struct {
	adminView
	Automation denmaAutomation
	Lists      []denmaAutoOption
	Templates  []denmaAutoOption
	FromEmail  string
}

// denmaAutomationsOn reports whether the app has automations: a center, or
// listmonk without multi-center; not the hub.
func denmaAutomationsOn(a *App) bool {
	return !a.ko.Bool("denma.multi_center") || a.ko.String("denma.center") != ""
}

// denmaStartAutomations makes the app's automation tables if they're missing
// and adds the job that sends them to its cron. Called by buildApp.
func denmaStartAutomations(a *App) {
	if !denmaAutomationsOn(a) || a.crons == nil {
		return
	}
	var exists bool
	if err := a.db.Get(&exists, `SELECT to_regclass('denma_automations') IS NOT NULL`); err != nil {
		a.log.Printf("denma: error checking the automation tables: %v", err)
		return
	}
	if _, err := a.db.Exec(denmaAutoTables); err != nil {
		a.log.Printf("denma: error making the automation tables: %v", err)
		return
	}
	if !exists {
		// New permissions: the roles that can manage campaigns get them.
		if _, err := a.db.Exec(`UPDATE roles SET permissions = permissions || $1::TEXT[]
			WHERE type = 'user' AND 'campaigns:manage' = ANY(permissions) AND NOT ($2 = ANY(permissions))`,
			pq.Array([]string{denmaAutoGet, denmaAutoManage}), denmaAutoGet); err != nil {
			a.log.Printf("denma: error adding the automation permissions to roles: %v", err)
		}
	}

	var mu sync.Mutex
	if _, err := denmaEveryMinute(a, 0, func() {
		if !mu.TryLock() {
			return
		}
		defer mu.Unlock()
		a.runAutomations()
	}); err != nil {
		a.log.Printf("denma: error starting automations: %v", err)
		return
	}
	a.crons.Start()
}

// runAutomations sends what's due.
func (a *App) runAutomations() {
	if a.emailMsgr == nil {
		return
	}
	var autos []denmaAutomation
	if err := a.db.Select(&autos, denmaAutoSelectSQL+` WHERE a.active AND a.template_id IS NOT NULL`); err != nil {
		a.log.Printf("denma: error getting automations: %v", err)
		return
	}
	for _, auto := range autos {
		a.runAutomation(auto)
	}
}

func (a *App) runAutomation(auto denmaAutomation) {
	// Mark who's due as sent first, so that no one gets it twice.
	var ids []int
	if err := a.db.Select(&ids, `
		INSERT INTO denma_automation_sends (automation_id, subscriber_id)
		SELECT a.id, sl.subscriber_id `+denmaAutoDueSQL+` AND a.id = $1
		GROUP BY a.id, sl.subscriber_id ORDER BY MIN(sl.created_at) LIMIT $2
		ON CONFLICT DO NOTHING RETURNING subscriber_id`, auto.ID, denmaAutoBatch); err != nil {
		a.log.Printf("denma: error getting automation %d's subscribers: %v", auto.ID, err)
		return
	}
	if len(ids) == 0 {
		return
	}

	tpl, err := a.autoTemplate(int(auto.TemplateID.Int), true)
	var subs []models.Subscriber
	if err == nil {
		err = a.db.Select(&subs, `SELECT id, created_at, updated_at, uuid, email, name, attribs, status FROM subscribers WHERE id = ANY($1)`, pq.Array(ids))
	}
	if err != nil {
		a.autoFailed(auto.ID, ids, err)
		return
	}
	sent := 0
	for _, s := range subs {
		msg, err := a.autoMessage(auto, tpl, s)
		if err == nil {
			err = a.manager.PushMessage(msg)
		}
		if err != nil {
			a.autoFailed(auto.ID, []int{s.ID}, err)
			continue
		}
		sent++
	}
	a.log.Printf("denma: automation %q sent %d e-mails", auto.Name, sent)
}

// autoFailed records that an automation's e-mail to subscribers wasn't sent.
func (a *App) autoFailed(id int, subIDs []int, err error) {
	a.log.Printf("denma: error sending automation %d: %v", id, err)
	if _, e := a.db.Exec(`UPDATE denma_automation_sends SET status = 'failed', error = $3
		WHERE automation_id = $1 AND subscriber_id = ANY($2)`, id, pq.Array(subIDs), err.Error()); e != nil {
		a.log.Printf("denma: error recording automation %d's failure: %v", id, e)
	}
}

// denmaAutoTemplate is an automation's template, ready to render.
type denmaAutoTemplate struct {
	Body        string
	Attachments []models.Attachment
}

// autoTemplate gets a template; for e-mail (inline), with its media inlined as
// listmonk does for transactional ones.
func (a *App) autoTemplate(id int, inline bool) (denmaAutoTemplate, error) {
	var t struct {
		Type string `db:"type"`
		Body string `db:"body"`
	}
	if err := a.db.Get(&t, `SELECT type, body FROM templates WHERE id = $1`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return denmaAutoTemplate{}, fmt.Errorf("template %d not found", id)
		}
		return denmaAutoTemplate{}, err
	}
	if t.Type != models.TemplateTypeTx && t.Type != models.TemplateTypeCampaignVisual {
		return denmaAutoTemplate{}, errors.New("Choose a transactional or visual template.")
	}
	if !inline {
		return denmaAutoTemplate{Body: t.Body}, nil
	}
	body, atts := a.manager.ApplyInlineImages(t.Body)
	return denmaAutoTemplate{Body: body, Attachments: atts}, nil
}

// denmaAutoData is what an automation's template is rendered with: what a
// transactional template (Subscriber, Tx) and a campaign's (Campaign, L) get.
type denmaAutoData struct {
	Subscriber models.Subscriber
	Campaign   *models.Campaign
	Tx         *models.TxMessage
	L          any
}

// reDenmaTrackLink is listmonk's https://...@TrackLink shorthand.
var reDenmaTrackLink = regexp.MustCompile(`(https?://[\p{L}\p{N}_\-\.~!#$&'()*+,/:;=?@\[\]%]*)@TrackLink`)

// autoMessage renders an automation's e-mail to a subscriber.
func (a *App) autoMessage(auto denmaAutomation, tpl denmaAutoTemplate, s models.Subscriber) (models.Message, error) {
	var (
		root      = strings.TrimSuffix(a.urlCfg.RootURL, "/")
		unsubURL  = fmt.Sprintf(a.urlCfg.UnsubURL, auto.UUID, s.UUID)
		optinURL  = fmt.Sprintf(a.urlCfg.OptinURL, s.UUID, "")
		viewURL   = fmt.Sprintf("%s/automation/%s/%s", root, auto.UUID, s.UUID)
		funcs     = template.FuncMap{}
		withURL   = func(u string) func(...any) string { return func(...any) string { return u } }
		emptyHTML = func(...any) template.HTML { return "" }
	)
	maps.Copy(funcs, a.manager.GenericTemplateFuncs())
	// Campaign templates' functions; there's no tracking.
	maps.Copy(funcs, template.FuncMap{
		"TrackLink":      func(u string, _ ...any) string { return u },
		"TrackView":      emptyHTML,
		"UnsubscribeURL": withURL(unsubURL),
		"ManageURL":      withURL(unsubURL + "?manage=true"),
		"OptinURL":       withURL(optinURL),
		"MessageURL":     withURL(viewURL),
		"ArchiveURL":     withURL(a.urlCfg.ArchiveURL),
		"RootURL":        withURL(a.urlCfg.RootURL),
	})

	data := denmaAutoData{
		Subscriber: s,
		Campaign:   &models.Campaign{UUID: auto.UUID, Name: auto.Name, Subject: auto.Subject},
		Tx:         &models.TxMessage{Data: map[string]any{}},
		L:          a.i18n,
	}

	var b bytes.Buffer
	subj, err := txttpl.New(models.BaseTpl).Funcs(txttpl.FuncMap(funcs)).Parse(auto.Subject)
	if err != nil {
		return models.Message{}, fmt.Errorf("error in the subject: %v", err)
	}
	if err := subj.Execute(&b, data); err != nil {
		return models.Message{}, fmt.Errorf("error in the subject: %v", err)
	}
	subject := strings.TrimSpace(b.String())
	b.Reset()

	body, err := template.New(models.BaseTpl).Funcs(funcs).Parse(reDenmaTrackLink.ReplaceAllString(tpl.Body, "$1"))
	if err != nil {
		return models.Message{}, fmt.Errorf("error in the template: %v", err)
	}
	if err := body.Execute(&b, data); err != nil {
		return models.Message{}, fmt.Errorf("error in the template: %v", err)
	}

	from := auto.FromEmail
	if from == "" {
		from = a.cfg.FromEmail
	}
	msg := models.Message{
		Subscriber:  s,
		To:          []string{s.Email},
		From:        from,
		Subject:     subject,
		ContentType: models.CampaignContentTypeHTML,
		Messenger:   emailMsgr,
		Body:        b.Bytes(),
		Attachments: tpl.Attachments,
		Headers:     textproto.MIMEHeader{},
	}
	msg.Headers.Set(models.EmailHeaderSubscriberUUID, s.UUID)
	if a.cfg.Privacy.UnsubHeader {
		msg.Headers.Set("List-Unsubscribe-Post", "List-Unsubscribe=One-Click")
		msg.Headers.Set("List-Unsubscribe", `<`+unsubURL+`>`)
	}
	return msg, nil
}

// denmaUnsubscribe is listmonk's unsubscribe (public.go), and for an
// automation's e-mail (campUUID is the automation's) unsubscribes from its
// lists. An opt-in e-mail's unsubscribe (its one-click List-Unsubscribe, with
// no campaign: dummyUUID) unsubscribes from the double opt-in lists still
// waiting for the subscriber's confirmation; listmonk's did nothing and said
// it had (upstream #3250).
func (a *App) denmaUnsubscribe(subUUID, campUUID string, blocklist bool) error {
	if !blocklist && campUUID == dummyUUID {
		_, err := a.db.Exec(`UPDATE subscriber_lists SET status = 'unsubscribed', updated_at = NOW()
			WHERE status = 'unconfirmed' AND list_id IN (SELECT id FROM lists WHERE optin = 'double')
			AND subscriber_id = (SELECT id FROM subscribers WHERE uuid = $1)`, subUUID)
		return err
	}
	if !blocklist && denmaAutomationsOn(a) {
		var id int
		err := a.db.Get(&id, `SELECT id FROM denma_automations WHERE uuid = $1`, campUUID)
		if err == nil {
			_, err = a.db.Exec(`UPDATE subscriber_lists SET status = 'unsubscribed', updated_at = NOW()
				WHERE list_id IN (SELECT list_id FROM denma_automation_lists WHERE automation_id = $1)
				AND status <> 'unsubscribed' AND subscriber_id = (SELECT id FROM subscribers WHERE uuid = $2)`,
				id, subUUID)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	return a.core.UnsubscribeByCampaign(subUUID, campUUID, blocklist)
}

// initDenmaAutomationHandlers registers the automation pages.
func initDenmaAutomationHandlers(g *echo.Group, a *App) {
	g.GET(path.Join(uriAdmin, "/automations"), a.ViewDenmaAutomations)
	g.GET(path.Join(uriAdmin, "/automations/:id"), a.ViewDenmaAutomation)
}

func initDenmaAutomationAPIHandlers(g *echo.Group, a *App) {
	g.POST("/api/denma/automations", a.auth.Perm(a.DenmaSaveAutomation, denmaAutoManage))
	g.POST("/api/denma/automations/test", a.auth.Perm(a.DenmaTestAutomation, denmaAutoManage))
	g.PUT("/api/denma/automations/:id", a.auth.Perm(hasID(a.DenmaSaveAutomation), denmaAutoManage))
	g.PUT("/api/denma/automations/:id/status", a.auth.Perm(hasID(a.DenmaSetAutomationStatus), denmaAutoManage))
	g.DELETE("/api/denma/automations/:id", a.auth.Perm(hasID(a.DenmaDeleteAutomation), denmaAutoManage))
}

// initDenmaPublicHandlers registers the public pages (handlers.go).
func initDenmaPublicHandlers(g *echo.Group, a *App) {
	if denmaAutomationsOn(a) {
		g.GET("/automation/:autoUUID/:subUUID", noIndex(a.hasUUID(a.ViewDenmaAutomationMessage, "autoUUID", "subUUID")))
	}
}

// automationsAllowed returns an error unless the app has automations and the
// user can see them.
func (a *App) automationsAllowed(v adminView) error {
	if !denmaAutomationsOn(a) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if !v.Can(denmaAutoGet) {
		return echo.NewHTTPError(http.StatusForbidden, a.i18n.Ts("globals.messages.permissionDenied", "name", denmaAutoGet))
	}
	return nil
}

// ViewDenmaAutomations renders the automations.
func (a *App) ViewDenmaAutomations(c echo.Context) error {
	v := newAdminView(c, "Automations", "", "denma.automations")
	if err := a.automationsAllowed(v); err != nil {
		return err
	}
	out := denmaAutomationsView{adminView: v, Automations: []denmaAutomation{}}
	if err := a.db.Select(&out.Automations, denmaAutoSelectSQL+` ORDER BY a.created_at`); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	var waiting []struct {
		ID int `db:"id"`
		N  int `db:"n"`
	}
	if err := a.db.Select(&waiting, `SELECT a.id, COUNT(DISTINCT sl.subscriber_id) AS n `+denmaAutoWaitingSQL+` GROUP BY a.id`); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	for _, w := range waiting {
		for i := range out.Automations {
			if out.Automations[i].ID == w.ID {
				out.Automations[i].Waiting = w.N
			}
		}
	}
	return c.Render(http.StatusOK, "admin-denma-automations", out)
}

// ViewDenmaAutomation renders an automation's page, or a new one's ("new").
func (a *App) ViewDenmaAutomation(c echo.Context) error {
	v := newAdminView(c, "Automation", "", "denma.automations")
	if err := a.automationsAllowed(v); err != nil {
		return err
	}
	out := denmaAutomationView{adminView: v, FromEmail: a.cfg.FromEmail}
	if c.Param("id") == "new" {
		out.Title = "New automation"
	} else {
		id, _ := strconv.Atoi(c.Param("id"))
		if err := a.db.Get(&out.Automation, denmaAutoSelectSQL+` WHERE a.id = $1`, id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return echo.NewHTTPError(http.StatusNotFound, "That automation doesn't exist.")
			}
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		out.Title = out.Automation.Name
	}
	if err := a.db.Select(&out.Lists, `SELECT id, name, type::TEXT, optin::TEXT, '' AS subject FROM lists ORDER BY name`); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if err := a.db.Select(&out.Templates, `SELECT id, name, type::TEXT, '' AS optin, subject FROM templates
		WHERE type IN ('tx', 'campaign_visual') ORDER BY name`); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.Render(http.StatusOK, "admin-denma-automation", out)
}

// validateAutomation checks and tidies a saved or tested automation.
func (a *App) validateAutomation(c echo.Context, f *denmaAutoForm) error {
	bad := func(msg string) error { return echo.NewHTTPError(http.StatusBadRequest, msg) }
	f.Name = strings.TrimSpace(f.Name)
	f.Subject = strings.TrimSpace(f.Subject)
	f.FromEmail = strings.TrimSpace(f.FromEmail)

	if !strHasLen(f.Name, 1, 200) {
		return bad("Enter the automation's name.")
	}
	var ids []int
	if err := a.db.Select(&ids, `SELECT id FROM lists WHERE id = ANY($1) ORDER BY id`, pq.Array(f.ListIDs)); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if len(ids) == 0 {
		return bad("Choose at least one list.")
	}
	if len(ids) > 100 {
		return bad("Choose at most 100 lists.")
	}
	f.ListIDs = ids
	u := auth.GetUser(c)
	if err := u.HasListPerm(auth.PermTypeManage, ids...); err != nil {
		return err
	}
	if !strHasLen(f.Subject, 1, 500) {
		return bad("Enter the subject.")
	}
	if f.FromEmail != "" {
		if _, err := mail.ParseAddress(f.FromEmail); err != nil {
			return bad(`The sender isn't a valid address. Use name@example.org or "Name" <name@example.org>.`)
		}
	}
	if f.DelayMinutes < 0 || f.DelayMinutes > denmaAutoMaxDelay {
		return bad("The wait can be up to a year.")
	}

	// The template, and that it renders.
	tpl, err := a.autoTemplate(f.TemplateID, false)
	if err != nil {
		return bad("Choose a template: a transactional or visual one.")
	}
	sample := models.Subscriber{UUID: dummyUUID, Email: "subscriber@example.org", Name: "Sample Subscriber", Attribs: models.JSON{}}
	if _, err := a.autoMessage(denmaAutomation{UUID: dummyUUID, Name: f.Name, Subject: f.Subject}, tpl, sample); err != nil {
		return bad(err.Error())
	}
	return nil
}

// DenmaSaveAutomation creates (POST) or updates (PUT) an automation.
func (a *App) DenmaSaveAutomation(c echo.Context) error {
	if !denmaAutomationsOn(a) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	var f denmaAutoForm
	if err := c.Bind(&f); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := a.validateAutomation(c, &f); err != nil {
		return err
	}

	tx, err := a.db.Beginx()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	defer tx.Rollback()

	var id int
	if c.Request().Method == http.MethodPost {
		err := tx.Get(&id, `INSERT INTO denma_automations
			(uuid, name, template_id, subject, from_email, delay_minutes, active, active_since)
			VALUES ($1, $2, $3, $4, $5, $6, $7, CASE WHEN $7 THEN NOW() END) RETURNING id`,
			uuid.Must(uuid.NewV4()).String(), f.Name, f.TemplateID, f.Subject, f.FromEmail, f.DelayMinutes, f.Active)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
	} else {
		id = getID(c)
		res, err := tx.Exec(`UPDATE denma_automations SET name = $2, template_id = $3, subject = $4,
			from_email = $5, delay_minutes = $6, updated_at = NOW() WHERE id = $1`,
			id, f.Name, f.TemplateID, f.Subject, f.FromEmail, f.DelayMinutes)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return echo.NewHTTPError(http.StatusNotFound, "That automation doesn't exist.")
		}
	}

	// Its lists. A list it keeps keeps its row; only the new ones are added.
	if _, err := tx.Exec(`DELETE FROM denma_automation_lists WHERE automation_id = $1 AND NOT (list_id = ANY($2))`, id, pq.Array(f.ListIDs)); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if _, err := tx.Exec(`INSERT INTO denma_automation_lists (automation_id, list_id)
		SELECT $1, UNNEST($2::INT[]) ON CONFLICT DO NOTHING`, id, pq.Array(f.ListIDs)); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{map[string]int{"id": id}})
}

// DenmaSetAutomationStatus turns an automation on or off. Turned on, it's for
// people who join its lists from then on.
func (a *App) DenmaSetAutomationStatus(c echo.Context) error {
	if !denmaAutomationsOn(a) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	var req struct {
		Active bool `json:"active"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	var auto denmaAutomation
	if err := a.db.Get(&auto, denmaAutoSelectSQL+` WHERE a.id = $1`, getID(c)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, "That automation doesn't exist.")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if req.Active && (len(auto.ListIDs) == 0 || !auto.TemplateID.Valid) {
		return echo.NewHTTPError(http.StatusBadRequest, "Its lists or its template were deleted. Choose others first.")
	}
	if _, err := a.db.Exec(`UPDATE denma_automations SET active = $2, updated_at = NOW(),
		active_since = CASE WHEN $2 AND NOT active THEN NOW() ELSE active_since END WHERE id = $1`, auto.ID, req.Active); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{true})
}

// DenmaDeleteAutomation deletes an automation, and the record of whom it sent.
func (a *App) DenmaDeleteAutomation(c echo.Context) error {
	if !denmaAutomationsOn(a) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if _, err := a.db.Exec(`DELETE FROM denma_automations WHERE id = $1`, getID(c)); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{true})
}

// DenmaTestAutomation sends the automation page's e-mail to an address: as
// to that subscriber, if there's one, or to a sample one.
func (a *App) DenmaTestAutomation(c echo.Context) error {
	if !denmaAutomationsOn(a) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	var f denmaAutoForm
	if err := c.Bind(&f); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := a.validateAutomation(c, &f); err != nil {
		return err
	}
	email, err := a.importer.SanitizeEmail(f.TestEmail)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "Enter the address to send the test to.")
	}
	if a.emailMsgr == nil {
		return echo.NewHTTPError(http.StatusBadRequest, "There's no e-mail server set up to send it.")
	}

	sub := models.Subscriber{UUID: dummyUUID, Email: email, Name: "Sample Subscriber", Attribs: models.JSON{}}
	var found models.Subscriber
	if err := a.db.Get(&found, `SELECT id, created_at, updated_at, uuid, email, name, attribs, status FROM subscribers WHERE LOWER(email) = LOWER($1)`, email); err == nil {
		sub = found
	}
	tpl, err := a.autoTemplate(f.TemplateID, true)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	msg, err := a.autoMessage(denmaAutomation{UUID: dummyUUID, Name: f.Name, Subject: "[Test] " + f.Subject, FromEmail: f.FromEmail}, tpl, sub)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	msg.To = []string{email}
	if err := a.manager.PushMessage(msg); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{true})
}

// ViewDenmaAutomationMessage shows an automation's e-mail to a subscriber
// (its "view in browser" link).
func (a *App) ViewDenmaAutomationMessage(c echo.Context) error {
	notFound := func() error {
		return c.Render(http.StatusNotFound, tplMessage,
			makeMsgTpl(a.i18n.T("public.notFoundTitle"), "", a.i18n.T("public.campaignNotFound")))
	}
	var auto denmaAutomation
	if err := a.db.Get(&auto, denmaAutoSelectSQL+` WHERE a.uuid = $1`, c.Param("autoUUID")); err != nil || !auto.TemplateID.Valid {
		return notFound()
	}
	var sub models.Subscriber
	if err := a.db.Get(&sub, `SELECT id, created_at, updated_at, uuid, email, name, attribs, status FROM subscribers WHERE uuid = $1`, c.Param("subUUID")); err != nil {
		return notFound()
	}
	tpl, err := a.autoTemplate(int(auto.TemplateID.Int), false)
	if err != nil {
		return notFound()
	}
	msg, err := a.autoMessage(auto, tpl, sub)
	if err != nil {
		return notFound()
	}
	return c.HTMLBlob(http.StatusOK, msg.Body)
}

// Wait is the automation's wait, in words: "right away", "after 3 days".
func (d denmaAutomation) Wait() string {
	n, unit := d.DelayMinutes, "minute"
	switch {
	case n == 0:
		return "right away"
	case n%1440 == 0:
		n, unit = n/1440, "day"
	case n%60 == 0:
		n, unit = n/60, "hour"
	}
	if n != 1 {
		unit += "s"
	}
	return fmt.Sprintf("after %d %s", n, unit)
}

// Lists is the automation's lists, in words: "A", "A or B", "A, B or C".
func (d denmaAutomation) Lists() string {
	n := len(d.ListNames)
	if n < 2 {
		return strings.Join(d.ListNames, "")
	}
	return strings.Join(d.ListNames[:n-1], ", ") + " or " + d.ListNames[n-1]
}
