package main

// denma: what the sending API tells the systems that use it, and its
// automatic pause (cmd/denma_api.go).
//
// Events: a center with keys records, in denma_api_events (by database
// triggers, whatever path did it), each address that
//
//   - unsubscribed, or complained (marked an e-mail as spam): its opt-out
//     (cmd/denma_optouts.go);
//   - bounced for good (a hard bounce of the center's mail);
//   - was blocklisted for another reason: its domain can't receive mail, or
//     it's on the shared blocklist (a hard bounce in another center), with
//     the reason.
//
// And how each API campaign goes, by a trigger on its status: started,
// paused, resumed, finished, cancelled (campaign_*, with the campaign's key
// and no address).
//
// Each event's place in the feed (seq, given as its ID) is given once it's
// committed (apiPublishEvents), in batches one after another, so that the
// feed only grows at its end: an event whose transaction commits after
// events with higher IDs (a long import's) comes after them, not before
// where a reader has already read.
//
// They're kept denmaAPIEventDays days. A key with the events permission gets
// those about addresses it has sent to (denma_api_contacts), since it did;
// a key that sends campaigns gets its campaigns'. GET /v1/events lists them
// (with the events permission), after an
// event ID; and each key with a webhook is sent them, in order, at most
// denmaAPIWebhookBatch at a time, by the minute's job: a POST of
// {"events": [...]}, signed with the key's webhook secret (the
// X-Listmonk-Signature header: t=<unix time>,v1=<hex HMAC-SHA256 of
// "<t>.<body>">). Anything but a 2xx answer is tried again later, waiting
// longer each time (up to denmaAPIWebhookMaxWait); later events wait their
// turn. Webhooks go only to https:// addresses, and not to private or local
// ones (both allowed by denma.api_private_webhooks, for development).
//
// Pause: every minute, a center with keys checks its sending since the later
// of a week ago and its last resume: its campaigns' and the API's e-mails,
// and its hard bounces and complaints. When there were at least
// denmaAPIPauseMinSends, and the hard bounces reach 5% (and
// denmaAPIPauseMinHard) or the complaints 0.1% (and
// denmaAPIPauseMinComplaints), the hub's "At risk" bands
// (assets/js/denma-stats.js), the API's sending is paused: its e-mails and
// new campaigns are refused (503), and its running campaigns are paused. The
// center's admin addresses and the hub's are told. A superadmin resumes it on
// Config -> API, which resumes those campaigns too.
//
// The job also starts API campaigns left "starting" (by a restart), deletes
// API campaigns' lists denmaAPIListKeepDays days after they're done and keys
// deleted denmaAPIKeepDeletedDays days ago, and reminds the admins of keys
// that expire.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/knadh/listmonk/internal/notifs"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

const (
	denmaAPIEventDays      = 30
	denmaAPIWebhookBatch   = 100 // events a POST
	denmaAPIWebhookPosts   = 10  // POSTs a key a minute
	denmaAPIWebhookTimeout = 10 * time.Second
	denmaAPIWebhookMaxWait = 6 * time.Hour

	denmaAPIPauseWindow        = 7 * 24 * time.Hour
	denmaAPIPauseMinSends      = 200
	denmaAPIPauseHardRate      = 0.05
	denmaAPIPauseMinHard       = 10
	denmaAPIPauseComplaintRate = 0.001
	denmaAPIPauseMinComplaints = 3
)

