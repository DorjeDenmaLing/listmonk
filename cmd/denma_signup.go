package main

// denma: sign-up webhooks, a center's own way for the forms on its website
// to sign people up: Gravity Forms' Webhooks add-on, or any form or service
// that can POST. A center's admins (center:manage) make them in its Config
// -> Webhooks (views/denma-webhooks.html), each with a name,
// the lists it signs people up to, optionally tags to give them (the
// center's, cmd/denma_tags.go), and a secret address of its own,
// <center>/signup/<token>, to paste into the form.
//
// A webhook does what the public subscription form does, without it having
// to be on (the hub's setting, for every center), and for private lists too:
//
//   - Its lists are double opt-in only, so that whoever signs up confirms by
//     e-mail before anything else is sent; an automation
//     (cmd/denma_automations.go) can move them on to a single opt-in list
//     once they've confirmed, as every new center's does.
//   - A new address is added, and sent the confirmation e-mail, with a consent
//     record in its attributes (consent_at, consent_source, and the form's
//     form_id, entry_id and page_url if sent), as the web-signup Lambda does.
//   - An existing subscriber gets the lists, and the confirmation if they're
//     not confirmed on them yet; someone who unsubscribed is sent the
//     re-subscription confirmation (cmd/denma_resubscribe.go), unless they
//     complained or bounced.
//   - Its tags are added to theirs, new subscriber or not. A campaign sent to
//     a tag reaches them only once they've confirmed.
//   - The opt-in checks apply (the shared blocklist, the domain check, at most
//     5 opt-in e-mails an address a day), and each webhook takes at most
//     denmaSignupPerHour sign-ups an hour.
//
// Fields, JSON or form-encoded: email (required); name, or first_name and
// last_name; consent (if sent, it has to be ticked); form_id, entry_id and
// page_url (kept with the consent record). Callers asking for HTML (a plain
// form in a browser) get listmonk's "check your e-mail" page, and a trusted
// "next" address (Settings -> Security) is followed; others get JSON. The
// answer never says whether the address was there already, or why someone
// can't be signed up again.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/knadh/listmonk/internal/utils"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

const (
	denmaSignupPerHour  = 200     // sign-ups a webhook takes an hour
	denmaSignupMaxHooks = 20      // webhooks a center can have
	denmaSignupMaxLists = 20      // lists a webhook can have
	denmaSignupMaxTags  = 20      // tags a webhook can have
	denmaSignupMaxBody  = 1 << 16 // bytes
)

// denmaSignupSQL makes the webhooks' tables, in a center (denmaFeaturesSQL).
const denmaSignupSQL = `
CREATE TABLE IF NOT EXISTS denma_signup_hooks (
    id           SERIAL PRIMARY KEY,
    name         TEXT NOT NULL,
    token        TEXT NOT NULL UNIQUE,
    uses         INTEGER NOT NULL DEFAULT 0,
    last_used_at TIMESTAMP WITH TIME ZONE NULL,
    created_at   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS denma_signup_hook_lists (
    hook_id INTEGER NOT NULL REFERENCES denma_signup_hooks(id) ON DELETE CASCADE,
    list_id INTEGER NOT NULL REFERENCES lists(id) ON DELETE CASCADE,
    PRIMARY KEY (hook_id, list_id)
);
-- The tags it gives (the center's: renaming or deleting one changes them).
ALTER TABLE denma_signup_hooks ADD COLUMN IF NOT EXISTS tags TEXT[] NOT NULL DEFAULT '{}';
`

// denmaSignupHook is a webhook, for Config -> Webhooks.
type denmaSignupHook struct {
	ID         int            `db:"id" json:"id"`
	Name       string         `db:"name" json:"name"`
	Token      string         `db:"token" json:"-"`
	URL        string         `db:"-" json:"url"`
	ListIDs    pq.Int64Array  `db:"list_ids" json:"list_ids"`
	Tags       pq.StringArray `db:"tags" json:"tags"`
	Uses       int            `db:"uses" json:"uses"`
	LastUsedAt sql.NullTime   `db:"last_used_at" json:"-"`
	LastUsed   *time.Time     `db:"-" json:"last_used_at"`
	CreatedAt  time.Time      `db:"created_at" json:"created_at"`
}

const denmaSignupHookSQL = `SELECT h.id, h.name, h.token, h.tags, h.uses, h.last_used_at, h.created_at,
	ARRAY(SELECT list_id FROM denma_signup_hook_lists hl WHERE hl.hook_id = h.id ORDER BY list_id) AS list_ids
	FROM denma_signup_hooks h`

