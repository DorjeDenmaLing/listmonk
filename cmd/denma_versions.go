package main

// denma: versions of campaigns, templates and automations. A save gives the version it started from and is refused
// if someone saved since; each save is kept in the item's history; editors say who else has it open.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

const (
	denmaVersionsMax      = 30 // kept of each item
	denmaVersionsMaxSQL   = "30"
	denmaVersionsSentDays = 60  // kept after a campaign's sending ends
	denmaVersionsDays     = 365 // kept of templates and automations
	denmaVersionAsideMax  = 5 << 20
)

// denmaVersionColumnsSQL gives campaigns and templates their version, in the hub too (denmaHubColumns).
const denmaVersionColumnsSQL = `
ALTER TABLE campaigns ADD COLUMN IF NOT EXISTS denma_version INT NOT NULL DEFAULT 1;
ALTER TABLE templates ADD COLUMN IF NOT EXISTS denma_version INT NOT NULL DEFAULT 1;
`

// denmaVersionsSQL makes the versions, their table and its triggers, in a center (denmaFeaturesSQL).
const denmaVersionsSQL = denmaVersionColumnsSQL + `
ALTER TABLE denma_automations ADD COLUMN IF NOT EXISTS denma_version INT NOT NULL DEFAULT 1;
CREATE TABLE IF NOT EXISTS denma_versions (
    id            BIGSERIAL PRIMARY KEY,
    campaign_id   INT REFERENCES campaigns(id) ON DELETE CASCADE,
    template_id   INT REFERENCES templates(id) ON DELETE CASCADE,
    automation_id INT REFERENCES denma_automations(id) ON DELETE CASCADE,
    version       INT NOT NULL,
    saved_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    user_id       INT,
    saved_by      TEXT NOT NULL DEFAULT '',
    via           TEXT NOT NULL DEFAULT '',
    set_aside     BOOLEAN NOT NULL DEFAULT false,
    changed       TEXT[] NOT NULL DEFAULT '{}',
    data          JSONB NOT NULL,
    CHECK (num_nonnulls(campaign_id, template_id, automation_id) = 1)
);
CREATE INDEX IF NOT EXISTS denma_versions_campaign ON denma_versions (campaign_id, id) WHERE campaign_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS denma_versions_template ON denma_versions (template_id, id) WHERE template_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS denma_versions_automation ON denma_versions (automation_id, id) WHERE automation_id IS NOT NULL;

-- An item as its editor loads it, or NULL if it's gone.
CREATE OR REPLACE FUNCTION denma_version_data(tbl TEXT, item INT) RETURNS JSONB LANGUAGE sql STABLE SET search_path FROM CURRENT AS $$
SELECT CASE tbl
WHEN 'campaigns' THEN (SELECT jsonb_build_object('name', c.name, 'subject', c.subject, 'preview_text', c.denma_preview_text,
    'from_email', c.from_email, 'reply_to', c.denma_reply_to, 'messenger', c.messenger, 'tags', to_jsonb(c.tags),
    'subscriber_tags', to_jsonb(c.denma_tags), 'send_at', c.send_at, 'attribs', c.attribs, 'headers', c.headers,
    'template_id', c.template_id, 'content_type', c.content_type, 'body', c.body, 'body_source', c.body_source,
    'altbody', c.altbody, 'archive', c.archive, 'archive_slug', c.archive_slug,
    'archive_template_id', c.archive_template_id, 'archive_meta', c.archive_meta,
    'lists', COALESCE((SELECT jsonb_agg(jsonb_build_object('id', l.list_id, 'name', l.list_name) ORDER BY l.list_id)
        FROM campaign_lists l WHERE l.campaign_id = c.id AND l.list_id IS NOT NULL), '[]'),
    'media', COALESCE((SELECT jsonb_agg(jsonb_build_object('id', m.media_id, 'filename', m.filename) ORDER BY m.media_id)
        FROM campaign_media m WHERE m.campaign_id = c.id AND m.media_id IS NOT NULL), '[]'))
    FROM campaigns c WHERE c.id = item)
WHEN 'templates' THEN (SELECT jsonb_build_object('name', t.name, 'type', t.type, 'subject', t.subject, 'body', t.body,
    'body_source', t.body_source) FROM templates t WHERE t.id = item)
WHEN 'denma_automations' THEN (SELECT jsonb_build_object('name', a.name, 'trigger_type', a.trigger_type,
    'trigger_tags', to_jsonb(a.trigger_tags), 'trigger_campaign_id', a.trigger_campaign_id, 'if_tags', to_jsonb(a.if_tags),
    'unless_tags', to_jsonb(a.unless_tags), 'add_tags', to_jsonb(a.add_tags), 'remove_tags', to_jsonb(a.remove_tags),
    'send_email', a.send_email, 'template_id', a.template_id, 'subject', a.subject, 'from_email', a.from_email,
    'delay_minutes', a.delay_minutes, 'each_time', a.each_time, 'quiet_days', a.quiet_days,
    'list_ids', to_jsonb(ARRAY(SELECT al.list_id FROM denma_automation_lists al WHERE al.automation_id = a.id AND al.kind = 'trigger' ORDER BY al.list_id)),
    'add_list_ids', to_jsonb(ARRAY(SELECT al.list_id FROM denma_automation_lists al WHERE al.automation_id = a.id AND al.kind = 'add' ORDER BY al.list_id)),
    'remove_list_ids', to_jsonb(ARRAY(SELECT al.list_id FROM denma_automation_lists al WHERE al.automation_id = a.id AND al.kind = 'remove' ORDER BY al.list_id)),
    'steps', COALESCE((SELECT jsonb_agg(jsonb_build_object('delay_minutes', s.delay_minutes, 'template_id', s.template_id,
        'subject', s.subject) ORDER BY s.step) FROM denma_automation_steps s WHERE s.automation_id = a.id), '[]'))
    FROM denma_automations a WHERE a.id = item)
END $$;

-- A change to the columns named, or a save (denma.saving), gives the row its next version, once a transaction.
CREATE OR REPLACE FUNCTION denma_bump_version() RETURNS TRIGGER LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE
    k TEXT := TG_TABLE_NAME || ':' || OLD.id;
    bumped TEXT := COALESCE(NULLIF(current_setting('denma.bumped', true), ''), ',');
    o JSONB;
    n JSONB;
BEGIN
    IF strpos(bumped, ',' || k || ',') > 0 THEN
        RETURN NEW;
    END IF;
    IF COALESCE(current_setting('denma.saving', true), '') <> k THEN
        o := to_jsonb(OLD);
        n := to_jsonb(NEW);
    END IF;
    IF COALESCE(current_setting('denma.saving', true), '') = k
        OR EXISTS (SELECT 1 FROM unnest(TG_ARGV) col WHERE o -> col IS DISTINCT FROM n -> col) THEN
        NEW.denma_version := OLD.denma_version + 1;
        PERFORM set_config('denma.bumped', bumped || k || ',', true);
    END IF;
    RETURN NEW;
END $$;

-- At commit, a new version is kept with who saved it (denma.saved_by...), unless nothing changed; at most 30.
CREATE OR REPLACE FUNCTION denma_record_version() RETURNS TRIGGER LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE
    col TEXT := TG_ARGV[0];
    d JSONB := denma_version_data(TG_TABLE_NAME::TEXT, NEW.id);
    cur INT;
    last_v INT;
    last_d JSONB;
BEGIN
    IF d IS NULL THEN
        RETURN NULL;
    END IF;
    EXECUTE format('SELECT denma_version FROM %I WHERE id = $1', TG_TABLE_NAME) INTO cur USING NEW.id;
    EXECUTE format('SELECT version, data FROM denma_versions WHERE %I = $1 AND NOT set_aside ORDER BY id DESC LIMIT 1', col)
        INTO last_v, last_d USING NEW.id;
    IF last_v = cur OR last_d = d THEN
        RETURN NULL;
    END IF;
    EXECUTE format('INSERT INTO denma_versions (%I, version, user_id, saved_by, via, changed, data) VALUES ($1, $2, $3, $4, $5, $6, $7)', col)
        USING NEW.id, cur, NULLIF(current_setting('denma.saved_by_id', true), '')::INT,
            COALESCE(current_setting('denma.saved_by', true), ''), COALESCE(current_setting('denma.via', true), ''),
            CASE WHEN last_d IS NULL THEN '{}'::TEXT[]
                ELSE ARRAY(SELECT key FROM jsonb_object_keys(d) key WHERE d -> key IS DISTINCT FROM last_d -> key ORDER BY key) END, d;
    EXECUTE format('DELETE FROM denma_versions WHERE %I = $1 AND id NOT IN (SELECT id FROM denma_versions WHERE %I = $1 ORDER BY id DESC LIMIT ` +
	denmaVersionsMaxSQL + `)', col, col) USING NEW.id;
    RETURN NULL;
END $$;

DROP TRIGGER IF EXISTS denma_bump_version ON campaigns;
CREATE TRIGGER denma_bump_version BEFORE UPDATE OF name, subject, from_email, body, altbody, content_type, send_at, headers,
    attribs, tags, messenger, template_id, archive, archive_slug, archive_template_id, archive_meta, body_source,
    denma_tags, denma_preview_text, denma_reply_to ON campaigns
    FOR EACH ROW EXECUTE FUNCTION denma_bump_version('name', 'subject', 'from_email', 'body', 'altbody', 'content_type',
    'send_at', 'headers', 'attribs', 'tags', 'messenger', 'template_id', 'archive', 'archive_slug', 'archive_template_id',
    'archive_meta', 'body_source', 'denma_tags', 'denma_preview_text', 'denma_reply_to');
DROP TRIGGER IF EXISTS denma_bump_version ON templates;
CREATE TRIGGER denma_bump_version BEFORE UPDATE OF name, subject, body, body_source ON templates
    FOR EACH ROW EXECUTE FUNCTION denma_bump_version('name', 'subject', 'body', 'body_source');
DROP TRIGGER IF EXISTS denma_bump_version ON denma_automations;
CREATE TRIGGER denma_bump_version BEFORE UPDATE OF name, template_id, subject, from_email, delay_minutes, trigger_type,
    trigger_tags, trigger_campaign_id, if_tags, unless_tags, add_tags, remove_tags, send_email, each_time, quiet_days
    ON denma_automations
    FOR EACH ROW EXECUTE FUNCTION denma_bump_version('name', 'template_id', 'subject', 'from_email', 'delay_minutes',
    'trigger_type', 'trigger_tags', 'trigger_campaign_id', 'if_tags', 'unless_tags', 'add_tags', 'remove_tags', 'send_email',
    'each_time', 'quiet_days');

DROP TRIGGER IF EXISTS denma_record_version ON campaigns;
CREATE CONSTRAINT TRIGGER denma_record_version AFTER UPDATE ON campaigns DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW WHEN (OLD.denma_version IS DISTINCT FROM NEW.denma_version) EXECUTE FUNCTION denma_record_version('campaign_id');
DROP TRIGGER IF EXISTS denma_record_version_new ON campaigns;
CREATE CONSTRAINT TRIGGER denma_record_version_new AFTER INSERT ON campaigns DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION denma_record_version('campaign_id');
DROP TRIGGER IF EXISTS denma_record_version ON templates;
CREATE CONSTRAINT TRIGGER denma_record_version AFTER UPDATE ON templates DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW WHEN (OLD.denma_version IS DISTINCT FROM NEW.denma_version) EXECUTE FUNCTION denma_record_version('template_id');
DROP TRIGGER IF EXISTS denma_record_version_new ON templates;
CREATE CONSTRAINT TRIGGER denma_record_version_new AFTER INSERT ON templates DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION denma_record_version('template_id');
DROP TRIGGER IF EXISTS denma_record_version ON denma_automations;
CREATE CONSTRAINT TRIGGER denma_record_version AFTER UPDATE ON denma_automations DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW WHEN (OLD.denma_version IS DISTINCT FROM NEW.denma_version) EXECUTE FUNCTION denma_record_version('automation_id');
DROP TRIGGER IF EXISTS denma_record_version_new ON denma_automations;
CREATE CONSTRAINT TRIGGER denma_record_version_new AFTER INSERT ON denma_automations DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION denma_record_version('automation_id');
`