// denmaAPIEventsSQL makes the events' table and triggers (denmaAPISQL).
const denmaAPIEventsSQL = `
CREATE TABLE IF NOT EXISTS denma_api_events (
    id          BIGSERIAL PRIMARY KEY,
    type        TEXT NOT NULL, -- unsubscribed, complained, bounced, blocklisted
    email       TEXT NOT NULL,
    reason      TEXT NOT NULL DEFAULT '',
    campaign_id INTEGER NULL,
    created_at  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS denma_api_events_created ON denma_api_events (created_at);
-- To find an opt-out's subscriber.
CREATE INDEX IF NOT EXISTS denma_subscribers_email_hash ON subscribers (denma_email_hash(email));

CREATE OR REPLACE FUNCTION denma_api_optout_event() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM denma_api_keys) THEN RETURN NULL; END IF;
    IF TG_OP = 'UPDATE' AND OLD.kind = NEW.kind AND OLD.created_at = NEW.created_at THEN RETURN NULL; END IF;
    INSERT INTO denma_api_events (type, email)
        SELECT NEW.kind, lower(s.email) FROM subscribers s WHERE denma_email_hash(s.email) = NEW.email_hash LIMIT 1;
    RETURN NULL;
END $$;
DROP TRIGGER IF EXISTS denma_api_optout_event ON denma_optouts;
CREATE TRIGGER denma_api_optout_event AFTER INSERT OR UPDATE ON denma_optouts
    FOR EACH ROW EXECUTE FUNCTION denma_api_optout_event();

CREATE OR REPLACE FUNCTION denma_api_bounce_event() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM denma_api_keys) THEN RETURN NULL; END IF;
    INSERT INTO denma_api_events (type, email, reason, campaign_id)
        SELECT 'bounced', lower(s.email), 'hard bounce', NEW.campaign_id FROM subscribers s WHERE s.id = NEW.subscriber_id;
    RETURN NULL;
END $$;
DROP TRIGGER IF EXISTS denma_api_bounce_event ON bounces;
CREATE TRIGGER denma_api_bounce_event AFTER INSERT ON bounces
    FOR EACH ROW WHEN (NEW.type = 'hard') EXECUTE FUNCTION denma_api_bounce_event();

CREATE OR REPLACE FUNCTION denma_api_blocklist_event() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM denma_api_keys) THEN RETURN NULL; END IF;
    IF jsonb_typeof(NEW.attribs) = 'object' AND coalesce(NEW.attribs->>'auto_blocklist_reason', '') <> ''
        -- Not again for a hard bounce here, which was an event already.
        AND NOT EXISTS (SELECT 1 FROM denma_api_events e WHERE e.type = 'bounced' AND e.email = lower(NEW.email)
            AND e.created_at > NOW() - INTERVAL '1 day') THEN
        INSERT INTO denma_api_events (type, email, reason) VALUES ('blocklisted', lower(NEW.email), NEW.attribs->>'auto_blocklist_reason');
    END IF;
    RETURN NULL;
END $$;
DROP TRIGGER IF EXISTS denma_api_blocklist_event ON subscribers;
CREATE TRIGGER denma_api_blocklist_event AFTER UPDATE OF status ON subscribers
    FOR EACH ROW WHEN (NEW.status = 'blocklisted' AND OLD.status <> 'blocklisted')
    EXECUTE FUNCTION denma_api_blocklist_event();

-- Since features version 22: how API campaigns go, an event about their key
-- with no address. Only when the status changes: sending updates campaigns
-- all the time.
ALTER TABLE denma_api_events ADD COLUMN IF NOT EXISTS key_id INTEGER NULL;
ALTER TABLE denma_api_events ALTER COLUMN email DROP NOT NULL;
CREATE INDEX IF NOT EXISTS denma_api_events_key ON denma_api_events (key_id, id) WHERE key_id IS NOT NULL;
CREATE OR REPLACE FUNCTION denma_api_campaign_event() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    INSERT INTO denma_api_events (type, key_id, campaign_id)
        SELECT CASE
            WHEN NEW.status = 'running' AND OLD.status = 'paused' THEN 'campaign_resumed'
            WHEN NEW.status = 'running' THEN 'campaign_started'
            ELSE 'campaign_' || NEW.status::TEXT END, x.key_id, NEW.id
        FROM denma_api_campaigns x WHERE x.campaign_id = NEW.id AND x.key_id IS NOT NULL;
    RETURN NULL;
END $$;
-- As an API campaign starts: the key's contacts (denma_api_contacts, for its
-- events) are those it goes to, and the name given (and first and last
-- names, cmd/denma_names.go) replaces a stand-in one (blank, their address,
-- or the importer's from it).
CREATE OR REPLACE FUNCTION denma_api_campaign_contacts() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    UPDATE subscribers s SET name = sl.meta->>'name', first_name = COALESCE(sl.meta->>'first_name', s.first_name),
        last_name = COALESCE(sl.meta->>'last_name', s.last_name), updated_at = NOW()
        FROM denma_api_campaigns x JOIN subscriber_lists sl ON sl.list_id = x.list_id
        WHERE x.campaign_id = NEW.id AND sl.subscriber_id = s.id AND COALESCE(sl.meta->>'name', '') <> ''
        AND sl.status <> 'unsubscribed' AND s.status <> 'blocklisted'
        AND LOWER(TRIM(s.name)) IN ('', LOWER(s.email), split_part(LOWER(s.email), '@', 1),
            LOWER(denma_tidy_name(replace(split_part(s.email, '@', 1), '.', ' '))));
    INSERT INTO denma_api_contacts (key_id, email_hash)
        SELECT x.key_id, denma_email_hash(s.email) FROM denma_api_campaigns x
        JOIN subscriber_lists sl ON sl.list_id = x.list_id AND sl.status <> 'unsubscribed'
        JOIN subscribers s ON s.id = sl.subscriber_id AND s.status <> 'blocklisted'
        WHERE x.campaign_id = NEW.id AND x.key_id IS NOT NULL
        ON CONFLICT DO NOTHING;
    RETURN NULL;
END $$;
DROP TRIGGER IF EXISTS denma_api_campaign_contacts ON campaigns;
CREATE TRIGGER denma_api_campaign_contacts AFTER UPDATE OF status ON campaigns
    FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status AND NEW.status = 'running')
    EXECUTE FUNCTION denma_api_campaign_contacts();
DROP TRIGGER IF EXISTS denma_api_campaign_event ON campaigns;
CREATE TRIGGER denma_api_campaign_event AFTER UPDATE OF status ON campaigns
    FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status AND NEW.status IN ('running', 'paused', 'finished', 'cancelled'))
    EXECUTE FUNCTION denma_api_campaign_event();

-- Since features version 31: each event's place in the feed (seq), given
-- once it's committed (apiPublishEvents); event_seq is the last one given.
-- Events from before keep their ID as their place, and new ones come after
-- every ID given so far, so readers' and webhooks' positions stay right.
ALTER TABLE denma_api_events ADD COLUMN IF NOT EXISTS seq BIGINT NULL;
ALTER TABLE denma_api_state ADD COLUMN IF NOT EXISTS event_seq BIGINT NULL;
UPDATE denma_api_events SET seq = id WHERE seq IS NULL AND (SELECT event_seq FROM denma_api_state) IS NULL;
UPDATE denma_api_state SET event_seq = (SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM denma_api_events_id_seq)
    WHERE event_seq IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS denma_api_events_seq ON denma_api_events (seq);
CREATE INDEX IF NOT EXISTS denma_api_events_unplaced ON denma_api_events (id) WHERE seq IS NULL;
CREATE INDEX IF NOT EXISTS denma_api_events_key_seq ON denma_api_events (key_id, seq) WHERE key_id IS NOT NULL;
DROP INDEX IF EXISTS denma_api_events_key;
`