func initDenmaSignupAPIHandlers(g *echo.Group, a *App) {
	pm := func(h echo.HandlerFunc) echo.HandlerFunc { return a.auth.Perm(h, denmaCenterPerm) }
	g.GET("/api/denma/center/signup-hooks", pm(a.DenmaGetSignupHooks))
	g.POST("/api/denma/center/signup-hooks", pm(a.DenmaCreateSignupHook))
	g.PUT("/api/denma/center/signup-hooks/:id", pm(a.DenmaUpdateSignupHook))
	g.POST("/api/denma/center/signup-hooks/:id/key", pm(a.DenmaNewSignupHookKey))
	g.DELETE("/api/denma/center/signup-hooks/:id", pm(a.DenmaDeleteSignupHook))
}

// initDenmaSignupPublicHandlers registers the webhooks' address, in a center.
func initDenmaSignupPublicHandlers(g *echo.Group, a *App) {
	if denmaHub != nil && a.ko.String("denma.center") != "" {
		g.POST("/signup/:token", a.DenmaSignup)
	}
}

// signupHooks returns the center's webhooks, oldest first.
func (a *App) signupHooks() ([]denmaSignupHook, error) {
	out := []denmaSignupHook{}
	if err := a.db.Select(&out, denmaSignupHookSQL+` ORDER BY h.id`); err != nil {
		return nil, err
	}
	for i := range out {
		a.signupHookView(&out[i])
	}
	return out, nil
}

func (a *App) signupHookView(h *denmaSignupHook) {
	h.URL = strings.TrimSuffix(a.urlCfg.RootURL, "/") + "/signup/" + h.Token
	if h.LastUsedAt.Valid {
		h.LastUsed = &h.LastUsedAt.Time
	}
	if h.ListIDs == nil {
		h.ListIDs = pq.Int64Array{}
	}
	if h.Tags == nil {
		h.Tags = pq.StringArray{}
	}
}

func (a *App) signupHook(id int) (denmaSignupHook, error) {
	var h denmaSignupHook
	if err := a.db.Get(&h, denmaSignupHookSQL+` WHERE h.id = $1`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return h, echo.NewHTTPError(http.StatusNotFound, "That webhook doesn't exist (any more).")
		}
		return h, echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	a.signupHookView(&h)
	return h, nil
}

// DenmaGetSignupHooks lists the center's webhooks.
func (a *App) DenmaGetSignupHooks(c echo.Context) error {
	if err := a.inCenter(); err != nil {
		return err
	}
	out, err := a.signupHooks()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{out})
}

// denmaSignupForm is a webhook as Config -> Webhooks sends it.
type denmaSignupForm struct {
	Name    string   `json:"name"`
	ListIDs []int    `json:"list_ids"`
	Tags    []string `json:"tags"`
}

