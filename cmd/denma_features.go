package main

// denma: features from the old production add-on's database triggers
// (listmonk-js/sql, installed by hand in DDL's database with DDL's lists,
// template and domains written in), here for every center, each set on its
// Advanced page (cmd/denma_center.go):
//
//   - Unsubscribe everywhere (denma.unsubscribe_everywhere): unsubscribing
//     from any list blocklists the subscriber and unsubscribes them from every
//     list, whatever path did it (the unsubscribe page, one-click List-
//     Unsubscribe, the admin).
//   - Automatic plain text (denma.plain_text_auto): every campaign's plain-
//     text version is made from the email as sent (its template included) on
//     each save, unless the campaign's "Edit the plain-text version by hand"
//     is ticked (attribs.plain_text_manual, views/campaign.html).
//   - UTM tags (denma.utm_domains): when a campaign is scheduled or starts,
//     links to these domains (and their subdomains) get utm_source=newsletter,
//     utm_medium=email and utm_campaign=<the campaign's name>. Links to the
//     app's own address, with template tags, or with their own utm_ tags are
//     left alone.
//   - Website signups (denma.signup_holding_list, denma.signup_target_list):
//     someone who confirms their subscription to the holding list (double
//     opt-in) is moved to the other list, confirmed, with
//     attribs.consent_confirmed_at.
//   - A default visual template (denma.visual_template): new visual campaigns
//     start from it, and it can't be deleted (as listmonk's default template).
//   - Imports are marked (always): subscribers an import adds get
//     attribs.imported_at, and automations skip them (cmd/denma_automations.go).
//
// The triggers and functions are installed in each center's schema when it
// loads, if they've changed (denmaFeaturesVersion), and read the settings when
// they run, so changing one needs no reinstall. Each function keeps the
// center's schema as its search path, so it works whatever the session's (the
// shared blocklist changes every center's subscribers from one connection). The first time, the old add-
// on's triggers on the center's tables (DDL's, in production) are read for
// their settings and dropped; their functions, owned by the database
// superuser, are left in public, unused.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/listmonk/models"
	"github.com/lib/pq"
)

// denmaFeaturesVersion is the version of denmaFeaturesSQL; a center with an
// older one gets it again when it loads.
const denmaFeaturesVersion = 3

// denmaFeatureDefaults are the settings' values in a center that doesn't
// have them yet.
var denmaFeatureDefaults = map[string]any{
	"denma.unsubscribe_everywhere": false,
	"denma.plain_text_auto":        true,
	"denma.utm_domains":            []string{},
	"denma.signup_holding_list":    0,
	"denma.signup_target_list":     0,
	"denma.visual_template":        0,
}

