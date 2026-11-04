package main

// denma: features installed in each center's schema as database triggers, set on its Config page.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

// denmaFeaturesVersion is the version of denmaFeaturesSQL.
const denmaFeaturesVersion = 66

// denmaFeatureDefaults are the settings' values in a center that doesn't have them yet.
var denmaFeatureDefaults = map[string]any{
	"denma.unsubscribe_everywhere": true,
	"denma.plain_text_auto":        true,
	"denma.utm_domains":            []string{},
	"denma.visual_template":        0,
	"denma.daily_cap":              0,  // cmd/denma_daily.go
	"denma.reply_to":               "", // cmd/denma_reply_to.go
	"denma.timezone":               "", // cmd/denma_sendwindow.go
	"denma.send_window":            denmaSendWindowDefault,
	"denma.postal_address":         "", // cmd/denma_address.go
	"denma.website":                "", // cmd/denma_footer.go
	"denma.contact_email":          "",
	"denma.attachment_max_mb":      denmaAttachDefaultMB, // cmd/denma_attachlimit.go
}

// features installs the features' triggers in a center, with its settings.
func (d *denmaCenters) features(c *denmaCenter, db *sqlx.DB) error {
	var ver int
	if err := db.Get(&ver, `SELECT COALESCE((SELECT value::INT FROM settings WHERE key = 'denma.features_version'), 0)`); err != nil {
		return err
	}
	if ver == denmaFeaturesVersion {
		return nil
	}
	// An older build leaves newer SQL as it is.
	if ver > denmaFeaturesVersion {
		lo.Printf("denma: center %s: its features are at version %d, newer than this build's %d; left as they are", c.Slug, ver, denmaFeaturesVersion)
		return nil
	}
	// Designs' template type (cmd/denma_design.go), outside the transaction.
	if ver < 11 {
		if _, err := db.Exec(`ALTER TYPE template_type ADD VALUE IF NOT EXISTS 'design'`); err != nil {
			return fmt.Errorf("adding the design template type: %w", err)
		}
	}

	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// The first time, the old add-on's settings, if it was installed.
	old := map[string]any{}
	var dropped []string
	if ver == 0 {
		if old, dropped, err = denmaOldAddon(tx); err != nil {
			return fmt.Errorf("reading the old add-on's triggers: %w", err)
		}
	}
	// Its values replace the defaults; settings the center has are kept.
	for k, v := range denmaFeatureDefaults {
		b, _ := json.Marshal(v)
		if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES ($1, $2::JSONB) ON CONFLICT (key) DO NOTHING`, k, string(b)); err != nil {
			return err
		}
	}
	for k, v := range old {
		b, _ := json.Marshal(v)
		if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES ($1, $2::JSONB)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, k, string(b)); err != nil {
			return err
		}
	}
	// Unsubscribe everywhere is turned on for every center, once.
	if ver < 7 {
		if _, err := tx.Exec(`UPDATE settings SET value = 'true'::JSONB WHERE key = 'denma.unsubscribe_everywhere'`); err != nil {
			return err
		}
	}
	// Automations' tables first: their versions are in denmaFeaturesSQL (cmd/denma_versions.go).
	if _, err := tx.Exec(denmaAutoTables); err != nil {
		return fmt.Errorf("making the automations' tables: %w", err)
	}
	if _, err := tx.Exec(denmaFeaturesSQL); err != nil {
		return fmt.Errorf("installing the triggers: %w", err)
	}
	// Every center gets its own design (cmd/denma_design.go).
	if ver < 9 {
		if err := denmaAddDefaultDesign(tx); err != nil {
			return fmt.Errorf("adding the center's design: %w", err)
		}
	}
	// Every center without one gets a default visual template.
	if ver < 10 {
		if err := denmaAddVisualDefault(tx); err != nil {
			return fmt.Errorf("adding the default visual template: %w", err)
		}
	}
	// The center's designs, made as visual templates, become designs.
	if ver < 11 {
		if _, err := tx.Exec(`UPDATE templates SET type = 'design' WHERE type = 'campaign_visual' AND id IN (
			SELECT (value #>> '{}')::INT FROM settings WHERE key IN ('denma.default_design', 'denma.design_template'))`); err != nil {
			return fmt.Errorf("making the designs designs: %w", err)
		}
	}
	// The Website signups setting becomes an automation.
	if ver < 16 {
		moved, err := denmaMigrateSignupSetting(tx)
		if err != nil {
			return fmt.Errorf("making the website signups' automation: %w", err)
		}
		if moved {
			lo.Printf("denma: center %s: its Website signups setting is now an automation", c.Slug)
		}
	}
	// Those the API added before have no consent (cmd/denma_noconsent.go).
	if ver < 22 {
		res, err := tx.Exec(denmaNoConsentMigrateSQL)
		if err != nil {
			return fmt.Errorf("recording those the API added: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			lo.Printf("denma: center %s: %d subscribers the API added have no consent", c.Slug, n)
		}
		// Keys that only subscribed people are cleared: sign-up webhooks do it (cmd/denma_signup.go).
		var idle []string
		if err := tx.Select(&idle, `SELECT name FROM denma_api_keys WHERE can = '{}' AND deleted_at IS NULL ORDER BY id`); err != nil {
			return fmt.Errorf("reading the API keys: %w", err)
		}
		for _, k := range idle {
			lo.Printf("denma: center %s: API key %q only subscribed people, which keys don't any more; a superadmin can make it a Server webhook", c.Slug, k)
		}
	}
	// No API users (cmd/denma_noapiusers.go).
	if ver < 23 {
		if err := denmaDeleteAPIUsers(tx, "center "+c.Slug); err != nil {
			return fmt.Errorf("deleting the API users: %w", err)
		}
	}
	// First and last names, from the names subscribers had (cmd/denma_names.go).
	if ver < 35 {
		res, err := tx.Exec(denmaSplitNamesSQL)
		if err != nil {
			return fmt.Errorf("splitting the subscribers' names: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			lo.Printf("denma: center %s: %d subscribers' names split into first and last names", c.Slug, n)
		}
	}
	// The stock templates' footers and the default design show the postal address (cmd/denma_address.go).
	if ver < 39 {
		n, err := denmaAddAddressToTemplates(tx)
		if err != nil {
			return fmt.Errorf("adding the postal address to the templates: %w", err)
		}
		if n > 0 {
			lo.Printf("denma: center %s: %d of its templates show its postal address", c.Slug, n)
		}
	}
	// Links to its files at the hub's uploads address point to its own (cmd/denma_images.go).
	if hub := d.current(); ver < 42 && hub != nil && hub.ko.String("upload.provider") == "filesystem" {
		root := strings.TrimSuffix(hub.urlCfg.RootURL, "/")
		uri := "/" + strings.Trim(hub.ko.String("upload.filesystem.upload_uri"), "/")
		var hubNames []string // the hub's own, served at its uploads address
		if err := hub.db.Select(&hubNames, `SELECT filename FROM media UNION SELECT thumb FROM media WHERE thumb <> ''`); err != nil {
			return fmt.Errorf("reading the hub's media: %w", err)
		}
		n, err := denmaRewriteHubUploads(tx, root, uri, root+denmaCenterPath+c.Slug+uri+"/", hubNames)
		if err != nil {
			return fmt.Errorf("pointing old upload links to the center's own: %w", err)
		}
		if n > 0 {
			lo.Printf("denma: center %s: %d templates and campaigns link its own uploads instead of the hub's address", c.Slug, n)
		}
	}
	// No opt-in campaigns (cmd/denma_optincampaigns.go).
	if ver < 43 {
		res, err := tx.Exec(denmaCancelOptinCampaignsSQL)
		if err != nil {
			return fmt.Errorf("cancelling opt-in campaigns: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			lo.Printf("denma: center %s: %d opt-in campaigns cancelled", c.Slug, n)
		}
	}
	// pgcrypto's functions in a center not made by the hub.
	if ver < 45 {
		if err := denmaCryptoFuncs(tx); err != nil {
			return err
		}
	}
	// Which Server webhook created each tag, from the activity log (cmd/denma_signup.go).
	if ver < 46 {
		if _, err := tx.Exec(denmaTagHooksSQL, c.Slug); err != nil {
			return fmt.Errorf("finding the tags webhooks created: %w", err)
		}
	}
	// The stock footers and default design show the website and contact e-mail (cmd/denma_footer.go).
	if ver < 48 {
		n, err := denmaAddContactToTemplates(tx)
		if err != nil {
			return fmt.Errorf("adding the website and contact e-mail to the templates: %w", err)
		}
		if n > 0 {
			lo.Printf("denma: center %s: %d of its templates show its website and contact e-mail", c.Slug, n)
		}
	}
	// A center has one design, its own; the setting that chose another goes (cmd/denma_design.go).
	if ver < 49 {
		if _, err := tx.Exec(`DELETE FROM settings WHERE key = 'denma.design_template'`); err != nil {
			return fmt.Errorf("removing the design setting: %w", err)
		}
	}
	// What's there is each item's first version (cmd/denma_versions.go).
	if ver < 66 {
		if _, err := tx.Exec(denmaVersionsStartSQL); err != nil {
			return fmt.Errorf("keeping the first versions: %w", err)
		}
	}
	// An import's joins and tags start automations only if asked (cmd/denma_imports.go).
	if ver < 51 {
		if _, err := tx.Exec(denmaImportEventsSQL); err != nil {
			return fmt.Errorf("leaving out imports' automation events: %w", err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES ('denma.features_version', $1::TEXT::JSONB)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, strconv.Itoa(denmaFeaturesVersion)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if len(dropped) > 0 {
		var got []string
		for k, v := range old {
			got = append(got, fmt.Sprintf("%s=%v", strings.TrimPrefix(k, "denma."), v))
		}
		sort.Strings(got)
		lo.Printf("denma: center %s: the old add-on's triggers (%s) are replaced by these settings: %s",
			c.Slug, strings.Join(dropped, ", "), strings.Join(got, " "))
	}
	return nil
}

var (
	reOldHolding  = regexp.MustCompile(`NEW\.list_id <> (\d+)`)
	reOldTarget   = regexp.MustCompile(`VALUES \(NEW\.subscriber_id, (\d+), 'confirmed'`)
	reOldTemplate = regexp.MustCompile(`t\.id = (\d+) AND t\.type = 'campaign_visual'`)
	reOldUTMHosts = regexp.MustCompile(`(?s)NOT \((.*?)\) THEN`)
	reOldQuoted   = regexp.MustCompile(`'([^']+)'`)
)

// denmaOldAddon reads the settings the old add-on's triggers had written in, and drops them.
func denmaOldAddon(tx *sqlx.Tx) (map[string]any, []string, error) {
	var trigs []struct {
		Name  string `db:"tgname"`
		Table string `db:"relname"`
		Src   string `db:"prosrc"`
	}
	if err := tx.Select(&trigs, `SELECT t.tgname, c.relname, p.prosrc FROM pg_trigger t
		JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_proc p ON p.oid = t.tgfoid
		WHERE c.relnamespace = current_schema()::REGNAMESPACE AND NOT t.tgisinternal AND t.tgname LIKE 'ddl\_%'`); err != nil {
		return nil, nil, err
	}
	set := map[string]any{}
	var dropped []string
	num := func(re *regexp.Regexp, s string) int {
		if m := re.FindStringSubmatch(s); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
		return 0
	}
	for _, t := range trigs {
		switch t.Name {
		case "ddl_unsubscribe_everywhere":
			set["denma.unsubscribe_everywhere"] = true
		case "ddl_campaign_altbody":
			set["denma.plain_text_auto"] = true
		case "ddl_confirm_website_signup":
			set["denma.signup_holding_list"] = num(reOldHolding, t.Src)
			set["denma.signup_target_list"] = num(reOldTarget, t.Src)
		case "ddl_campaign_add_template":
			set["denma.visual_template"] = num(reOldTemplate, t.Src)
		case "ddl_campaign_utm":
			// The domains are in the URL function's IF ... NOT (...) THEN.
			var src string
			if err := tx.Get(&src, `SELECT prosrc FROM pg_proc WHERE proname = 'ddl_utm_url' ORDER BY oid DESC LIMIT 1`); err != nil && err != sql.ErrNoRows {
				return nil, nil, err
			}
			domains := []string{}
			if m := reOldUTMHosts.FindStringSubmatch(src); m != nil {
				for _, q := range reOldQuoted.FindAllStringSubmatch(m[1], -1) {
					if d := strings.TrimPrefix(q[1], "%."); d != "" && !denmaHasString(domains, d) {
						domains = append(domains, d)
					}
				}
			}
			set["denma.utm_domains"] = domains
		}
		if _, err := tx.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s`, pq.QuoteIdentifier(t.Name), pq.QuoteIdentifier(t.Table))); err != nil {
			return nil, nil, err
		}
		dropped = append(dropped, t.Name)
	}
	return set, dropped, nil
}

func denmaHasString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// denmaFeaturesSQL installs the features' functions and triggers; their names sort in run order.
const denmaFeaturesSQL = `
CREATE OR REPLACE FUNCTION denma_setting(k TEXT) RETURNS JSONB
LANGUAGE sql STABLE SET search_path FROM CURRENT AS $$ SELECT value FROM settings WHERE key = k $$;

-- Unsubscribe everywhere.
CREATE OR REPLACE FUNCTION denma_unsubscribe_everywhere() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    IF pg_trigger_depth() > 1 OR denma_setting('denma.unsubscribe_everywhere') IS DISTINCT FROM 'true'::JSONB THEN
        RETURN NULL;  -- the cascade below fires this again; stop there
    END IF;
    UPDATE subscribers SET status = 'blocklisted', updated_at = NOW()
        WHERE id = NEW.subscriber_id AND status <> 'blocklisted';
    UPDATE subscriber_lists SET status = 'unsubscribed', updated_at = NOW()
        WHERE subscriber_id = NEW.subscriber_id AND status <> 'unsubscribed';
    RETURN NULL;
END;
$$;
DROP TRIGGER IF EXISTS denma_unsubscribe_everywhere ON subscriber_lists;
CREATE TRIGGER denma_unsubscribe_everywhere
    AFTER UPDATE OF status ON subscriber_lists FOR EACH ROW
    WHEN (NEW.status = 'unsubscribed' AND OLD.status IS DISTINCT FROM 'unsubscribed')
    EXECUTE FUNCTION denma_unsubscribe_everywhere();

-- Automatic plain text.
CREATE OR REPLACE FUNCTION denma_html_entities(t TEXT) RETURNS TEXT
LANGUAGE plpgsql IMMUTABLE SET search_path FROM CURRENT AS $$
DECLARE
    m TEXT[];
BEGIN
    t := replace(replace(replace(t, '&nbsp;', ' '), '&#160;', ' '), '&copy;', '©');
    t := replace(replace(replace(t, '&lt;', '<'), '&gt;', '>'), '&quot;', '"');
    t := replace(replace(replace(t, '&#39;', ''''), '&apos;', ''''), '&rsquo;', '’');
    t := replace(replace(replace(t, '&lsquo;', '‘'), '&ldquo;', '“'), '&rdquo;', '”');
    t := replace(replace(replace(t, '&ndash;', '–'), '&mdash;', '—'), '&hellip;', '…');
    t := replace(replace(replace(t, '&shy;', ''), '&bull;', '•'), '&middot;', '·');
    t := replace(replace(replace(t, '&reg;', '®'), '&trade;', '™'), '&euro;', '€');
    LOOP  -- numeric entities: &#8217; and &#x2019;
        m := regexp_match(t, '&#([xX]?)([0-9A-Fa-f]{1,6});');
        EXIT WHEN m IS NULL;
        t := replace(t, '&#' || m[1] || m[2] || ';',
            chr(CASE WHEN m[1] = '' THEN m[2]::INT ELSE ('x' || lpad(m[2], 8, '0'))::BIT(32)::INT END));
    END LOOP;
    RETURN replace(t, '&amp;', '&');  -- last, keeping "&amp;lt;" as "&lt;"
END;
$$;

-- The text of an HTML fragment on one line: tags removed, images as their alt text.
CREATE OR REPLACE FUNCTION denma_inline_text(html TEXT) RETURNS TEXT
LANGUAGE sql IMMUTABLE SET search_path FROM CURRENT AS $$
    SELECT trim(regexp_replace(denma_html_entities(regexp_replace(
        regexp_replace(html, '<img\s[^>]*alt\s*=\s*"([^"]*)"[^>]*>', ' \1 ', 'gi'),
        '<[^>]*>', ' ', 'g')), '\s+', ' ', 'g'));
$$;

CREATE OR REPLACE FUNCTION denma_html_to_text(html TEXT) RETURNS TEXT
LANGUAGE plpgsql IMMUTABLE SET search_path FROM CURRENT AS $$
DECLARE
    t TEXT := coalesce(html, '');
    out TEXT := '';
    m TEXT[];
    pos INT;
    label TEXT;
    url TEXT;
    link TEXT;
    acts TEXT[] := '{}';
BEGIN
    t := regexp_replace(t, '\{\{\s*TrackView\s*\}\}', '', 'g');
    -- Template actions are set aside and put back whole.
    LOOP
        m := regexp_match(t, '\{\{.*?\}\}');
        EXIT WHEN m IS NULL;
        acts := acts || m[1];
        t := overlay(t PLACING chr(1) || cardinality(acts) || chr(2) FROM strpos(t, m[1]) FOR length(m[1]));
    END LOOP;
    FOREACH label IN ARRAY ARRAY['head', 'style', 'script', 'title'] LOOP
        t := regexp_replace(t, format('<%s\M[^>]*>(?:[^<]|<(?!/%s))*</%s>', label, label, label), '', 'gi');
    END LOOP;
    t := regexp_replace(t, '<!--(?:[^-]|-(?!->))*-->', '', 'g');
    t := regexp_replace(t, '\s+', ' ', 'g');  -- HTML whitespace; line breaks come from tags below

    -- Links: "text (URL)".
    LOOP
        m := regexp_match(t, '<a\s[^>]*href\s*=\s*"([^"]*)"[^>]*>((?:[^<]|<(?!/a>))*)</a>', 'i');
        EXIT WHEN m IS NULL;
        pos := strpos(t, substring(t FROM '<a\s[^>]*href\s*=\s*"[^"]*"[^>]*>(?:[^<]|<(?!/a>))*</a>'));
        url := denma_html_entities(m[1]);
        label := denma_inline_text(m[2]);
        link := CASE
            WHEN url ~* '^mailto:' THEN coalesce(nullif(label, ''), substr(url, 8))
            WHEN label = '' OR label = url OR 'https://' || label = url OR 'http://' || label = url
                 OR 'https://' || label || '/' = url THEN url
            ELSE label || ' (' || url || ')'
        END;
        out := out || left(t, pos - 1) || link;
        t := substr(t, pos + length(substring(t FROM '<a\s[^>]*href\s*=\s*"[^"]*"[^>]*>(?:[^<]|<(?!/a>))*</a>')));
    END LOOP;
    t := out || t;

    t := regexp_replace(t, '<img\s[^>]*>', '', 'gi');
    t := regexp_replace(t, '<li\M[^>]*>', E'\n- ', 'gi');
    t := regexp_replace(t, '</li>', '', 'gi');
    t := regexp_replace(t, '<br\s*/?>', E'\n', 'gi');
    t := regexp_replace(t, '</?(p|div|h[1-6]|tr|td|th|table|tbody|ul|ol|blockquote|hr|section|header|footer)\M[^>]*>', E'\n', 'gi');
    t := regexp_replace(t, '<[^>]*>', '', 'g');
    t := denma_html_entities(t);
    t := regexp_replace(t, '[ \t]+', ' ', 'g');
    t := regexp_replace(t, ' *\n *', E'\n', 'g');
    t := regexp_replace(t, E'\n{3,}', E'\n\n', 'g');
    FOR i IN REVERSE cardinality(acts)..1 LOOP
        t := replace(t, chr(1) || i || chr(2), acts[i]);
    END LOOP;
    RETURN trim(BOTH E' \n' FROM t);
END;
$$;

-- The email a campaign sends, as plain text: richtext, HTML and markdown in their template.
CREATE OR REPLACE FUNCTION denma_campaign_text(content_type TEXT, body TEXT, template_id INT) RETURNS TEXT
LANGUAGE plpgsql STABLE SET search_path FROM CURRENT AS $$
DECLARE
    html TEXT := body;
    tpl TEXT;
BEGIN
    IF content_type IN ('richtext', 'html', 'markdown') THEN
        SELECT t.body INTO tpl FROM templates t WHERE t.id = template_id;
        IF tpl IS NOT NULL AND tpl ~ '\{\{\s*template\s+"content"\s+\.\s*\}\}' THEN
            html := regexp_replace(tpl, '\{\{\s*template\s+"content"\s+\.\s*\}\}',
                replace(CASE WHEN content_type = 'markdown' THEN replace(body, E'\n', '<br>') ELSE body END, '\', '\\'));
        END IF;
    END IF;
    RETURN denma_html_to_text(html);
END;
$$;

CREATE OR REPLACE FUNCTION denma_campaign_altbody() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    IF NEW.content_type = 'plain' OR denma_setting('denma.plain_text_auto') IS DISTINCT FROM 'true'::JSONB THEN
        RETURN NEW;
    END IF;
    IF NEW.attribs -> 'plain_text_manual' = 'true'::JSONB AND coalesce(NEW.altbody, '') <> '' THEN
        RETURN NEW;  -- written by hand
    END IF;
    NEW.altbody := denma_campaign_text(NEW.content_type::TEXT, NEW.body, NEW.template_id);
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS denma_campaign_altbody ON campaigns;
CREATE TRIGGER denma_campaign_altbody
    BEFORE INSERT OR UPDATE OF body, altbody, template_id, content_type, status, attribs ON campaigns
    FOR EACH ROW EXECUTE FUNCTION denma_campaign_altbody();

-- UTM tags.
CREATE OR REPLACE FUNCTION denma_utm_url(url TEXT, campaign TEXT, domains TEXT[], own TEXT) RETURNS TEXT
LANGUAGE plpgsql IMMUTABLE SET search_path FROM CURRENT AS $$
DECLARE
    suffix TEXT := '';
    frag TEXT := '';
    host TEXT;
BEGIN
    IF url LIKE '%{{%' THEN
        RETURN url;  -- a template expression, not a literal URL
    END IF;
    IF url ~ '@TrackLink$' THEN  -- listmonk's click-tracking marker
        suffix := '@TrackLink';
        url := left(url, length(url) - length(suffix));
    END IF;

    host := lower(substring(url FROM '^[Hh][Tt][Tt][Pp][Ss]?://([^/?#:@]+)'));
    IF host IS NULL OR host = own
       OR NOT EXISTS (SELECT 1 FROM unnest(domains) d WHERE host = d OR host LIKE '%.' || d) THEN
        RETURN url || suffix;
    END IF;

    IF position('#' IN url) > 0 THEN
        frag := substring(url FROM position('#' IN url));
        url := left(url, position('#' IN url) - 1);
    END IF;

    -- Drop tags this function added before (always the last parameters).
    url := regexp_replace(url, '[?&](amp;)?utm_source=newsletter&(amp;)?utm_medium=email&(amp;)?utm_campaign=[^&#]*$', '');

    IF url ~* '[?&](amp;)?utm_' THEN  -- the author set their own UTM tags
        RETURN url || frag || suffix;
    END IF;

    RETURN url || CASE WHEN position('?' IN url) > 0 THEN '&' ELSE '?' END
        || 'utm_source=newsletter&utm_medium=email&utm_campaign=' || campaign
        || frag || suffix;
END;
$$;

-- Rewrites every link in body matching pattern (group 1: text before the URL; group 2: the URL).
CREATE OR REPLACE FUNCTION denma_utm_rewrite(body TEXT, pattern TEXT, campaign TEXT, domains TEXT[], own TEXT) RETURNS TEXT
LANGUAGE plpgsql IMMUTABLE SET search_path FROM CURRENT AS $$
DECLARE
    out TEXT := '';
    rest TEXT := coalesce(body, '');
    m TEXT[];
    pos INT;
BEGIN
    LOOP
        m := regexp_match(rest, pattern);
        EXIT WHEN m IS NULL;
        pos := strpos(rest, m[1] || m[2]);
        out := out || left(rest, pos - 1) || m[1] || denma_utm_url(m[2], campaign, domains, own);
        rest := substr(rest, pos + length(m[1]) + length(m[2]));
    END LOOP;
    RETURN out || rest;
END;
$$;

CREATE OR REPLACE FUNCTION denma_campaign_utm() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE
    domains TEXT[];
    own TEXT;
    slug TEXT;
    html_links CONSTANT TEXT := '(href\s*=\s*["''])([^"''\s<>]+)';
    md_links CONSTANT TEXT := '((?:^|[^!])\[[^\]]*\]\()([^()\s<>]+)';  -- [text](url), not ![image](url)
    bare_urls CONSTANT TEXT := '((?:^|[\s(<>"'']))(https?://[^\s<>"''()]*[^\s<>"''().,;:!?])';
    md_bare_urls CONSTANT TEXT := '((?:^|[\s<>"'']))(https?://[^\s<>"''()]*[^\s<>"''().,;:!?])';
BEGIN
    SELECT array_agg(lower(trim(d))) INTO domains
        FROM jsonb_array_elements_text(coalesce(denma_setting('denma.utm_domains'), '[]'::JSONB)) d WHERE trim(d) <> '';
    IF domains IS NULL THEN
        RETURN NEW;
    END IF;
    own := lower(substring(denma_setting('app.root_url') #>> '{}' FROM '^[Hh][Tt][Tt][Pp][Ss]?://([^/?#:@]+)'));

    slug := left(trim(BOTH '-' FROM regexp_replace(lower(coalesce(NEW.name, '')), '[^a-z0-9]+', '-', 'g')), 60);
    IF slug = '' THEN
        slug := 'campaign-' || NEW.id;
    END IF;

    IF NEW.content_type IN ('richtext', 'html', 'visual') THEN
        NEW.body := denma_utm_rewrite(NEW.body, html_links, slug, domains, own);
    ELSIF NEW.content_type = 'markdown' THEN
        NEW.body := denma_utm_rewrite(denma_utm_rewrite(denma_utm_rewrite(NEW.body, md_links, slug, domains, own),
            html_links, slug, domains, own), md_bare_urls, slug, domains, own);
    ELSE
        NEW.body := denma_utm_rewrite(NEW.body, bare_urls, slug, domains, own);
    END IF;
    IF NEW.altbody IS NOT NULL AND NEW.altbody <> '' THEN
        NEW.altbody := denma_utm_rewrite(NEW.altbody, bare_urls, slug, domains, own);
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS denma_campaign_utm ON campaigns;
CREATE TRIGGER denma_campaign_utm
    BEFORE UPDATE OF status ON campaigns FOR EACH ROW
    WHEN (NEW.status IN ('scheduled', 'running') AND OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION denma_campaign_utm();

-- Website signups' holding list is an automation now (cmd/denma_automations.go).
DROP TRIGGER IF EXISTS denma_confirm_signup ON subscriber_lists;
DROP FUNCTION IF EXISTS denma_confirm_signup();

-- The default visual template: new visual campaigns start from it; it can't be deleted.
CREATE OR REPLACE FUNCTION denma_campaign_add_template() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE
    tpl INT := coalesce((denma_setting('denma.visual_template') #>> '{}')::INT, 0);
    b TEXT;
    src TEXT;
BEGIN
    IF tpl <> 0 AND NEW.content_type = 'visual' AND coalesce(NEW.body, '') = '' AND NEW.body_source IS NULL THEN
        SELECT t.body, t.body_source INTO b, src FROM templates t WHERE t.id = tpl AND t.type = 'campaign_visual';
        IF FOUND THEN
            NEW.body := b;
            NEW.body_source := src;
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS denma_campaign_add_template ON campaigns;
CREATE TRIGGER denma_campaign_add_template
    BEFORE INSERT ON campaigns FOR EACH ROW EXECUTE FUNCTION denma_campaign_add_template();

CREATE OR REPLACE FUNCTION denma_protect_visual_template() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    IF OLD.id IN (coalesce((denma_setting('denma.visual_template') #>> '{}')::INT, 0),
                  coalesce((denma_setting('denma.default_design') #>> '{}')::INT, 0)) THEN
        RETURN NULL;  -- skipped; listmonk reports it as for its default template
    END IF;
    RETURN OLD;
END;
$$;
DROP TRIGGER IF EXISTS denma_protect_visual_template ON templates;
CREATE TRIGGER denma_protect_visual_template
    BEFORE DELETE ON templates FOR EACH ROW EXECUTE FUNCTION denma_protect_visual_template();

-- Imports are marked (attribs.imported_at); the importer is recognised by its queries.
CREATE OR REPLACE FUNCTION denma_is_import() RETURNS BOOLEAN
LANGUAGE sql STABLE SET search_path FROM CURRENT AS $$
    SELECT strpos(current_query(), 'INSERT INTO subscribers as s (uuid, email, name, attribs, status)') > 0
        OR strpos(current_query(), 'INSERT INTO subscribers (uuid, email, name, attribs, status)') > 0;
$$;

-- Attributes with imported_at added (an import may leave them JSON null).
CREATE OR REPLACE FUNCTION denma_with_import_mark(attribs JSONB, mark JSONB) RETURNS JSONB
LANGUAGE sql STABLE SET search_path FROM CURRENT AS $$
    SELECT (CASE WHEN jsonb_typeof(attribs) = 'object' THEN attribs ELSE '{}'::JSONB END)
        || jsonb_build_object('imported_at', COALESCE(mark, to_jsonb(to_char(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))));
$$;

CREATE OR REPLACE FUNCTION denma_is_imported(attribs JSONB) RETURNS BOOLEAN
LANGUAGE sql IMMUTABLE SET search_path FROM CURRENT AS $$
    SELECT jsonb_typeof(attribs) = 'object' AND attribs ? 'imported_at';
$$;
` + denmaImportsSQL + ` -- (before the triggers that use them, cmd/denma_imports.go)

-- The attributes the center keeps itself (denmaReservedAttribs but tags, cmd/denma_signup.go).
CREATE OR REPLACE FUNCTION denma_is_center_attrib(k TEXT) RETURNS BOOLEAN
LANGUAGE sql IMMUTABLE AS $$
    SELECT k IN ('consent_at', 'consent_source', 'consent_confirmed_at', 'consents', 'resubscribed_at', 'imported_at', 'consent_import')
        OR k LIKE 'auto\_blocklist\_%' OR k LIKE 'denma\_%';
$$;

-- new's attributes, with the center's attributes taken from old.
CREATE OR REPLACE FUNCTION denma_keep_center_attribs(new JSONB, old JSONB) RETURNS JSONB
LANGUAGE sql IMMUTABLE SET search_path FROM CURRENT AS $$
    SELECT COALESCE((SELECT jsonb_object_agg(key, value) FROM jsonb_each(CASE WHEN jsonb_typeof(new) = 'object' THEN new ELSE '{}' END)
            WHERE NOT denma_is_center_attrib(key)), '{}'::JSONB)
        || COALESCE((SELECT jsonb_object_agg(key, value) FROM jsonb_each(CASE WHEN jsonb_typeof(old) = 'object' THEN old ELSE '{}' END)
            WHERE denma_is_center_attrib(key)), '{}'::JSONB);
$$;

-- New subscribers from an import are marked (cmd/denma_imports.go).
CREATE OR REPLACE FUNCTION denma_mark_imported_subscriber() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    IF NOT denma_is_import() THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'INSERT' THEN
        NEW.attribs := denma_with_import_mark(denma_keep_center_attribs(NEW.attribs, NULL), NULL)
            || CASE WHEN denma_import_opt('consent') THEN denma_import_consent(NEW.attribs) ELSE '{}' END
            || CASE WHEN NEW.status = 'blocklisted' THEN denma_import_bounced(NEW.attribs) ELSE '{}' END;
    ELSE
        NEW.attribs := denma_keep_center_attribs(NEW.attribs, OLD.attribs)
            || CASE WHEN denma_import_opt('consent') AND COALESCE(OLD.attribs->>'consent_at', '') = ''
                THEN denma_import_consent(NEW.attribs) ELSE '{}' END;
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS denma_mark_imported_subscriber ON subscribers;
CREATE TRIGGER denma_mark_imported_subscriber
    BEFORE INSERT OR UPDATE OF attribs ON subscribers
    FOR EACH ROW EXECUTE FUNCTION denma_mark_imported_subscriber();

-- Existing subscribers an import adds to a list.
CREATE OR REPLACE FUNCTION denma_mark_imported_subscription() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    IF denma_is_import() THEN
        UPDATE subscribers SET attribs = denma_with_import_mark(attribs, NULL)
        WHERE id = NEW.subscriber_id AND NOT COALESCE(denma_is_imported(attribs), false);
    END IF;
    RETURN NULL;
END;
$$;
DROP TRIGGER IF EXISTS denma_mark_imported_subscription ON subscriber_lists;
CREATE TRIGGER denma_mark_imported_subscription
    AFTER INSERT ON subscriber_lists
    FOR EACH ROW EXECUTE FUNCTION denma_mark_imported_subscription();

-- Subscriber tags (cmd/denma_tags.go): lowercase, trimmed, unique, ordered, and only the center's.
CREATE OR REPLACE FUNCTION denma_norm_tags(t JSONB) RETURNS JSONB
LANGUAGE sql IMMUTABLE SET search_path FROM CURRENT AS $$
    SELECT COALESCE(jsonb_agg(DISTINCT x ORDER BY x), '[]'::JSONB) FROM (
        SELECT lower(regexp_replace(btrim(p), '\s+', ' ', 'g')) AS x
        FROM (
            SELECT t #>> '{}' AS v WHERE jsonb_typeof(t) IN ('string', 'number', 'boolean')
            UNION ALL
            SELECT e #>> '{}' FROM jsonb_array_elements(CASE WHEN jsonb_typeof(t) = 'array' THEN t ELSE '[]'::JSONB END) e
                WHERE jsonb_typeof(e) IN ('string', 'number', 'boolean')
        ) s, regexp_split_to_table(s.v, '[,|]') p
    ) n WHERE x <> '' AND length(x) <= 100;
$$;

CREATE TABLE IF NOT EXISTS denma_tags (
    tag        TEXT PRIMARY KEY,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);` + denmaTagAliasesSQL + `
-- Existing subscribers' tags become the center's tags.
INSERT INTO denma_tags (tag) SELECT DISTINCT t FROM subscribers,
    jsonb_array_elements_text(denma_norm_tags(attribs->'tags')) t
    WHERE jsonb_typeof(attribs) = 'object' AND attribs ? 'tags'
        AND NOT EXISTS (SELECT 1 FROM denma_tag_aliases a WHERE a.alias = t)
    ON CONFLICT DO NOTHING;

CREATE OR REPLACE FUNCTION denma_subscriber_tags() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE
    tags JSONB;
BEGIN
    IF jsonb_typeof(NEW.attribs) IS DISTINCT FROM 'object' THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND denma_is_import() AND jsonb_typeof(OLD.attribs) = 'object' AND OLD.attribs ? 'tags' THEN
        NEW.attribs := NEW.attribs || jsonb_build_object('tags', jsonb_build_array(OLD.attribs->'tags', COALESCE(NEW.attribs->'tags', '[]'::JSONB)));
    END IF;
    IF NOT NEW.attribs ? 'tags' THEN
        RETURN NEW;
    END IF;
    -- Nested lists are flattened one level.
    IF jsonb_typeof(NEW.attribs->'tags') = 'array' THEN
        SELECT COALESCE(jsonb_agg(f), '[]'::JSONB) INTO tags
            FROM jsonb_array_elements(NEW.attribs->'tags') e,
            LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(e) = 'array' THEN e ELSE jsonb_build_array(e) END) f;
        tags := denma_norm_tags(tags);
    ELSE
        tags := denma_norm_tags(NEW.attribs->'tags');
    END IF;
    SELECT COALESCE(jsonb_agg(DISTINCT x ORDER BY x), '[]'::JSONB) INTO tags FROM (
        SELECT CASE WHEN EXISTS (SELECT 1 FROM denma_tags d WHERE d.tag = t) THEN t
            ELSE (SELECT a.tag FROM denma_tag_aliases a WHERE a.alias = t) END AS x
        FROM jsonb_array_elements_text(tags) t
    ) m WHERE x IS NOT NULL;
    IF tags = '[]'::JSONB THEN
        NEW.attribs := NEW.attribs - 'tags';
    ELSE
        NEW.attribs := jsonb_set(NEW.attribs, '{tags}', tags);
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS denma_subscriber_tags ON subscribers;
CREATE TRIGGER denma_subscriber_tags
    BEFORE INSERT OR UPDATE OF attribs ON subscribers
    FOR EACH ROW EXECUTE FUNCTION denma_subscriber_tags();
CREATE INDEX IF NOT EXISTS denma_subscribers_tags ON subscribers USING GIN ((attribs->'tags'));

-- The tags a campaign is sent to, besides its lists.
ALTER TABLE campaigns ADD COLUMN IF NOT EXISTS denma_tags TEXT[] NOT NULL DEFAULT '{}';
INSERT INTO denma_tags (tag) SELECT DISTINCT t FROM campaigns, unnest(denma_tags) t
    WHERE NOT EXISTS (SELECT 1 FROM denma_tag_aliases a WHERE a.alias = t) ON CONFLICT DO NOTHING;
` + denmaPreviewTextSQL + denmaReplyToSQL + denmaReplyToMoveSQL + denmaRetriesSQL + denmaResubscribeSQL + denmaSignupSQL + denmaOptOutsSQL + denmaAPISQL + denmaNoConsentSQL + denmaSharedAppliedSQL + denmaOptinTokenSQL + denmaNamesSQL + denmaCampaignStatsSQL + denmaMessagesSQL + denmaConsentCapSQL + denmaPrevContactsSQL + denmaImportBlocklistSQL + denmaTrigramSQL + denmaVersionsSQL

// denmaVisualTemplate is the center's default visual template's ID (0 for none), from the database.
func (a *App) denmaVisualTemplate() int {
	var id int
	err := a.db.Get(&id, `SELECT COALESCE((SELECT (value #>> '{}')::INT FROM settings WHERE key = 'denma.visual_template'), 0)`)
	if err != nil {
		a.log.Printf("denma: error reading the default visual template: %v", err)
	}
	return id
}

// denmaTemplateOut is a template as the API gives it, marked if it's the default visual template.
type denmaTemplateOut struct {
	models.Template
	VisualDefault bool `json:"visual_default"`
}

// denmaMarkVisual marks the default visual template (id) among tpls.
func denmaMarkVisual(tpls []models.Template, id int) []denmaTemplateOut {
	out := make([]denmaTemplateOut, len(tpls))
	for i, t := range tpls {
		out[i] = denmaTemplateOut{t, id != 0 && t.ID == id && t.Type == models.TemplateTypeCampaignVisual}
	}
	return out
}

// denmaTemplateType is template id's type ("" if there's none).
func (a *App) denmaTemplateType(id int) string {
	var typ string
	_ = a.db.Get(&typ, `SELECT COALESCE((SELECT type::TEXT FROM templates WHERE id = $1), '')`, id)
	return typ
}

// denmaSetVisualDefault makes template id the default; only campaign and visual templates can be.
func (a *App) denmaSetVisualDefault(c echo.Context, id int, typ string) error {
	switch {
	case typ == "":
		return echo.NewHTTPError(http.StatusNotFound, "There's no such template.")
	case typ != models.TemplateTypeCampaignVisual:
		return echo.NewHTTPError(http.StatusBadRequest, "Only a campaign template (HTML) or a visual one can be a default.")
	case !a.denmaInCenter():
		return echo.NewHTTPError(http.StatusBadRequest, "Visual templates are for centers.")
	}
	if _, err := a.db.Exec(`INSERT INTO settings (key, value) VALUES ('denma.visual_template', $1::TEXT::JSONB)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()`, strconv.Itoa(id)); err != nil {
		return denmaInternal(err)
	}
	return a.GetTemplates(c)
}

// denmaAddVisualDefault makes listmonk's sample visual template the center's default if it has none.
func denmaAddVisualDefault(tx *sqlx.Tx) error {
	var id int
	if err := tx.Get(&id, `SELECT COALESCE((SELECT t.id FROM settings s JOIN templates t ON t.id = (s.value #>> '{}')::INT AND t.type = 'campaign_visual'
		WHERE s.key = 'denma.visual_template'), 0)`); err != nil || id != 0 {
		return err
	}
	if err := tx.Get(&id, `SELECT COALESCE((SELECT MIN(id) FROM templates WHERE type = 'campaign_visual' AND name = 'Sample visual template'), 0)`); err != nil {
		return err
	}
	if id == 0 {
		body, err := fs.Read("/static/email-templates/default-visual.tpl")
		if err != nil {
			return err
		}
		src, err := fs.Read("/static/email-templates/default-visual.json")
		if err != nil {
			return err
		}
		if err := tx.Get(&id, `INSERT INTO templates (name, type, subject, body, body_source) VALUES ('Sample visual template', 'campaign_visual', '', $1, $2) RETURNING id`,
			string(body), string(src)); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`INSERT INTO settings (key, value) VALUES ('denma.visual_template', $1::TEXT::JSONB)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()`, strconv.Itoa(id))
	return err
}

// denmaNewCampaignFormat is a new campaign's format: Visual if there's a default visual template.
func (a *App) denmaNewCampaignFormat(tpls []models.Template) string {
	if id := a.denmaVisualTemplate(); id != 0 {
		for _, t := range tpls {
			if t.ID == id && t.Type == models.TemplateTypeCampaignVisual {
				return models.CampaignContentTypeVisual
			}
		}
	}
	return models.CampaignContentTypeRichtext
}

// denmaPlainAuto reports whether the center makes campaigns' plain text automatically.
func (a *App) denmaPlainAuto() bool {
	return a.ko.String("denma.center") != "" && a.ko.Bool("denma.plain_text_auto")
}

var reDenmaDomain = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,}$`)

// denmaCheckFeatures checks the Config page's feature settings: bare hostnames, templates that exist.
func (a *App) denmaCheckFeatures(f *denmaCenterForm) error {
	domains := []string{}
	for _, d := range f.UTMDomains {
		d = strings.ToLower(strings.TrimSpace(d))
		d = strings.TrimPrefix(strings.TrimPrefix(d, "https://"), "http://")
		d = strings.TrimPrefix(d, "www.")
		if i := strings.IndexAny(d, "/?#:"); i >= 0 {
			d = d[:i]
		}
		if d == "" || denmaHasString(domains, d) {
			continue
		}
		if !reDenmaDomain.MatchString(d) {
			return fmt.Errorf("%q isn't a domain (such as example.org).", d)
		}
		domains = append(domains, d)
	}
	if len(domains) > 50 {
		return fmt.Errorf("use at most 50 domains for UTM tags")
	}
	f.UTMDomains = domains

	return nil
}