// denmaVersionsStartSQL keeps what's there as each item's first version, once (features version 66).
const denmaVersionsStartSQL = `
INSERT INTO denma_versions (campaign_id, version, saved_at, data)
    SELECT id, denma_version, updated_at, denma_version_data('campaigns', id) FROM campaigns
    WHERE status NOT IN ('finished', 'cancelled') AND NOT EXISTS (SELECT 1 FROM denma_versions v WHERE v.campaign_id = campaigns.id);
INSERT INTO denma_versions (template_id, version, saved_at, data)
    SELECT id, denma_version, updated_at, denma_version_data('templates', id) FROM templates
    WHERE NOT EXISTS (SELECT 1 FROM denma_versions v WHERE v.template_id = templates.id);
INSERT INTO denma_versions (automation_id, version, saved_at, data)
    SELECT id, denma_version, updated_at, denma_version_data('denma_automations', id) FROM denma_automations
    WHERE NOT EXISTS (SELECT 1 FROM denma_versions v WHERE v.automation_id = denma_automations.id);
`

// denmaVersionKind is what a kind of item's versions are kept in.
type denmaVersionKind struct {
	table, col, noun string
}

var denmaVersionKinds = map[string]denmaVersionKind{
	"campaign":   {"campaigns", "campaign_id", "campaign"},
	"template":   {"templates", "template_id", "template"},
	"automation": {"denma_automations", "automation_id", "automation"},
}

