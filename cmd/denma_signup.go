package main

// denma: sign-up webhooks, the way other systems subscribe people to a
// center: they don't send e-mail (that's the sending API, cmd/denma_api.go).
// Each is made in the center's Config -> Webhooks (views/denma-webhooks.html),
// with a name, the lists it signs people up to, tags to give everyone it
// signs up (the center's, cmd/denma_tags.go), and the tags a request may give
// (request_tags). There are two kinds:
//
//   - Form webhooks, for the forms on the center's website (Gravity Forms'
//     Webhooks add-on, or any form that can POST), made by its admins
//     (center:manage), at a secret address of their own, <center>/signup/<token>.
//     Its address can end up in a page, so it can only start a double opt-in:
//     its lists are double opt-in only, and whoever signs up confirms by e-mail
//     before anything else is sent; an automation (cmd/denma_automations.go)
//     can move them on to a single opt-in list once they've confirmed.
//   - Server webhooks, for systems that vouch for the person's consent, such
//     as rg-sync (a Retreat Guru registration with the newsletter box ticked),
//     made only by superadmins, at <center>/signup/server with the secret as
//     "Authorization: Bearer <secret>". It can subscribe people at once with
//     "confirmed" and a consent record, to single opt-in lists too, and
//     re-subscribe someone who unsubscribed (cmd/denma_resubscribe.go), at
//     most denmaSignupResubPerDay a day in the center.
//
// What a request gives is written once (attributes, name, its tags): only when
// it adds the person. An existing subscriber gets the lists and the webhook's
// own tags, and their record is unchanged. Server webhooks are the exception:
// they add to what the subscriber has, never replacing a value
// (appendSubscriber), so that each registration's consent is kept.
//
// Attributes the center keeps itself (denmaReservedAttribs) can't be given: a
// Form webhook drops them (a real sign-up is never refused for a form's
// field names), a Server webhook refuses them, but for "consents".
//
// The opt-in checks apply (the shared blocklist, the domain check, at most 5
// opt-in e-mails an address a day). A Form webhook takes at most its per_hour
// sign-ups an hour, and the admins are told when it's reached; a Server
// webhook at most denmaSignupServerPerMinute requests a minute. Tokens and
// secrets are kept as their SHA-256, and shown once. The answer never says
// whether the address was there already, or why someone can't be signed up
// again. Log lines carry no addresses.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/internal/utils"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

const (
	denmaSignupForm   = "form"
	denmaSignupServer = "server"

	denmaSignupDefaultPerHour  = 50      // a Form webhook's sign-ups an hour, to start with
	denmaSignupMaxPerHour      = 200     // the most it can be given
	denmaSignupServerPerMinute = 600     // a Server webhook's requests a minute
	denmaSignupResubPerDay     = 10      // people Server webhooks re-subscribe at once in a center, in 24 hours
	denmaSignupMaxHooks        = 20      // webhooks a center can have
	denmaSignupMaxLists        = 20      // lists a webhook can have
	denmaSignupMaxTags         = 20      // tags a webhook can have, of each kind
	denmaSignupMaxBody         = 1 << 16 // bytes
	denmaSignupSecretPrefix    = "lms_"  // a Server webhook's secret, for secret scanners
	denmaSignupHookCtx         = "denma_signup_hook"
)

// denmaReservedAttribs are attributes the center keeps itself: the consent
// record and re-subscribing (cmd/denma_resubscribe.go), imports (automations
// don't e-mail them), and tags (given on their own); and any starting
// auto_blocklist_ (the domain check, cmd/denma_emailcheck.go) or denma_. No
// request can give them, but a Server webhook's consents. Admins' changes
// keep a subscriber's (denmaKeepCenterAttribs), as imports do
// (denma_keep_center_attribs, cmd/denma_features.go).
var denmaReservedAttribs = []string{"consent_at", "consent_source", "consent_confirmed_at", "consents",
	"resubscribed_at", "imported_at", "tags"}

func denmaIsReservedAttrib(k string) bool {
	return slices.Contains(denmaReservedAttribs, k) || strings.HasPrefix(k, "auto_blocklist_") || strings.HasPrefix(k, "denma_")
}

// denmaReservedAttrib returns the first reserved attribute (by name) in
// attribs, or "". consents is allowed with allowConsents.
func denmaReservedAttrib(attribs map[string]any, allowConsents bool) string {
	keys := make([]string, 0, len(attribs))
	for k := range attribs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if denmaIsReservedAttrib(k) && !(allowConsents && k == "consents") {
			return k
		}
	}
	return ""
}

// denmaReservedAttribMsg says why an attribute can't be given.
func denmaReservedAttribMsg(k string) string {
	if k == "tags" {
		return `"tags" can't be in "attribs": give them as "tags".`
	}
	return fmt.Sprintf(`"attribs" can't give %q: it's an attribute the center keeps itself.`, k)
}

// denmaKeepCenterAttribs keeps the center's attributes (the reserved ones but
// tags) out of an admin's change: those given are dropped, and subscriber
// id's (0 for a new one) are kept.
func (a *App) denmaKeepCenterAttribs(id int, attribs *models.JSON) error {
	for k := range *attribs {
		if k != "tags" && denmaIsReservedAttrib(k) {
			delete(*attribs, k)
		}
	}
	if id == 0 {
		return nil
	}
	var cur models.JSON
	if err := a.db.Get(&cur, `SELECT CASE WHEN jsonb_typeof(attribs) = 'object' THEN attribs ELSE '{}' END FROM subscribers WHERE id = $1`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // listmonk says it's not found
		}
		return err
	}
	for k, v := range cur {
		if k != "tags" && denmaIsReservedAttrib(k) {
			if *attribs == nil {
				*attribs = models.JSON{}
			}
			(*attribs)[k] = v
		}
	}
	return nil
}

// denmaSignupBadSecrets limits requests to /signup/server with secrets that
// don't work, by client address.
var denmaSignupBadSecrets = newDenmaThrottle(20, 15*time.Minute)

