package main

import (
	"strings"

	"github.com/knadh/listmonk/models"
	"github.com/lib/pq"
)

// denma: the Campaigns list shows how many people opened and clicked each
// campaign, as Campaign reports and the dashboards count them, instead of
// listmonk's totals of opens and clicks (so someone who opened it three
// times counted three). A campaign sent without individual tracking
// (denmaTrackedCampSQL) shows its totals, as Analytics does, as people
// aren't known.

// denmaCampaignPeople fills in camps' people who opened and clicked.
func (a *App) denmaCampaignPeople(camps []models.Campaign) {
	if len(camps) == 0 {
		return
	}
	ids := make([]int, len(camps))
	for i, c := range camps {
		ids[i] = c.ID
	}
	var rows []struct {
		ID      int  `db:"id"`
		Opened  int  `db:"opened"`
		Clicked int  `db:"clicked"`
		Tracked bool `db:"tracked"`
	}
	if err := a.db.Select(&rows, `SELECT c.id,
		(SELECT COUNT(DISTINCT v.subscriber_id) FROM campaign_views v WHERE v.campaign_id = c.id) AS opened,
		(SELECT COUNT(DISTINCT k.subscriber_id) FROM link_clicks k WHERE k.campaign_id = c.id) AS clicked,
		`+strings.ReplaceAll(denmaTrackedCampSQL, "S.", "")+` AS tracked
		FROM campaigns c WHERE c.id = ANY($1)`, pq.Array(ids)); err != nil {
		a.log.Printf("denma: error counting campaigns' people: %v", err)
		return
	}
	byID := map[int]int{}
	for i, c := range camps {
		byID[c.ID] = i
	}
	for _, r := range rows {
		c := &camps[byID[r.ID]]
		c.DenmaPeople = &models.DenmaCampaignPeople{Opened: r.Opened, Clicked: r.Clicked, Tracked: r.Tracked}
	}
}
