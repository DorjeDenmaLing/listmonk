package main

// denma: subscriber tags, a center's own labels for its subscribers
// (volunteer, donor, retreat-2026), which subscribers never see. They're
// kept in the subscriber's attributes (attribs.tags, a list), so they go
// wherever attributes go: the API, imports, exports, templates. A trigger
// keeps them lowercase, trimmed and without duplicates however they're set
// (denmaFeaturesSQL, cmd/denma_features.go).
//
// A center's tags are added under Subscribers -> Tags (views/denma-tags.html,
// the denma_tags table), which lists them with how many have each, and
// renames or deletes them. A subscriber can only have those: they're given
// or taken away on the subscriber's page, for the subscribers picked (or
// every one matching a search) on the Subscribers page, or by an import
// (which leaves out tags the center doesn't have); search finds them with
// tag:volunteer.
//
// A campaign can be sent to tags as well as lists (campaigns.denma_tags,
// "subscriber_tags" in the API): it goes to everyone on its lists or with
// any of its tags, once each. A tag reaches only active subscribers: those
// not blocklisted, with at least one subscription that isn't unsubscribed
// (confirmed, on a double opt-in list), so it never reaches someone who
// didn't sign up or confirm (queries/campaigns.sql). Since a tag can reach
// anyone in the center, sending to tags needs access to all lists.

import (
	"fmt"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"

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

// denmaNormTags tidies tags as the database does (denma_norm_tags): split at
// commas and |, lowercase, trimmed, spaces collapsed, without empty ones,
// duplicates or ones over 100 characters, sorted.
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

// denmaCampaignColumnsSQL adds what listmonk's campaign queries read of ours:
// the tags column and the failed sends (cmd/denma_retries.go).
const denmaCampaignColumnsSQL = `ALTER TABLE campaigns ADD COLUMN IF NOT EXISTS denma_tags TEXT[] NOT NULL DEFAULT '{}';` + denmaRetriesSQL

// denmaInstallColumns adds them on a new install (install.go), before its
// queries are prepared, as a center's features (denmaFeaturesSQL) or the hub
// (denmaHubColumns) add them later.
func denmaInstallColumns(db *sqlx.DB) {
	if _, err := db.Exec(denmaCampaignColumnsSQL); err != nil {
		lo.Fatalf("denma: error adding the campaign tags column and failed sends: %v", err)
	}
}

// denmaHubColumns adds the campaign tags column and the failed sends' table
// (cmd/denma_retries.go) to the hub's schema, which has no features installed
// but runs listmonk's campaign queries (which read them), and the daily
// sending limit and its reserve (cmd/denma_daily.go, which also replaces
// listmonk's sliding window with it) and the admin's logo
// (partials/denma/topnav.html) to its settings. Called on every start
// (denmaPrepareHub).
func denmaHubColumns(db *sqlx.DB) {
	if _, err := db.Exec(denmaCampaignColumnsSQL +
		`INSERT INTO settings (key, value) VALUES ('denma.daily_limit', '0'), ('denma.daily_reserve', '5'),
			('denma.admin_logo_url', '""'), ('denma.admin_icon_url', '""')
			ON CONFLICT (key) DO NOTHING;` + denmaMoveSlidingWindowSQL); err != nil {
		lo.Fatalf("denma: error adding the campaign tags column and failed sends to the hub: %v", err)
	}
}

// denmaTagActiveSQL is true for an active subscriber (alias subscribers): not
// blocklisted, with a subscription that isn't unsubscribed (or, on a double
// opt-in list, is confirmed). The campaign queries use the same condition.
const denmaTagActiveSQL = `(subscribers.status <> 'blocklisted' AND EXISTS (SELECT 1 FROM subscriber_lists dsl
	JOIN lists dl ON dl.id = dsl.list_id WHERE dsl.subscriber_id = subscribers.id
	AND (CASE WHEN dl.optin = 'double' THEN dsl.status = 'confirmed' ELSE dsl.status <> 'unsubscribed' END)))`

// denmaTagCount is a tag, how many subscribers have it, and how many of them a
// campaign to it would reach.
type denmaTagCount struct {
	Tag         string `db:"tag" json:"tag"`
	Subscribers int    `db:"subscribers" json:"subscribers"`
	Active      int    `db:"active" json:"active"`
}

// denmaTags returns the center's tags, by name.
func (a *App) denmaTags() ([]denmaTagCount, error) {
	out := []denmaTagCount{}
	err := a.db.Select(&out, `SELECT t.tag, COUNT(subscribers.id) AS subscribers,
		COUNT(subscribers.id) FILTER (WHERE `+denmaTagActiveSQL+`) AS active
		FROM denma_tags t LEFT JOIN subscribers ON jsonb_typeof(subscribers.attribs->'tags') = 'array' AND subscribers.attribs->'tags' ? t.tag
		GROUP BY t.tag ORDER BY t.tag`)
	return out, err
}

// denmaUnknownTags returns an error naming the tags (tidied) the center
// doesn't have, if any.
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
	return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("There's no tag%s %s. Pick from the center's tags, or add %s under Subscribers -> Tags first.",
		s, strings.Join(missing, ", "), it))
}