// denmaSignupSQL makes the webhooks' tables, in a center (denmaFeaturesSQL).
const denmaSignupSQL = `
CREATE TABLE IF NOT EXISTS denma_signup_hooks (
    id           SERIAL PRIMARY KEY,
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE, -- SHA-256 of its token (form) or secret (server), hex
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
-- Since features version 22: tokens kept as their SHA-256 (shown once).
ALTER TABLE denma_signup_hooks ADD COLUMN IF NOT EXISTS token_hint TEXT NOT NULL DEFAULT '';
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = current_schema()
        AND table_name = 'denma_signup_hooks' AND column_name = 'token') THEN
        ALTER TABLE denma_signup_hooks ADD COLUMN token_hash TEXT NULL;
        UPDATE denma_signup_hooks SET token_hash = encode(sha256(convert_to(token, 'UTF8')), 'hex'), token_hint = right(token, 4);
        ALTER TABLE denma_signup_hooks ALTER COLUMN token_hash SET NOT NULL;
        ALTER TABLE denma_signup_hooks DROP COLUMN token;
        ALTER TABLE denma_signup_hooks ADD CONSTRAINT denma_signup_hooks_token_hash_key UNIQUE (token_hash);
    END IF;
END $$;
-- Its kind, the tags a request may give, and each kind's settings.
ALTER TABLE denma_signup_hooks ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'form' CHECK (kind IN ('form', 'server'));
ALTER TABLE denma_signup_hooks ADD COLUMN IF NOT EXISTS request_tags TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE denma_signup_hooks ADD COLUMN IF NOT EXISTS per_hour INTEGER NOT NULL DEFAULT 50;      -- form
ALTER TABLE denma_signup_hooks ADD COLUMN IF NOT EXISTS create_tags BOOLEAN NOT NULL DEFAULT FALSE; -- server
ALTER TABLE denma_signup_hooks ADD COLUMN IF NOT EXISTS allowed_from TEXT[] NOT NULL DEFAULT '{}'; -- server
ALTER TABLE denma_signup_hooks ADD COLUMN IF NOT EXISTS created_by TEXT NOT NULL DEFAULT '';
ALTER TABLE denma_signup_hooks ADD COLUMN IF NOT EXISTS old_token_hash TEXT NULL;                 -- server: the secret replaced
ALTER TABLE denma_signup_hooks ADD COLUMN IF NOT EXISTS old_token_until TIMESTAMP WITH TIME ZONE NULL;
ALTER TABLE denma_signup_hooks ADD COLUMN IF NOT EXISTS last_ip TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS denma_signup_hooks_old_hash ON denma_signup_hooks (old_token_hash) WHERE old_token_hash IS NOT NULL;
-- The last alert about each webhook, by kind (signupAlert).
CREATE TABLE IF NOT EXISTS denma_signup_alerts (
    hook_id INTEGER NOT NULL REFERENCES denma_signup_hooks(id) ON DELETE CASCADE,
    kind    TEXT NOT NULL,
    sent_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (hook_id, kind)
);
-- Re-subscriptions at once ("confirmed") by Server webhooks, for the
-- center's denmaSignupResubPerDay. Kept two days.
CREATE TABLE IF NOT EXISTS denma_resub_usage (
    id         BIGSERIAL PRIMARY KEY,
    hook_id    INTEGER NULL REFERENCES denma_signup_hooks(id) ON DELETE SET NULL,
    n          INTEGER NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS denma_resub_usage_created ON denma_resub_usage (created_at);
-- Lists Server webhooks lost (deleted by the center's admins), for the
-- superadmins who made them to be told (signupLostLists).
CREATE TABLE IF NOT EXISTS denma_signup_lost (
    hook_id   INTEGER NOT NULL REFERENCES denma_signup_hooks(id) ON DELETE CASCADE,
    list_name TEXT NOT NULL,
    lost_at   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE OR REPLACE FUNCTION denma_signup_list_gone() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    INSERT INTO denma_signup_lost (hook_id, list_name)
        SELECT hl.hook_id, OLD.name FROM denma_signup_hook_lists hl
        JOIN denma_signup_hooks h ON h.id = hl.hook_id AND h.kind = 'server' WHERE hl.list_id = OLD.id;
    RETURN OLD;
END $$;
DROP TRIGGER IF EXISTS denma_signup_list_gone ON lists;
CREATE TRIGGER denma_signup_list_gone BEFORE DELETE ON lists
    FOR EACH ROW EXECUTE FUNCTION denma_signup_list_gone();
`

// denmaSignupHook is a webhook, for Config -> Webhooks.
type denmaSignupHook struct {
	ID          int            `db:"id" json:"id"`
	Name        string         `db:"name" json:"name"`
	Kind        string         `db:"kind" json:"kind"`
	TokenHash   string         `db:"token_hash" json:"-"`
	TokenHint   string         `db:"token_hint" json:"token_hint"`
	ListIDs     pq.Int64Array  `db:"list_ids" json:"list_ids"`
	Tags        pq.StringArray `db:"tags" json:"tags"`
	RequestTags pq.StringArray `db:"request_tags" json:"request_tags"`
	PerHour     int            `db:"per_hour" json:"per_hour"`
	CreateTags  bool           `db:"create_tags" json:"create_tags"`
	AllowedFrom pq.StringArray `db:"allowed_from" json:"allowed_from"`
	CreatedBy   string         `db:"created_by" json:"created_by"`
	OldUntil    *time.Time     `db:"old_token_until" json:"old_token_until"`
	Uses        int            `db:"uses" json:"uses"`
	LastUsedAt  sql.NullTime   `db:"last_used_at" json:"-"`
	LastUsed    *time.Time     `db:"-" json:"last_used_at"`
	CreatedAt   time.Time      `db:"created_at" json:"created_at"`

	// Its address: a Form webhook's with only the token's hint, but when it's
	// made or given a new token (URL); a Server webhook's, and its secret
	// only when it's made or given a new one.
	Address string `db:"-" json:"address"`
	URL     string `db:"-" json:"url,omitempty"`
	Secret  string `db:"-" json:"secret,omitempty"`
}

