package main

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
)

const (
	denmaStuckAfter    = 30 * time.Minute
	denmaStuckCacheFor = time.Minute
)

// denmaStuck remembers what /health/sending last found.
var denmaStuck = struct {
	sync.Mutex
	at    time.Time
	count int
	moved map[string]denmaCampaignMoved
}{moved: map[string]denmaCampaignMoved{}}

// denmaCampaignMoved is a running campaign's sent count, and when it last changed.
type denmaCampaignMoved struct {
	sent int
	at   time.Time
}

// DenmaHealthSending is the hub's /health/sending: a count of stuck things in all centers, once a minute.
func (a *App) DenmaHealthSending(c echo.Context) error {
	d := denmaHub
	if d == nil || a.ko.String("denma.center") != "" {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	denmaStuck.Lock()
	defer denmaStuck.Unlock()
	if time.Since(denmaStuck.at) >= denmaStuckCacheFor {
		denmaStuck.count, denmaStuck.moved = d.countStuck(denmaStuck.moved)
		denmaStuck.at = time.Now()
	}
	return c.JSON(http.StatusOK, okResp{map[string]int{"stuck": denmaStuck.count}})
}

// countStuck counts stuck campaigns and queued API e-mails over the running centers.
func (d *denmaCenters) countStuck(was map[string]denmaCampaignMoved) (int, map[string]denmaCampaignMoved) {
	d.mu.RLock()
	centers := make(map[string]*denmaCenter, len(d.bySlug))
	for slug, c := range d.bySlug {
		centers[slug] = c
	}
	d.mu.RUnlock()

	now := time.Now()
	atLimit := !denmaDaily.Status().FreesAt.IsZero()
	moved := make(map[string]denmaCampaignMoved)
	stuck := 0
	for slug, c := range centers {
		a := c.app
		var row struct {
			Overdue int `db:"overdue"`
			Paused  int `db:"paused"`
			Queued  int `db:"queued"`
		}
		if err := a.db.Get(&row, `SELECT
			(SELECT COUNT(*) FROM campaigns WHERE status = 'scheduled' AND send_at < NOW() - $1 * INTERVAL '1 second') AS overdue,
			(SELECT COUNT(DISTINCT c.id) FROM campaigns c JOIN denma_send_failures f ON f.campaign_id = c.id
				WHERE c.status = 'paused' AND f.failed_at > NOW() - INTERVAL '1 day' AND f.failed_at >= c.updated_at - INTERVAL '10 minutes') AS paused,
			(SELECT COUNT(*) FROM denma_api_outbox WHERE created_at < NOW() - $1 * INTERVAL '1 second') AS queued`,
			int(denmaStuckAfter.Seconds())); err != nil {
			lo.Printf("denma: error checking center %s's sending: %v", slug, err)
			continue
		}
		stuck += row.Overdue + row.Paused + row.Queued

		var running []struct {
			ID   int `db:"id"`
			Sent int `db:"sent"`
		}
		if err := a.db.Select(&running, `SELECT id, sent FROM campaigns WHERE status = 'running' AND sent < to_send`); err != nil {
			lo.Printf("denma: error checking center %s's campaigns: %v", slug, err)
			continue
		}
		open := a.denmaHours().open(now)
		centerAtLimit := false
		if cc := denmaCenterCounter(a.ko); cc != nil {
			centerAtLimit = !cc.Status().FreesAt.IsZero()
		}
		for _, r := range running {
			key := fmt.Sprintf("%s/%d", slug, r.ID)
			m, ok := was[key]
			if !ok || m.sent != r.Sent || !open || atLimit || centerAtLimit {
				m = denmaCampaignMoved{sent: r.Sent, at: now}
			}
			moved[key] = m
			if now.Sub(m.at) >= denmaStuckAfter {
				stuck++
			}
		}
	}
	return stuck, moved
}