// denmaCheckSubscriberTags checks that a subscriber's attributes (as sent to
// the API) only have the center's tags. Called by CreateSubscriber,
// UpdateSubscriber and PatchSubscriber.
func (a *App) denmaCheckSubscriberTags(attribs map[string]any) error {
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
	if tags = denmaNormTags(tags); len(tags) == 0 {
		return nil
	}
	return a.denmaUnknownTags(tags)
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
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
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

// denmaAllSubscribers returns an error unless the user may change every
// subscriber (subscribers:manage, and all lists).
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
	res, err := a.db.Exec(`INSERT INTO denma_tags (tag) VALUES ($1) ON CONFLICT DO NOTHING`, tag[0])
	if err != nil {
		a.log.Printf("denma: error adding the tag %q: %v", tag[0], err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error adding the tag.")
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("%s is already a tag.", tag[0]))
	}
	return c.JSON(http.StatusOK, okResp{map[string]string{"tag": tag[0]}})
}

// DenmaChangeTag renames a tag (merging it into another of that name), or
// deletes it from the center and every subscriber.
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

	// The center's tags first: the trigger keeps subscribers to them.
	res, err := tx.Exec(`DELETE FROM denma_tags WHERE tag = $1`, from[0])
	if err == nil {
		if n, _ := res.RowsAffected(); n == 0 {
			return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("There's no tag %s.", from[0]))
		}
		if len(to) == 1 {
			_, err = tx.Exec(`INSERT INTO denma_tags (tag) VALUES ($1) ON CONFLICT DO NOTHING`, to[0])
		}
	}
	// When subscribers got it (cmd/denma_automations.go) goes with the new
	// name first, so that renaming doesn't set off automations.
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
	if err == nil {
		_, err = tx.Exec(`UPDATE denma_signup_hooks SET tags = ARRAY(SELECT DISTINCT x FROM unnest(array_remove(tags, $1::TEXT) || $2::TEXT[]) x ORDER BY x)
			WHERE $1::TEXT = ANY(tags)`, from[0], pq.Array(to))
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

// denmaTagsSet is the new attributes when adding (add, a boolean
// parameter) or removing tags (tags, a TEXT[] parameter); the trigger tidies
// them.
func denmaTagsSet(add, tags string) string {
	cur := `COALESCE(CASE WHEN jsonb_typeof(attribs->'tags') = 'array' THEN attribs->'tags' END, '[]'::JSONB)`
	return `(CASE WHEN jsonb_typeof(attribs) = 'object' THEN attribs ELSE '{}'::JSONB END) || jsonb_build_object('tags',
		CASE WHEN ` + add + `::BOOLEAN THEN ` + cur + ` || to_jsonb(` + tags + `::TEXT[]) ELSE ` + cur + ` - ` + tags + `::TEXT[] END)`
}

// denmaTagsByQuerySQL adds or removes tags ($5, $6) for the subscribers
// matching a query (models.Queries.ExecSubQueryTpl's %query%, with $1-$4).
var denmaTagsByQuerySQL = `WITH subs AS (%query%)
UPDATE subscribers SET attribs = ` + denmaTagsSet("$5", "$6") + `, updated_at = NOW()
WHERE id = ANY(SELECT id FROM subs)`

// DenmaTagSubscribers adds or removes tags for the subscribers picked (ids),
// or for every one matching a search (as the Subscribers page's other bulk
// actions).
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
		if err := a.hasSubPerm(user, req.SubscriberIDs); err != nil {
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
	if err := a.denmaSearch(user, &req.Search, &req.Query); err != nil {
		return err
	}
	listIDs := user.GetPermittedListIDs(req.ListIDs)
	if all, _ := user.GetPermittedLists(auth.PermTypeGet | auth.PermTypeManage); !all && len(listIDs) == 0 {
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

// sanitizeDenmaSQL is the core's sanitizeSQLExp for a condition built by
// denmaSearch (it only trims a trailing semicolon).
func sanitizeDenmaSQL(q string) string {
	return strings.TrimSuffix(strings.TrimSpace(q), ";")
}

// denmaCampaignTags tidies a campaign's tags and checks that it may have
// them: regular campaigns only, by users with access to all lists. Called
// by CreateCampaign and UpdateCampaign.
func (a *App) denmaCampaignTags(o *campReq, c echo.Context) error {
	o.SubscriberTags = denmaNormTags(o.SubscriberTags)
	if len(o.SubscriberTags) == 0 {
		return nil
	}
	if o.Type == models.CampaignTypeOptin {
		return echo.NewHTTPError(http.StatusBadRequest, "Opt-in campaigns go to lists only, not tags.")
	}
	if u := auth.GetUser(c); !denmaAllLists(&u) {
		return echo.NewHTTPError(http.StatusForbidden, "Sending to tags needs access to all lists.")
	}
	// (One the center doesn't have is a typo, or a tag since deleted.)
	return a.denmaUnknownTags(o.SubscriberTags)
}

// denmaSaveCampaignTags saves a campaign's tags (after listmonk saved the
// rest) into out, the campaign returned.
func (a *App) denmaSaveCampaignTags(id int, tags []string, out *models.Campaign) error {
	if tags == nil {
		tags = []string{}
	}
	if _, err := a.db.Exec(`UPDATE campaigns SET denma_tags = $2 WHERE id = $1`, id, pq.Array(tags)); err != nil {
		a.log.Printf("denma: error saving campaign %d's tags: %v", id, err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error saving the campaign's tags.")
	}
	out.SubscriberTags = tags
	return nil
}