// checkSignupForm trims and checks a webhook: a name, one or more of the
// center's double opt-in lists, and any of its tags.
func (a *App) checkSignupForm(c echo.Context) (denmaSignupForm, error) {
	var f denmaSignupForm
	if err := c.Bind(&f); err != nil {
		return f, echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	f.Name = strings.TrimSpace(f.Name)
	if !strHasLen(f.Name, 1, 200) {
		return f, echo.NewHTTPError(http.StatusBadRequest, "Give the webhook a name, such as the form it's for.")
	}
	if len(f.ListIDs) == 0 {
		return f, echo.NewHTTPError(http.StatusBadRequest, "Choose at least one list.")
	}
	if len(f.ListIDs) > denmaSignupMaxLists {
		return f, echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Choose at most %d lists.", denmaSignupMaxLists))
	}
	var lists []denmaOption
	if err := a.db.Select(&lists, `SELECT id, name, optin::TEXT AS optin FROM lists WHERE id = ANY($1)`, pq.Array(f.ListIDs)); err != nil {
		return f, echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if len(lists) != len(dedupInts(f.ListIDs)) {
		return f, echo.NewHTTPError(http.StatusBadRequest, "One of the lists doesn't exist (any more).")
	}
	for _, l := range lists {
		if l.Optin != string(models.ListOptinDouble) {
			return f, echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf(
				"%s is single opt-in. A webhook's lists have to be double opt-in, so that whoever signs up confirms by e-mail first. To end up on a single opt-in list, use a double opt-in list here, and an automation (Automations) to move them on once they've confirmed.", l.Name))
		}
	}
	f.ListIDs = dedupInts(f.ListIDs)
	if f.Tags = denmaNormTags(f.Tags); len(f.Tags) > denmaSignupMaxTags {
		return f, echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Choose at most %d tags.", denmaSignupMaxTags))
	}
	if err := a.denmaUnknownTags(f.Tags); err != nil {
		return f, err
	}
	return f, nil
}

func dedupInts(in []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(in))
	for _, n := range in {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// saveSignupLists sets a webhook's lists.
func saveSignupLists(tx *sql.Tx, id int, listIDs []int) error {
	if _, err := tx.Exec(`DELETE FROM denma_signup_hook_lists WHERE hook_id = $1`, id); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO denma_signup_hook_lists (hook_id, list_id) SELECT $1, UNNEST($2::INT[])`, id, pq.Array(listIDs))
	return err
}

// DenmaCreateSignupHook makes a webhook, with a new secret address.
func (a *App) DenmaCreateSignupHook(c echo.Context) error {
	if err := a.inCenter(); err != nil {
		return err
	}
	f, err := a.checkSignupForm(c)
	if err != nil {
		return err
	}
	var n int
	if err := a.db.Get(&n, `SELECT COUNT(*) FROM denma_signup_hooks`); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if n >= denmaSignupMaxHooks {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("A center can have at most %d webhooks.", denmaSignupMaxHooks))
	}
	token, err := utils.GenerateRandomString(32)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	tx, err := a.db.Begin()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	defer tx.Rollback()
	var id int
	if err := tx.QueryRow(`INSERT INTO denma_signup_hooks (name, token, tags) VALUES ($1, $2, $3) RETURNING id`, f.Name, token, pq.Array(f.Tags)).Scan(&id); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if err := saveSignupLists(tx, id, f.ListIDs); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	h, err := a.signupHook(id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, okResp{h})
}

// DenmaUpdateSignupHook renames a webhook or changes its lists or tags.
func (a *App) DenmaUpdateSignupHook(c echo.Context) error {
	if err := a.inCenter(); err != nil {
		return err
	}
	id, _ := strconv.Atoi(c.Param("id"))
	if _, err := a.signupHook(id); err != nil {
		return err
	}
	f, err := a.checkSignupForm(c)
	if err != nil {
		return err
	}
	tx, err := a.db.Begin()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE denma_signup_hooks SET name = $2, tags = $3, updated_at = NOW() WHERE id = $1`, id, f.Name, pq.Array(f.Tags)); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if err := saveSignupLists(tx, id, f.ListIDs); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	h, err := a.signupHook(id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, okResp{h})
}

// DenmaNewSignupHookKey gives a webhook a new secret address; the old one
// stops working.
func (a *App) DenmaNewSignupHookKey(c echo.Context) error {
	if err := a.inCenter(); err != nil {
		return err
	}
	id, _ := strconv.Atoi(c.Param("id"))
	token, err := utils.GenerateRandomString(32)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	res, err := a.db.Exec(`UPDATE denma_signup_hooks SET token = $2, updated_at = NOW() WHERE id = $1`, id, token)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return echo.NewHTTPError(http.StatusNotFound, "That webhook doesn't exist (any more).")
	}
	h, err := a.signupHook(id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, okResp{h})
}

// DenmaDeleteSignupHook deletes a webhook; its address stops working.
func (a *App) DenmaDeleteSignupHook(c echo.Context) error {
	if err := a.inCenter(); err != nil {
		return err
	}
	id, _ := strconv.Atoi(c.Param("id"))
	if _, err := a.db.Exec(`DELETE FROM denma_signup_hooks WHERE id = $1`, id); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{true})
}

// denmaSignupCount is each webhook's sign-ups this hour (center/ID), for
// denmaSignupPerHour.
var denmaSignupCount = struct {
	sync.Mutex
	m map[string]*denmaSignupWindow
}{m: map[string]*denmaSignupWindow{}}

type denmaSignupWindow struct {
	start time.Time
	n     int
}

// signupAllowed counts a sign-up against its webhook's hourly limit.
func (a *App) signupAllowed(id int) bool {
	key := fmt.Sprintf("%s/%d", a.ko.String("denma.center"), id)
	now := time.Now()
	denmaSignupCount.Lock()
	defer denmaSignupCount.Unlock()
	w := denmaSignupCount.m[key]
	if w == nil || now.Sub(w.start) >= time.Hour {
		w = &denmaSignupWindow{start: now}
		denmaSignupCount.m[key] = w
	}
	w.n++
	return w.n <= denmaSignupPerHour
}

var reDenmaControl = regexp.MustCompile(`[\x00-\x1f\x7f]`)