const denmaSignupHookSQL = `SELECT h.id, h.name, h.kind, h.token_hash, h.token_hint, h.tags, h.request_tags, h.per_hour,
	h.create_tags, h.allowed_from, h.created_by, CASE WHEN h.old_token_until > NOW() THEN h.old_token_until END AS old_token_until,
	h.uses, h.last_used_at, h.created_at,
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

// initDenmaSignupPublicHandlers registers the webhooks' addresses, and their
// description, in a center.
func initDenmaSignupPublicHandlers(g *echo.Group, a *App) {
	if denmaHub != nil && a.ko.String("denma.center") != "" {
		g.GET("/signup/openapi.json", a.DenmaSignupOpenAPI) // cmd/denma_signup_openapi.go
		g.POST("/signup/server", a.DenmaSignupServer)
		g.POST("/signup/:token", a.DenmaSignup)
	}
}

// denmaIsSuperadmin is whether the request's user is a superadmin (in a
// center, the only ones with the Super Admin role, cmd/denma_hierarchy.go).
func denmaIsSuperadmin(c echo.Context) bool {
	u, ok := c.Get(auth.UserHTTPCtxKey).(auth.User)
	return ok && u.ID > 0 && !denmaBound(u)
}

// denmaNewSignupToken makes a Form webhook's token (in its address) or a
// Server webhook's secret, returning it, its hash and its hint.
func denmaNewSignupToken(kind string) (string, string, string, error) {
	n, prefix := 32, ""
	if kind == denmaSignupServer {
		n, prefix = 40, denmaSignupSecretPrefix
	}
	r, err := utils.GenerateRandomString(n)
	if err != nil {
		return "", "", "", err
	}
	tok := prefix + r
	return tok, denmaAPIHash(tok), tok[len(tok)-4:], nil
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
	root := strings.TrimSuffix(a.urlCfg.RootURL, "/")
	if h.Kind == denmaSignupServer {
		h.Address = root + "/signup/server"
	} else {
		h.Address = root + "/signup/…" + h.TokenHint
	}
	if h.LastUsedAt.Valid {
		h.LastUsed = &h.LastUsedAt.Time
	}
	if h.ListIDs == nil {
		h.ListIDs = pq.Int64Array{}
	}
	for _, p := range []*pq.StringArray{&h.Tags, &h.RequestTags, &h.AllowedFrom} {
		if *p == nil {
			*p = pq.StringArray{}
		}
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

// signupHookToChange is a webhook the user may change: a Server webhook only
// by a superadmin.
func (a *App) signupHookToChange(c echo.Context) (denmaSignupHook, error) {
	id, _ := strconv.Atoi(c.Param("id"))
	h, err := a.signupHook(id)
	if err != nil {
		return h, err
	}
	if h.Kind == denmaSignupServer && !denmaIsSuperadmin(c) {
		return h, echo.NewHTTPError(http.StatusForbidden, "Only a superadmin can change a Server webhook.")
	}
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

// denmaSignupHookForm is a webhook as Config -> Webhooks sends it.
type denmaSignupHookForm struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	ListIDs     []int    `json:"list_ids"`
	Tags        []string `json:"tags"`
	RequestTags []string `json:"request_tags"`
	PerHour     int      `json:"per_hour"`
	CreateTags  bool     `json:"create_tags"`
	AllowedFrom []string `json:"allowed_from"`
}

// checkSignupForm trims and checks a webhook of a kind: a name, one or more of
// the center's lists (double opt-in ones for a Form webhook), and any of its
// tags; a Form webhook's hourly limit, a Server webhook's addresses.
func (a *App) checkSignupForm(c echo.Context, kind string) (denmaSignupHookForm, error) {
	var f denmaSignupHookForm
	if err := c.Bind(&f); err != nil {
		return f, echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	f.Kind = kind
	f.Name = strings.TrimSpace(f.Name)
	if !strHasLen(f.Name, 1, 200) {
		return f, echo.NewHTTPError(http.StatusBadRequest, "Give the webhook a name, such as the form or system it's for.")
	}
	if len(f.ListIDs) == 0 {
		return f, echo.NewHTTPError(http.StatusBadRequest, "Choose at least one list.")
	}
	if len(f.ListIDs) > denmaSignupMaxLists {
		return f, echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Choose at most %d lists.", denmaSignupMaxLists))
	}
	var lists []denmaOption
	if err := a.db.Select(&lists, `SELECT id, name, optin::TEXT AS optin FROM lists WHERE id = ANY($1)
		AND NOT ('`+denmaAPIListTag+`' = ANY(COALESCE(tags, '{}')))`, pq.Array(f.ListIDs)); err != nil {
		return f, echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if len(lists) != len(dedupInts(f.ListIDs)) {
		return f, echo.NewHTTPError(http.StatusBadRequest, "One of the lists doesn't exist (any more).")
	}
	if kind == denmaSignupForm {
		for _, l := range lists {
			if l.Optin != string(models.ListOptinDouble) {
				return f, echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf(
					"%s is single opt-in. A Form webhook's lists have to be double opt-in, so that whoever signs up confirms by e-mail first. To end up on a single opt-in list, use a double opt-in list here, and an automation (Automations) to move them on once they've confirmed.", l.Name))
			}
		}
	}
	f.ListIDs = dedupInts(f.ListIDs)
	f.Tags, f.RequestTags = denmaNormTags(f.Tags), denmaNormTags(f.RequestTags)
	if len(f.Tags) > denmaSignupMaxTags || len(f.RequestTags) > denmaSignupMaxTags {
		return f, echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Choose at most %d tags of each kind.", denmaSignupMaxTags))
	}
	if err := a.denmaUnknownTags(append(append([]string{}, f.Tags...), f.RequestTags...)); err != nil {
		return f, err
	}
	switch kind {
	case denmaSignupForm:
		if f.PerHour == 0 {
			f.PerHour = denmaSignupDefaultPerHour
		}
		if f.PerHour < 1 || f.PerHour > denmaSignupMaxPerHour {
			return f, echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("A form takes from 1 to %d sign-ups an hour.", denmaSignupMaxPerHour))
		}
		f.CreateTags, f.AllowedFrom = false, []string{}
	case denmaSignupServer:
		f.PerHour = denmaSignupDefaultPerHour
		from := []string{}
		for _, s := range f.AllowedFrom {
			if s = strings.TrimSpace(s); s == "" {
				continue
			}
			if err := denmaCheckAllowedFrom(s); err != nil {
				return f, echo.NewHTTPError(http.StatusBadRequest, err.Error())
			}
			from = append(from, s)
		}
		if len(from) > denmaAPIMaxFrom {
			return f, echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Give at most %d addresses.", denmaAPIMaxFrom))
		}
		f.AllowedFrom = from
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

// DenmaCreateSignupHook makes a webhook, with a new token (a Form webhook's,
// in its address) or secret (a Server webhook's), shown this once. Only a
// superadmin makes a Server webhook.
func (a *App) DenmaCreateSignupHook(c echo.Context) error {
	if err := a.inCenter(); err != nil {
		return err
	}
	kind := c.QueryParam("kind")
	switch kind {
	case "", denmaSignupForm:
		kind = denmaSignupForm
	case denmaSignupServer:
		if !denmaIsSuperadmin(c) {
			return echo.NewHTTPError(http.StatusForbidden, "Only a superadmin can make a Server webhook. Ask one to, for a system that vouches for people's consent.")
		}
	default:
		return echo.NewHTTPError(http.StatusBadRequest, "A webhook is a form or a server one.")
	}
	f, err := a.checkSignupForm(c, kind)
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
	tok, hash, hint, err := denmaNewSignupToken(kind)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	tx, err := a.db.Begin()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	defer tx.Rollback()
	var id int
	if err := tx.QueryRow(`INSERT INTO denma_signup_hooks (name, kind, token_hash, token_hint, tags, request_tags, per_hour, create_tags, allowed_from, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
		f.Name, kind, hash, hint, pq.Array(f.Tags), pq.Array(f.RequestTags), f.PerHour, f.CreateTags, pq.Array(f.AllowedFrom), denmaAPIWho(c)).Scan(&id); err != nil {
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
	a.signupShowToken(&h, tok)
	return c.JSON(http.StatusOK, okResp{h})
}

// signupShowToken puts a new token or secret in a webhook, to show once.
func (a *App) signupShowToken(h *denmaSignupHook, tok string) {
	if h.Kind == denmaSignupServer {
		h.Secret = tok
		return
	}
	h.URL = strings.TrimSuffix(a.urlCfg.RootURL, "/") + "/signup/" + tok
}

