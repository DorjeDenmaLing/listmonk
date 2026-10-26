package main

// denma: other names for a center's tags (Subscribers -> Tags, Other
// names), for tags another system names its own way: Retreat Guru's category
// slugs (on-land, self-guided-and-group-retreats) for DDL's tags (on land,
// self-guided and solitary retreats). A tag given by another name is the tag
// itself: the trigger maps a subscriber's (denma_subscriber_tags,
// cmd/denma_features.go), so imports, the admin API and sign-ups all get the
// tag; sign-up webhooks map a request's before checking it against their
// request tags; an import's preview shows the tag. Campaigns, automations
// and webhooks' settings take the tags themselves.

import (
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

const denmaTagAliasesMax = 20 // other names a tag can have

// denmaTagAliasesSQL is the other names (features 47), each another name
// for one tag only, never a tag's own.
const denmaTagAliasesSQL = `
CREATE TABLE IF NOT EXISTS denma_tag_aliases (
    alias      TEXT PRIMARY KEY,
    tag        TEXT NOT NULL REFERENCES denma_tags (tag) ON DELETE CASCADE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS denma_tag_aliases_tag ON denma_tag_aliases (tag);
`

// denmaResolveTags tidies tags and gives those named by another name as the
// tag. A tag's own name wins (should one be added while it's another's).
func (a *App) denmaResolveTags(tags []string) ([]string, error) {
	tags = denmaNormTags(tags)
	if len(tags) == 0 {
		return tags, nil
	}
	out := []string{}
	if err := a.db.Select(&out, `SELECT DISTINCT CASE WHEN EXISTS (SELECT 1 FROM denma_tags d WHERE d.tag = t) THEN t
		ELSE COALESCE((SELECT a.tag FROM denma_tag_aliases a WHERE a.alias = t), t) END
		FROM unnest($1::TEXT[]) t`, pq.Array(tags)); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// denmaLockTags makes changes to the center's tags and their other names wait
// for each other, so that a name can't become both, and two changes can't
// deadlock.
func denmaLockTags(tx *sqlx.Tx) error {
	_, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext(current_schema() || '/denma_tags'))`)
	return err
}

// denmaAliasOf is the tag that name is another name for, or "".
func (a *App) denmaAliasOf(q interface {
	Select(any, string, ...any) error
}, name string) (string, error) {
	var tags []string
	err := q.Select(&tags, `SELECT tag FROM denma_tag_aliases WHERE alias = $1`, name)
	if len(tags) == 0 {
		return "", err
	}
	return tags[0], err
}

// DenmaSetTagAliases sets a tag's other names (all of them).
func (a *App) DenmaSetTagAliases(c echo.Context) error {
	var req struct {
		Tag     string   `json:"tag"`
		Aliases []string `json:"aliases"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	tag := denmaNormTags([]string{req.Tag})
	if len(tag) != 1 {
		return echo.NewHTTPError(http.StatusBadRequest, "Name one tag.")
	}
	aliases := slices.DeleteFunc(denmaNormTags(req.Aliases), func(s string) bool { return s == tag[0] })
	if len(aliases) > denmaTagAliasesMax {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("A tag can have at most %d other names.", denmaTagAliasesMax))
	}

	tx, err := a.db.Beginx()
	if err != nil {
		a.log.Printf("denma: error setting the tag %q's other names: %v", tag[0], err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error saving the other names.")
	}
	defer tx.Rollback()

	var found, tags []string
	var taken []struct {
		Alias string `db:"alias"`
		Tag   string `db:"tag"`
	}
	err = denmaLockTags(tx)
	if err == nil {
		err = tx.Select(&found, `SELECT tag FROM denma_tags WHERE tag = $1`, tag[0])
	}
	if err == nil {
		err = tx.Select(&tags, `SELECT tag FROM denma_tags WHERE tag = ANY($1) ORDER BY tag`, pq.Array(aliases))
	}
	if err == nil {
		err = tx.Select(&taken, `SELECT alias, tag FROM denma_tag_aliases WHERE alias = ANY($1) AND tag <> $2 ORDER BY alias`,
			pq.Array(aliases), tag[0])
	}
	if err != nil {
		a.log.Printf("denma: error setting the tag %q's other names: %v", tag[0], err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error saving the other names.")
	}
	switch {
	case len(found) == 0:
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("There's no tag %s.", tag[0]))
	case len(tags) > 0:
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("%s: already a tag. A tag can't be another's other name.", strings.Join(tags, ", ")))
	case len(taken) > 0:
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("%s is already another name for %s.", taken[0].Alias, taken[0].Tag))
	}

	_, err = tx.Exec(`DELETE FROM denma_tag_aliases WHERE tag = $1`, tag[0])
	if err == nil && len(aliases) > 0 {
		_, err = tx.Exec(`INSERT INTO denma_tag_aliases (alias, tag) SELECT UNNEST($1::TEXT[]), $2`, pq.Array(aliases), tag[0])
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		a.log.Printf("denma: error setting the tag %q's other names: %v", tag[0], err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error saving the other names.")
	}
	return c.JSON(http.StatusOK, okResp{map[string]any{"tag": tag[0], "aliases": aliases}})
}
