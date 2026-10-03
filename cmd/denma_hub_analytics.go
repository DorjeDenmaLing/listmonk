package main

// denma: the hub's Analytics page (views/denma-hub-analytics.html,
// assets/js/views/denma-hub-analytics.js): audience growth and mailbox
// providers for every center, together or one at a time. The figures are the
// centers' Analytics' (cmd/denma_stats.go, views/denma-analytics.js), added
// up over the period rather than per campaign, and computed here in a few
// queries per center instead of one request per figure. They're added up here
// too: the page gets each center's totals and one center's (or all centers')
// full figures, not every center's day by day.
//
// Mailbox providers count every campaign that started in the period and was
// sent with individual tracking (as on the hub's dashboard, cmd/denma_hub.go),
// not only a center's latest.

import (
	"fmt"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

// denmaProviders are the mailbox providers, by address domain (* is any
// ending), as in views/denma-analytics.js (PROVIDERS in denma-stats.js).
// Every other address is "other".
var denmaProviders = []struct {
	Key     string
	Domains []string
}{
	{"gmail", []string{"gmail.com", "googlemail.com"}},
	{"microsoft", []string{"outlook.*", "hotmail.*", "live.*", "msn.com", "windowslive.com"}},
	{"yahoo", []string{"yahoo.*", "ymail.com", "rocketmail.com", "aol.*", "aim.com"}},
	{"apple", []string{"icloud.com", "me.com", "mac.com"}},
}

// denmaProviderSQL is the provider of the address in col, matched as the
// search's domain: is (cmd/denma_search.go).
func denmaProviderSQL(col string) string {
	var b strings.Builder
	b.WriteString("CASE")
	for _, p := range denmaProviders {
		conds := make([]string, len(p.Domains))
		for i, d := range p.Domains {
			conds[i] = col + " ILIKE " + pq.QuoteLiteral("%@"+strings.ReplaceAll(denmaLikeEscape(d), "*", "%"))
		}
		fmt.Fprintf(&b, " WHEN %s THEN '%s'", strings.Join(conds, " OR "), p.Key)
	}
	b.WriteString(" ELSE 'other' END")
	return b.String()
}

// In the queries below, S. is the center's schema (denmaInSchema) and P(x)
// the provider of address x (denmaProviderSQL).

// Active subscribers now, by provider (the "active" figure).
const denmaActiveSQL = `SELECT P(s.email) AS provider, COUNT(*) AS n FROM S.subscribers s
	WHERE s.status = 'enabled' AND EXISTS (SELECT 1 FROM S.subscriber_lists sl JOIN S.lists l ON l.id = sl.list_id
		WHERE sl.subscriber_id = s.id AND sl.status <> 'unsubscribed' AND NOT (l.optin = 'double' AND sl.status = 'unconfirmed'))
	GROUP BY 1`

// Subscribers gained and lost from $1 to $2 (the "new", "unsub" and "removed"
// figures), by day in time zone $3 and provider. Each counts once: an
// unsubscribe on the day of the latest in the period, a removal on the day of
// the first bounce in the period.
const denmaGrowthSQL = `WITH bounced AS (
	SELECT subscriber_id, MIN(created_at) AS at FROM S.bounces
	WHERE subscriber_id IS NOT NULL AND created_at >= $1 AND created_at < $2 GROUP BY subscriber_id
), ev AS (
	SELECT 'new' AS kind, s.id, s.created_at AS at FROM S.subscribers s
	WHERE s.created_at >= $1 AND s.created_at < $2 AND EXISTS (SELECT 1 FROM S.subscriber_lists sl JOIN S.lists l ON l.id = sl.list_id
		WHERE sl.subscriber_id = s.id AND NOT (l.optin = 'double' AND sl.status = 'unconfirmed'))
	UNION ALL
	SELECT 'unsub', sl.subscriber_id, MAX(sl.updated_at) FROM S.subscriber_lists sl
	WHERE sl.status = 'unsubscribed' AND sl.updated_at >= $1 AND sl.updated_at < $2
		AND sl.subscriber_id NOT IN (SELECT subscriber_id FROM bounced)
	GROUP BY sl.subscriber_id
	UNION ALL
	SELECT 'removed', b.subscriber_id, b.at FROM bounced b JOIN S.subscribers s ON s.id = b.subscriber_id
	WHERE s.status = 'blocklisted' OR NOT EXISTS (SELECT 1 FROM S.subscriber_lists sl WHERE sl.subscriber_id = s.id AND sl.status <> 'unsubscribed')
)
SELECT ev.kind, TO_CHAR(ev.at AT TIME ZONE $3, 'YYYY-MM-DD') AS day, P(s.email) AS provider, COUNT(*) AS n
	FROM ev JOIN S.subscribers s ON s.id = ev.id GROUP BY 1, 2, 3`

// The tracked campaigns that started from $1 to $2 (not opt-in
// confirmations): who received them, opened and clicked (unique per
// campaign; the "received", "opens" and "clicks" figures), by provider, and
// how many campaigns.
const denmaReachSQL = `WITH camp AS (
	SELECT c.id, c.started_at, c.max_subscriber_id FROM S.campaigns c
	WHERE c.started_at >= $1 AND c.started_at < $2 AND c.sent > 0 AND c.type <> 'optin'
		AND NOT EXISTS (SELECT 1 FROM S.campaign_views v WHERE v.campaign_id = c.id AND v.subscriber_id IS NULL)
), got AS (
	SELECT DISTINCT camp.id, s.id AS subscriber_id, s.email FROM camp
	JOIN S.campaign_lists cl ON cl.campaign_id = camp.id
	JOIN S.subscriber_lists sl ON sl.list_id = cl.list_id AND sl.created_at <= camp.started_at
	JOIN S.lists l ON l.id = sl.list_id
	JOIN S.subscribers s ON s.id = sl.subscriber_id
	WHERE s.id <= camp.max_subscriber_id AND (s.status <> 'blocklisted' OR s.updated_at > camp.started_at)
		AND ((l.optin = 'double' AND sl.status = 'confirmed') OR (l.optin <> 'double' AND sl.status <> 'unsubscribed')
			OR (sl.status = 'unsubscribed' AND sl.updated_at > camp.started_at))
), seen AS (
	SELECT DISTINCT 'opens' AS kind, v.campaign_id, v.subscriber_id FROM S.campaign_views v JOIN camp ON camp.id = v.campaign_id
	WHERE v.subscriber_id IS NOT NULL
	UNION ALL
	SELECT DISTINCT 'clicks', k.campaign_id, k.subscriber_id FROM S.link_clicks k JOIN camp ON camp.id = k.campaign_id
	WHERE k.subscriber_id IS NOT NULL
)
SELECT 'received' AS kind, P(got.email) AS provider, COUNT(*) AS n FROM got GROUP BY 1, 2
UNION ALL
SELECT seen.kind, P(s.email), COUNT(*) FROM seen JOIN S.subscribers s ON s.id = seen.subscriber_id GROUP BY 1, 2
UNION ALL
SELECT 'campaigns', '', COUNT(*) FROM camp`

// denmaAnalytics is the Analytics figures of a center, or of several added up.
type denmaAnalytics struct {
	// Active subscribers now, by provider.
	Active map[string]int `json:"active"`
	// The period's new, unsub, removed, received, opens and clicks, by provider.
	Cur map[string]map[string]int `json:"cur"`
	// The previous period's new, unsub and removed.
	Prev map[string]int `json:"prev"`
	// The period's new, unsub and removed by day (YYYY-MM-DD).
	Days map[string]map[string]int `json:"days"`
	// Tracked campaigns in the period.
	Campaigns int `json:"campaigns"`
}

func newDenmaAnalytics() denmaAnalytics {
	return denmaAnalytics{Active: map[string]int{}, Cur: map[string]map[string]int{}, Prev: map[string]int{}, Days: map[string]map[string]int{}}
}

// add adds x's figures to f's.
func (f *denmaAnalytics) add(x denmaAnalytics) {
	for p, n := range x.Active {
		f.Active[p] += n
	}
	for k, m := range x.Cur {
		if f.Cur[k] == nil {
			f.Cur[k] = map[string]int{}
		}
		for p, n := range m {
			f.Cur[k][p] += n
		}
	}
	for k, n := range x.Prev {
		f.Prev[k] += n
	}
	for day, m := range x.Days {
		if f.Days[day] == nil {
			f.Days[day] = map[string]int{}
		}
		for k, n := range m {
			f.Days[day][k] += n
		}
	}
	f.Campaigns += x.Campaigns
}

func denmaSum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// denmaHubAnalytics is one center on the hub's Analytics page: its totals,
// for the By center table.
type denmaHubAnalytics struct {
	Slug   string `json:"slug"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error"`
	// Whether its figures were counted (not if its queries failed).
	Counted bool `json:"counted"`

	Active    int `json:"active"`
	New       int `json:"new"`
	Unsub     int `json:"unsub"`
	Removed   int `json:"removed"`
	Received  int `json:"received"`
	Opens     int `json:"opens"`
	Clicks    int `json:"clicks"`
	Campaigns int `json:"campaigns"`
}

// ViewDenmaHubAnalytics renders the hub's Analytics page (superadmins).
func (a *App) ViewDenmaHubAnalytics(c echo.Context) error {
	if _, err := a.hub(c); err != nil {
		return err
	}
	return c.Render(http.StatusOK, "admin-denma-hub-analytics", newAdminView(c, "Analytics", "", "denma.analytics"))
}

// DenmaHubAnalytics returns the figures for a period (from, to) and the
// previous one (prev_from, prev_to), RFC 3339 times; days are in the time
// zone tz (an IANA name; UTC if Postgres doesn't know it). Every center's
// totals ("centers"), and the full figures ("total") of one (center, a slug)
// or of all of them added up.
func (a *App) DenmaHubAnalytics(c echo.Context) error {
	d, err := a.hub(c)
	if err != nil {
		return err
	}
	t, err := denmaPeriods(c)
	if err != nil {
		return err
	}

	db := d.current().db
	tz := c.QueryParam("tz")
	var known bool
	if tz != "" {
		if err := db.Get(&known, `SELECT EXISTS (SELECT 1 FROM pg_timezone_names WHERE name = $1)`, tz); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
	}
	if !known {
		tz = "UTC"
	}

	reg, err := d.registered()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	var (
		providers = strings.NewReplacer("P(s.email)", denmaProviderSQL("s.email"), "P(got.email)", denmaProviderSQL("got.email"))
		inSchema  = func(q, schema string) string { return denmaInSchema(providers.Replace(q), schema) }
		key       = fmt.Sprintf("analytics|%v|%s", t, tz)
		out       = make([]denmaHubAnalytics, len(reg))
		figs      = make([]denmaAnalytics, len(reg))
	)
	denmaEach(len(reg), func(i int) {
		x := reg[i]
		r := &out[i]
		*r = denmaHubAnalytics{Slug: x.Slug, Name: x.Name, Status: x.Status, Error: x.Error}

		// Cached as a whole: not to be changed after.
		f, err := denmaCached(x.Schema+"|"+key, func() (denmaAnalytics, error) {
			type row struct {
				Kind     string `db:"kind"`
				Day      string `db:"day"`
				Provider string `db:"provider"`
				N        int    `db:"n"`
			}
			var active, cur, prev, reach []row
			err := db.Select(&active, inSchema(denmaActiveSQL, x.Schema))
			if err == nil {
				err = db.Select(&cur, inSchema(denmaGrowthSQL, x.Schema), t[0], t[1], tz)
			}
			if err == nil {
				err = db.Select(&prev, inSchema(denmaGrowthSQL, x.Schema), t[2], t[3], tz)
			}
			if err == nil {
				err = db.Select(&reach, inSchema(denmaReachSQL, x.Schema), t[0], t[1])
			}
			if err != nil {
				return denmaAnalytics{}, err
			}

			f := newDenmaAnalytics()
			add := func(k, p string, n int) {
				if f.Cur[k] == nil {
					f.Cur[k] = map[string]int{}
				}
				f.Cur[k][p] += n
			}
			for _, v := range active {
				f.Active[v.Provider] += v.N
			}
			for _, v := range cur {
				add(v.Kind, v.Provider, v.N)
				if f.Days[v.Day] == nil {
					f.Days[v.Day] = map[string]int{}
				}
				f.Days[v.Day][v.Kind] += v.N
			}
			for _, v := range prev {
				f.Prev[v.Kind] += v.N
			}
			for _, v := range reach {
				if v.Kind == "campaigns" {
					f.Campaigns = v.N
				} else {
					add(v.Kind, v.Provider, v.N)
				}
			}
			return f, nil
		})
		if err != nil {
			if r.Error == "" {
				r.Error = err.Error()
			}
			return
		}

		figs[i] = f
		r.Counted = true
		r.Active, r.Campaigns = denmaSum(f.Active), f.Campaigns
		r.New, r.Unsub, r.Removed = denmaSum(f.Cur["new"]), denmaSum(f.Cur["unsub"]), denmaSum(f.Cur["removed"])
		r.Received, r.Opens, r.Clicks = denmaSum(f.Cur["received"]), denmaSum(f.Cur["opens"]), denmaSum(f.Cur["clicks"])
	})

	// The full figures of the center asked for, or of all of them.
	center := c.QueryParam("center")
	if !slices.ContainsFunc(reg, func(x denmaCenterRef) bool { return x.Slug == center }) {
		center = ""
	}
	total := newDenmaAnalytics()
	for i, r := range out {
		if r.Counted && (center == "" || r.Slug == center) {
			total.add(figs[i])
		}
	}

	return c.JSON(http.StatusOK, okResp{map[string]any{
		"centers": out,
		"center":  center,
		"total":   total,
	}})
}

// denmaFiguresTTL is how long the hub keeps each center's figures for a
// period: superadmins move between its pages, pick centers and reload, and
// each fresh answer is a few queries in each of hundreds of centers.
const denmaFiguresTTL = time.Minute

var denmaFigures = struct {
	sync.Mutex
	m      map[string]denmaFigure
	pruned time.Time
}{m: map[string]denmaFigure{}}

type denmaFigure struct {
	at time.Time
	v  any
}

// denmaCached returns what's kept under key if it's fresh, or else f's
// result, which it keeps unless f failed.
func denmaCached[T any](key string, f func() (T, error)) (T, error) {
	now := time.Now()
	c := &denmaFigures
	c.Lock()
	e, ok := c.m[key]
	c.Unlock()
	if ok && now.Sub(e.at) < denmaFiguresTTL {
		return e.v.(T), nil
	}

	v, err := f()
	if err != nil {
		return v, err
	}
	c.Lock()
	defer c.Unlock()
	if now.Sub(c.pruned) > denmaFiguresTTL {
		for k, e := range c.m {
			if now.Sub(e.at) >= denmaFiguresTTL {
				delete(c.m, k)
			}
		}
		c.pruned = now
	}
	c.m[key] = denmaFigure{at: now, v: v}
	return v, nil
}

// denmaCenterRef is a registered center as the hub lists it.
type denmaCenterRef struct {
	Slug     string `db:"slug"`
	Name     string `db:"name"`
	Schema   string `db:"schema_name"`
	Status   string `db:"status"`
	Error    string `db:"-"` // why it didn't load
	Loaded   bool   `db:"-"`
	Starting bool   `db:"-"` // still loading after a start
	Path     string `db:"-"` // its address, e.g. /c/ddl/
}

// registered returns every registered center, by name: its name is the
// running center's site name.
func (d *denmaCenters) registered() ([]denmaCenterRef, error) {
	var reg []denmaCenterRef
	if err := d.base.db.Select(&reg, `SELECT slug, name, schema_name, status FROM denma.centers ORDER BY name`); err != nil {
		return nil, err
	}
	root := d.current().urlCfg.RootPath

	d.mu.RLock()
	defer d.mu.RUnlock()
	for i := range reg {
		r := &reg[i]
		r.Error = d.failed[r.Slug]
		r.Starting = d.starting[r.Slug]
		r.Path = path.Join(root, denmaCenterPath, r.Slug) + "/"
		if ctr := d.bySlug[r.Slug]; ctr != nil {
			r.Loaded = true
			r.Name = ctr.app.ko.String("app.site_name")
		}
	}
	return reg, nil
}

// denmaEach runs f(0) to f(n-1), a few at a time, and waits for them.
func denmaEach(n int, f func(i int)) {
	done := make(chan struct{})
	slots := make(chan struct{}, 6) // queries at a time
	for i := 0; i < n; i++ {
		go func(i int) {
			slots <- struct{}{}
			defer func() { <-slots; done <- struct{}{} }()
			f(i)
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}
}
