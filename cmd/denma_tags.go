package main

// denma: subscriber tags, a center's own labels for its subscribers
// (volunteer, donor, retreat-2026), which subscribers never see. They're
// kept in the subscriber's attributes (attribs.tags, a list), so they go
// wherever attributes go: the API, imports, exports, templates. A trigger
// keeps them lowercase, trimmed and without duplicates however they're set
// (denmaFeaturesSQL, cmd/denma_features.go).
//
// Subscribers -> Tags (views/denma-tags.html) lists a center's tags with how
// many have each, and renames or deletes them. Tags are added or removed on
// the subscriber's page, for the subscribers picked (or every one matching a
// search) on the Subscribers page, or by an import; search finds them with
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

// denmaHubColumns adds the campaign tags column to the hub's schema, which
// has no features installed but runs listmonk's campaign queries (which read
// it). Called on every start (denmaPrepareHub).
func denmaHubColumns(db *sqlx.DB) {
	if _, err := db.Exec(`ALTER TABLE campaigns ADD COLUMN IF NOT EXISTS denma_tags TEXT[] NOT NULL DEFAULT '{}'`); err != nil {
		lo.Fatalf("denma: error adding the campaign tags column to the hub: %v", err)
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
	err := a.db.Select(&out, `SELECT t AS tag, COUNT(*) AS subscribers, COUNT(*) FILTER (WHERE `+denmaTagActiveSQL+`) AS active
		FROM subscribers, jsonb_array_elements_text(CASE WHEN jsonb_typeof(attribs->'tags') = 'array' THEN attribs->'tags' ELSE '[]'::JSONB END) t
		GROUP BY t ORDER BY t`)
	return out, err
}

// denmaTagNames returns the center's tags' names, for the pickers.
func (a *App) denmaTagNames() []string {
	out := []string{}
	if err := a.db.Select(&out, `SELECT DISTINCT t FROM subscribers,
		jsonb_array_elements_text(CASE WHEN jsonb_typeof(attribs->'tags') = 'array' THEN attribs->'tags' ELSE '[]'::JSONB END) t ORDER BY t`); err != nil {
		a.log.Printf("denma: error reading the tags: %v", err)
	}
	return out
}

func initDenmaTagHandlers(g *echo.Group, a *App) {
	g.GET(path.Join(uriAdmin, "/subscribers/tags"), a.ViewDenmaTags)
}

func initDenmaTagAPIHandlers(g *echo.Group, a *App) {
	pm := a.auth.Perm
	g.GET("/api/denma/tags", pm(a.DenmaGetTags, "subscribers:get_all"))
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

// DenmaChangeTag renames a tag (merging it into another of that name), or
// deletes it from every subscriber.
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
	var to []string
	switch req.Action {
	case "rename":
		if to = denmaNormTags([]string{req.To}); len(to) != 1 {
			return echo.NewHTTPError(http.StatusBadRequest, "Enter the tag's new name (one tag, without commas).")
		}
	case "delete":
	default:
		return echo.NewHTTPError(http.StatusBadRequest, "action must be rename or delete")
	}
	// Removing the old one and adding the new; the trigger tidies the result.
	res, err := a.db.Exec(`UPDATE subscribers SET attribs = attribs || jsonb_build_object('tags', (attribs->'tags') - $1::TEXT || to_jsonb($2::TEXT[])),
		updated_at = NOW() WHERE jsonb_typeof(attribs->'tags') = 'array' AND attribs->'tags' ? $1::TEXT`, from[0], pq.Array(to))
	if err != nil {
		a.log.Printf("denma: error changing the tag %q: %v", from[0], err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error changing the tag.")
	}
	n, _ := res.RowsAffected()
	// Campaigns not yet sent follow the rename (or lose the tag).
	if _, err := a.db.Exec(`UPDATE campaigns SET denma_tags = ARRAY(SELECT DISTINCT x FROM unnest(array_remove(denma_tags, $1::TEXT) || $2::TEXT[]) x ORDER BY x)
		WHERE $1::TEXT = ANY(denma_tags) AND status IN ('draft', 'scheduled', 'paused')`, from[0], pq.Array(to)); err != nil {
		a.log.Printf("denma: error changing the tag %q on campaigns: %v", from[0], err)
	}
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
	// A tag no one has is a typo (or a tag since deleted): it would reach no one.
	var missing []string
	if err := a.db.Select(&missing, `SELECT t FROM unnest($1::TEXT[]) t WHERE NOT EXISTS (
		SELECT 1 FROM subscribers WHERE jsonb_typeof(attribs->'tags') = 'array' AND attribs->'tags' ? t)`, pq.Array(o.SubscriberTags)); err != nil {
		a.log.Printf("denma: error checking campaign tags: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error checking the tags.")
	}
	if len(missing) > 0 {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("No subscriber has the tag %s. Check the spelling (pick it from the suggestions), or tag subscribers first.",
			strings.Join(missing, ", ")))
	}
	return nil
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