// denmaSignupField is a field's value on one line, trimmed to limit characters.
func denmaSignupField(fields map[string]string, key string, limit int) string {
	v := strings.TrimSpace(reDenmaControl.ReplaceAllString(fields[key], " "))
	if r := []rune(v); len(r) > limit {
		v = string(r[:limit])
	}
	return v
}

// signupFields reads a webhook's fields: a JSON object, or a form.
func signupFields(c echo.Context) (map[string]string, error) {
	req := c.Request()
	req.Body = http.MaxBytesReader(c.Response(), req.Body, denmaSignupMaxBody)
	out := map[string]string{}
	if strings.Contains(req.Header.Get(echo.HeaderContentType), "json") {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		var raw map[string]any
		if err := json.Unmarshal(b, &raw); err != nil {
			return nil, errors.New("the body isn't a JSON object")
		}
		for k, v := range raw {
			switch x := v.(type) {
			case string:
				out[k] = x
			case float64, bool:
				out[k] = fmt.Sprint(x)
			case []any: // a checkbox's choices
				parts := []string{}
				for _, p := range x {
					if s, ok := p.(string); ok && s != "" {
						parts = append(parts, s)
					}
				}
				out[k] = strings.Join(parts, ", ")
			}
		}
		return out, nil
	}
	form, err := c.FormParams()
	if err != nil {
		return nil, err
	}
	for k, v := range form {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out, nil
}

// DenmaSignup is a webhook's address: someone signing up on the center's
// website.
func (a *App) DenmaSignup(c echo.Context) error {
	html := strings.Contains(c.Request().Header.Get(echo.HeaderAccept), "text/html")
	reply := func(code int, msg string) error {
		if html {
			title := a.i18n.T("public.subTitle")
			if code >= 400 {
				title = a.i18n.T("public.errorTitle")
			}
			return c.Render(code, tplMessage, makeMsgTpl(title, "", msg))
		}
		if code >= 400 {
			return c.JSON(code, map[string]string{"message": msg})
		}
		return c.JSON(code, okResp{true})
	}

	// The webhook, by its secret.
	token := c.Param("token")
	var h denmaSignupHook
	err := a.db.Get(&h, denmaSignupHookSQL+` WHERE h.token = $1`, token)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			a.log.Printf("denma: error finding a sign-up webhook: %v", err)
			return reply(http.StatusInternalServerError, a.i18n.T("public.errorProcessingRequest"))
		}
		return reply(http.StatusNotFound, "This sign-up address isn't in use (any more).")
	}
	logf := func(result string) {
		a.log.Printf("denma: sign-up webhook %d (%s): %s", h.ID, h.Name, result) // no addresses
	}

	fields, err := signupFields(c)
	if err != nil {
		logf("unreadable request")
		return reply(http.StatusBadRequest, "The request couldn't be read: "+err.Error())
	}
	// A bot filling listmonk's form's hidden field.
	if fields["nonce"] != "" {
		logf("skipped: a bot")
		return reply(http.StatusOK, a.i18n.T("public.subOptinPending"))
	}
	if _, ok := fields["consent"]; ok {
		switch strings.ToLower(denmaSignupField(fields, "consent", 100)) {
		case "", "0", "false", "no", "off", "unchecked":
			logf("no consent")
			return reply(http.StatusBadRequest, "Tick the box to agree to receive our e-mails.")
		}
	}
	email, err := a.importer.SanitizeEmail(denmaSignupField(fields, "email", 1000))
	if err != nil {
		logf("invalid e-mail address")
		return reply(http.StatusBadRequest, a.i18n.T("subscribers.invalidEmail"))
	}
	name := denmaSignupField(fields, "name", 200)
	if name == "" {
		name = strings.TrimSpace(denmaSignupField(fields, "first_name", 100) + " " + denmaSignupField(fields, "last_name", 100))
	}
	if name == "" {
		name = strings.Split(email, "@")[0]
	}

	// Its lists that still exist and are double opt-in.
	var listIDs []int
	if err := a.db.Select(&listIDs, `SELECT id FROM lists WHERE id = ANY($1) AND optin = 'double' ORDER BY id`, h.ListIDs); err != nil {
		a.log.Printf("denma: error reading sign-up webhook %d's lists: %v", h.ID, err)
		return reply(http.StatusInternalServerError, a.i18n.T("public.errorProcessingRequest"))
	}
	if len(listIDs) == 0 {
		logf("no double opt-in lists")
		return reply(http.StatusGone, "This sign-up form has no lists to sign up to.")
	}
	if !a.signupAllowed(h.ID) {
		logf("too many sign-ups this hour")
		return reply(http.StatusTooManyRequests, "Too many sign-ups for now. Try again in an hour.")
	}
	if _, err := a.db.Exec(`UPDATE denma_signup_hooks SET uses = uses + 1, last_used_at = NOW() WHERE id = $1`, h.ID); err != nil {
		a.log.Printf("denma: error counting sign-up webhook %d's use: %v", h.ID, err)
	}

	// The consent record, as the web-signup Lambda keeps it.
	website := map[string]any{}
	for k, n := range map[string]int{"form_id": 20, "entry_id": 20, "page_url": 500} {
		if v := denmaSignupField(fields, k, n); v != "" {
			website[k] = v
		}
	}
	consent := map[string]any{
		"consent_at":     time.Now().UTC().Format(time.RFC3339),
		"consent_source": "sign-up webhook: " + h.Name,
	}
	if len(website) > 0 {
		consent["website"] = website
	}

	result, err := a.signupSubscribe(email, name, listIDs, consent)
	if err != nil {
		var he *echo.HTTPError
		if errors.As(err, &he) && he.Code < 500 {
			logf("refused: " + fmt.Sprint(he.Message))
			return reply(he.Code, fmt.Sprint(he.Message)) // the opt-in limit, an address that can't receive mail
		}
		a.log.Printf("denma: error in sign-up webhook %d: %v", h.ID, err)
		return reply(http.StatusInternalServerError, a.i18n.T("public.errorProcessingRequest"))
	}
	// Its tags (the trigger keeps them to the center's).
	if len(h.Tags) > 0 {
		if _, err := a.db.Exec(`UPDATE subscribers SET attribs = `+denmaTagsSet("TRUE", "$2")+`, updated_at = NOW() WHERE LOWER(email) = LOWER($1)`,
			email, h.Tags); err != nil {
			a.log.Printf("denma: error tagging a sign-up from webhook %d: %v", h.ID, err)
		}
	}
	logf(result)

	// A trusted next page (Settings -> Security), as listmonk's form has.
	if next := strings.TrimSpace(fields["next"]); next != "" && html {
		for _, d := range a.cfg.Security.TrustedURLs {
			if d != "*" && next == d {
				if u, err := url.Parse(next); err == nil && u.IsAbs() {
					return c.Redirect(http.StatusSeeOther, next)
				}
			}
		}
	}
	return reply(http.StatusOK, a.i18n.T("public.subOptinPending"))
}