// denmaAPIKeyEventsSQL is a key's events after a place in the feed ($1), at
// most $2: with the events permission ($4), those about the addresses the
// key ($3) has sent to, since it did; if it sends campaigns ($5), its
// campaigns'. Their place is their ID.
const denmaAPIKeyEventsSQL = `SELECT e.seq AS id, e.type, COALESCE(e.email, '') AS email, e.reason, e.campaign_id, e.created_at
	FROM denma_api_events e
	WHERE e.seq > $1 AND ((e.key_id = $3 AND $5) OR (e.key_id IS NULL AND $4 AND EXISTS (SELECT 1 FROM denma_api_contacts k
		WHERE k.key_id = $3 AND k.email_hash = denma_email_hash(e.email) AND k.created_at <= e.created_at)))
	ORDER BY e.seq LIMIT $2`

// apiPublishEvents gives the events committed since it last ran their
// places in the feed, in ID order, after the last place given. One runs at a
// time (the state row's lock), and each commits before the next starts, so
// places are seen in order.
func (a *App) apiPublishEvents() error {
	tx, err := a.db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var last int64
	if err := tx.Get(&last, `SELECT COALESCE(event_seq, 0) FROM denma_api_state FOR UPDATE`); err != nil {
		return err
	}
	var n int64
	if err := tx.Get(&n, `WITH p AS (SELECT id, ROW_NUMBER() OVER (ORDER BY id) AS n FROM denma_api_events WHERE seq IS NULL),
		u AS (UPDATE denma_api_events e SET seq = $1 + p.n FROM p WHERE e.id = p.id RETURNING 1)
		SELECT COUNT(*) FROM u`, last); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	if _, err := tx.Exec(`UPDATE denma_api_state SET event_seq = $1`, last+n); err != nil {
		return err
	}
	return tx.Commit()
}

// denmaAPIKeyEventsArgs are denmaAPIKeyEventsSQL's arguments for a key that
// can do can.
func denmaAPIKeyEventsArgs(after int64, limit, keyID int, can []string) []any {
	return []any{after, limit, keyID, denmaHasString(can, denmaAPICanEvents), denmaHasString(can, denmaAPICanCampaigns)}
}

