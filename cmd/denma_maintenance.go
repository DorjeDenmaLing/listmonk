package main

// denma: Maintenance (Settings -> Maintenance) for every center. The page is
// the hub's (centers don't show settings), but the data it cleans up
// (orphan subscribers, unconfirmed subscriptions, old
// analytics) is the centers'; the hub has none. So in the hub its API acts on
// one center, chosen on the page (views/maintenance.html), or on every
// running center, several at a time. A center's own listmonk handler does the
// work for one center; for all, the counts are added up, and if any center
// fails the response is an error that names it (the others are done).
// Analytics exported from all centers get a first "center" column.
//
// The handlers replace listmonk's at the same paths (registered after them:
// Echo keeps the last); without centers, or in a center, they are listmonk's.

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
)

func initDenmaMaintenanceHandlers(g *echo.Group, a *App) {
	pm := a.auth.Perm
	g.DELETE("/api/maintenance/subscribers/:type", pm(a.DenmaGCSubscribers, "settings:maintain"))
	g.DELETE("/api/maintenance/analytics/:type", pm(a.DenmaGCCampaignAnalytics, "settings:maintain"))
	g.GET("/api/maintenance/analytics/:type/export", pm(a.DenmaExportCampaignAnalytics, "settings:maintain"))
	g.DELETE("/api/maintenance/subscriptions/unconfirmed", pm(a.DenmaGCSubscriptions, "settings:maintain"))
}

// denmaMaintainIn returns the centers a maintenance request is for: the
// center= one, or every running one (nil, nil without centers or in one).
func (a *App) denmaMaintainIn(c echo.Context) ([]*denmaCenter, error) {
	if denmaHub == nil || a.ko.String("denma.center") != "" {
		return nil, nil
	}
	if slug := c.QueryParam("center"); slug != "" {
		ctr := denmaHub.get(slug)
		if ctr == nil {
			return nil, echo.NewHTTPError(http.StatusNotFound, "center not found or not running")
		}
		return []*denmaCenter{ctr}, nil
	}
	ctrs := denmaHub.loaded()
	sort.Slice(ctrs, func(i, j int) bool { return ctrs[i].Slug < ctrs[j].Slug })
	return ctrs, nil
}

// denmaEachCenter runs fn in every center, four at a time, and returns the
// sum of its counts, or an error naming the centers it failed in.
func denmaEachCenter(ctrs []*denmaCenter, fn func(*App) (int, error)) (int, error) {
	var (
		mu     sync.Mutex
		total  int
		failed []string
		wg     sync.WaitGroup
		jobs   = make(chan *denmaCenter)
	)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctr := range jobs {
				n, err := fn(ctr.app)
				mu.Lock()
				if err != nil {
					failed = append(failed, fmt.Sprintf("%s (%v)", ctr.Slug, err))
				} else {
					total += n
				}
				mu.Unlock()
			}
		}()
	}
	for _, ctr := range ctrs {
		jobs <- ctr
	}
	close(jobs)
	wg.Wait()
	if len(failed) > 0 {
		sort.Strings(failed)
		return total, echo.NewHTTPError(http.StatusInternalServerError,
			fmt.Sprintf("Done in %d of %d centers (%d removed); failed in %s.", len(ctrs)-len(failed), len(ctrs), total, strings.Join(failed, ", ")))
	}
	return total, nil
}

// DenmaGCSubscribers deletes orphan subscribers. Not blocklisted ones, which
// listmonk's page offers too: every unsubscribe blocklists
// (cmd/denma_unsubscribe.go), so deleting them would forget that they opted
// out, and an import or an API call could add them again.
func (a *App) DenmaGCSubscribers(c echo.Context) error {
	if denmaHub != nil && c.Param("type") == "blocklisted" {
		return echo.NewHTTPError(http.StatusBadRequest,
			"Blocklisted subscribers can't be deleted: they include everyone who unsubscribed, and deleting them would forget that they opted out.")
	}
	ctrs, err := a.denmaMaintainIn(c)
	switch {
	case err != nil:
		return err
	case ctrs == nil:
		return a.GCSubscribers(c)
	case len(ctrs) == 1:
		return ctrs[0].app.GCSubscribers(c)
	}
	if c.Param("type") != "orphan" {
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.T("globals.messages.invalidData"))
	}
	n, err := denmaEachCenter(ctrs, func(app *App) (int, error) {
		return app.core.DeleteOrphanSubscribers()
	})
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, okResp{struct {
		Count int `json:"count"`
	}{n}})
}

