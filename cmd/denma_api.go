package main

// denma: the sending API, for systems outside listmonk that send their own
// e-mail through a center: a member database, a booking system, a website.
// They keep their own people and write their own messages; the center sends
// them with everything a center's own mail gets (the verified sending domain,
// the shared blocklist and domain checks, opt-outs, bounces, the daily cap).
// Each system is a key, made on the center's Config -> API page
// (views/denma-api.html) by its admins (center:manage), which can do one or
// more of sending transactional e-mails, sending campaigns and subscribing
// people (denmaAPICanAll). The API is at <center>/v1/ (cmd/denma_api_send.go):
//
//   - POST /v1/messages: a transactional e-mail (password reset, receipt,
//     notice) to one address or a few, from one of the center's
//     transactional templates, by name, with the caller's data.
//   - POST /v1/campaigns: a campaign to a list of addresses the caller sends,
//     from one of the center's campaign templates, by name. Its recipients go
//     on a list of its own, which is deleted a week after it's done.
//   - POST /v1/subscribers: add someone to the key's lists, and tag them
//     (rg-sync).
//   - GET /v1/events: unsubscribes, complaints, bounces and blocklistings, as
//     each key's webhook is also sent them (cmd/denma_api_events.go).
//
// Templates are made beforehand, in the center (Campaigns -> Templates); the
// API only names them. A key is sent as "Authorization: Bearer <key>", is
// stored as its SHA-256 (it's shown once, when made), and can be limited to
// requests from some addresses (IPs, networks, or host names). Each key
// makes at most denmaAPIPerMinute requests a minute.
//
// A sending key has a fixed sender and a daily limit of e-mails. A key
// re-subscribes at most denmaAPIResubPerDay people a day. Keys can expire,
// be replaced with an overlap, and be restored for denmaAPIKeepDeletedDays
// days after they're deleted.
//
// The center's sending through the API pauses on its own when its bounce or
// complaint rate reaches the hub's "At risk" band (cmd/denma_api_events.go);
// a superadmin resumes it.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/internal/utils"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

const (
	denmaAPIMaxKeys   = 20  // keys a center can have
	denmaAPIMaxLists  = 20  // lists a key can add subscribers to
	denmaAPIMaxFrom   = 20  // addresses a key can be limited to
	denmaAPIPerMinute = 600 // requests a key makes a minute
	denmaAPIKeyPrefix = "lmk_"
	denmaAPIKeyCtx    = "denma_api_key"
	denmaAPIResultCtx = "denma_api_result" // what the request did, for the activity log (apiLog)
	denmaAPIHostTTL   = 5 * time.Minute    // how long a host name's addresses are kept
	denmaAPIListTag   = "api-send"         // on the lists of the API's campaigns

	denmaAPIDefaultDailyLimit = 1000 // e-mails a sending key sends in 24 hours, to start with
	denmaAPIResubPerDay       = 10   // people a key re-subscribes at once ("confirmed") in 24 hours
	denmaAPIKeepDeletedDays   = 7    // days a deleted key can be restored
	denmaAPIMaxOverlapHours   = 7 * 24
)

// denmaAPIBadKeys limits requests with keys that don't work, by client address.
var denmaAPIBadKeys = newDenmaThrottle(20, 15*time.Minute)