// denmaSaver is who's saving: the user's ID and name, and the MCP key it came through, if any.
func denmaSaver(c echo.Context) (int, string, string) {
	u := auth.GetUser(c)
	via := ""
	if m, ok := denmaMCPOf(c); ok {
		via = "MCP key " + strconv.Quote(m.KeyName)
	}
	name := u.Name
	if name == "" {
		name = u.Username
	}
	return u.ID, name, via
}

// denmaSaveAs records who saves in tx, for the history.
func denmaSaveAs(c echo.Context, tx *sqlx.Tx) error {
	id, name, via := denmaSaver(c)
	_, err := tx.Exec(`SELECT set_config('denma.saved_by_id', $1, true), set_config('denma.saved_by', $2, true),
		set_config('denma.via', $3, true)`, strconv.Itoa(id), name, via)
	return err
}

// denmaBeginSave locks the item in tx and refuses the save unless it's at version; the save then makes the next.
func denmaBeginSave(c echo.Context, tx *sqlx.Tx, kind string, id, version int) error {
	k := denmaVersionKinds[kind]
	if version <= 0 {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Nothing was saved: the save doesn't say which version of the %s "+
			"it started from. Reload the page (copy your changes first), or read the %s again and give its \"version\".", k.noun, k.noun))
	}
	var cur int
	if err := tx.Get(&cur, `SELECT denma_version FROM `+k.table+` WHERE id = $1 FOR UPDATE`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("That %s doesn't exist.", k.noun))
		}
		return denmaInternal(err)
	}
	if cur != version {
		var last struct {
			SavedBy string    `db:"saved_by"`
			Via     string    `db:"via"`
			SavedAt time.Time `db:"saved_at"`
		}
		who := "Someone"
		if err := tx.Get(&last, `SELECT saved_by, via, saved_at FROM denma_versions WHERE `+k.col+` = $1 AND NOT set_aside
			ORDER BY id DESC LIMIT 1`, id); err == nil && last.SavedBy != "" {
			who = last.SavedBy
			if last.Via != "" {
				who += " (via " + last.Via + ")"
			}
		}
		return echo.NewHTTPError(http.StatusConflict, fmt.Sprintf("%s saved this %s after you opened it, so nothing was saved. "+
			"Read it again and make your change on what's there now.", who, k.noun))
	}
	if _, err := tx.Exec(`SELECT set_config('denma.saving', $1, true)`, k.table+":"+strconv.Itoa(id)); err != nil {
		return denmaInternal(err)
	}
	if err := denmaSaveAs(c, tx); err != nil {
		return denmaInternal(err)
	}
	return nil
}

