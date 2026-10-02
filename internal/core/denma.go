package core

// denma: the campaigns query with a search condition (cmd/denma_search.go).

import (
	"net/http"
	"strings"

	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

// denmaCampaignsWhere is where the condition goes in query-campaigns
// (queries/campaigns.sql).
const denmaCampaignsWhere = "WHERE ($1 = 0 OR id = $1)"

// DenmaQueryCampaigns is QueryCampaigns, with cond (an SQL condition on
// campaigns c, built by the search language from known fields and quoted
// values) in place of the plain search.
func (c *Core) DenmaQueryCampaigns(cond string, statuses, tags []string, typ, orderBy, order string, getAll bool, permittedLists []int, offset, limit int) (models.Campaigns, int, error) {
	if !strings.Contains(c.q.QueryCampaigns, denmaCampaignsWhere) {
		c.log.Printf("denma: query-campaigns has changed; searching campaigns needs updating (internal/core/denma.go)")
		return nil, 0, echo.NewHTTPError(http.StatusInternalServerError, "Searching campaigns is broken; see the log.")
	}
	_, stmt := makeSearchQuery("", orderBy, order, c.q.QueryCampaigns, campQuerySortFields)
	stmt = strings.Replace(stmt, denmaCampaignsWhere, denmaCampaignsWhere+" AND "+cond, 1)

	if statuses == nil {
		statuses = []string{}
	}
	if tags == nil {
		tags = []string{}
	}

	var out models.Campaigns
	if err := c.db.Select(&out, stmt, 0, pq.StringArray(statuses), pq.StringArray(tags), "", typ, getAll, pq.Array(permittedLists), offset, limit); err != nil {
		c.log.Printf("error fetching campaigns: %v", err)
		return nil, 0, echo.NewHTTPError(http.StatusInternalServerError,
			c.i18n.Ts("globals.messages.errorFetching", "name", "{globals.terms.campaign}", "error", pqErrMsg(err)))
	}
	for i := range out {
		if out[i].Tags == nil {
			out[i].Tags = []string{}
		}
	}
	if err := out.LoadStats(c.q.GetCampaignStats); err != nil {
		c.log.Printf("error fetching campaign stats: %v", err)
		return nil, 0, echo.NewHTTPError(http.StatusInternalServerError,
			c.i18n.Ts("globals.messages.errorFetching", "name", "{globals.terms.campaigns}", "error", pqErrMsg(err)))
	}

	total := 0
	if len(out) > 0 {
		total = out[0].Total
	}
	return out, total, nil
}
