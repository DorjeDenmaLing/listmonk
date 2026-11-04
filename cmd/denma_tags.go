package main

// denma: subscriber tags, a center's own labels kept in attribs.tags (denmaFeaturesSQL tidies them).

import (
	"database/sql"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

var (
	reDenmaTagSep   = regexp.MustCompile(`[,|]`)
	reDenmaTagSpace = regexp.MustCompile(`\s+`)
)

// denmaNormTags tidies tags as denma_norm_tags does: lowercase, trimmed, deduplicated, at most 100 chars.
func denmaNormTags(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, t := range in {
		for _, p := range reDenmaTagSep.Split(t, -1) {
			p = strings.ToLower(strings.TrimSpace(reDenmaTagSpace.ReplaceAllString(p, " ")))
			if p == "" || len([]rune(p)) > 100 || seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// denmaCampaignColumnsSQL adds the columns listmonk's campaign and export queries read.
const denmaCampaignColumnsSQL = `ALTER TABLE campaigns ADD COLUMN IF NOT EXISTS denma_tags TEXT[] NOT NULL DEFAULT '{}';` +
	denmaPreviewTextSQL + denmaReplyToSQL + denmaRetriesSQL + denmaNamesColumnsSQL

// denmaPreviewTextSQL adds the campaign's preview text (models.Campaign PreviewText).
const denmaPreviewTextSQL = `ALTER TABLE campaigns ADD COLUMN IF NOT EXISTS denma_preview_text TEXT NOT NULL DEFAULT '';`

// denmaPreviewTextMax is the most characters of preview text.
const denmaPreviewTextMax = 200

// denmaInstallColumns adds the columns on a new install, before the queries are prepared.
func denmaInstallColumns(db *sqlx.DB) {
	if _, err := db.Exec(denmaCampaignColumnsSQL + denmaMediaLabelSQL); err != nil {
		lo.Fatalf("denma: error adding the campaign tags column, failed sends and media labels: %v", err)
	}
}

// denmaHubColumns adds the hub's campaign tags, failed sends, daily limit and logo; run on every start.
func denmaHubColumns(db *sqlx.DB) {
	if _, err := db.Exec(denmaCampaignColumnsSQL + denmaVersionColumnsSQL +
		`INSERT INTO settings (key, value) VALUES ('denma.daily_limit', '0'), ('denma.daily_reserve', '5'),
			('denma.admin_logo_url', '""'), ('denma.admin_icon_url', '""'), ('denma.audit_days', '365')
			ON CONFLICT (key) DO NOTHING;` + denmaMoveSlidingWindowSQL); err != nil {
		lo.Fatalf("denma: error adding the campaign tags column and failed sends to the hub: %v", err)
	}
	if err := denmaDeleteAPIUsers(db, "the hub"); err != nil { // cmd/denma_noapiusers.go
		lo.Fatalf("denma: error deleting the hub's API users: %v", err)
	}
}

// denmaTagActiveSQL is true for an active subscriber (alias subscribers); the campaign queries use it too.
const denmaTagActiveSQL = `(subscribers.status <> 'blocklisted' AND EXISTS (SELECT 1 FROM subscriber_lists dsl
	JOIN lists dl ON dl.id = dsl.list_id WHERE dsl.subscriber_id = subscribers.id AND ` + denmaAPIListActive + `
	AND (CASE WHEN dl.optin = 'double' THEN dsl.status = 'confirmed' ELSE dsl.status <> 'unsubscribed' END)))`

// denmaTagCount is a tag, how many subscribers have it, and how many a campaign to it would reach.
type denmaTagCount struct {
	Tag         string `db:"tag" json:"tag"`
	Subscribers int    `db:"subscribers" json:"subscribers"`
	Active      int    `db:"active" json:"active"`
	// Its other names (cmd/denma_tagaliases.go).
	Aliases pq.StringArray `db:"aliases" json:"aliases"`
}

// denmaTags returns the center's tags, by name.
func (a *App) denmaTags() ([]denmaTagCount, error) {
	out := []denmaTagCount{}
	err := a.db.Select(&out, `SELECT t.tag, COUNT(subscribers.id) AS subscribers,
		COUNT(subscribers.id) FILTER (WHERE `+denmaTagActiveSQL+`) AS active,
		ARRAY(SELECT alias FROM denma_tag_aliases a WHERE a.tag = t.tag ORDER BY alias) AS aliases
		FROM denma_tags t LEFT JOIN subscribers ON jsonb_typeof(subscribers.attribs->'tags') = 'array' AND subscribers.attribs->'tags' ? t.tag
		GROUP BY t.tag ORDER BY t.tag`)
	return out, err
}

// denmaUnknownTags returns an error naming the tidied tags the center doesn't have.
func (a *App) denmaUnknownTags(tags []string) error {
	var missing []string
	if err := a.db.Select(&missing, `SELECT t FROM unnest($1::TEXT[]) t WHERE NOT EXISTS (SELECT 1 FROM denma_tags d WHERE d.tag = t)`,
		pq.Array(tags)); err != nil {
		a.log.Printf("denma: error checking tags: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error checking the tags.")
	}
	if len(missing) == 0 {
		return nil
	}
	s, it := "", "it"
	if len(missing) > 1 {
		s, it = "s", "them"
	}
	return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("There's no tag%s %s. Pick from the center's tags, or add %s under Contacts -> Tags first.",
		s, strings.Join(missing, ", "), it))
}

// denmaCheckSubscriberTags checks a subscriber's attributes only have the center's tags or other names.
func (a *App) denmaCheckSubscriberTags(attribs map[string]any) error {
	tags, err := a.denmaResolveTags(denmaAttribTags(attribs))
	if err != nil {
		a.log.Printf("denma: error checking tags: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error checking the tags.")
	}
	if len(tags) == 0 {
		return nil
	}
	return a.denmaUnknownTags(tags)
}

// denmaAttribTags is the tags in attributes' "tags" (a string or list, nested once), tidied.
func denmaAttribTags(attribs map[string]any) []string {
	var tags []string
	var add func(v any, depth int)
	add = func(v any, depth int) {
		switch v := v.(type) {
		case []any:
			if depth < 2 {
				for _, e := range v {
					add(e, depth+1)
				}
			}
		case string, float64, bool:
			tags = append(tags, fmt.Sprint(v))
		}
	}
	add(attribs["tags"], 0)
	return denmaNormTags(tags)
}

func initDenmaTagHandlers(g *echo.Group, a *App) {
	g.GET(path.Join(uriAdmin, "/subscribers/tags"), a.ViewDenmaTags)
}

func initDenmaTagAPIHandlers(g *echo.Group, a *App) {
	pm := a.auth.Perm
	g.GET("/api/denma/tags", pm(a.DenmaGetTags, "subscribers:get_all"))
	g.POST("/api/denma/tags", pm(a.DenmaAddTag, "subscribers:manage"))
	g.PUT("/api/denma/tags", pm(a.DenmaChangeTag, "subscribers:manage"))
	g.PUT("/api/denma/tags/subscribers", pm(a.DenmaTagSubscribers, "subscribers:manage"))
	g.PUT("/api/denma/tags/aliases", pm(a.DenmaSetTagAliases, "subscribers:manage"))
}

type denmaTagsView struct {
	adminView
	Tags []denmaTagCount
}

// ViewDenmaTags renders Subscribers -> Tags.
func (a *App) ViewDenmaTags(c echo.Context) error {
	v := newAdminView(c, "Tags", "", "subscribers.tags")
	if !v.Can("subscribers:get_all") {
		return echo.NewHTTPError(http.StatusForbidden, a.i18n.Ts("globals.messages.permissionDenied", "name", "subscribers:get_all"))
	}
	tags, err := a.denmaTags()
	if err != nil {
		return denmaInternal(err)
	}
	return c.Render(http.StatusOK, "admin-denma-tags", denmaTagsView{adminView: v, Tags: tags})
}

// DenmaGetTags returns the center's tags with their counts.
func (a *App) DenmaGetTags(c echo.Context) error {
	tags, err := a.denmaTags()
	if err != nil {
		a.log.Printf("denma: error reading the tags: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error reading the tags.")
	}
	return c.JSON(http.StatusOK, okResp{tags})
}

// denmaAllSubscribers returns an error unless the user may change every subscriber.
func (a *App) denmaAllSubscribers(c echo.Context) error {
	u := auth.GetUser(c)
	if !denmaAllLists(&u) || !u.HasPerm("subscribers:manage") {
		return echo.NewHTTPError(http.StatusForbidden, "Changing a tag for everyone needs access to all subscribers.")
	}
	return nil
}

// DenmaAddTag adds a tag to the center.
func (a *App) DenmaAddTag(c echo.Context) error {
	var req struct {
		Tag string `json:"tag"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	tag := denmaNormTags([]string{req.Tag})
	if len(tag) != 1 {
		return echo.NewHTTPError(http.StatusBadRequest, "Enter one tag, without commas, of up to 100 characters.")
	}
	tx, err := a.db.Beginx()
	if err != nil {
		a.log.Printf("denma: error adding the tag %q: %v", tag[0], err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error adding the tag.")
	}
	defer tx.Rollback()
	var of string
	var res sql.Result
	if err = denmaLockTags(tx); err == nil {
		of, err = a.denmaAliasOf(tx, tag[0])
	}
	if err == nil && of != "" {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("%s is another name for the tag %s. Remove it from its other names first.", tag[0], of))
	}
	if err == nil {
		res, err = tx.Exec(`INSERT INTO denma_tags (tag) VALUES ($1) ON CONFLICT DO NOTHING`, tag[0])
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		a.log.Printf("denma: error adding the tag %q: %v", tag[0], err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error adding the tag.")
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("%s is already a tag.", tag[0]))
	}
	return c.JSON(http.StatusOK, okResp{map[string]string{"tag": tag[0]}})
}

// DenmaChangeTag renames a tag (merging into an existing one) or deletes it everywhere.
func (a *App) DenmaChangeTag(c echo.Context) error {
	if err := a.denmaAllSubscribers(c); err != nil {
		return err
	}
	var req struct {
		Action string `json:"action"` // rename, delete
		Tag    string `json:"tag"`
		To     string `json:"to"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	from := denmaNormTags([]string{req.Tag})
	if len(from) != 1 {
		return echo.NewHTTPError(http.StatusBadRequest, "Name one tag.")
	}
	to := []string{} // not nil: pq.Array(nil) is NULL, which would blank every tag the subscribers have
	switch req.Action {
	case "rename":
		if to = denmaNormTags([]string{req.To}); len(to) != 1 {
			return echo.NewHTTPError(http.StatusBadRequest, "Enter the tag's new name (one tag, without commas).")
		}
	case "delete":
	default:
		return echo.NewHTTPError(http.StatusBadRequest, "action must be rename or delete")
	}
	if len(to) == 1 && to[0] == from[0] {
		return c.JSON(http.StatusOK, okResp{map[string]int64{"subscribers": 0}})
	}

	tx, err := a.db.Beginx()
	if err != nil {
		a.log.Printf("denma: error changing the tag %q: %v", from[0], err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error changing the tag.")
	}
	defer tx.Rollback()

	// The new name is added first, then the old goes (cmd/denma_tagaliases.go).
	var res sql.Result
	err = denmaLockTags(tx)
	if err == nil && len(to) == 1 {
		var of string
		_, err = tx.Exec(`DELETE FROM denma_tag_aliases WHERE alias = $1 AND tag = $2`, to[0], from[0])
		if err == nil {
			of, err = a.denmaAliasOf(tx, to[0])
		}
		if err == nil && of != "" {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("%s is another name for the tag %s. Remove it from its other names first.", to[0], of))
		}
		if err == nil {
			_, err = tx.Exec(`INSERT INTO denma_tags (tag) VALUES ($1) ON CONFLICT DO NOTHING`, to[0])
		}
		if err == nil {
			_, err = tx.Exec(`UPDATE denma_tag_aliases SET tag = $2 WHERE tag = $1`, from[0], to[0])
		}
	}
	// An automation keeps its tags.
	if err == nil && len(to) == 0 && denmaAutomationsOn(a) {
		var names []string
		err = tx.Select(&names, `SELECT name FROM denma_automations
			WHERE $1::TEXT = ANY(trigger_tags || if_tags || unless_tags || add_tags || remove_tags) ORDER BY name LIMIT 5`, from[0])
		if err == nil && len(names) > 0 {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Automations use the tag %s: %s. Take it off them first.", from[0], strings.Join(names, ", ")))
		}
	}
	if err == nil {
		res, err = tx.Exec(`DELETE FROM denma_tags WHERE tag = $1`, from[0])
	}
	if err == nil {
		if n, _ := res.RowsAffected(); n == 0 {
			return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("There's no tag %s.", from[0]))
		}
	}
	// The time subscribers got it (cmd/denma_automations.go) goes to the new name first.
	if err == nil && len(to) == 1 && denmaAutomationsOn(a) {
		_, err = tx.Exec(`INSERT INTO denma_tag_log (subscriber_id, tag, added_at) SELECT subscriber_id, $2, added_at
			FROM denma_tag_log WHERE tag = $1 ON CONFLICT DO NOTHING`, from[0], to[0])
	}
	// Removing the old one and adding the new; the trigger tidies the result.
	if err == nil {
		res, err = tx.Exec(`UPDATE subscribers SET attribs = attribs || jsonb_build_object('tags', (attribs->'tags') - $1::TEXT || to_jsonb($2::TEXT[])),
			updated_at = NOW() WHERE jsonb_typeof(attribs->'tags') = 'array' AND attribs->'tags' ? $1::TEXT`, from[0], pq.Array(to))
	}
	// Sign-up webhooks (cmd/denma_signup.go) and automations follow it too.
	if err == nil && denmaAutomationsOn(a) {
		for _, col := range []string{"trigger_tags", "if_tags", "unless_tags", "add_tags", "remove_tags"} {
			if _, err = tx.Exec(`UPDATE denma_automations SET `+col+` = ARRAY(SELECT DISTINCT x FROM unnest(array_remove(`+col+`, $1::TEXT) || $2::TEXT[]) x ORDER BY x),
				updated_at = NOW() WHERE $1::TEXT = ANY(`+col+`)`, from[0], pq.Array(to)); err != nil {
				break
			}
		}
	}
	for _, col := range []string{"tags", "request_tags"} {
		if err == nil {
			_, err = tx.Exec(`UPDATE denma_signup_hooks SET `+col+` = ARRAY(SELECT DISTINCT x FROM unnest(array_remove(`+col+`, $1::TEXT) || $2::TEXT[]) x ORDER BY x)
				WHERE $1::TEXT = ANY(`+col+`)`, from[0], pq.Array(to))
		}
	}
	// Campaigns not yet sent follow the rename (or lose the tag).
	if err == nil {
		_, err = tx.Exec(`UPDATE campaigns SET denma_tags = ARRAY(SELECT DISTINCT x FROM unnest(array_remove(denma_tags, $1::TEXT) || $2::TEXT[]) x ORDER BY x)
			WHERE $1::TEXT = ANY(denma_tags) AND status IN ('draft', 'scheduled', 'paused')`, from[0], pq.Array(to))
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		a.log.Printf("denma: error changing the tag %q: %v", from[0], err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error changing the tag.")
	}
	n, _ := res.RowsAffected()
	return c.JSON(http.StatusOK, okResp{map[string]int64{"subscribers": n}})
}

// denmaTagsSet is the new attributes when adding (add) or removing tags (tags, a TEXT[] parameter).
func denmaTagsSet(add, tags string) string {
	cur := `COALESCE(CASE WHEN jsonb_typeof(attribs->'tags') = 'array' THEN attribs->'tags' END, '[]'::JSONB)`
	return `(CASE WHEN jsonb_typeof(attribs) = 'object' THEN attribs ELSE '{}'::JSONB END) || jsonb_build_object('tags',
		CASE WHEN ` + add + `::BOOLEAN THEN ` + cur + ` || to_jsonb(` + tags + `::TEXT[]) ELSE ` + cur + ` - ` + tags + `::TEXT[] END)`
}

// denmaTagsByQuerySQL adds or removes tags ($5, $6) for the subscribers matching a query (%query%, $1-$4).
var denmaTagsByQuerySQL = `WITH subs AS (%query%)
UPDATE subscribers SET attribs = ` + denmaTagsSet("$5", "$6") + `, updated_at = NOW()
WHERE id = ANY(SELECT id FROM subs)`

// DenmaTagSubscribers adds or removes tags for the subscribers picked (ids) or matching a search.
func (a *App) DenmaTagSubscribers(c echo.Context) error {
	user := auth.GetUser(c)
	var req struct {
		subQueryReq
		Tags []string `json:"tags"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	tags := denmaNormTags(req.Tags)
	if len(tags) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "Enter a tag.")
	}
	if req.Action != "add" && req.Action != "remove" {
		return echo.NewHTTPError(http.StatusBadRequest, "action must be add or remove")
	}
	add := req.Action == "add"
	if add {
		if err := a.denmaUnknownTags(tags); err != nil {
			return err
		}
	}

	if len(req.SubscriberIDs) > 0 {
		if err := a.hasSubPerm(user, auth.PermTypeManage, req.SubscriberIDs); err != nil {
			return err
		}
		if _, err := a.db.Exec(`UPDATE subscribers SET attribs = `+denmaTagsSet("$2", "$3")+`, updated_at = NOW() WHERE id = ANY($1::INT[])`,
			pq.Array(req.SubscriberIDs), add, pq.Array(tags)); err != nil {
			a.log.Printf("denma: error tagging subscribers: %v", err)
			return echo.NewHTTPError(http.StatusInternalServerError, "Error changing the tags.")
		}
		return c.JSON(http.StatusOK, okResp{true})
	}

	// Every subscriber matching the search, as BlocklistSubscribersByQuery.
	req.Search = strings.TrimSpace(req.Search)
	req.Query = formatSQLExp(req.Query)
	if req.All {
		req.Search, req.Query = "", ""
	} else if req.Search == "" && req.Query == "" {
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.Ts("globals.messages.invalidFields", "name", "query"))
	}
	if err := a.denmaBulkSearch(user, &req.subQueryReq); err != nil {
		return err
	}
	listIDs := user.GetPermittedListIDs(auth.PermTypeManage, req.ListIDs)
	if all, _ := user.GetPermittedLists(auth.PermTypeManage); !all && len(listIDs) == 0 {
		return echo.NewHTTPError(http.StatusForbidden, a.i18n.Ts("globals.messages.permissionDenied", "name", "lists"))
	}
	if err := a.queries.ExecSubQueryTpl(req.Search, sanitizeDenmaSQL(req.Query), denmaTagsByQuerySQL, listIDs, a.db, req.SubscriptionStatus, add, pq.Array(tags)); err != nil {
		a.log.Printf("denma: error tagging subscribers: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error changing the tags.")
	}
	return c.JSON(http.StatusOK, okResp{true})
}

// denmaAllLists reports whether a user has access to every list.
func denmaAllLists(u *auth.User) bool {
	all, _ := u.GetPermittedLists(auth.PermTypeGet | auth.PermTypeManage)
	return all
}

// sanitizeDenmaSQL is the core's sanitizeSQLExp for a condition built by denmaSearch.
func sanitizeDenmaSQL(q string) string {
	return strings.TrimSuffix(strings.TrimSpace(q), ";")
}

// denmaCheckCampaign tidies and checks a campaign's columns of ours: preview text, reply-to and tags.
func (a *App) denmaCheckCampaign(o *campReq, c echo.Context) error {
	if err := denmaNoOptinCampaign(o.Type); err != nil { // cmd/denma_optincampaigns.go
		return err
	}
	o.PreviewText = strings.TrimSpace(o.PreviewText)
	if utf8.RuneCountInString(o.PreviewText) > denmaPreviewTextMax {
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("The preview text is too long; at most %d characters.", denmaPreviewTextMax))
	}
	var err error
	if o.ReplyTo, err = denmaCheckReplyTo(o.ReplyTo); err != nil { // cmd/denma_reply_to.go
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := a.denmaCheckAttachments(o.MediaIDs); err != nil { // cmd/denma_attachlimit.go
		return err
	}

	o.SubscriberTags = denmaNormTags(o.SubscriberTags)
	if len(o.SubscriberTags) == 0 {
		return nil
	}
	if u := auth.GetUser(c); !denmaAllLists(&u) {
		return echo.NewHTTPError(http.StatusForbidden, "Sending to tags needs access to all lists.")
	}
	return a.denmaUnknownTags(o.SubscriberTags)
}

// denmaSaveCampaign saves a campaign's tags, preview text and reply-to into out, after listmonk's save.
func (a *App) denmaSaveCampaign(id int, o *campReq, out *models.Campaign) error {
	tags := o.SubscriberTags
	if tags == nil {
		tags = []string{}
	}
	if _, err := a.db.Exec(`UPDATE campaigns SET denma_tags = $2, denma_preview_text = $3, denma_reply_to = $4 WHERE id = $1`,
		id, pq.Array(tags), o.PreviewText, o.ReplyTo); err != nil {
		a.log.Printf("denma: error saving campaign %d's tags, preview text and reply-to: %v", id, err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error saving the campaign's tags, preview text and reply-to.")
	}
	out.SubscriberTags = tags
	out.PreviewText = o.PreviewText
	out.ReplyTo = o.ReplyTo
	return nil
}