// denmaUpdateCampaign saves a campaign over the version it started from (UpdateCampaign).
func (a *App) denmaUpdateCampaign(c echo.Context, id int, o *campReq) (models.Campaign, error) {
	tx, err := a.db.Beginx()
	if err != nil {
		return models.Campaign{}, denmaInternal(err)
	}
	defer tx.Rollback()
	if err := denmaBeginSave(c, tx, "campaign", id, o.DenmaVersion); err != nil {
		return models.Campaign{}, err
	}
	if err := a.core.DenmaUpdateCampaignTx(tx, id, o.Campaign, o.ListIDs, o.MediaIDs); err != nil {
		return models.Campaign{}, err
	}
	tags := o.SubscriberTags
	if tags == nil {
		tags = []string{}
	}
	if _, err := tx.Exec(`UPDATE campaigns SET denma_tags = $2, denma_preview_text = $3, denma_reply_to = $4 WHERE id = $1`,
		id, pq.Array(tags), o.PreviewText, o.ReplyTo); err != nil {
		return models.Campaign{}, denmaInternal(err)
	}
	if err := tx.Commit(); err != nil {
		return models.Campaign{}, denmaInternal(err)
	}
	return a.core.GetCampaign(id, "", "")
}

// denmaUpdateTemplate saves a template over the version it started from (UpdateTemplate).
func (a *App) denmaUpdateTemplate(c echo.Context, id int, o models.Template) (models.Template, error) {
	tx, err := a.db.Beginx()
	if err != nil {
		return models.Template{}, denmaInternal(err)
	}
	defer tx.Rollback()
	if err := denmaBeginSave(c, tx, "template", id, o.DenmaVersion); err != nil {
		return models.Template{}, err
	}
	if err := a.core.DenmaUpdateTemplateTx(tx, id, o.Name, o.Subject, []byte(o.Body), o.BodySource); err != nil {
		return models.Template{}, err
	}
	if err := tx.Commit(); err != nil {
		return models.Template{}, denmaInternal(err)
	}
	return a.core.GetTemplate(id, false)
}