// features installs the features' triggers in a center (db, its schema)
// if needed, with its settings (and the old add-on's, the first time).
func (d *denmaCenters) features(c *denmaCenter, db *sqlx.DB) error {
	var ver int
	if err := db.Get(&ver, `SELECT COALESCE((SELECT value::INT FROM settings WHERE key = 'denma.features_version'), 0)`); err != nil {
		return err
	}
	if ver == denmaFeaturesVersion {
		return nil
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
			return fmt.Errorf("reading the old add-on's triggers: %v", err)
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
	if _, err := tx.Exec(denmaFeaturesSQL); err != nil {
		return fmt.Errorf("installing the triggers: %v", err)
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

// denmaOldAddon reads the settings the old add-on's triggers on this schema's
// tables had written in, and drops the triggers.
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

// denmaFeaturesSQL installs (or updates) the features' functions and
// triggers in the current schema. Trigger names sort in the order they must
// run: the default visual template, then the plain text, then the UTM tags.
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
    RETURN replace(t, '&amp;', '&');  -- last, so "&amp;lt;" stays "&lt;"
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
BEGIN
    FOREACH label IN ARRAY ARRAY['head', 'style', 'script', 'title'] LOOP
        t := regexp_replace(t, format('<%s\M[^>]*>(?:[^<]|<(?!/%s))*</%s>', label, label, label), '', 'gi');
    END LOOP;
    t := regexp_replace(t, '<!--(?:[^-]|-(?!->))*-->', '', 'g');
    t := regexp_replace(t, '\{\{\s*TrackView\s*\}\}', '', 'g');
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
    RETURN trim(BOTH E' \n' FROM t);
END;
$$;

-- The email a campaign sends, as plain text: richtext, HTML and markdown in
-- their template.
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

-- Rewrites every link in body that matches pattern, whose first group is the
-- text before the URL and whose second is the URL.
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

-- Website signups: confirming the holding list moves them to the other one.
CREATE OR REPLACE FUNCTION denma_confirm_signup() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE
    holding INT := coalesce((denma_setting('denma.signup_holding_list') #>> '{}')::INT, 0);
    target INT := coalesce((denma_setting('denma.signup_target_list') #>> '{}')::INT, 0);
BEGIN
    IF holding = 0 OR target = 0 OR holding = target OR NEW.list_id <> holding THEN
        RETURN NULL;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM lists WHERE id = target) THEN
        RETURN NULL;
    END IF;
    IF EXISTS (SELECT 1 FROM subscribers WHERE id = NEW.subscriber_id AND status <> 'blocklisted') THEN
        INSERT INTO subscriber_lists (subscriber_id, list_id, status, meta)
        VALUES (NEW.subscriber_id, target, 'confirmed', NEW.meta)
        ON CONFLICT (subscriber_id, list_id) DO UPDATE
            SET status = 'confirmed', meta = subscriber_lists.meta || EXCLUDED.meta, updated_at = NOW()
            WHERE subscriber_lists.status = 'unconfirmed';
        -- Attributes may be JSON null, which || would make an array.
        UPDATE subscribers
        SET attribs = (CASE WHEN jsonb_typeof(attribs) = 'object' THEN attribs ELSE '{}'::JSONB END) || jsonb_build_object('consent_confirmed_at',
                to_char(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')),
            updated_at = NOW()
        WHERE id = NEW.subscriber_id;
    END IF;
    DELETE FROM subscriber_lists WHERE subscriber_id = NEW.subscriber_id AND list_id = holding;
    RETURN NULL;
END;
$$;
DROP TRIGGER IF EXISTS denma_confirm_signup ON subscriber_lists;
CREATE TRIGGER denma_confirm_signup
    AFTER UPDATE OF status ON subscriber_lists FOR EACH ROW
    WHEN (NEW.status = 'confirmed' AND OLD.status IS DISTINCT FROM 'confirmed')
    EXECUTE FUNCTION denma_confirm_signup();

-- The default visual template: new visual campaigns start from it, and it
-- can't be deleted.
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
    IF OLD.id = coalesce((denma_setting('denma.visual_template') #>> '{}')::INT, 0) THEN
        RETURN NULL;  -- skipped; listmonk reports it as for its default template
    END IF;
    RETURN OLD;
END;
$$;
DROP TRIGGER IF EXISTS denma_protect_visual_template ON templates;
CREATE TRIGGER denma_protect_visual_template
    BEFORE DELETE ON templates FOR EACH ROW EXECUTE FUNCTION denma_protect_visual_template();

-- Imports are marked (attribs.imported_at), so automations skip them. The
-- importer is recognised by its query (listmonk's upsert-subscriber).
CREATE OR REPLACE FUNCTION denma_is_import() RETURNS BOOLEAN
LANGUAGE sql STABLE SET search_path FROM CURRENT AS $$
    SELECT strpos(current_query(), 'INSERT INTO subscribers as s (uuid, email, name, attribs, status)') > 0;
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

-- New subscribers from an import; an import's "overwrite" keeps an earlier mark.
CREATE OR REPLACE FUNCTION denma_mark_imported_subscriber() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    IF NOT denma_is_import() THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NOT COALESCE(denma_is_imported(NEW.attribs), false) THEN
            NEW.attribs := denma_with_import_mark(NEW.attribs, NULL);
        END IF;
    ELSIF COALESCE(denma_is_imported(OLD.attribs), false) AND NOT COALESCE(denma_is_imported(NEW.attribs), false) THEN
        NEW.attribs := denma_with_import_mark(NEW.attribs, OLD.attribs->'imported_at');
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
`

// denmaVisualTemplate is the center's default visual template's ID (0 for
// none).
func (a *App) denmaVisualTemplate() int {
	return a.ko.Int("denma.visual_template")
}

// denmaNewCampaignFormat is a new campaign's format: Visual if the center has
// a default visual template (it then starts from it), else listmonk's.
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

// denmaPlainAuto reports whether the center makes campaigns' plain text
// automatically (always without centers: false).
func (a *App) denmaPlainAuto() bool {
	return a.ko.String("denma.center") != "" && a.ko.Bool("denma.plain_text_auto")
}

var reDenmaDomain = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,}$`)

// denmaCheckFeatures checks and tidies the Advanced page's feature settings:
// domains as bare hostnames, lists and the template that exist.
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

	if (f.SignupHoldingList == 0) != (f.SignupTargetList == 0) {
		return fmt.Errorf("choose both website signup lists, or neither")
	}
	if f.SignupHoldingList != 0 {
		if f.SignupHoldingList == f.SignupTargetList {
			return fmt.Errorf("the website signup lists must be two different lists")
		}
		var optin string
		if err := a.db.Get(&optin, `SELECT optin::TEXT FROM lists WHERE id = $1`, f.SignupHoldingList); err != nil {
			return fmt.Errorf("the website signups' holding list doesn't exist")
		}
		if optin != "double" {
			return fmt.Errorf("the website signups' holding list must be double opt-in: confirming it is what moves people on")
		}
		var n int
		if err := a.db.Get(&n, `SELECT COUNT(*) FROM lists WHERE id = $1`, f.SignupTargetList); err != nil || n == 0 {
			return fmt.Errorf("the list website signups move to doesn't exist")
		}
	}
	if f.VisualTemplate != 0 {
		var n int
		if err := a.db.Get(&n, `SELECT COUNT(*) FROM templates WHERE id = $1 AND type = 'campaign_visual'`, f.VisualTemplate); err != nil || n == 0 {
			return fmt.Errorf("the default visual template doesn't exist")
		}
	}
	return nil
}