// denmaAPIContacts records that a key has sent to or subscribed addresses
// (lowercase), for its events.
func denmaAPIContacts(db interface {
	Exec(string, ...any) (sql.Result, error)
}, keyID int, emails []string) error {
	if keyID == 0 || len(emails) == 0 {
		return nil
	}
	_, err := db.Exec(`INSERT INTO denma_api_contacts (key_id, email_hash)
		SELECT $1, denma_email_hash(e) FROM unnest($2::TEXT[]) AS e ON CONFLICT DO NOTHING`, keyID, pq.Array(emails))
	return err
}

// denmaAPIEvent is an event, as the API and webhooks give it.
type denmaAPIEvent struct {
	ID         int64     `db:"id" json:"id"`
	Type       string    `db:"type" json:"type"`
	Email      string    `db:"email" json:"email,omitempty"` // none for a campaign's
	Reason     string    `db:"reason" json:"reason"`
	CampaignID *int      `db:"campaign_id" json:"campaign_id"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
}

// DenmaAPIEvents lists events after an ID (after, 0 for the oldest kept),
// oldest first, at most limit (100, up to 1,000).
func (a *App) DenmaAPIEvents(c echo.Context) error {
	after, _ := strconv.ParseInt(c.QueryParam("after"), 10, 64)
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 1000)
	out := []denmaAPIEvent{}
	k := apiKeyOf(c)
	if err := a.apiPublishEvents(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if err := a.db.Select(&out, denmaAPIKeyEventsSQL, denmaAPIKeyEventsArgs(after, limit, k.ID, k.Can)...); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	next := after
	if len(out) > 0 {
		next = out[len(out)-1].ID
	}
	return c.JSON(http.StatusOK, okResp{map[string]any{"events": out, "after": next, "more": len(out) == limit}})
}

// denmaStartAPIJobs adds the API's minute job to a center's cron. Called by
// buildApp.
func denmaStartAPIJobs(a *App) {
	if a.crons == nil || !a.denmaInCenter() {
		return
	}
	if _, err := denmaEveryMinute(a, 40, "api", func() {
		a.signupLostLists() // cmd/denma_signup.go
		var n int
		if err := a.db.Get(&n, `SELECT COUNT(*) FROM denma_api_keys`); err != nil || n == 0 {
			return // no keys (or not installed yet)
		}
		a.apiCheckPause()
		a.apiResumeStarting()
		a.apiCleanUp()
		a.apiExpiryReminders()
		if err := a.apiPublishEvents(); err != nil {
			a.log.Printf("denma: error placing the API's events: %v", err)
		}
		a.apiSendWebhooks()
	}); err != nil {
		a.log.Printf("denma: error adding the API's job: %v", err)
	}
}

// ---- Pause ----

// denmaAPIRates is the center's sending since a time, for the pause.
type denmaAPIRates struct {
	Sends      int `db:"sends"`
	Hard       int `db:"hard"`
	Complaints int `db:"complaints"`
}

// denmaAPIPauseReason says why the rates call for a pause ("" if they
// don't).
func denmaAPIPauseReason(r denmaAPIRates) string {
	if r.Sends < denmaAPIPauseMinSends {
		return ""
	}
	pct := func(n int) string { return strconv.FormatFloat(100*float64(n)/float64(r.Sends), 'f', 2, 64) + "%" }
	if r.Hard >= denmaAPIPauseMinHard && float64(r.Hard) >= denmaAPIPauseHardRate*float64(r.Sends) {
		return fmt.Sprintf("%d hard bounces in %d e-mails (%s), at Amazon SES's review level (5%%).", r.Hard, r.Sends, pct(r.Hard))
	}
	if r.Complaints >= denmaAPIPauseMinComplaints && float64(r.Complaints) >= denmaAPIPauseComplaintRate*float64(r.Sends) {
		return fmt.Sprintf("%d spam complaints in %d e-mails (%s), at Amazon SES's review level (0.1%%).", r.Complaints, r.Sends, pct(r.Complaints))
	}
	return ""
}

// apiCheckPause pauses the API's sending if the center's bounces or
// complaints call for it.
func (a *App) apiCheckPause() {
	st, err := a.apiState()
	if err != nil || st.PausedAt != nil {
		return
	}
	since := time.Now().Add(-denmaAPIPauseWindow)
	if st.ResumedAt != nil && st.ResumedAt.After(since) {
		since = *st.ResumedAt
	}
	var r denmaAPIRates
	if err := a.db.Get(&r, `SELECT
		(SELECT COALESCE(SUM(sent), 0) FROM campaigns WHERE started_at >= $1)
			+ (SELECT COALESCE(SUM(sent), 0) FROM denma_api_messages WHERE created_at >= $1) AS sends,
		(SELECT COUNT(*) FROM bounces WHERE type = 'hard' AND created_at >= $1) AS hard,
		(SELECT COUNT(*) FROM bounces WHERE type = 'complaint' AND created_at >= $1) AS complaints`, since); err != nil {
		a.log.Printf("denma: error checking the API's bounce rates: %v", err)
		return
	}
	reason := denmaAPIPauseReason(r)
	if reason == "" {
		return
	}
	a.apiPause(reason)
}

// apiPause pauses the API's sending and its running campaigns, and tells the
// center's and the hub's admins.
func (a *App) apiPause(reason string) {
	res, err := a.db.Exec(`UPDATE denma_api_state SET paused_at = NOW(), paused_reason = $1 WHERE paused_at IS NULL`, reason)
	if err != nil {
		a.log.Printf("denma: error pausing the API: %v", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return
	}
	// Its running campaigns paused, and its scheduled ones back to drafts,
	// each marked with what it was (denma_api_paused), for the resume.
	var ids []int
	if err := a.db.Select(&ids, `UPDATE campaigns c SET attribs = c.attribs || jsonb_build_object('denma_api_paused', c.status::TEXT)
		FROM denma_api_campaigns x WHERE x.campaign_id = c.id AND c.status IN ('running', 'scheduled') RETURNING c.id`); err != nil {
		a.log.Printf("denma: error pausing the API's campaigns: %v", err)
	}
	if _, err := a.db.Exec(`UPDATE campaigns SET status = 'draft' WHERE id = ANY($1) AND status = 'scheduled'`, pq.Array(ids)); err != nil {
		a.log.Printf("denma: error holding the API's scheduled campaigns: %v", err)
	}
	for _, id := range ids {
		if _, err := a.core.UpdateCampaignStatus(id, "paused"); err == nil {
			a.manager.StopCampaign(id)
		}
	}
	center := a.ko.String("app.site_name")
	a.log.Printf("denma: the API's sending is paused (%d campaigns): %s", len(ids), reason)

	data := map[string]any{
		"Center": center, "Reason": reason, "Campaigns": len(ids),
		"URL": strings.TrimSuffix(a.urlCfg.RootURL, "/") + "/admin/center/api",
	}
	subject := "Sending through the API is paused: " + center
	if err := a.notifs.NotifySystem(subject, "denma-api-paused", data, nil); err != nil {
		a.log.Printf("denma: error telling the center's admins of the API's pause: %v", err)
	}
	if hub := denmaHub.current(); hub != nil && hub.notifs != nil {
		if err := hub.notifs.NotifySystem(subject, "denma-api-paused", data, nil); err != nil {
			a.log.Printf("denma: error telling the superadmins of the API's pause: %v", err)
		}
	}
}

// denmaAddAPITpls adds the API's alert and pause e-mails to a notifier's
// templates. Called by buildApp, before they're first used.
func denmaAddAPITpls(n *notifs.Notifs) error {
	if _, err := n.Tpls.New("denma-api-alert").Parse(`{{ template "header" . }}
<h2>API key "{{ index . "Key" }}"</h2>
<p>{{ index . "Center" }}'s API key "{{ index . "Key" }}" {{ index . "What" }}.</p>
<p>{{ index . "Detail" }}</p>
<p><a href="{{ index . "URL" }}" class="button">Open the API page</a></p>
{{ template "footer" }}`); err != nil {
		return err
	}
	// The admins' e-mails asking for consent (cmd/denma_consentcap.go).
	if _, err := n.Tpls.New("denma-consent-alert").Parse(`{{ template "header" . }}
<h2>E-mails asking for consent</h2>
<p>{{ index . "Center" }}'s admins {{ index . "What" }}.</p>
<p>{{ index . "Detail" }}</p>
<p><a href="{{ index . "URL" }}" class="button">Open the activity log</a></p>
{{ template "footer" }}`); err != nil {
		return err
	}
	// A sign-up webhook's (cmd/denma_signup.go).
	if _, err := n.Tpls.New("denma-signup-alert").Parse(`{{ template "header" . }}
<h2>Sign-up webhook "{{ index . "Hook" }}"</h2>
<p>{{ index . "Center" }}'s sign-up webhook "{{ index . "Hook" }}" {{ index . "What" }}.</p>
<p>{{ index . "Detail" }}</p>
<p><a href="{{ index . "URL" }}" class="button">Open the Webhooks page</a></p>
{{ template "footer" }}`); err != nil {
		return err
	}
	_, err := n.Tpls.New("denma-api-paused").Parse(`{{ template "header" . }}
<h2>Sending through the API is paused</h2>
<p>{{ index . "Center" }}'s e-mails sent by other systems, through its API, are paused: {{ index . "Reason" }}</p>
<p>Its API e-mails are refused, and its API campaigns are paused ({{ index . "Campaigns" }} were sending). Its other campaigns go on.
Find out why (the center's Bounces page, and the system sending the e-mails) before resuming: Amazon SES pauses sending for every center when an account's rates stay this high.</p>
<p><a href="{{ index . "URL" }}" class="button">Open the API page</a></p>
<p style="color: #666; font-size: 12px;">A superadmin resumes it there.</p>
{{ template "footer" }}`)
	return err
}

// ---- Housekeeping ----

// apiResumeStarting starts API campaigns left starting (by a restart).
func (a *App) apiResumeStarting() {
	var ids []int
	if err := a.db.Select(&ids, `SELECT x.campaign_id FROM denma_api_campaigns x JOIN campaigns c ON c.id = x.campaign_id
		WHERE x.state = 'starting' AND c.status = 'draft' AND x.created_at < NOW() - INTERVAL '2 minutes'`); err != nil {
		a.log.Printf("denma: error finding API campaigns to start: %v", err)
		return
	}
	for _, id := range ids {
		go a.apiStart(id)
	}
}

// apiCleanUp deletes old events, finished API campaigns' lists (and cancels
// drafts never started), old key use,
// replaced keys past their overlap, and deleted keys past restoring.
func (a *App) apiCleanUp() {
	if _, err := a.db.Exec(`DELETE FROM denma_api_events WHERE created_at < NOW() - make_interval(days => $1)`, denmaAPIEventDays); err != nil {
		a.log.Printf("denma: error deleting old API events: %v", err)
	}
	if _, err := a.db.Exec(`DELETE FROM lists WHERE id IN (SELECT x.list_id FROM denma_api_campaigns x
		JOIN campaigns c ON c.id = x.campaign_id
		WHERE c.status IN ('finished', 'cancelled') AND c.updated_at < NOW() - make_interval(days => $1))`, denmaAPIListKeepDays); err != nil {
		a.log.Printf("denma: error deleting old API campaigns' lists: %v", err)
	}
	// Drafts never started are cancelled (their lists then go, as above).
	if _, err := a.db.Exec(`UPDATE campaigns c SET status = 'cancelled', updated_at = NOW() FROM denma_api_campaigns x
		WHERE x.campaign_id = c.id AND c.status = 'draft' AND x.state = 'draft' AND x.created_at < NOW() - make_interval(days => $1)`,
		denmaAPIDraftDays); err != nil {
		a.log.Printf("denma: error cancelling old API campaign drafts: %v", err)
	}
	if _, err := a.db.Exec(`DELETE FROM denma_api_messages WHERE created_at < NOW() - INTERVAL '90 days'`); err != nil {
		a.log.Printf("denma: error deleting old API e-mail records: %v", err)
	}
	if _, err := a.db.Exec(`DELETE FROM denma_resub_usage WHERE created_at < NOW() - INTERVAL '2 days'`); err != nil { // cmd/denma_signup.go
		a.log.Printf("denma: error deleting old re-subscription counts: %v", err)
	}
	if _, err := a.db.Exec(`DELETE FROM denma_consent_requests WHERE created_at < NOW() - INTERVAL '2 days'`); err != nil { // cmd/denma_consentcap.go
		a.log.Printf("denma: error deleting old consent e-mail counts: %v", err)
	}
	if _, err := a.db.Exec(`DELETE FROM denma_api_usage WHERE created_at < NOW() - INTERVAL '2 days'`); err != nil {
		a.log.Printf("denma: error deleting old API key use: %v", err)
	}
	if _, err := a.db.Exec(`UPDATE denma_api_keys SET old_key_hash = NULL, old_key_until = NULL WHERE old_key_until < NOW()`); err != nil {
		a.log.Printf("denma: error forgetting replaced API keys: %v", err)
	}
	if _, err := a.db.Exec(`DELETE FROM denma_api_keys WHERE deleted_at < NOW() - make_interval(days => $1)`, denmaAPIKeepDeletedDays); err != nil {
		a.log.Printf("denma: error deleting API keys deleted %d days ago: %v", denmaAPIKeepDeletedDays, err)
	}
}

// apiExpiryReminders notifies the admins of keys expiring within 14 days,
// within 3, and expired.
func (a *App) apiExpiryReminders() {
	var keys []struct {
		ID        int       `db:"id"`
		Name      string    `db:"name"`
		ExpiresAt time.Time `db:"expires_at"`
		Notice    string    `db:"expiry_notice"`
	}
	if err := a.db.Select(&keys, `SELECT id, name, expires_at, expiry_notice FROM denma_api_keys
		WHERE deleted_at IS NULL AND expires_at < NOW() + INTERVAL '14 days' AND expiry_notice <> 'expired'`); err != nil {
		a.log.Printf("denma: error reading API keys' expiry dates: %v", err)
		return
	}
	for _, k := range keys {
		left := time.Until(k.ExpiresAt)
		date := k.ExpiresAt.UTC().Format("Monday 2 January 2006")
		notice, what := "14", "expires on "+date
		detail := "The system using it can't use it after that. To keep it working, give it a later expiry date on the API page; or give it a new key with an expiry date, and the system the new key."
		switch {
		case left <= 0:
			notice, what = "expired", "has expired"
			detail = "It expired on " + date + ", and the system using it can't use it any more. Give it a later expiry date on the API page if it should still work."
		case left <= 3*24*time.Hour:
			notice = "3"
		}
		if notice == k.Notice {
			continue
		}
		res, err := a.db.Exec(`UPDATE denma_api_keys SET expiry_notice = $2 WHERE id = $1 AND expiry_notice = $3`, k.ID, notice, k.Notice)
		if err != nil {
			a.log.Printf("denma: error recording API key %d's expiry reminder: %v", k.ID, err)
			continue
		}
		if n, _ := res.RowsAffected(); n == 1 {
			a.apiNotify(k.Name, what, detail)
		}
	}
}

// ---- Webhooks ----

// denmaWebhookPrivate is whether webhooks may go to private and local
// addresses (development).
func denmaWebhookPrivate(a *App) bool {
	return a.ko.Bool("denma.api_private_webhooks")
}

// denmaPublicIP reports whether an address is on the internet: not
// loopback, private, link-local (cloud metadata), unspecified, or another
// range that isn't (denmaNotPublic).
func denmaPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	for _, n := range denmaNotPublic {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

// denmaNotPublic are ranges Go's checks don't cover that aren't the
// internet's: shared address space (carrier-grade NAT), IETF protocol
// assignments, benchmarking, and NAT64 (an IPv4 address in an IPv6 one).
var denmaNotPublic = func() []*net.IPNet {
	var out []*net.IPNet
	for _, s := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15", "64:ff9b::/96", "64:ff9b:1::/48"} {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}()

// denmaWebhookClient sends webhooks, to public addresses only unless
// private.
func denmaWebhookClient(private bool) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	if !private {
		dialer.Control = func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || !denmaPublicIP(ip) {
				return fmt.Errorf("%s is a private or local address", host)
			}
			return nil
		}
	}
	return &http.Client{
		Timeout: denmaAPIWebhookTimeout,
		Transport: &http.Transport{
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: 5 * time.Second,
			MaxIdleConns:        10,
			IdleConnTimeout:     30 * time.Second,
		},
		// Redirects aren't followed: the address should be the webhook's own.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// denmaWebhookSignature signs a webhook's body.
func denmaWebhookSignature(secret string, t int64, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(m, "%d.", t)
	m.Write(body)
	return fmt.Sprintf("t=%d,v1=%s", t, hex.EncodeToString(m.Sum(nil)))
}

// postWebhook sends events to a webhook, returning why it failed.
func (a *App) postWebhook(to, secret string, events []denmaAPIEvent) error {
	body, err := json.Marshal(map[string]any{"center": a.ko.String("app.site_name"), "events": events})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), denmaAPIWebhookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, to, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "listmonk-webhooks/1.0")
	req.Header.Set("X-Listmonk-Signature", denmaWebhookSignature(secret, time.Now().Unix(), body))
	resp, err := denmaWebhookClient(denmaWebhookPrivate(a)).Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return ue.Err // without the address
		}
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("it answered %s", resp.Status)
	}
	return nil
}

// denmaWebhookWait is how long to wait after the nth failure in a row: 1, 2,
// 4... minutes, up to denmaAPIWebhookMaxWait.
func denmaWebhookWait(n int) time.Duration {
	if n > 12 {
		return denmaAPIWebhookMaxWait
	}
	return min(time.Duration(1<<max(n-1, 0))*time.Minute, denmaAPIWebhookMaxWait)
}

// apiSendWebhooks sends each key's webhook its new events.
func (a *App) apiSendWebhooks() {
	var keys []struct {
		ID       int            `db:"id"`
		URL      string         `db:"webhook_url"`
		Secret   string         `db:"webhook_secret"`
		After    int64          `db:"webhook_after"`
		Failures int            `db:"webhook_failures"`
		Can      pq.StringArray `db:"can"`
	}
	if err := a.db.Select(&keys, `SELECT id, webhook_url, webhook_secret, webhook_after, webhook_failures, can FROM denma_api_keys
		WHERE webhook_url <> '' AND deleted_at IS NULL AND ('events' = ANY(can) OR 'campaigns' = ANY(can))
		AND (expires_at IS NULL OR expires_at > NOW())
		AND (webhook_next_at IS NULL OR webhook_next_at <= NOW())
		AND EXISTS (SELECT 1 FROM denma_api_events e WHERE e.seq > webhook_after)`); err != nil {
		a.log.Printf("denma: error reading the API's webhooks: %v", err)
		return
	}
	for _, k := range keys {
		// The newest event before reading, to skip to if none are the key's.
		var top int64
		if err := a.db.Get(&top, `SELECT COALESCE(MAX(seq), 0) FROM denma_api_events`); err != nil {
			a.log.Printf("denma: error reading the API's events: %v", err)
			return
		}
		for range denmaAPIWebhookPosts {
			var events []denmaAPIEvent
			if err := a.db.Select(&events, denmaAPIKeyEventsSQL, denmaAPIKeyEventsArgs(k.After, denmaAPIWebhookBatch, k.ID, k.Can)...); err != nil {
				a.log.Printf("denma: error reading the API's events: %v", err)
				return
			}
			if len(events) == 0 {
				// None of its own: skip to top.
				if _, err := a.db.Exec(`UPDATE denma_api_keys SET webhook_after = GREATEST(webhook_after, $3)
					WHERE id = $1 AND webhook_after = $2`, k.ID, k.After, top); err != nil {
					a.log.Printf("denma: error recording API key %d's webhook: %v", k.ID, err)
				}
				break
			}
			if err := a.postWebhook(k.URL, k.Secret, events); err != nil {
				k.Failures++
				a.log.Printf("denma: API key %d's webhook failed (%d in a row): %v", k.ID, k.Failures, err)
				if _, err2 := a.db.Exec(`UPDATE denma_api_keys SET webhook_failures = $2, webhook_error = $3, webhook_next_at = $4 WHERE id = $1`,
					k.ID, k.Failures, err.Error(), time.Now().Add(denmaWebhookWait(k.Failures))); err2 != nil {
					a.log.Printf("denma: error recording API key %d's webhook failure: %v", k.ID, err2)
				}
				break
			}
			k.After, k.Failures = events[len(events)-1].ID, 0
			if _, err := a.db.Exec(`UPDATE denma_api_keys SET webhook_after = $2, webhook_failures = 0, webhook_error = '',
				webhook_next_at = NULL, webhook_ok_at = NOW() WHERE id = $1`, k.ID, k.After); err != nil {
				a.log.Printf("denma: error recording API key %d's webhook: %v", k.ID, err)
				break
			}
		}
	}
}

// DenmaTestAPIWebhook sends a key's webhook a test event, now, and says
// how it went.
func (a *App) DenmaTestAPIWebhook(c echo.Context) error {
	if err := a.inCenter(); err != nil {
		return err
	}
	id, _ := strconv.Atoi(c.Param("id"))
	k, err := a.apiKey(id)
	if err != nil {
		return err
	}
	if k.WebhookURL == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "The key has no webhook.")
	}
	ev := denmaAPIEvent{ID: 0, Type: "test", Email: "test@example.org", Reason: "a test from the center's Config -> API page", CreatedAt: time.Now()}
	if err := a.postWebhook(k.WebhookURL, k.WebhookSecret, []denmaAPIEvent{ev}); err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "The webhook didn't take it: "+err.Error())
	}
	return c.JSON(http.StatusOK, okResp{true})
}