// denmaCreatedBy names who made a new item in its first versions.
func (a *App) denmaCreatedBy(c echo.Context, kind string, id int) {
	uid, name, via := denmaSaver(c)
	if _, err := a.db.Exec(`UPDATE denma_versions SET user_id = $2, saved_by = $3, via = $4
		WHERE `+denmaVersionKinds[kind].col+` = $1 AND saved_by = ''`, id, uid, name, via); err != nil {
		a.log.Printf("denma: error naming who made %s %d: %v", kind, id, err)
	}
}

// denmaVersion is one version, as the history lists it.
type denmaVersion struct {
	ID       int64           `db:"id" json:"id"`
	Version  int             `db:"version" json:"version"`
	SavedAt  time.Time       `db:"saved_at" json:"saved_at"`
	SavedBy  string          `db:"saved_by" json:"saved_by"`
	Via      string          `db:"via" json:"via"`
	SetAside bool            `db:"set_aside" json:"set_aside"`
	Changed  pq.StringArray  `db:"changed" json:"changed"`
	Data     json.RawMessage `db:"data" json:"data,omitempty"`
}

// denmaVersionItem is the kind and ID a request is about, once the user may see (or change) it.
func (a *App) denmaVersionItem(c echo.Context, manage bool) (denmaVersionKind, int, error) {
	k, ok := denmaVersionKinds[c.Param("kind")]
	id, _ := strconv.Atoi(c.Param("id"))
	if !ok || id <= 0 {
		return k, 0, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	u := auth.GetUser(c)
	switch c.Param("kind") {
	case "campaign":
		t, perms := auth.PermTypeGet, []string{"campaigns:get", "campaigns:get_all"}
		if manage {
			t, perms = auth.PermTypeManage, []string{"campaigns:manage", "campaigns:manage_all"}
		}
		if !u.HasPerm(perms[0]) && !u.HasPerm(perms[1]) {
			return k, 0, echo.NewHTTPError(http.StatusForbidden, a.i18n.Ts("globals.messages.permissionDenied", "name", perms[0]))
		}
		if err := a.checkCampaignPerm(t, id, c); err != nil {
			return k, 0, err
		}
	case "template", "automation":
		perm := c.Param("kind") + "s:get"
		if manage {
			perm = c.Param("kind") + "s:manage"
		}
		if !u.HasPerm(perm) {
			return k, 0, echo.NewHTTPError(http.StatusForbidden, a.i18n.Ts("globals.messages.permissionDenied", "name", perm))
		}
	}
	if c.Param("kind") == "automation" && !denmaAutomationsOn(a) {
		return k, 0, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	return k, id, nil
}

// DenmaGetVersions returns an item's versions, newest first, without their contents.
func (a *App) DenmaGetVersions(c echo.Context) error {
	k, id, err := a.denmaVersionItem(c, false)
	if err != nil {
		return err
	}
	out := []denmaVersion{}
	if err := a.db.Select(&out, `SELECT id, version, saved_at, saved_by, via, set_aside, changed FROM denma_versions
		WHERE `+k.col+` = $1 ORDER BY id DESC`, id); err != nil {
		return denmaInternal(err)
	}
	return c.JSON(http.StatusOK, okResp{out})
}

// DenmaGetVersion returns one version with its contents; "now" is the item as it is, with its version.
func (a *App) DenmaGetVersion(c echo.Context) error {
	k, id, err := a.denmaVersionItem(c, false)
	if err != nil {
		return err
	}
	var out denmaVersion
	if c.Param("vid") == "now" {
		err = a.db.Get(&out, `SELECT 0 AS id, t.denma_version AS version, COALESCE(v.saved_at, NOW()) AS saved_at,
			COALESCE(v.saved_by, '') AS saved_by, COALESCE(v.via, '') AS via, false AS set_aside, '{}'::TEXT[] AS changed,
			denma_version_data($2, t.id) AS data
			FROM `+k.table+` t LEFT JOIN LATERAL (SELECT * FROM denma_versions WHERE `+k.col+` = t.id AND NOT set_aside
				AND version = t.denma_version ORDER BY id DESC LIMIT 1) v ON true
			WHERE t.id = $1`, id, k.table)
	} else {
		vid, _ := strconv.ParseInt(c.Param("vid"), 10, 64)
		err = a.db.Get(&out, `SELECT id, version, saved_at, saved_by, via, set_aside, changed, data FROM denma_versions
			WHERE id = $1 AND `+k.col+` = $2`, vid, id)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusNotFound, "That version doesn't exist.")
	}
	if err != nil {
		return denmaInternal(err)
	}
	return c.JSON(http.StatusOK, okResp{out})
}

// DenmaSetAsideVersion keeps the person's own edit, which they chose not to save over someone else's, in the history.
func (a *App) DenmaSetAsideVersion(c echo.Context) error {
	k, id, err := a.denmaVersionItem(c, true)
	if err != nil {
		return err
	}
	var req struct {
		Version int             `json:"version"`
		Data    json.RawMessage `json:"data"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if len(req.Data) > denmaVersionAsideMax {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "The version to keep is too large.")
	}
	var obj map[string]any
	if json.Unmarshal(req.Data, &obj) != nil || len(obj) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "The version to keep is missing.")
	}
	uid, name, via := denmaSaver(c)
	var vid int64
	if err := a.db.Get(&vid, `INSERT INTO denma_versions (`+k.col+`, version, user_id, saved_by, via, set_aside, data)
		SELECT $1::INT, $2::INT, $3::INT, $4::TEXT, $5::TEXT, true, $6::JSONB FROM `+k.table+` WHERE id = $1 RETURNING id`,
		id, req.Version, uid, name, via, string(req.Data)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("That %s doesn't exist.", k.noun))
		}
		return denmaInternal(err)
	}
	if _, err := a.db.Exec(`DELETE FROM denma_versions WHERE `+k.col+` = $1 AND id NOT IN (SELECT id FROM denma_versions
		WHERE `+k.col+` = $1 ORDER BY id DESC LIMIT $2)`, id, denmaVersionsMax); err != nil {
		a.log.Printf("denma: error keeping %s %d's newest versions: %v", k.noun, id, err)
	}
	return c.JSON(http.StatusOK, okResp{map[string]int64{"id": vid}})
}

// denmaEditors is who has each item open to edit, by center, kind and item: each tab, and when it last said so.
var denmaEditors = struct {
	sync.Mutex
	m map[string]map[string]denmaEditor
}{m: map[string]map[string]denmaEditor{}}

type denmaEditor struct {
	uid  int
	name string
	seen time.Time
}

const denmaEditingTTL = 75 * time.Second // an editor says it's open every 30 seconds

var reDenmaTab = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)

// denmaEditorsOf records (or ends) a tab's editing of key, and returns who else has it open: their names, and
// whether one of them is the same person in another window.
func denmaEditorsOf(key, tab string, uid int, name string, leaving bool) ([]string, bool) {
	denmaEditors.Lock()
	defer denmaEditors.Unlock()
	now := time.Now()
	for k, tabs := range denmaEditors.m {
		for t, e := range tabs {
			if now.Sub(e.seen) > denmaEditingTTL {
				delete(tabs, t)
			}
		}
		if len(tabs) == 0 {
			delete(denmaEditors.m, k)
		}
	}
	tabs := denmaEditors.m[key]
	if leaving {
		delete(tabs, tab)
	} else {
		if tabs == nil {
			tabs = map[string]denmaEditor{}
			denmaEditors.m[key] = tabs
		}
		tabs[tab] = denmaEditor{uid: uid, name: name, seen: now}
	}
	var names []string
	seen := map[int]bool{}
	self := false
	for t, e := range tabs {
		if t == tab {
			continue
		}
		if e.uid == uid {
			self = true
			continue
		}
		if !seen[e.uid] {
			seen[e.uid] = true
			names = append(names, e.name)
		}
	}
	sort.Strings(names)
	return names, self
}

// DenmaEditing records that a tab has an item open to edit, and answers who else has, and the item's version.
func (a *App) DenmaEditing(c echo.Context) error {
	k, id, err := a.denmaVersionItem(c, false)
	if err != nil {
		return err
	}
	var req struct {
		Tab     string `json:"tab"`
		Leaving bool   `json:"leaving"`
	}
	if err := c.Bind(&req); err != nil || !reDenmaTab.MatchString(req.Tab) {
		return echo.NewHTTPError(http.StatusBadRequest, "A tab's ID is needed.")
	}
	uid, name, _ := denmaSaver(c)
	others, self := denmaEditorsOf(a.ko.String("denma.center")+"/"+c.Param("kind")+"/"+strconv.Itoa(id), req.Tab, uid, name, req.Leaving)
	out := struct {
		Others  []string   `json:"others"`
		Self    bool       `json:"self"`
		Version int        `db:"version" json:"version"`
		SavedBy string     `db:"saved_by" json:"saved_by"`
		Via     string     `db:"via" json:"via"`
		SavedAt *time.Time `db:"saved_at" json:"saved_at"`
	}{Others: others, Self: self}
	if out.Others == nil {
		out.Others = []string{}
	}
	if err := a.db.Get(&out, `SELECT t.denma_version AS version, COALESCE(v.saved_by, '') AS saved_by, COALESCE(v.via, '') AS via,
		v.saved_at FROM `+k.table+` t LEFT JOIN LATERAL (SELECT saved_by, via, saved_at FROM denma_versions
			WHERE `+k.col+` = t.id AND NOT set_aside AND version = t.denma_version ORDER BY id DESC LIMIT 1) v ON true
		WHERE t.id = $1`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("That %s doesn't exist.", k.noun))
		}
		return denmaInternal(err)
	}
	return c.JSON(http.StatusOK, okResp{out})
}

// initDenmaVersionHandlers registers the history's API, and who's editing.
func initDenmaVersionHandlers(g *echo.Group, a *App) {
	g.POST("/api/denma/editing/:kind/:id", a.DenmaEditing)
	g.GET("/api/denma/versions/:kind/:id", a.DenmaGetVersions)
	g.GET("/api/denma/versions/:kind/:id/:vid", a.DenmaGetVersion)
	g.POST("/api/denma/versions/:kind/:id/aside", a.DenmaSetAsideVersion)
}

// denmaPruneVersions deletes versions of campaigns sent over 60 days ago, and of the rest over a year old.
func denmaPruneVersions(ctx context.Context, db denmaExecer) (int64, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM denma_versions v USING campaigns c WHERE v.campaign_id = c.id
		AND c.status IN ('finished', 'cancelled') AND c.updated_at < NOW() - make_interval(days => $1)`, denmaVersionsSentDays)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	res, err = db.ExecContext(ctx, `DELETE FROM denma_versions WHERE campaign_id IS NULL
		AND saved_at < NOW() - make_interval(days => $1)`, denmaVersionsDays)
	if err != nil {
		return n, err
	}
	m, _ := res.RowsAffected()
	return n + m, nil
}