// signupSubscribe signs someone up to lists (double opt-in), returning what
// became of it, for the log.
func (a *App) signupSubscribe(email, name string, listIDs []int, consent map[string]any) (string, error) {
	optin := func(sub models.Subscriber, hasOptin bool) (string, error) {
		if hasOptin {
			return "confirmation sent", nil
		}
		// listmonk sends it only with Settings -> Privacy's "send opt-in
		// confirmation" on; it's sent anyway, as a webhook's lists are all
		// double opt-in.
		if !a.ko.Bool("app.send_optin_confirmation") {
			n, err := a.fnOptinNotify(sub, listIDs)
			if err != nil {
				return "", err
			}
			if n > 0 {
				return "confirmation sent", nil
			}
		}
		return "already confirmed", nil
	}

	sub, hasOptin, err := a.core.InsertSubscriber(models.Subscriber{
		Name: name, Email: email, Status: models.SubscriberStatusEnabled, Attribs: models.JSON(consent),
	}, listIDs, nil, false, true)
	if err == nil && sub.Status == models.SubscriberStatusBlockListed {
		// An address that opted out before, and was deleted
		// (cmd/denma_optouts.go): added blocklisted.
		status, err := a.denmaResubscribe(sub, listIDs, consent)
		return "opted out before: " + status, err
	}
	if err == nil {
		r, err := optin(sub, hasOptin)
		return "new subscriber: " + r, err
	}
	var he *echo.HTTPError
	if !errors.As(err, &he) || he.Code != http.StatusConflict {
		return "", err
	}

	// They're here already.
	sub, err = a.core.GetSubscriber(0, "", email)
	if err != nil {
		return "", err
	}
	if sub.Status == models.SubscriberStatusBlockListed { // cmd/denma_resubscribe.go
		status, err := a.denmaResubscribe(sub, listIDs, consent)
		return "blocklisted: " + status, err
	}
	sub, hasOptin, err = a.core.UpdateSubscriberWithLists(sub.ID, sub, listIDs, nil, false, false, true, nil, true)
	if err != nil {
		return "", err
	}
	r, err := optin(sub, hasOptin)
	return "existing subscriber: " + r, err
}