// denmaAPISQL makes the API's tables, in a center (denmaFeaturesSQL).
const denmaAPISQL = `
CREATE TABLE IF NOT EXISTS denma_api_keys (
    id               SERIAL PRIMARY KEY,
    name             TEXT NOT NULL,
    key_hash         TEXT NOT NULL UNIQUE,  -- SHA-256 of the key, hex
    key_hint         TEXT NOT NULL,         -- its last 4 characters
    allowed_from     TEXT[] NOT NULL DEFAULT '{}',
    webhook_url      TEXT NOT NULL DEFAULT '',
    webhook_secret   TEXT NOT NULL DEFAULT '',
    webhook_after    BIGINT NOT NULL DEFAULT 0, -- the last event delivered
    webhook_failures INTEGER NOT NULL DEFAULT 0,
    webhook_next_at  TIMESTAMP WITH TIME ZONE NULL,
    webhook_error    TEXT NOT NULL DEFAULT '',
    webhook_ok_at    TIMESTAMP WITH TIME ZONE NULL,
    uses             INTEGER NOT NULL DEFAULT 0,
    last_used_at     TIMESTAMP WITH TIME ZONE NULL,
    last_ip          TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
-- The lists POST /v1/subscribers can add people to.
CREATE TABLE IF NOT EXISTS denma_api_key_lists (
    key_id  INTEGER NOT NULL REFERENCES denma_api_keys(id) ON DELETE CASCADE,
    list_id INTEGER NOT NULL REFERENCES lists(id) ON DELETE CASCADE,
    PRIMARY KEY (key_id, list_id)
);
-- The campaigns the API made, each with a list of its own.
CREATE TABLE IF NOT EXISTS denma_api_campaigns (
    campaign_id     INTEGER PRIMARY KEY REFERENCES campaigns(id) ON DELETE CASCADE,
    key_id          INTEGER NULL REFERENCES denma_api_keys(id) ON DELETE SET NULL,
    list_id         INTEGER NULL REFERENCES lists(id) ON DELETE SET NULL,
    idempotency_key TEXT NULL,
    state           TEXT NOT NULL DEFAULT 'draft', -- draft, starting (checking addresses), started
    error           TEXT NOT NULL DEFAULT '',      -- why it didn't start
    created_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS denma_api_campaigns_idem ON denma_api_campaigns (key_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
-- Transactional e-mails: one row a request.
CREATE TABLE IF NOT EXISTS denma_api_messages (
    id              BIGSERIAL PRIMARY KEY,
    key_id          INTEGER NULL REFERENCES denma_api_keys(id) ON DELETE SET NULL,
    template_id     INTEGER NULL,
    sent            INTEGER NOT NULL DEFAULT 0,
    skipped         INTEGER NOT NULL DEFAULT 0,
    idempotency_key TEXT NULL,
    result          JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS denma_api_messages_idem ON denma_api_messages (key_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS denma_api_messages_created ON denma_api_messages (created_at);
-- Whether the API's sending is paused (cmd/denma_api_events.go).
CREATE TABLE IF NOT EXISTS denma_api_state (
    id            BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    paused_at     TIMESTAMP WITH TIME ZONE NULL,
    paused_reason TEXT NOT NULL DEFAULT '',
    resumed_at    TIMESTAMP WITH TIME ZONE NULL
);
INSERT INTO denma_api_state (id) VALUES (TRUE) ON CONFLICT DO NOTHING;
-- What each key can do (denmaAPICanAll), since features version 20. Keys
-- made before keep what they could: send, and subscribe if they had lists.
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema() AND table_name = 'denma_api_keys' AND column_name = 'can') THEN
        ALTER TABLE denma_api_keys ADD COLUMN can TEXT[] NOT NULL DEFAULT '{}'
            CHECK (can <@ ARRAY['messages', 'campaigns', 'subscribers']);
        UPDATE denma_api_keys k SET can = ARRAY['messages', 'campaigns'] || CASE
            WHEN EXISTS (SELECT 1 FROM denma_api_key_lists l WHERE l.key_id = k.id) THEN ARRAY['subscribers'] ELSE '{}' END;
    END IF;
END $$;
-- Since features version 21.
ALTER TABLE denma_api_keys ADD COLUMN IF NOT EXISTS created_by TEXT NOT NULL DEFAULT '';
ALTER TABLE denma_api_keys ADD COLUMN IF NOT EXISTS expires_at TIMESTAMP WITH TIME ZONE NULL;
ALTER TABLE denma_api_keys ADD COLUMN IF NOT EXISTS expiry_notice TEXT NOT NULL DEFAULT ''; -- the last reminder: 14, 3, expired
ALTER TABLE denma_api_keys ADD COLUMN IF NOT EXISTS sender TEXT NOT NULL DEFAULT '';        -- '' for the center's
ALTER TABLE denma_api_keys ADD COLUMN IF NOT EXISTS daily_limit INTEGER NOT NULL DEFAULT 1000;
ALTER TABLE denma_api_keys ADD COLUMN IF NOT EXISTS old_key_hash TEXT NULL;
ALTER TABLE denma_api_keys ADD COLUMN IF NOT EXISTS old_key_until TIMESTAMP WITH TIME ZONE NULL;
ALTER TABLE denma_api_keys ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMP WITH TIME ZONE NULL;
ALTER TABLE denma_api_keys ADD COLUMN IF NOT EXISTS deleted_by TEXT NOT NULL DEFAULT '';
ALTER TABLE denma_api_keys ADD COLUMN IF NOT EXISTS leaked BOOLEAN NOT NULL DEFAULT FALSE;
CREATE UNIQUE INDEX IF NOT EXISTS denma_api_keys_old_hash ON denma_api_keys (old_key_hash) WHERE old_key_hash IS NOT NULL;
-- The events permission; existing keys get it.
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'denma_api_keys'::regclass AND contype = 'c'
        AND pg_get_constraintdef(oid) LIKE '%events%') THEN
        ALTER TABLE denma_api_keys DROP CONSTRAINT IF EXISTS denma_api_keys_can_check;
        ALTER TABLE denma_api_keys ADD CONSTRAINT denma_api_keys_can_check
            CHECK (can <@ ARRAY['messages', 'campaigns', 'subscribers', 'events']);
        UPDATE denma_api_keys SET can = can || ARRAY['events'];
    END IF;
END $$;
-- The addresses (hashed) each key has sent to or subscribed, for its events.
CREATE TABLE IF NOT EXISTS denma_api_contacts (
    key_id     INTEGER NOT NULL REFERENCES denma_api_keys(id) ON DELETE CASCADE,
    email_hash TEXT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (key_id, email_hash)
);
-- Each key's use of its daily limits: send, resub. Kept two days.
CREATE TABLE IF NOT EXISTS denma_api_usage (
    id         BIGSERIAL PRIMARY KEY,
    key_id     INTEGER NOT NULL REFERENCES denma_api_keys(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    n          INTEGER NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS denma_api_usage_key ON denma_api_usage (key_id, kind, created_at);
-- The last alert about each key, by kind (apiAlert).
CREATE TABLE IF NOT EXISTS denma_api_alerts (
    key_id  INTEGER NOT NULL REFERENCES denma_api_keys(id) ON DELETE CASCADE,
    kind    TEXT NOT NULL,
    sent_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (key_id, kind)
);
` + denmaAPIEventsSQL

// What a key can do. Each of the API's sending, subscribing and events
// endpoints needs one (apiNeeds); the rest (its status, the templates) any
// key can use.
const (
	denmaAPICanMessages    = "messages"    // POST /v1/messages
	denmaAPICanCampaigns   = "campaigns"   // /v1/campaigns...
	denmaAPICanSubscribers = "subscribers" // POST /v1/subscribers, to the key's lists
	denmaAPICanEvents      = "events"      // GET /v1/events, and the webhook
)

var (
	denmaAPICanAll  = []string{denmaAPICanMessages, denmaAPICanCampaigns, denmaAPICanSubscribers, denmaAPICanEvents}
	denmaAPICanText = map[string]string{
		denmaAPICanMessages:    "send transactional e-mails",
		denmaAPICanCampaigns:   "send campaigns",
		denmaAPICanSubscribers: "subscribe people to lists",
		denmaAPICanEvents:      "read the events",
	}
)

// denmaAPISends is whether a key can send e-mail.
func denmaAPISends(can []string) bool {
	return denmaHasString(can, denmaAPICanMessages) || denmaHasString(can, denmaAPICanCampaigns)
}

// denmaAPICanForm checks what a key is to do (in denmaAPICanAll's order),
// with how many lists it can subscribe people to.
func denmaAPICanForm(can []string, lists int) ([]string, error) {
	out := []string{}
	for _, s := range can {
		if !denmaHasString(denmaAPICanAll, s) {
			return nil, fmt.Errorf("%q isn't something a key can do.", s)
		}
	}
	for _, s := range denmaAPICanAll {
		if denmaHasString(can, s) {
			out = append(out, s)
		}
	}
	switch {
	case len(out) == 0:
		return nil, errors.New("Choose what the key can do.")
	case denmaHasString(out, denmaAPICanSubscribers) && lists == 0:
		return nil, errors.New("Choose the lists it can subscribe people to.")
	}
	return out, nil
}

// apiNeeds refuses a request from a key that can't do what.
func apiNeeds(what string, next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if !denmaHasString(apiKeyOf(c).Can, what) {
			return echo.NewHTTPError(http.StatusForbidden, fmt.Sprintf(
				"This key can't %s. What a key can do is set on the center's Config -> API page.", denmaAPICanText[what]))
		}
		return next(c)
	}
}