// DenmaUpdateSignupHook renames a webhook or changes its lists, tags and
// settings; its kind stays.
func (a *App) DenmaUpdateSignupHook(c echo.Context) error {
	if err := a.inCenter(); err != nil {
		return err
	}
	h, err := a.signupHookToChange(c)
	if err != nil {
		return err
	}
	f, err := a.checkSignupForm(c, h.Kind)
	if err != nil {
		return err
	}
	tx, err := a.db.Begin()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE denma_signup_hooks SET name = $2, tags = $3, request_tags = $4, per_hour = $5,
		create_tags = $6, allowed_from = $7, updated_at = NOW() WHERE id = $1`,
		h.ID, f.Name, pq.Array(f.Tags), pq.Array(f.RequestTags), f.PerHour, f.CreateTags, pq.Array(f.AllowedFrom)); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if err := saveSignupLists(tx, h.ID, f.ListIDs); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if h, err = a.signupHook(h.ID); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, okResp{h})
}

// DenmaNewSignupHookKey gives a webhook a new token or secret, shown this
// once. A Form webhook's old address stops working at once; a Server
// webhook's old secret at once, or after "overlap_hours" (up to a week).
func (a *App) DenmaNewSignupHookKey(c echo.Context) error {
	if err := a.inCenter(); err != nil {
		return err
	}
	h, err := a.signupHookToChange(c)
	if err != nil {
		return err
	}
	var r struct {
		OverlapHours int `json:"overlap_hours"`
	}
	_ = c.Bind(&r)
	if h.Kind != denmaSignupServer {
		r.OverlapHours = 0
	}
	if r.OverlapHours < 0 || r.OverlapHours > denmaAPIMaxOverlapHours {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("The old secret can keep working for at most %d hours.", denmaAPIMaxOverlapHours))
	}
	tok, hash, hint, err := denmaNewSignupToken(h.Kind)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if _, err := a.db.Exec(`UPDATE denma_signup_hooks SET
		old_token_hash = CASE WHEN $4 > 0 THEN token_hash END,
		old_token_until = CASE WHEN $4 > 0 THEN NOW() + make_interval(hours => $4) END,
		token_hash = $2, token_hint = $3, updated_at = NOW() WHERE id = $1`, h.ID, hash, hint, r.OverlapHours); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if h, err = a.signupHook(h.ID); err != nil {
		return err
	}
	a.signupShowToken(&h, tok)
	return c.JSON(http.StatusOK, okResp{h})
}

// DenmaDeleteSignupHook deletes a webhook; its address or secret stops
// working.
func (a *App) DenmaDeleteSignupHook(c echo.Context) error {
	if err := a.inCenter(); err != nil {
		return err
	}
	h, err := a.signupHookToChange(c)
	if err != nil {
		if he, ok := err.(*echo.HTTPError); ok && he.Code == http.StatusNotFound {
			return c.JSON(http.StatusOK, okResp{true})
		}
		return err
	}
	if _, err := a.db.Exec(`DELETE FROM denma_signup_hooks WHERE id = $1`, h.ID); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{true})
}

// denmaSignupCount is each Form webhook's sign-ups this hour (center/ID),
// for its per_hour.
var denmaSignupCount = struct {
	sync.Mutex
	m map[string]*denmaSignupWindow
}{m: map[string]*denmaSignupWindow{}}

type denmaSignupWindow struct {
	start time.Time
	n     int
}

// signupAllowed counts a sign-up against its webhook's limit for a window.
func (a *App) signupAllowed(id, limit int, window time.Duration) bool {
	key := fmt.Sprintf("%s/%d", a.ko.String("denma.center"), id)
	now := time.Now()
	denmaSignupCount.Lock()
	defer denmaSignupCount.Unlock()
	w := denmaSignupCount.m[key]
	if w == nil || now.Sub(w.start) >= window {
		w = &denmaSignupWindow{start: now}
		denmaSignupCount.m[key] = w
	}
	w.n++
	return w.n <= limit
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

// denmaSignupRequest is what a Form webhook is sent: its fields (one value
// each), and the attributes and tags it gives.
type denmaSignupRequest struct {
	Fields  map[string]string
	Attribs map[string]any
	Tags    []string
}

// reDenmaAttribField is an attribute given as its own field: attribs.<name>
// or attribs[<name>].
var reDenmaAttribField = regexp.MustCompile(`^attribs(?:\.([^.\[\]]+)|\[([^\[\]]+)\])$`)

// signupRequest reads a Form webhook's request: a JSON object, or a form.
// Attributes are an "attribs" object, or each its own field (attribs.<name>
// or attribs[<name>], as form builders can send them); tags, "tags" (a list,
// or separated by commas).
func signupRequest(c echo.Context) (denmaSignupRequest, error) {
	req := c.Request()
	req.Body = http.MaxBytesReader(c.Response(), req.Body, denmaSignupMaxBody)
	out := denmaSignupRequest{Fields: map[string]string{}, Attribs: map[string]any{}}
	attrib := func(k string, v any) bool {
		if m := reDenmaAttribField.FindStringSubmatch(k); m != nil {
			out.Attribs[m[1]+m[2]] = v
			return true
		}
		return false
	}
	if strings.Contains(req.Header.Get(echo.HeaderContentType), "json") {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return out, err
		}
		var raw map[string]any
		if err := json.Unmarshal(b, &raw); err != nil {
			return out, errors.New("the body isn't a JSON object")
		}
		for k, v := range raw {
			if k == "attribs" {
				if m, ok := v.(map[string]any); ok {
					for ak, av := range m {
						out.Attribs[ak] = av
					}
				}
				continue
			}
			if attrib(k, v) {
				continue
			}
			switch x := v.(type) {
			case string:
				out.Fields[k] = x
			case float64, bool:
				out.Fields[k] = fmt.Sprint(x)
			case []any: // a checkbox's choices
				parts := []string{}
				for _, p := range x {
					if s, ok := p.(string); ok && s != "" {
						parts = append(parts, s)
					}
				}
				out.Fields[k] = strings.Join(parts, ", ")
			}
		}
	} else {
		form, err := c.FormParams()
		if err != nil {
			return out, err
		}
		for k, v := range form {
			if len(v) == 0 {
				continue
			}
			if k == "tags" {
				out.Fields[k] = strings.Join(v, ", ")
				continue
			}
			if !attrib(k, v[0]) {
				out.Fields[k] = v[0]
			}
		}
	}
	if t := out.Fields["tags"]; t != "" {
		out.Tags = denmaNormTags([]string{t})
	}
	return out, nil
}

// DenmaSignup is a Form webhook's address: someone signing up on the center's
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

	// The webhook, by its token's hash.
	var h denmaSignupHook
	err := a.db.Get(&h, denmaSignupHookSQL+` WHERE h.token_hash = $1 AND h.kind = 'form'`, denmaAPIHash(c.Param("token")))
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

	r, err := signupRequest(c)
	if err != nil {
		logf("unreadable request")
		return reply(http.StatusBadRequest, "The request couldn't be read: "+err.Error())
	}
	fields := r.Fields
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
	// First and last names, else the name (cmd/denma_names.go).
	first, last := denmaSignupField(fields, "first_name", denmaNameMax), denmaSignupField(fields, "last_name", denmaNameMax)
	name := denmaJoinName(first, last)
	if name == "" {
		name = denmaSignupField(fields, "name", 200)
	}
	if name == "" {
		name = strings.Split(email, "@")[0]
	}

	// What it can't give is left out, and the rest of the sign-up goes on: a
	// form's field names shouldn't lose anyone their sign-up.
	var dropped []string
	if _, ok := fields["confirmed"]; ok {
		dropped = append(dropped, "confirmed")
	}
	for k := range r.Attribs {
		if denmaIsReservedAttrib(k) {
			delete(r.Attribs, k)
			dropped = append(dropped, "attribs."+k)
		}
	}
	if b, _ := json.Marshal(r.Attribs); len(b) > denmaAPIMaxAttribs {
		r.Attribs = map[string]any{}
		dropped = append(dropped, fmt.Sprintf("attribs (over %d bytes)", denmaAPIMaxAttribs))
	}
	tags := []string{}
	for _, t := range r.Tags {
		if slices.Contains(h.RequestTags, t) {
			tags = append(tags, t)
		} else {
			dropped = append(dropped, "tag "+t)
		}
	}
	if len(dropped) > 0 {
		slices.Sort(dropped)
		logf("left out: " + strings.Join(dropped, ", "))
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
	if !a.signupAllowed(h.ID, h.PerHour, time.Hour) {
		logf("too many sign-ups this hour")
		a.signupAlert(h, "per_hour", fmt.Sprintf("reached its limit of %d sign-ups an hour", h.PerHour),
			"Sign-ups past it are refused until the hour is over. A bot may be using the form to send confirmation e-mails to people who didn't ask for them: "+
				"turn on the form's spam protection (Gravity Forms' Turnstile or reCAPTCHA field). If the form really gets that many, raise the webhook's limit.")
		return reply(http.StatusTooManyRequests, "Too many sign-ups for now. Try again in an hour.")
	}
	if _, err := a.db.Exec(`UPDATE denma_signup_hooks SET uses = uses + 1, last_used_at = NOW(), last_ip = $2 WHERE id = $1`, h.ID, c.RealIP()); err != nil {
		a.log.Printf("denma: error counting sign-up webhook %d's use: %v", h.ID, err)
	}

	// The consent record, as the web-signup Lambda kept it, and the
	// attributes given; for someone new.
	website := map[string]any{}
	for k, n := range map[string]int{"form_id": 20, "entry_id": 20, "page_url": 500} {
		if v := denmaSignupField(fields, k, n); v != "" {
			website[k] = v
		}
	}
	attribs := map[string]any{}
	for k, v := range r.Attribs {
		attribs[k] = v
	}
	attribs["consent_at"] = time.Now().UTC().Format(time.RFC3339)
	attribs["consent_source"] = "sign-up webhook: " + h.Name
	if len(website) > 0 && attribs["website"] == nil {
		attribs["website"] = website
	}

	if err := a.denmaSignupDomain(email); err != nil { // member or not (cmd/denma_signupanswers.go)
		logf("refused: the address's domain can't receive e-mail")
		return reply(http.StatusBadRequest, fmt.Sprint(err.(*echo.HTTPError).Message))
	}
	a.denmaNoConsentSignup(email) // their own sign-up (cmd/denma_noconsent.go)
	if first != "" || last != "" {
		attribs = denmaWithNames(attribs, first, last) // a new subscriber's
	}
	result, isNew, err := a.signupSubscribe(email, name, listIDs, attribs)
	if errors.Is(err, errDenmaOptinLimited) { // answered as sent: the limit would say they're new (cmd/denma_signupanswers.go)
		result, err = "opt-in limit reached: no e-mail sent", nil
	}
	if err != nil {
		var he *echo.HTTPError
		if errors.As(err, &he) && he.Code < 500 {
			logf("refused: " + fmt.Sprint(he.Message))
			return reply(he.Code, fmt.Sprint(he.Message)) // an address that can't receive mail
		}
		a.log.Printf("denma: error in sign-up webhook %d: %v", h.ID, err)
		return reply(http.StatusInternalServerError, a.i18n.T("public.errorProcessingRequest"))
	}
	// Its tags, and those the request gave if they're new (the trigger keeps
	// them to the center's).
	give := append([]string{}, h.Tags...)
	if isNew {
		give = append(give, tags...)
	}
	if len(give) > 0 {
		if _, err := a.db.Exec(`UPDATE subscribers SET attribs = `+denmaTagsSet("TRUE", "$2")+`, updated_at = NOW() WHERE LOWER(email) = LOWER($1)`,
			email, pq.Array(give)); err != nil {
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
// became of it, for the log, and whether they're new. attribs are theirs only
// if they're new: an existing subscriber's are kept.
func (a *App) signupSubscribe(email, name string, listIDs []int, attribs map[string]any) (string, bool, error) {
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
		Name: name, Email: email, Status: models.SubscriberStatusEnabled, Attribs: models.JSON(attribs),
	}, listIDs, nil, false, true)
	// The consent record, added when they confirm a re-subscription.
	consent := map[string]any{}
	for _, k := range []string{"consent_at", "consent_source", "website"} {
		if v, ok := attribs[k]; ok {
			consent[k] = v
		}
	}
	if err == nil && sub.Status == models.SubscriberStatusBlockListed {
		// An address that opted out before, and was deleted
		// (cmd/denma_optouts.go): added blocklisted.
		status, err := a.denmaResubscribe(sub, listIDs, consent)
		return "opted out before: " + status, true, err
	}
	if err == nil {
		r, err := optin(sub, hasOptin)
		return "new subscriber: " + r, true, err
	}
	var he *echo.HTTPError
	if !errors.As(err, &he) || he.Code != http.StatusConflict {
		return "", false, err
	}

	// They're here already: their record stays as it is.
	sub, err = a.core.GetSubscriber(0, "", email)
	if err != nil {
		return "", false, err
	}
	if sub.Status == models.SubscriberStatusBlockListed { // cmd/denma_resubscribe.go
		status, err := a.denmaResubscribe(sub, listIDs, consent)
		return "blocklisted: " + status, false, err
	}
	sub, hasOptin, err = a.core.UpdateSubscriberWithLists(sub.ID, sub, listIDs, nil, false, false, true, nil, true)
	if err != nil {
		return "", false, err
	}
	r, err := optin(sub, hasOptin)
	return "existing subscriber: " + r, false, err
}

// signupAlert notifies the admins about a webhook, at most once a day per
// kind.
func (a *App) signupAlert(h denmaSignupHook, kind, what, detail string) {
	res, err := a.db.Exec(`INSERT INTO denma_signup_alerts (hook_id, kind) VALUES ($1, $2)
		ON CONFLICT (hook_id, kind) DO UPDATE SET sent_at = NOW() WHERE denma_signup_alerts.sent_at < NOW() - INTERVAL '1 day'`, h.ID, kind)
	if err != nil {
		a.log.Printf("denma: error recording an alert about sign-up webhook %d: %v", h.ID, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return // told already today
	}
	a.signupNotify(h, what, detail)
}

// signupNotify e-mails the center's admins and the superadmins about a
// webhook.
func (a *App) signupNotify(h denmaSignupHook, what, detail string) {
	center := a.ko.String("app.site_name")
	a.log.Printf("denma: sign-up webhook %q %s", h.Name, what)
	data := map[string]any{
		"Center": center, "Hook": h.Name, "What": what, "Detail": detail,
		"URL": strings.TrimSuffix(a.urlCfg.RootURL, "/") + "/admin/center/webhooks",
	}
	subject := fmt.Sprintf("Sign-up webhook %q %s: %s", h.Name, what, center)
	if err := a.notifs.NotifySystem(subject, "denma-signup-alert", data, nil); err != nil {
		a.log.Printf("denma: error telling the center's admins about a sign-up webhook: %v", err)
	}
	if hub := denmaHub.current(); hub != nil && hub.notifs != nil {
		if err := hub.notifs.NotifySystem(subject, "denma-signup-alert", data, nil); err != nil {
			a.log.Printf("denma: error telling the superadmins about a sign-up webhook: %v", err)
		}
	}
}

// ---- Server webhooks: POST <center>/signup/server ----

// signupHookOf is the request's Server webhook (after DenmaSignupServer has
// found it), for the activity log.
func signupHookOf(c echo.Context) denmaSignupHook {
	h, _ := c.Get(denmaSignupHookCtx).(denmaSignupHook)
	return h
}

// signupLog records what a Server webhook's request did: in the server log,
// and the activity log's entry (by webhook, never the request).
func (a *App) signupLog(c echo.Context, format string, args ...any) {
	h := signupHookOf(c)
	msg := fmt.Sprintf(format, args...)
	a.log.Printf("denma: sign-up webhook %d (%s): %s", h.ID, h.Name, msg) // no addresses
	res, _ := c.Get(denmaAPIResultCtx).([]string)
	c.Set(denmaAPIResultCtx, append(res, reDenmaAddress.ReplaceAllString(msg, "(an address)"))) // the activity log's
}

type denmaSignupServerReq struct {
	Email     string         `json:"email"`
	Name      string         `json:"name"`
	FirstName string         `json:"first_name"`
	LastName  string         `json:"last_name"`
	Lists     []int          `json:"lists"`
	Attribs   map[string]any `json:"attribs"`
	Tags      []string       `json:"tags"`
	Confirmed bool           `json:"confirmed"`
}

// What became of someone a Server webhook signs up, for the activity log;
// the answer is denmaSignupReceived for everyone.
const (
	denmaSignupReceived            = "received"
	denmaSignupSubscribed          = "subscribed"
	denmaSignupResubscribed        = "re-subscribed"
	denmaSignupConfirmationSent    = "confirmation sent"
	denmaSignupConfirmationNotSent = "added; confirmation not sent"
	denmaSignupNotSubscribed       = "not subscribed"
)

// DenmaSignupServer is a Server webhook's address: someone another system
// signs up. With "confirmed" (and a consent record, denmaConsentAt) they're
// subscribed at once; otherwise double opt-in lists send the confirmation
// e-mail.
//
// Someone who unsubscribed from the center is re-subscribed at once with
// "confirmed" and a consent dated after the opt-out, up to
// denmaSignupResubPerDay a day in the center; otherwise they're sent the
// re-subscription confirmation (cmd/denma_resubscribe.go). Complaints,
// bounces and an older consent leave them alone.
//
// What's sent is added to the subscriber, never replacing anything
// (appendSubscriber); their first consent is their consent_at and
// consent_source. Tags are added; those the webhook may create are added to
// the center's.
//
// The answer is the same whatever became of them, as a leaked secret's
// mustn't say whether the center had someone; the activity log has it.
func (a *App) DenmaSignupServer(c echo.Context) error {
	if err := a.inCenter(); err != nil {
		return err
	}
	ip := c.RealIP()
	if wait := denmaSignupBadSecrets.wait(ip); wait > 0 {
		return denmaTooMany(wait)
	}
	secret, ok := denmaBearer(c.Request().Header.Get(echo.HeaderAuthorization), denmaSignupSecretPrefix)
	if !ok {
		denmaSignupBadSecrets.add(ip)
		c.Response().Header().Set("WWW-Authenticate", `Bearer realm="listmonk"`)
		return echo.NewHTTPError(http.StatusUnauthorized, "Send the webhook's secret as Authorization: Bearer <secret>.")
	}
	var h denmaSignupHook
	hash := denmaAPIHash(secret)
	if err := a.db.Get(&h, denmaSignupHookSQL+` WHERE h.kind = 'server'
		AND (h.token_hash = $1 OR (h.old_token_hash = $1 AND h.old_token_until > NOW()))`, hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			denmaSignupBadSecrets.add(ip)
			return echo.NewHTTPError(http.StatusUnauthorized, "This secret isn't in use (any more).")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if !denmaFromAllowed(ip, h.AllowedFrom) {
		a.log.Printf("denma: sign-up webhook %d (%s) refused from %s: not one of its addresses", h.ID, h.Name, ip)
		return echo.NewHTTPError(http.StatusForbidden, fmt.Sprintf("This webhook can't be used from %s.", ip))
	}
	c.Set(denmaSignupHookCtx, h)
	if !a.signupAllowed(-h.ID, denmaSignupServerPerMinute, time.Minute) {
		return echo.NewHTTPError(http.StatusTooManyRequests, fmt.Sprintf("At most %d requests a minute. Try again in a minute.", denmaSignupServerPerMinute))
	}
	if _, err := a.db.Exec(`UPDATE denma_signup_hooks SET uses = uses + 1, last_used_at = NOW(), last_ip = $2 WHERE id = $1`, h.ID, ip); err != nil {
		a.log.Printf("denma: error counting sign-up webhook %d's use: %v", h.ID, err)
	}

	c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, denmaSignupMaxBody)
	var r denmaSignupServerReq
	if err := denmaAPIBind(c, &r); err != nil {
		return err
	}
	email, err := a.importer.SanitizeEmail(strings.TrimSpace(r.Email))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, `"email" isn't a valid address.`)
	}
	email = strings.ToLower(email)

	// Its lists that still exist: those given, which have to be its, or all.
	listIDs := dedupInts(r.Lists)
	for _, id := range listIDs {
		if !slices.Contains(h.ListIDs, int64(id)) {
			return echo.NewHTTPError(http.StatusForbidden, fmt.Sprintf("This webhook can't sign people up to list %d.", id))
		}
	}
	if len(listIDs) == 0 {
		for _, id := range h.ListIDs {
			listIDs = append(listIDs, int(id))
		}
	}
	var have []int
	if err := a.db.Select(&have, `SELECT id FROM lists WHERE id = ANY($1) ORDER BY id`, pq.Array(listIDs)); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if listIDs = have; len(listIDs) == 0 {
		return echo.NewHTTPError(http.StatusGone, "This webhook has no lists to sign people up to.")
	}

	name := denmaSignupField(map[string]string{"n": r.Name}, "n", 200)
	first, last := denmaTidyName(r.FirstName), denmaTidyName(r.LastName) // cmd/denma_names.go
	if n := denmaJoinName(first, last); n != "" {
		name = n
	}
	if k := denmaReservedAttrib(r.Attribs, true); k != "" {
		return echo.NewHTTPError(http.StatusBadRequest, denmaReservedAttribMsg(k))
	}
	if len(r.Attribs) > 0 {
		if b, _ := json.Marshal(r.Attribs); len(b) > denmaAPIMaxAttribs {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf(`"attribs" is over %d bytes.`, denmaAPIMaxAttribs))
		}
	}
	if len(r.Tags) > denmaAPIMaxTags {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf(`"tags" can have at most %d tags.`, denmaAPIMaxTags))
	}
	var tags, left []string
	for _, t := range denmaNormTags(r.Tags) {
		if h.CreateTags || slices.Contains(h.RequestTags, t) {
			tags = append(tags, t)
		} else {
			left = append(left, t)
		}
	}
	if len(left) > 0 {
		a.signupLog(c, "tags it can't give left out: %s", strings.Join(left, ", "))
	}
	tags = append(tags, h.Tags...)

	// Their consent: when (the latest of the record's, to compare with an
	// opt-out), and the record's first as their consent_at and
	// consent_source if they have none.
	var consentAt time.Time
	if r.Confirmed {
		if consentAt, err = denmaConsentAt(r.Attribs); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
	}

	// New tags are added once the request is valid, before anything about
	// the person, so that tags_created says nothing about them.
	created := []string{}
	if h.CreateTags && len(tags) > 0 {
		if err := a.db.Select(&created, `INSERT INTO denma_tags (tag) SELECT UNNEST($1::TEXT[])
			ON CONFLICT DO NOTHING RETURNING tag`, pq.Array(tags)); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		sort.Strings(created)
		if len(created) > 0 {
			a.signupLog(c, "tags added to the center: %s", strings.Join(created, ", "))
		}
	}

	if r.Attribs == nil {
		r.Attribs = map[string]any{}
	}
	if first, src := denmaFirstConsent(r.Attribs); r.Confirmed && src != "" {
		r.Attribs["consent_at"] = first.UTC().Format(time.RFC3339)
		r.Attribs["consent_source"] = src
	}

	type result struct {
		Result      string   `json:"result"`
		TagsCreated []string `json:"tags_created"` // new to the center: added to its tags
	}
	var outcome string
	var added, resubscribed []int
	done := func(why string) error {
		if why != "" {
			a.signupLog(c, "subscriber: %s (%s)", outcome, why) // for the log, not the answer
		} else {
			a.signupLog(c, "subscriber: %s", outcome)
		}
		return c.JSON(http.StatusOK, okResp{result{Result: denmaSignupReceived, TagsCreated: created}})
	}

	// Them, added if they're new. An address that opted out before is added
	// blocklisted (cmd/denma_optouts.go).
	newName := name
	if newName == "" {
		newName = strings.Split(email, "@")[0]
	}
	if _, err := a.db.Exec(`INSERT INTO subscribers (uuid, email, name, first_name, last_name, status) VALUES (gen_random_uuid(), $1, $2, $3, $4, 'enabled')
		ON CONFLICT DO NOTHING`, email, newName, first, last); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	var sub models.Subscriber
	if err := a.db.Get(&sub, `SELECT id, uuid, email, name, first_name, last_name, attribs, status, created_at, updated_at FROM subscribers WHERE LOWER(email) = $1`, email); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	// Vouched for: no confirmation e-mail runs the address's checks.
	if r.Confirmed {
		if why := a.signupVouchedCheck(sub); why != "" {
			outcome = denmaSignupNotSubscribed
			return done(why)
		}
	}

	// Blocklisted: a complaint, a bounce, or a consent from before they
	// unsubscribed.
	blocked := sub.Status == models.SubscriberStatusBlockListed
	if blocked {
		why, err := a.denmaResubRefused(sub)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if why != "" {
			outcome = denmaSignupNotSubscribed
			return done(why)
		}
		if r.Confirmed {
			var optedOut sql.NullTime
			if err := a.db.Get(&optedOut, `SELECT MAX(created_at) FROM denma_optouts
				WHERE email_hash = denma_email_hash($1) OR canon_hash = denma_canon_hash($1)`, email); err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
			}
			if optedOut.Valid && !consentAt.After(optedOut.Time) {
				outcome = denmaSignupNotSubscribed
				return done("consent from before they unsubscribed")
			}
		}
	}

	if err := a.appendSubscriber(sub.ID, email, name, first, last, r.Attribs, tags); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	// Signing up through another system that vouches for their consent lifts
	// the API's fence; otherwise it's their sign-up, to confirm
	// (cmd/denma_noconsent.go).
	if r.Confirmed {
		if err := denmaNoConsentLift(a.db, email); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
	} else {
		a.denmaNoConsentSignup(email)
	}

	// Unsubscribed from the center: re-subscribed now, or on confirming.
	if blocked {
		now := false
		if r.Confirmed {
			ok, err := a.signupResubClaim(h.ID)
			if err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
			}
			if !ok {
				a.signupAlert(h, "resub", fmt.Sprintf("re-subscribed %d people in a day", denmaSignupResubPerDay),
					fmt.Sprintf("Server webhooks re-subscribe people who had unsubscribed, saying they've agreed again. They did that for %d people "+
						"in this center in the last 24 hours, the most they can; others since are sent the e-mail to confirm it themselves. "+
						"If its system shouldn't re-subscribe that many, its secret may have leaked: a superadmin can give it a new one.", denmaSignupResubPerDay))
			}
			now = ok
		}
		if !now {
			claim, ok, err := a.signupConsentClaim(h)
			if err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
			}
			if !ok {
				outcome = denmaSignupConfirmationNotSent
				return done("the day's e-mails asking for consent are used up")
			}
			status, err := a.denmaResubscribe(sub, listIDs, nil)
			if err != nil {
				a.denmaConsentUnclaim(claim)
			}
			var he *echo.HTTPError
			switch {
			case errors.As(err, &he) && he.Code < 500:
				outcome = denmaSignupNotSubscribed
				return done(fmt.Sprint(he.Message))
			case err != nil:
				return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
			}
			outcome = denmaSignupConfirmationSent
			return done(status)
		}
		if err := a.db.Select(&resubscribed, `SELECT list_id FROM subscriber_lists
			WHERE subscriber_id = $1 AND list_id = ANY($2) AND status = 'unsubscribed' ORDER BY list_id`, sub.ID, pq.Array(listIDs)); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		lists, err := a.denmaListsByID(pq.Int64Array(int64s(listIDs)))
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if err := a.denmaResubConfirm(sub.ID, lists, nil, models.JSON{"resubscribed": true, "via": "sign-up webhook: " + h.Name}, false); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		outcome = denmaSignupResubscribed
		return done("")
	}

	// The lists: confirmed if single opt-in or "confirmed". Lists they'd
	// unsubscribed from are as for anyone new.
	var rows []struct {
		ListID int            `db:"list_id"`
		Status string         `db:"status"`
		Was    sql.NullString `db:"was"`
	}
	if err := a.db.Select(&rows, `WITH was AS (
			SELECT list_id, status::TEXT AS status FROM subscriber_lists WHERE subscriber_id = $1 AND list_id = ANY($2)
		)
		INSERT INTO subscriber_lists (subscriber_id, list_id, status)
		SELECT $1, l.id, CASE WHEN l.optin = 'single' OR $3 THEN 'confirmed'::subscription_status ELSE 'unconfirmed'::subscription_status END
		FROM lists l WHERE l.id = ANY($2)
		ON CONFLICT (subscriber_id, list_id) DO UPDATE SET status = CASE
			WHEN subscriber_lists.status = 'confirmed' THEN subscriber_lists.status
			ELSE EXCLUDED.status END, updated_at = NOW()
		RETURNING list_id, status::TEXT AS status, (SELECT status FROM was WHERE was.list_id = subscriber_lists.list_id) AS was`,
		sub.ID, pq.Array(listIDs), r.Confirmed); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	var unconfirmed []int
	for _, row := range rows {
		if row.Status == models.SubscriptionStatusUnsubscribed {
			continue // kept so by the database (cmd/denma_optouts.go)
		}
		added = append(added, row.ListID)
		if row.Was.String == models.SubscriptionStatusUnsubscribed {
			resubscribed = append(resubscribed, row.ListID)
		}
		if row.Status == models.SubscriptionStatusUnconfirmed {
			unconfirmed = append(unconfirmed, row.ListID)
		}
	}
	switch {
	case len(added) == 0:
		outcome = denmaSignupNotSubscribed
		return done("unsubscribed from the lists, or no consent")
	case len(unconfirmed) > 0:
		// The confirmation e-mail, with the opt-in checks (cmd/denma_emailcheck.go),
		// within the day's (cmd/denma_consentcap.go).
		claim, ok, err := a.signupConsentClaim(h)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if !ok {
			outcome = denmaSignupConfirmationNotSent
			return done("the day's e-mails asking for consent are used up")
		}
		n, err := a.fnOptinNotify(sub, unconfirmed)
		if err != nil || n == 0 {
			a.denmaConsentUnclaim(claim)
		}
		if err != nil {
			var he *echo.HTTPError
			if errors.As(err, &he) && he.Code < 500 {
				outcome = denmaSignupConfirmationNotSent
				return done(fmt.Sprint(he.Message))
			}
			return err
		}
		outcome = denmaSignupConfirmationSent
	case len(resubscribed) > 0:
		outcome = denmaSignupResubscribed
	default:
		outcome = denmaSignupSubscribed
	}
	return done("")
}