// DenmaGCSubscriptions deletes unconfirmed subscriptions older than a date.
func (a *App) DenmaGCSubscriptions(c echo.Context) error {
	ctrs, err := a.denmaMaintainIn(c)
	switch {
	case err != nil:
		return err
	case ctrs == nil:
		return a.GCSubscriptions(c)
	case len(ctrs) == 1:
		return ctrs[0].app.GCSubscriptions(c)
	}
	t, err := time.Parse(time.RFC3339, c.FormValue("before_date"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.T("globals.messages.invalidData"))
	}
	n, err := denmaEachCenter(ctrs, func(app *App) (int, error) {
		return app.core.DeleteUnconfirmedSubscriptions(t)
	})
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, okResp{struct {
		Count int `json:"count"`
	}{n}})
}

// DenmaGCCampaignAnalytics deletes campaign views and/or clicks older than a
// date.
func (a *App) DenmaGCCampaignAnalytics(c echo.Context) error {
	ctrs, err := a.denmaMaintainIn(c)
	switch {
	case err != nil:
		return err
	case ctrs == nil:
		return a.GCCampaignAnalytics(c)
	case len(ctrs) == 1:
		return ctrs[0].app.GCCampaignAnalytics(c)
	}
	t, err := time.Parse(time.RFC3339, c.FormValue("before_date"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.T("globals.messages.invalidData"))
	}
	typ := c.Param("type")
	if typ != "all" && typ != "views" && typ != "clicks" {
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.T("globals.messages.invalidData"))
	}
	if _, err := denmaEachCenter(ctrs, func(app *App) (int, error) {
		if typ != "clicks" {
			if err := app.core.DeleteCampaignViews(t); err != nil {
				return 0, err
			}
		}
		if typ != "views" {
			return 0, app.core.DeleteCampaignLinkClicks(t)
		}
		return 0, nil
	}); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, okResp{true})
}

// DenmaExportCampaignAnalytics streams campaign views or clicks since a date
// as CSV; from all centers, with a first "center" column.
func (a *App) DenmaExportCampaignAnalytics(c echo.Context) error {
	ctrs, err := a.denmaMaintainIn(c)
	switch {
	case err != nil:
		return err
	case ctrs == nil:
		return a.ExportCampaignAnalytics(c)
	case len(ctrs) == 1:
		return ctrs[0].app.ExportCampaignAnalytics(c)
	}
	since, err := time.Parse(time.RFC3339, c.QueryParam("since"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.T("globals.messages.invalidData"))
	}
	typ := c.Param("type")
	if typ != "views" && typ != "clicks" {
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.T("globals.messages.invalidData"))
	}

	hdr := c.Response().Header()
	hdr.Set(echo.HeaderContentType, "text/csv")
	hdr.Set(echo.HeaderContentDisposition, "attachment; filename=campaign_"+typ+"_all_centers.csv")
	hdr.Set("Cache-Control", "no-cache")
	wr := csv.NewWriter(c.Response())

	head := []string{"center", "campaign_id", "campaign_uuid", "campaign_name", "subscriber_id", "subscriber_uuid", "email", "subscriber_name"}
	if typ == "clicks" {
		head = append(head, "url")
	}
	_ = wr.Write(append(head, "created_at"))

	for _, ctr := range ctrs {
		app := ctr.app
		if typ == "views" {
			next := app.core.ExportCampaignViews(since, app.cfg.DBBatchSize)
			for {
				rows, err := next()
				if err != nil {
					a.log.Printf("denma: error exporting %s's views: %v", ctr.Slug, err)
					return nil // the CSV has started; it ends here
				}
				if len(rows) == 0 {
					break
				}
				for _, r := range rows {
					_ = wr.Write([]string{ctr.Slug, strconv.Itoa(r.CampaignID), r.CampaignUUID, r.CampaignName,
						strconv.Itoa(r.SubscriberID), r.SubscriberUUID, r.Email, r.SubscriberName, r.CreatedAt.Format(time.RFC3339)})
				}
				wr.Flush()
			}
			continue
		}
		next := app.core.ExportCampaignLinkClicks(since, app.cfg.DBBatchSize)
		for {
			rows, err := next()
			if err != nil {
				a.log.Printf("denma: error exporting %s's clicks: %v", ctr.Slug, err)
				return nil
			}
			if len(rows) == 0 {
				break
			}
			for _, r := range rows {
				_ = wr.Write([]string{ctr.Slug, strconv.Itoa(r.CampaignID), r.CampaignUUID, r.CampaignName,
					strconv.Itoa(r.SubscriberID), r.SubscriberUUID, r.Email, r.SubscriberName, r.URL, r.CreatedAt.Format(time.RFC3339)})
			}
			wr.Flush()
		}
	}
	wr.Flush()
	return nil
}

// denmaPickCenters are the running centers the hub's Maintenance and Logs
// pages offer (none elsewhere).
func (a *App) denmaPickCenters() []denmaNamedSlug {
	if denmaHub == nil || a.ko.String("denma.center") != "" {
		return nil
	}
	out := []denmaNamedSlug{}
	for _, ctr := range denmaHub.loaded() {
		out = append(out, denmaNamedSlug{Slug: ctr.Slug, Name: ctr.Name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