// denmaAPIKey is a key, for Config -> API and the API.
type denmaAPIKey struct {
	ID              int            `db:"id" json:"id"`
	Name            string         `db:"name" json:"name"`
	KeyHint         string         `db:"key_hint" json:"key_hint"`
	Can             pq.StringArray `db:"can" json:"can"` // denmaAPICanAll
	AllowedFrom     pq.StringArray `db:"allowed_from" json:"allowed_from"`
	ListIDs         pq.Int64Array  `db:"list_ids" json:"list_ids"`
	WebhookURL      string         `db:"webhook_url" json:"webhook_url"`
	WebhookSecret   string         `db:"webhook_secret" json:"webhook_secret"`
	WebhookFailures int            `db:"webhook_failures" json:"webhook_failures"`
	WebhookError    string         `db:"webhook_error" json:"webhook_error"`
	WebhookOKAt     *time.Time     `db:"webhook_ok_at" json:"webhook_ok_at"`
	WebhookNextAt   *time.Time     `db:"webhook_next_at" json:"webhook_next_at"`
	Uses            int            `db:"uses" json:"uses"`
	LastUsedAt      *time.Time     `db:"last_used_at" json:"last_used_at"`
	LastIP          string         `db:"last_ip" json:"last_ip"`
	CreatedAt       time.Time      `db:"created_at" json:"created_at"`
	CreatedBy       string         `db:"created_by" json:"created_by"`
	ExpiresAt       *time.Time     `db:"expires_at" json:"expires_at"`
	Sender          string         `db:"sender" json:"sender"` // "" for the center's
	DailyLimit      int            `db:"daily_limit" json:"daily_limit"`
	Sent24h         int            `db:"sent_24h" json:"sent_24h"` // of its daily limit
	OldKeyUntil     *time.Time     `db:"old_key_until" json:"old_key_until"`
	DeletedAt       *time.Time     `db:"deleted_at" json:"deleted_at"`
	DeletedBy       string         `db:"deleted_by" json:"deleted_by"`
	Leaked          bool           `db:"leaked" json:"leaked"`

	Key string `db:"-" json:"key,omitempty"` // only when it's made
}

const denmaAPIKeySQL = `SELECT k.id, k.name, k.key_hint, k.can, k.allowed_from, k.webhook_url, k.webhook_secret,
	k.webhook_failures, k.webhook_error, k.webhook_ok_at, k.webhook_next_at, k.uses, k.last_used_at, k.last_ip, k.created_at,
	k.created_by, k.expires_at, k.sender, k.daily_limit, k.deleted_at, k.deleted_by, k.leaked,
	CASE WHEN k.old_key_until > NOW() THEN k.old_key_until END AS old_key_until,
	(SELECT COALESCE(SUM(n), 0) FROM denma_api_usage u WHERE u.key_id = k.id AND u.kind = 'send'
		AND u.created_at > NOW() - INTERVAL '24 hours')::INT AS sent_24h,
	ARRAY(SELECT list_id FROM denma_api_key_lists kl WHERE kl.key_id = k.id ORDER BY list_id) AS list_ids
	FROM denma_api_keys k`

func initDenmaAPIAdminHandlers(g *echo.Group, a *App) {
	g.GET(path.Join(uriAdmin, "/center/api"), a.ViewDenmaAPI)
}

func initDenmaAPIKeyHandlers(g *echo.Group, a *App) {
	pm := func(h echo.HandlerFunc) echo.HandlerFunc { return a.auth.Perm(h, denmaCenterPerm) }
	g.GET("/api/denma/center/api-keys", pm(a.DenmaGetAPIKeys))
	g.POST("/api/denma/center/api-keys", pm(a.DenmaCreateAPIKey))
	g.PUT("/api/denma/center/api-keys/:id", pm(a.DenmaUpdateAPIKey))
	g.POST("/api/denma/center/api-keys/:id/key", pm(a.DenmaNewAPIKey))
	g.POST("/api/denma/center/api-keys/:id/webhook-secret", pm(a.DenmaNewAPIWebhookSecret))
	g.POST("/api/denma/center/api-keys/:id/webhook-test", pm(a.DenmaTestAPIWebhook))
	g.DELETE("/api/denma/center/api-keys/:id", pm(a.DenmaDeleteAPIKey))
	g.POST("/api/denma/center/api-keys/:id/restore", pm(a.DenmaRestoreAPIKey))
	g.POST("/api/denma/center/api/resume", pm(a.DenmaResumeAPI))
}