// signupVouchedCheck runs the opt-in e-mail's checks (denmaCheckOptin) for a
// sign-up vouched for, which isn't sent one: an address on the shared
// blocklist, unless an admin enabled it here on purpose, or whose domain
// can't receive mail, is blocklisted. It returns why, or "".
func (a *App) signupVouchedCheck(sub models.Subscriber) string {
	if reason, shared := denmaSharedBlock(a.db, sub.Email); shared {
		if r, _ := sub.Attribs["auto_blocklist_reason"].(string); r != "" && sub.Status != models.SubscriberStatusBlockListed {
			return "" // enabled here on purpose
		}
		denmaBlocklist(a.db, sub.Email, reason)
		return "on the shared blocklist: " + reason
	}
	if bad, reason := denmaEmailBad(sub.Email, denmaEmailSkip(a.ko)); bad {
		denmaBlocklist(a.db, sub.Email, reason)
		denmaBlockEverywhere(sub.Email, reason, a.ko.String("denma.center"))
		return "can't receive e-mail: " + reason
	}
	return ""
}

// signupResubClaim takes one of the center's denmaSignupResubPerDay
// re-subscriptions at once, if there's room, for a webhook.
func (a *App) signupResubClaim(hookID int) (bool, error) {
	tx, err := a.db.Beginx()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	// One claim at a time in the center.
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext(current_schema() || '/denma_resub_usage'))`); err != nil {
		return false, err
	}
	var used int
	if err := tx.Get(&used, `SELECT COALESCE(SUM(n), 0)::INT FROM denma_resub_usage WHERE created_at > NOW() - INTERVAL '24 hours'`); err != nil {
		return false, err
	}
	if used+1 > denmaSignupResubPerDay {
		return false, nil
	}
	if _, err := tx.Exec(`INSERT INTO denma_resub_usage (hook_id, n) VALUES ($1, 1)`, hookID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// denmaConsentSkew is how far in the future a consent's time may be (the
// other system's clock).
const denmaConsentSkew = 5 * time.Minute

// denmaConsentAt returns the latest "at" of attribs.consents, whose entries
// each need "at" (RFC 3339) and "source".
func denmaConsentAt(attribs map[string]any) (time.Time, error) {
	const need = `"confirmed" needs a consent record: "attribs": {"consents": [{"at": "<when they agreed, RFC 3339>", "source": "<how>"}]}.`
	list, _ := attribs["consents"].([]any)
	var at time.Time
	for _, e := range list {
		m, _ := e.(map[string]any)
		s, _ := m["at"].(string)
		src, _ := m["source"].(string)
		t, err := time.Parse(time.RFC3339, s)
		if err != nil || strings.TrimSpace(src) == "" {
			return time.Time{}, errors.New(need + ` Each needs "at" and "source".`)
		}
		if t.After(time.Now().Add(denmaConsentSkew)) {
			return time.Time{}, fmt.Errorf(`A consent's "at" (%s) is in the future.`, s)
		}
		if t.After(at) {
			at = t
		}
	}
	if at.IsZero() {
		return time.Time{}, errors.New(need)
	}
	return at, nil
}

// denmaFirstConsent is attribs.consents' earliest entry: when and its
// source ("" if there's none, checked by denmaConsentAt).
func denmaFirstConsent(attribs map[string]any) (time.Time, string) {
	var at time.Time
	src := ""
	list, _ := attribs["consents"].([]any)
	for _, e := range list {
		m, _ := e.(map[string]any)
		s, _ := m["at"].(string)
		t, err := time.Parse(time.RFC3339, s)
		if err != nil || (src != "" && !t.Before(at)) {
			continue
		}
		if v, _ := m["source"].(string); strings.TrimSpace(v) != "" {
			at, src = t, strings.TrimSpace(v)
		}
	}
	return at, src
}

// appendSubscriber adds a Server webhook's name, attributes and tags to a
// subscriber's, with their row locked, so that two requests at once both
// count. The name replaces theirs only if it's a stand-in (denmaGivenName);
// the attributes are added to theirs (denmaAppendJSON), and the tags too.
func (a *App) appendSubscriber(id int, email, name, first, last string, attribs map[string]any, tags []string) error {
	tx, err := a.db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var cur struct {
		Name    string `db:"name"`
		Attribs []byte `db:"attribs"`
	}
	if err := tx.Get(&cur, `SELECT name, attribs FROM subscribers WHERE id = $1 FOR UPDATE`, id); err != nil {
		return err
	}
	m := map[string]any{}
	if err := json.Unmarshal(cur.Attribs, &m); err != nil || m == nil {
		m = map[string]any{} // not an object: listmonk's own handling starts afresh too
	}
	m = denmaAppendJSON(m, map[string]any(attribs)).(map[string]any)
	if len(tags) > 0 {
		add := make([]any, len(tags))
		for i, t := range tags {
			add[i] = t
		}
		have, _ := m["tags"].([]any) // an array, kept so by the database (denma_norm_tags)
		m["tags"] = denmaAppendJSON(have, add)
	}
	newName := denmaGivenName(cur.Name, name, email)
	if newName != cur.Name && (first != "" || last != "") {
		m = denmaWithNames(m, first, last) // in place of a stand-in (cmd/denma_names.go)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE subscribers SET name = $2, attribs = $3, updated_at = NOW() WHERE id = $1`,
		id, newName, string(b)); err != nil {
		return err
	}
	return tx.Commit()
}