// denmaAPIHash is a key's SHA-256, as stored.
func denmaAPIHash(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// denmaNewAPIKey makes a key, returning it, its hash and its hint.
func denmaNewAPIKey() (string, string, string, error) {
	r, err := utils.GenerateRandomString(40)
	if err != nil {
		return "", "", "", err
	}
	key := denmaAPIKeyPrefix + r
	return key, denmaAPIHash(key), key[len(key)-4:], nil
}

func (a *App) apiKeys() ([]denmaAPIKey, error) {
	out := []denmaAPIKey{}
	if err := a.db.Select(&out, denmaAPIKeySQL+` ORDER BY k.id`); err != nil {
		return nil, err
	}
	for i := range out {
		denmaAPIKeyView(&out[i])
	}
	return out, nil
}

func denmaAPIKeyView(k *denmaAPIKey) {
	if k.Can == nil {
		k.Can = pq.StringArray{}
	}
	if k.AllowedFrom == nil {
		k.AllowedFrom = pq.StringArray{}
	}
	if k.ListIDs == nil {
		k.ListIDs = pq.Int64Array{}
	}
}

func (a *App) apiKey(id int) (denmaAPIKey, error) {
	var k denmaAPIKey
	if err := a.db.Get(&k, denmaAPIKeySQL+` WHERE k.id = $1`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return k, echo.NewHTTPError(http.StatusNotFound, "That key doesn't exist (any more).")
		}
		return k, echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	denmaAPIKeyView(&k)
	return k, nil
}

// denmaAPIView is Config -> API.
type denmaAPIView struct {
	adminView
	Keys      []denmaAPIKey
	Lists     []denmaOption
	Templates []denmaAPITemplate
	BaseURL   string
	State     denmaAPIState
	Sending   denmaCapStatus
	Super     bool
	Recent    []denmaAPIRecent
	PerMinute int
	Spec      json.RawMessage // the OpenAPI description (cmd/denma_api_openapi.go)
	Sender    string          // the center's, a key's unless it has its own
	KeepDays  int             // denmaAPIKeepDeletedDays
}

// denmaAPITemplate is a template the API can name.
type denmaAPITemplate struct {
	Name string `db:"name" json:"name"`
	Type string `db:"type" json:"type"` // tx, campaign, campaign_visual
}

// denmaAPIRecent is a day of the API's sending, for its page.
type denmaAPIRecent struct {
	Day       time.Time `db:"day" json:"day"`
	Messages  int       `db:"messages" json:"messages"`
	Campaigns int       `db:"campaigns" json:"campaigns"`
	Sent      int       `db:"sent" json:"sent"`
}

// ViewDenmaAPI renders Config -> API.
func (a *App) ViewDenmaAPI(c echo.Context) error {
	v, err := a.centerView(c, "API", "config.api")
	if err != nil {
		return err
	}
	view := denmaAPIView{adminView: v, Lists: []denmaOption{}, Templates: []denmaAPITemplate{}, Recent: []denmaAPIRecent{},
		BaseURL: strings.TrimSuffix(a.urlCfg.RootURL, "/") + "/v1", Sending: a.denmaCapStatus(),
		Super: v.Profile.UserRole.ID == auth.SuperAdminRoleID, PerMinute: denmaAPIPerMinute,
		Sender: a.cfg.FromEmail, KeepDays: denmaAPIKeepDeletedDays}
	if view.Keys, err = a.apiKeys(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if err := a.db.Select(&view.Lists, `SELECT id, name, optin::TEXT AS optin FROM lists
		WHERE NOT ($1 = ANY(COALESCE(tags, '{}'))) ORDER BY name`, denmaAPIListTag); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if view.Templates, err = a.apiTemplates(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if view.State, err = a.apiState(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if view.Spec, err = a.apiOpenAPI(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	// The last week's sending, a day at a time.
	if err := a.db.Select(&view.Recent, `WITH d AS (
			SELECT date_trunc('day', created_at) AS day, COUNT(*) AS messages, 0 AS campaigns, SUM(sent) AS sent
			FROM denma_api_messages WHERE created_at > NOW() - INTERVAL '7 days' GROUP BY 1
			UNION ALL
			SELECT date_trunc('day', a.created_at), 0, COUNT(*), SUM(c.sent)
			FROM denma_api_campaigns a JOIN campaigns c ON c.id = a.campaign_id
			WHERE a.created_at > NOW() - INTERVAL '7 days' GROUP BY 1
		)
		SELECT day, SUM(messages)::INT AS messages, SUM(campaigns)::INT AS campaigns, SUM(sent)::INT AS sent
		FROM d GROUP BY day ORDER BY day DESC`); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.Render(http.StatusOK, "admin-denma-api", view)
}

// apiTemplates is the center's templates the API can name, by name.
func (a *App) apiTemplates() ([]denmaAPITemplate, error) {
	out := []denmaAPITemplate{}
	err := a.db.Select(&out, `SELECT name, type::TEXT AS type FROM templates
		WHERE type IN ('tx', 'campaign', 'campaign_visual') ORDER BY type DESC, name`)
	return out, err
}

// DenmaGetAPIKeys lists the center's keys.
func (a *App) DenmaGetAPIKeys(c echo.Context) error {
	if err := a.inCenter(); err != nil {
		return err
	}
	out, err := a.apiKeys()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{out})
}

// denmaAPIKeyForm is a key as Config -> API sends it.
type denmaAPIKeyForm struct {
	Name        string   `json:"name"`
	Can         []string `json:"can"` // denmaAPICanAll
	ListIDs     []int    `json:"list_ids"`
	AllowedFrom []string `json:"allowed_from"`
	WebhookURL  string   `json:"webhook_url"`
	Sender      string   `json:"sender"`      // a sending key's; "" for the center's
	DailyLimit  int      `json:"daily_limit"` // a sending key's e-mails in 24 hours
	ExpiresOn   string   `json:"expires_on"`  // YYYY-MM-DD (UTC), or "" for never

	expiresAt *time.Time // the end of ExpiresOn
}

var reDenmaHostName = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// checkAPIKeyForm trims and checks a key's settings.
func (a *App) checkAPIKeyForm(c echo.Context) (denmaAPIKeyForm, error) {
	var f denmaAPIKeyForm
	if err := c.Bind(&f); err != nil {
		return f, echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	bad := func(msg string) error { return echo.NewHTTPError(http.StatusBadRequest, msg) }
	f.Name = strings.TrimSpace(f.Name)
	if !strHasLen(f.Name, 1, 200) {
		return f, bad("Give the key a name, such as the system that uses it.")
	}
	f.ListIDs = dedupInts(f.ListIDs)
	can, err := denmaAPICanForm(f.Can, len(f.ListIDs))
	if err != nil {
		return f, bad(err.Error())
	}
	f.Can = can
	if !denmaHasString(f.Can, denmaAPICanSubscribers) {
		f.ListIDs = []int{} // only for subscribing
	}
	if len(f.ListIDs) > denmaAPIMaxLists {
		return f, bad(fmt.Sprintf("Choose at most %d lists.", denmaAPIMaxLists))
	}
	if len(f.ListIDs) > 0 {
		var n int
		if err := a.db.Get(&n, `SELECT COUNT(*) FROM lists WHERE id = ANY($1)`, pq.Array(f.ListIDs)); err != nil {
			return f, echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if n != len(f.ListIDs) {
			return f, bad("One of the lists doesn't exist (any more).")
		}
	}
	from := []string{}
	for _, s := range f.AllowedFrom {
		if s = strings.ToLower(strings.TrimSpace(s)); s == "" {
			continue
		}
		if err := denmaCheckAllowedFrom(s); err != nil {
			return f, bad(err.Error())
		}
		if !denmaHasString(from, s) {
			from = append(from, s)
		}
	}
	if len(from) > denmaAPIMaxFrom {
		return f, bad(fmt.Sprintf("Give at most %d addresses.", denmaAPIMaxFrom))
	}
	f.AllowedFrom = from

	// The webhook: https only (http too with denmaWebhookPrivate).
	f.WebhookURL = strings.TrimSpace(f.WebhookURL)
	if f.WebhookURL != "" {
		u, err := url.Parse(f.WebhookURL)
		if err != nil || u.Host == "" || len(f.WebhookURL) > 2000 ||
			(u.Scheme != "https" && !(u.Scheme == "http" && denmaWebhookPrivate(a))) {
			return f, bad("The webhook needs a full https:// address, so that the addresses in its events are encrypted on the way.")
		}
		if u.User != nil {
			return f, bad("Put no user name or password in the webhook's address; check its signature instead.")
		}
		if !denmaHasString(f.Can, denmaAPICanEvents) {
			return f, bad("The webhook is sent the key's events: let the key read the events too.")
		}
	}

	// What a sending key sends from, and how much a day.
	f.Sender = strings.TrimSpace(f.Sender)
	if !denmaAPISends(f.Can) {
		f.Sender, f.DailyLimit = "", denmaAPIDefaultDailyLimit
	} else {
		if f.DailyLimit < 1 || f.DailyLimit > denmaMaxDailyCap {
			return f, bad(fmt.Sprintf("Give it a daily limit: the most e-mails it can send in 24 hours, from 1 to %d.", denmaMaxDailyCap))
		}
		if f.Sender != "" {
			if len(f.Sender) > 300 || !reFromAddress.MatchString(f.Sender) {
				if _, err := a.importer.SanitizeEmail(f.Sender); err != nil || len(f.Sender) > 300 {
					return f, bad(`The sender isn't a valid address. Use name@example.org or "Name <name@example.org>".`)
				}
			}
			if err := a.denmaCheckSender(f.Sender); err != nil { // the center's domains (cmd/denma_domains.go)
				return f, denmaBadRequest(err)
			}
		}
	}

	// When it expires: at the end of the day (UTC).
	if f.ExpiresOn = strings.TrimSpace(f.ExpiresOn); f.ExpiresOn != "" {
		d, err := time.Parse("2006-01-02", f.ExpiresOn)
		if err != nil {
			return f, bad("The expiry date isn't a date.")
		}
		t := d.Add(24*time.Hour - time.Second)
		if !t.After(time.Now()) {
			return f, bad("The expiry date has to be in the future.")
		}
		f.expiresAt = &t
	}
	return f, nil
}

// denmaCheckAllowedFrom checks an address a key can be used from: an IP
// address, a network (CIDR), or a host name.
func denmaCheckAllowedFrom(s string) error {
	if net.ParseIP(s) != nil {
		return nil
	}
	if _, _, err := net.ParseCIDR(s); err == nil {
		return nil
	}
	if reDenmaHostName.MatchString(s) {
		return nil
	}
	return fmt.Errorf("%q isn't an IP address, a network (such as 203.0.113.0/24) or a host name.", s)
}

func saveAPIKeyLists(tx *sql.Tx, id int, listIDs []int) error {
	if _, err := tx.Exec(`DELETE FROM denma_api_key_lists WHERE key_id = $1`, id); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO denma_api_key_lists (key_id, list_id) SELECT $1, UNNEST($2::INT[])`, id, pq.Array(listIDs))
	return err
}

// denmaAPIWho is who's changing a key: their name and username.
func denmaAPIWho(c echo.Context) string {
	u, ok := c.Get(auth.UserHTTPCtxKey).(auth.User)
	if !ok {
		return ""
	}
	if u.Name == "" || u.Name == u.Username {
		return u.Username
	}
	return fmt.Sprintf("%s (%s)", u.Name, u.Username)
}

// apiKeysInUse is how many keys the center has, not counting deleted ones.
func (a *App) apiKeysInUse() (int, error) {
	var n int
	err := a.db.Get(&n, `SELECT COUNT(*) FROM denma_api_keys WHERE deleted_at IS NULL`)
	return n, err
}

// DenmaCreateAPIKey makes a key; it's shown this once.
func (a *App) DenmaCreateAPIKey(c echo.Context) error {
	if err := a.inCenter(); err != nil {
		return err
	}
	f, err := a.checkAPIKeyForm(c)
	if err != nil {
		return err
	}
	n, err := a.apiKeysInUse()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if n >= denmaAPIMaxKeys {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("A center can have at most %d keys.", denmaAPIMaxKeys))
	}
	key, hash, hint, err := denmaNewAPIKey()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	secret, err := utils.GenerateRandomString(32)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	tx, err := a.db.Begin()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	defer tx.Rollback()
	var id int
	// The webhook starts after the events there are now.
	if err := tx.QueryRow(`INSERT INTO denma_api_keys (name, key_hash, key_hint, can, allowed_from, webhook_url, webhook_secret, webhook_after,
		sender, daily_limit, expires_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, (SELECT COALESCE(MAX(id), 0) FROM denma_api_events), $8, $9, $10, $11) RETURNING id`,
		f.Name, hash, hint, pq.Array(f.Can), pq.Array(f.AllowedFrom), f.WebhookURL, "whsec_"+secret,
		f.Sender, f.DailyLimit, f.expiresAt, denmaAPIWho(c)).Scan(&id); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if err := saveAPIKeyLists(tx, id, f.ListIDs); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	k, err := a.apiKey(id)
	if err != nil {
		return err
	}
	k.Key = key
	return c.JSON(http.StatusOK, okResp{k})
}

// DenmaUpdateAPIKey changes a key's name, what it can do, its lists,
// addresses, webhook, sender, daily limit or expiry date.
func (a *App) DenmaUpdateAPIKey(c echo.Context) error {
	if err := a.inCenter(); err != nil {
		return err
	}
	id, _ := strconv.Atoi(c.Param("id"))
	old, err := a.apiKey(id)
	if err != nil {
		return err
	}
	if old.DeletedAt != nil {
		return echo.NewHTTPError(http.StatusConflict, "The key is deleted; restore it first.")
	}
	f, err := a.checkAPIKeyForm(c)
	if err != nil {
		return err
	}
	tx, err := a.db.Begin()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	defer tx.Rollback()
	// A new webhook address starts after the current events; a new expiry
	// date resets its reminders.
	if _, err := tx.Exec(`UPDATE denma_api_keys SET name = $2, allowed_from = $3, webhook_url = $4, can = $6, updated_at = NOW(),
		webhook_after = CASE WHEN $5 THEN (SELECT COALESCE(MAX(id), 0) FROM denma_api_events) ELSE webhook_after END,
		webhook_failures = CASE WHEN $5 THEN 0 ELSE webhook_failures END,
		webhook_next_at = CASE WHEN $5 THEN NULL ELSE webhook_next_at END,
		webhook_error = CASE WHEN $5 THEN '' ELSE webhook_error END,
		sender = $7, daily_limit = $8,
		expiry_notice = CASE WHEN expires_at IS DISTINCT FROM $9 THEN '' ELSE expiry_notice END, expires_at = $9
		WHERE id = $1`, id, f.Name, pq.Array(f.AllowedFrom), f.WebhookURL, f.WebhookURL != old.WebhookURL, pq.Array(f.Can),
		f.Sender, f.DailyLimit, f.expiresAt); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if err := saveAPIKeyLists(tx, id, f.ListIDs); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	k, err := a.apiKey(id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, okResp{k})
}

// DenmaNewAPIKey replaces a key. The old one stops working at once, or after
// "overlap_hours" (up to a week).
func (a *App) DenmaNewAPIKey(c echo.Context) error {
	if err := a.inCenter(); err != nil {
		return err
	}
	id, _ := strconv.Atoi(c.Param("id"))
	var r struct {
		OverlapHours int `json:"overlap_hours"`
	}
	if err := c.Bind(&r); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if r.OverlapHours < 0 || r.OverlapHours > denmaAPIMaxOverlapHours {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("The old key can keep working for at most %d hours.", denmaAPIMaxOverlapHours))
	}
	key, hash, hint, err := denmaNewAPIKey()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	res, err := a.db.Exec(`UPDATE denma_api_keys SET
		old_key_hash = CASE WHEN $4 > 0 THEN key_hash END,
		old_key_until = CASE WHEN $4 > 0 THEN NOW() + make_interval(hours => $4) END,
		key_hash = $2, key_hint = $3, updated_at = NOW()
		WHERE id = $1 AND deleted_at IS NULL`, id, hash, hint, r.OverlapHours)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return echo.NewHTTPError(http.StatusNotFound, "That key doesn't exist (any more).")
	}
	k, err := a.apiKey(id)
	if err != nil {
		return err
	}
	k.Key = key
	return c.JSON(http.StatusOK, okResp{k})
}

// DenmaNewAPIWebhookSecret replaces a key's webhook signing secret.
func (a *App) DenmaNewAPIWebhookSecret(c echo.Context) error {
	if err := a.inCenter(); err != nil {
		return err
	}
	id, _ := strconv.Atoi(c.Param("id"))
	secret, err := utils.GenerateRandomString(32)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if _, err := a.db.Exec(`UPDATE denma_api_keys SET webhook_secret = $2, updated_at = NOW() WHERE id = $1`, id, "whsec_"+secret); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	k, err := a.apiKey(id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, okResp{k})
}

// DenmaDeleteAPIKey deletes a key, restorable for denmaAPIKeepDeletedDays
// days. With ?leaked=true, its unfinished campaigns are cancelled.
func (a *App) DenmaDeleteAPIKey(c echo.Context) error {
	if err := a.inCenter(); err != nil {
		return err
	}
	id, _ := strconv.Atoi(c.Param("id"))
	leaked := c.QueryParam("leaked") == "true"
	res, err := a.db.Exec(`UPDATE denma_api_keys SET deleted_at = NOW(), deleted_by = $2, leaked = $3,
		old_key_hash = NULL, old_key_until = NULL, updated_at = NOW() WHERE id = $1 AND deleted_at IS NULL`, id, denmaAPIWho(c), leaked)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return echo.NewHTTPError(http.StatusNotFound, "That key doesn't exist (any more).")
	}
	cancelled := 0
	if leaked {
		if cancelled, err = a.apiCancelKeyCampaigns(id); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "The key is deleted, but its campaigns couldn't all be cancelled: "+err.Error())
		}
		a.log.Printf("denma: API key %d deleted as leaked; %d of its campaigns cancelled", id, cancelled)
	}
	return c.JSON(http.StatusOK, okResp{map[string]any{"cancelled": cancelled}})
}

// apiCancelKeyCampaigns cancels a key's campaigns that haven't finished,
// returning how many.
func (a *App) apiCancelKeyCampaigns(keyID int) (int, error) {
	var camps []struct {
		ID     int    `db:"id"`
		Status string `db:"status"`
	}
	if err := a.db.Select(&camps, `SELECT c.id, c.status::TEXT AS status FROM denma_api_campaigns x JOIN campaigns c ON c.id = x.campaign_id
		WHERE x.key_id = $1 AND c.status IN ('draft', 'scheduled', 'running', 'paused')`, keyID); err != nil {
		return 0, err
	}
	for i, cp := range camps {
		if err := a.apiCancelCampaign(cp.ID, cp.Status); err != nil {
			return i, err
		}
	}
	return len(camps), nil
}

// DenmaRestoreAPIKey restores a deleted key. One deleted as leaked gets a
// new key, shown once.
func (a *App) DenmaRestoreAPIKey(c echo.Context) error {
	if err := a.inCenter(); err != nil {
		return err
	}
	id, _ := strconv.Atoi(c.Param("id"))
	k, err := a.apiKey(id)
	if err != nil {
		return err
	}
	if k.DeletedAt == nil {
		return echo.NewHTTPError(http.StatusConflict, "The key isn't deleted.")
	}
	n, err := a.apiKeysInUse()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if n >= denmaAPIMaxKeys {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("A center can have at most %d keys: delete another first.", denmaAPIMaxKeys))
	}
	key, hash, hint := "", "", ""
	if k.Leaked {
		if key, hash, hint, err = denmaNewAPIKey(); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
	}
	if _, err := a.db.Exec(`UPDATE denma_api_keys SET deleted_at = NULL, deleted_by = '', leaked = FALSE, updated_at = NOW(),
		key_hash = CASE WHEN $2 <> '' THEN $2 ELSE key_hash END, key_hint = CASE WHEN $2 <> '' THEN $3 ELSE key_hint END
		WHERE id = $1 AND deleted_at IS NOT NULL`, id, hash, hint); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if k, err = a.apiKey(id); err != nil {
		return err
	}
	k.Key = key
	return c.JSON(http.StatusOK, okResp{k})
}

// denmaAPIRate is each key's requests this minute (center/ID), for
// denmaAPIPerMinute.
var denmaAPIRate = struct {
	sync.Mutex
	m map[string]*denmaSignupWindow
}{m: map[string]*denmaSignupWindow{}}

func (a *App) apiAllowed(id int) bool {
	key := fmt.Sprintf("%s/%d", a.ko.String("denma.center"), id)
	now := time.Now()
	denmaAPIRate.Lock()
	defer denmaAPIRate.Unlock()
	w := denmaAPIRate.m[key]
	if w == nil || now.Sub(w.start) >= time.Minute {
		w = &denmaSignupWindow{start: now}
		denmaAPIRate.m[key] = w
	}
	w.n++
	return w.n <= denmaAPIPerMinute
}

// denmaAPIHosts caches host names' addresses, for keys limited to them.
var denmaAPIHosts = struct {
	sync.Mutex
	m map[string]denmaAPIHost
}{m: map[string]denmaAPIHost{}}

type denmaAPIHost struct {
	ips []net.IP
	at  time.Time
}

// denmaLookupHost returns a host name's addresses, from the cache if recent.
var denmaLookupHost = func(host string) []net.IP {
	denmaAPIHosts.Lock()
	h, ok := denmaAPIHosts.m[host]
	denmaAPIHosts.Unlock()
	if ok && time.Since(h.at) < denmaAPIHostTTL {
		return h.ips
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil && ok {
		return h.ips // keep the last answer while DNS fails
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, ad := range addrs {
		ips = append(ips, ad.IP)
	}
	denmaAPIHosts.Lock()
	denmaAPIHosts.m[host] = denmaAPIHost{ips: ips, at: time.Now()}
	denmaAPIHosts.Unlock()
	return ips
}

// denmaFromAllowed reports whether a request from ip may use a key limited
// to allowed (anyone, if it's empty).
func denmaFromAllowed(ip string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	addr := net.ParseIP(ip)
	if addr == nil {
		return false
	}
	for _, s := range allowed {
		if a := net.ParseIP(s); a != nil {
			if a.Equal(addr) {
				return true
			}
			continue
		}
		if _, n, err := net.ParseCIDR(s); err == nil {
			if n.Contains(addr) {
				return true
			}
			continue
		}
		for _, a := range denmaLookupHost(s) {
			if a.Equal(addr) {
				return true
			}
		}
	}
	return false
}

// denmaAPIAuthed is a key as a request uses it.
type denmaAPIAuthed struct {
	ID          int            `db:"id"`
	Name        string         `db:"name"`
	Can         pq.StringArray `db:"can"`
	AllowedFrom pq.StringArray `db:"allowed_from"`
	ListIDs     pq.Int64Array  `db:"list_ids"`
	Sender      string         `db:"sender"` // "" for the center's
	DailyLimit  int            `db:"daily_limit"`
	ExpiresAt   *time.Time     `db:"expires_at"`
}

// apiAuth checks a request's key (or the one it replaced, during its
// overlap): not deleted or expired, allowed from the client's address, and
// under its rate limit.
func (a *App) apiAuth(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if err := a.inCenter(); err != nil {
			return err
		}
		ip := c.RealIP()
		if wait := denmaAPIBadKeys.wait(ip); wait > 0 {
			return denmaTooMany(wait)
		}
		key, ok := denmaBearer(c.Request().Header.Get(echo.HeaderAuthorization))
		if !ok {
			denmaAPIBadKeys.add(ip)
			c.Response().Header().Set("WWW-Authenticate", `Bearer realm="listmonk"`)
			return echo.NewHTTPError(http.StatusUnauthorized, "Send the key as Authorization: Bearer <key>.")
		}
		var k denmaAPIAuthed
		if err := a.db.Get(&k, `SELECT id, name, can, allowed_from, sender, daily_limit, expires_at,
			ARRAY(SELECT list_id FROM denma_api_key_lists kl WHERE kl.key_id = k.id) AS list_ids
			FROM denma_api_keys k WHERE deleted_at IS NULL
			AND (key_hash = $1 OR (old_key_hash = $1 AND old_key_until > NOW()))`, denmaAPIHash(key)); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				denmaAPIBadKeys.add(ip)
				return echo.NewHTTPError(http.StatusUnauthorized, "This key isn't in use (any more).")
			}
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if k.ExpiresAt != nil && !k.ExpiresAt.After(time.Now()) {
			return echo.NewHTTPError(http.StatusUnauthorized, fmt.Sprintf(
				"This key expired on %s. The center's admins can give it a later date on its Config -> API page.", k.ExpiresAt.UTC().Format("2006-01-02")))
		}
		if !denmaFromAllowed(ip, k.AllowedFrom) {
			a.log.Printf("denma: API key %d (%s) refused from %s: not one of its addresses", k.ID, k.Name, ip)
			return echo.NewHTTPError(http.StatusForbidden, fmt.Sprintf("This key can't be used from %s.", ip))
		}
		if !a.apiAllowed(k.ID) {
			return echo.NewHTTPError(http.StatusTooManyRequests, fmt.Sprintf("At most %d requests a minute. Try again in a minute.", denmaAPIPerMinute))
		}
		if _, err := a.db.Exec(`UPDATE denma_api_keys SET uses = uses + 1, last_used_at = NOW(), last_ip = $2 WHERE id = $1`, k.ID, ip); err != nil {
			a.log.Printf("denma: error counting API key %d's use: %v", k.ID, err)
		}
		c.Set(denmaAPIKeyCtx, k)
		return next(c)
	}
}

// denmaBearer is the key in an Authorization header: "Bearer <key>", the
// scheme in any case (HTTP's are case-insensitive).
func denmaBearer(hdr string) (string, bool) {
	scheme, key, ok := strings.Cut(strings.TrimSpace(hdr), " ")
	key = strings.TrimSpace(key)
	return key, ok && strings.EqualFold(scheme, "Bearer") && strings.HasPrefix(key, denmaAPIKeyPrefix)
}

// apiKeyOf is the request's key (after apiAuth).
func apiKeyOf(c echo.Context) denmaAPIAuthed {
	k, _ := c.Get(denmaAPIKeyCtx).(denmaAPIAuthed)
	return k
}

// ---- Each key's daily limits, and telling the admins ----

// apiClaim takes n of a key's 24-hour allowance of a kind, if limit leaves
// room, returning whether it did and how many were used before.
func (a *App) apiClaim(keyID int, kind string, n, limit int) (bool, int, error) {
	tx, err := a.db.Beginx()
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback()
	// One claim at a time per key.
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext(current_schema() || '/denma_api_usage'), $1::INT)`, keyID); err != nil {
		return false, 0, err
	}
	var used int
	if err := tx.Get(&used, `SELECT COALESCE(SUM(n), 0)::INT FROM denma_api_usage
		WHERE key_id = $1 AND kind = $2 AND created_at > NOW() - INTERVAL '24 hours'`, keyID, kind); err != nil {
		return false, 0, err
	}
	if used+n > limit {
		return false, used, nil
	}
	if _, err := tx.Exec(`INSERT INTO denma_api_usage (key_id, kind, n) VALUES ($1, $2, $3)`, keyID, kind, n); err != nil {
		return false, 0, err
	}
	return true, used, tx.Commit()
}

// apiUnclaim gives back n of a claim that wasn't used.
func (a *App) apiUnclaim(keyID int, kind string, n int) {
	if n <= 0 {
		return
	}
	if _, err := a.db.Exec(`INSERT INTO denma_api_usage (key_id, kind, n) VALUES ($1, $2, $3)`, keyID, kind, -n); err != nil {
		a.log.Printf("denma: error giving back API key %d's use: %v", keyID, err)
	}
}

const (
	denmaAPIUseSend  = "send"  // e-mails, against the key's daily limit
	denmaAPIUseResub = "resub" // re-subscriptions, against denmaAPIResubPerDay
)

// apiSendClaim claims n e-mails of the key's daily limit, or answers 429 and
// alerts the admins.
func (a *App) apiSendClaim(c echo.Context, n int) error {
	k := apiKeyOf(c)
	ok, used, err := a.apiClaim(k.ID, denmaAPIUseSend, n, k.DailyLimit)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if ok {
		return nil
	}
	a.apiAlert(k.ID, k.Name, denmaAPIUseSend, fmt.Sprintf("reached its daily limit of %d e-mails", k.DailyLimit),
		fmt.Sprintf("It asked to send %d more after %d in the last 24 hours, and was refused. If that's more than the system using it should send, "+
			"the key may have leaked: delete it as leaked, which cancels its campaigns too. If it isn't, raise its daily limit.", n, used))
	return echo.NewHTTPError(http.StatusTooManyRequests, fmt.Sprintf(
		"This key's daily limit of %d e-mails leaves room for %d more now (%d in the last 24 hours), not %d. Send fewer, try again later, or ask the center's admins to raise the key's limit.",
		k.DailyLimit, max(k.DailyLimit-used, 0), used, n))
}

// apiAlert notifies the admins about a key, at most once a day per kind.
func (a *App) apiAlert(keyID int, keyName, kind, what, detail string) {
	res, err := a.db.Exec(`INSERT INTO denma_api_alerts (key_id, kind) VALUES ($1, $2)
		ON CONFLICT (key_id, kind) DO UPDATE SET sent_at = NOW() WHERE denma_api_alerts.sent_at < NOW() - INTERVAL '1 day'`, keyID, kind)
	if err != nil {
		a.log.Printf("denma: error recording an alert about API key %d: %v", keyID, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return // told already today
	}
	a.apiNotify(keyName, what, detail)
}

// apiNotify e-mails the center's admins and the superadmins about a key.
func (a *App) apiNotify(keyName, what, detail string) {
	center := a.ko.String("app.site_name")
	a.log.Printf("denma: API key %q %s", keyName, what)
	data := map[string]any{
		"Center": center, "Key": keyName, "What": what, "Detail": detail,
		"URL": strings.TrimSuffix(a.urlCfg.RootURL, "/") + "/admin/center/api",
	}
	subject := fmt.Sprintf("API key %q %s: %s", keyName, what, center)
	if err := a.notifs.NotifySystem(subject, "denma-api-alert", data, nil); err != nil {
		a.log.Printf("denma: error telling the center's admins about an API key: %v", err)
	}
	if hub := denmaHub.current(); hub != nil && hub.notifs != nil {
		if err := hub.notifs.NotifySystem(subject, "denma-api-alert", data, nil); err != nil {
			a.log.Printf("denma: error telling the superadmins about an API key: %v", err)
		}
	}
}

// denmaAPIState is whether the API's sending is paused.
type denmaAPIState struct {
	PausedAt     *time.Time `db:"paused_at" json:"paused_at"`
	PausedReason string     `db:"paused_reason" json:"paused_reason"`
	ResumedAt    *time.Time `db:"resumed_at" json:"resumed_at"`
}

func (a *App) apiState() (denmaAPIState, error) {
	var s denmaAPIState
	err := a.db.Get(&s, `SELECT paused_at, paused_reason, resumed_at FROM denma_api_state`)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	return s, err
}

// apiCanSend refuses to send while the API is paused.
func (a *App) apiCanSend() error {
	s, err := a.apiState()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if s.PausedAt != nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable,
			"Sending through the API is paused: "+s.PausedReason+" A superadmin has to resume it, on the center's Config -> API page.")
	}
	return nil
}

// DenmaResumeAPI resumes the API's sending after a pause; superadmins only.
// Its campaigns paused with it are resumed too.
func (a *App) DenmaResumeAPI(c echo.Context) error {
	if err := a.inCenter(); err != nil {
		return err
	}
	if auth.GetUser(c).UserRole.ID != auth.SuperAdminRoleID {
		return echo.NewHTTPError(http.StatusForbidden, "Only a superadmin can resume the API's sending.")
	}
	if _, err := a.db.Exec(`UPDATE denma_api_state SET paused_at = NULL, paused_reason = '', resumed_at = NOW()`); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	// The campaigns the pause stopped, as they were (cmd/denma_api_events.go).
	var camps []struct {
		ID     int        `db:"id"`
		Was    string     `db:"was"`
		SendAt *time.Time `db:"send_at"`
	}
	if err := a.db.Select(&camps, `UPDATE campaigns c SET attribs = c.attribs - 'denma_api_paused'
		FROM denma_api_campaigns x WHERE x.campaign_id = c.id AND c.status IN ('paused', 'draft')
			AND COALESCE(c.attribs->>'denma_api_paused', '') <> ''
		RETURNING c.id, (SELECT attribs->>'denma_api_paused' FROM campaigns o WHERE o.id = c.id) AS was, c.send_at`); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	for _, cm := range camps {
		status := "running"
		if cm.Was == "scheduled" && cm.SendAt != nil && cm.SendAt.After(time.Now()) {
			status = "scheduled"
		}
		if _, err := a.core.UpdateCampaignStatus(cm.ID, status); err != nil {
			a.log.Printf("denma: error resuming API campaign %d: %v", cm.ID, err)
		}
	}
	a.log.Printf("denma: the API's sending resumed by %s (%d campaigns resumed)", auth.GetUser(c).Username, len(camps))
	return c.JSON(http.StatusOK, okResp{true})
}