// denmaGivenName is a subscriber's name after a request gives one: theirs,
// unless it's a stand-in (empty, their address or the part before its @).
func denmaGivenName(cur, given, email string) string {
	local, _, _ := strings.Cut(email, "@")
	switch c := strings.ToLower(strings.TrimSpace(cur)); {
	case given == "":
		return cur
	case c == "", c == strings.ToLower(email), c == strings.ToLower(local),
		c == strings.ToLower(strings.Join(strings.Fields(strings.ReplaceAll(local, ".", " ")), " ")): // the importer's
		return given
	}
	return cur
}

// denmaAppendJSON adds add to old (decoded JSON) without taking anything
// away: what old doesn't have is added, objects are merged a key at a time,
// arrays get the items they don't have yet, and a value old already has is
// kept.
func denmaAppendJSON(old, add any) any {
	if add == nil {
		return old
	}
	if old == nil {
		return add
	}
	switch o := old.(type) {
	case map[string]any:
		if ad, ok := add.(map[string]any); ok {
			out := make(map[string]any, len(o)+len(ad))
			for k, v := range o {
				out[k] = v
			}
			keys := make([]string, 0, len(ad))
			for k, v := range ad {
				if v != nil { // null adds nothing
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			for _, k := range keys {
				out[k] = denmaAppendJSON(o[k], ad[k])
			}
			return out
		}
	case []any:
		if ad, ok := add.([]any); ok {
			out := append([]any{}, o...)
			for _, v := range ad {
				if !denmaJSONHas(out, v) {
					out = append(out, v)
				}
			}
			return out
		}
	}
	return old
}

func denmaJSONHas(list []any, v any) bool {
	for _, e := range list {
		if denmaJSONEqual(e, v) {
			return true
		}
	}
	return false
}

// denmaJSONEqual compares decoded JSON values (encoding sorts objects' keys).
func denmaJSONEqual(x, y any) bool {
	a, err1 := json.Marshal(x)
	b, err2 := json.Marshal(y)
	return err1 == nil && err2 == nil && string(a) == string(b)
}

// signupLostLists tells the center's admins and the superadmins about the
// lists Server webhooks have lost: a center's admin deleted them, and the
// system using the webhook signs no one up to them any more. Run by the
// minute's job (cmd/denma_api_events.go).
func (a *App) signupLostLists() {
	var lost []struct {
		HookID int            `db:"hook_id"`
		Names  pq.StringArray `db:"names"`
		Left   int            `db:"left"`
	}
	if err := a.db.Select(&lost, `WITH gone AS (DELETE FROM denma_signup_lost RETURNING hook_id, list_name)
		SELECT hook_id, ARRAY_AGG(DISTINCT list_name ORDER BY list_name) AS names,
			(SELECT COUNT(*) FROM denma_signup_hook_lists hl WHERE hl.hook_id = gone.hook_id)::INT AS left
		FROM gone GROUP BY hook_id`); err != nil {
		var pe *pq.Error
		if !(errors.As(err, &pe) && pe.Code == "42P01") { // not installed yet
			a.log.Printf("denma: error reading sign-up webhooks' lost lists: %v", err)
		}
		return
	}
	for _, l := range lost {
		h, err := a.signupHook(l.HookID)
		if err != nil {
			continue
		}
		left := fmt.Sprintf("It still signs people up to %d other list(s).", l.Left)
		if l.Left == 0 {
			left = "It has no lists left, so it signs no one up: its requests are refused (410) until it's given lists."
		}
		a.signupNotify(h, "lost "+orList(l.Names, "and"),
			"The center deleted "+orList(l.Names, "and")+", which this Server webhook signed people up to. "+left+
				" A superadmin can choose its lists on the center's Config -> Webhooks page.")
	}
}
