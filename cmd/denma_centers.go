package main

// denma: many centers in one listmonk process: the hub at /, each center at /c/<slug>/.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"path"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gdgvda/cron"
	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/internal/manager"
	"github.com/knadh/listmonk/internal/messenger/email"
	"github.com/knadh/listmonk/internal/subimporter"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

const denmaCenterPath = "/c/"

var (
	reDenmaSlug   = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	reDenmaSchema = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
)

// denmaHub is the running centers, when denma.multi_center is on.
var denmaHub *denmaCenters

// denmaCenterOwnSettings are a center's own settings; every other setting is the hub's.
var denmaCenterOwnSettings = []string{
	"app.root_url", "upload.filesystem.upload_path", "upload.s3.bucket_path",
	"migrations", // the schema's own version
	// And those on the center's Config page (cmd/denma_center.go).
	"app.site_name", "app.logo_url", "app.favicon_url", "app.lang",
	"app.from_email", "app.notify_emails",
	"denma.default_design",                 // cmd/denma_design.go
	"denma.daily_cap",                      // cmd/denma_daily.go
	"denma.postal_address",                 // cmd/denma_address.go
	"denma.website", "denma.contact_email", // cmd/denma_footer.go
	"denma.unsubscribe_everywhere", "denma.plain_text_auto", "denma.utm_domains", // cmd/denma_features.go
	"denma.visual_template",               // cmd/denma_features.go
	"denma.reply_to",                      // cmd/denma_reply_to.go
	"denma.timezone", "denma.send_window", // cmd/denma_sendwindow.go
	"denma.attachment_max_mb", // cmd/denma_attachlimit.go
}

// denmaCenterSettingsPath reports whether a path is a settings page or API, which only the hub has.
func denmaCenterSettingsPath(p string) bool {
	for _, pre := range []string{"/admin/settings", "/api/settings", "/api/logs", "/api/maintenance"} {
		if p == pre || strings.HasPrefix(p, pre+"/") {
			return true
		}
	}
	return false
}

// denmaCenterAdminRole is the role a center's own admins get: everything but settings and raw SQL.
const denmaCenterAdminRole = "Center Admin"

var denmaCenterAdminExcluded = map[string]bool{
	"settings:get": true, "settings:manage": true, "settings:maintain": true,
	"subscribers:sql_query": true,
}

// denmaCenter is one center: its registry row and, once loaded, its App.
type denmaCenter struct {
	ID     int    `db:"id"`
	Slug   string `db:"slug"`
	Name   string `db:"name"`
	Schema string `db:"schema_name"`

	app    *App
	router *echo.Echo

	holdScan bool // its App picks up no campaign until the one before has stopped (cmd/denma_lifecycle.go)

	// Settings set when the center is provisioned (by the hub).
	initial map[string]any
}

// denmaCenters holds the loaded centers, by slug.
type denmaCenters struct {
	mu     sync.RWMutex
	bySlug map[string]*denmaCenter
	base   *App // the hub, served at /

	// The hub once reloaded: its App and router.
	reBase   *App
	reRouter *echo.Echo

	baseSchema string            // the hub's schema
	failed     map[string]string // centers that didn't load: slug -> error
	starting   map[string]bool   // centers still loading after a start

	// Serializes changes to database objects and files while a center loads.
	setupMu sync.Mutex
	timings denmaTimings

	// Serializes enable, disable and reload swaps.
	lifeMu sync.Mutex

	stopping map[string][]chan struct{} // slug -> Apps being stopped (cmd/denma_lifecycle.go), under mu

	// Serializes passes of propagateShared.
	propMu      sync.Mutex
	propRunning bool
	propAgain   bool
}

// denmaNoExit turns errors that would stop the server while a center loads into a recoverable panic.
var denmaNoExit func(error) any

type denmaLoadError struct{ err error }

var errDenmaStarting = errors.New("the center is still starting up; try again in a moment")

// isStarting reports whether a center is still loading after a start.
func (d *denmaCenters) isStarting(slug string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.starting[slug]
}

// current is the hub's App, reloaded or not.
func (d *denmaCenters) current() *App {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.reBase != nil {
		return d.reBase
	}
	return d.base
}

// baseRouter is the reloaded hub's router, or nil before a reload.
func (d *denmaCenters) baseRouter() *echo.Echo {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.reRouter
}

func (d *denmaCenters) get(slug string) *denmaCenter {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.bySlug[slug]
}

func (d *denmaCenters) set(c *denmaCenter) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.bySlug[c.Slug] = c
	delete(d.failed, c.Slug)
}

func (d *denmaCenters) setFailed(slug string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failed[slug] = err.Error()
}

// loaded is a snapshot of the running centers.
func (d *denmaCenters) loaded() []*denmaCenter {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]*denmaCenter, 0, len(d.bySlug))
	for _, c := range d.bySlug {
		out = append(out, c)
	}
	return out
}

// initDenmaCenters loads the registered centers and mounts them on the hub's server.
func initDenmaCenters(srv *echo.Echo, base *App) {
	if !base.ko.Bool("denma.multi_center") {
		return
	}

	if err := denmaInitRegistry(base.db); err != nil {
		lo.Fatalf("denma: error setting up the center registry: %v", err)
	}
	denmaSeedCenters(base.db, os.Getenv("DENMA_SEED_CENTERS"))

	var list []*denmaCenter
	if err := base.db.Select(&list, `SELECT id, slug, name, schema_name FROM denma.centers WHERE status = 'enabled' ORDER BY slug`); err != nil {
		lo.Fatalf("denma: error reading centers: %v", err)
	}

	denmaNoExit = func(err error) any { return denmaLoadError{err} }

	d := &denmaCenters{bySlug: make(map[string]*denmaCenter, len(list)), base: base, failed: map[string]string{}, starting: map[string]bool{}}
	if err := base.db.Get(&d.baseSchema, `SELECT current_schema()`); err != nil {
		lo.Fatalf("denma: error reading the hub's schema: %v", err)
	}
	denmaHub = d

	// The centers load in the background, several at a time, while the hub serves.
	for _, c := range list {
		d.starting[c.Slug] = true
	}
	go d.loadAll(list, base.ko.Int("denma.center_load_workers"))
	go d.watchDomains()        // cmd/denma_domains.go
	go d.watchPendingBounces() // cmd/denma_bounces.go
	go d.pruneAuditLog()       // cmd/denma_audit.go
	go d.pruneCenters()        // expired sessions, old e-mail records (cmd/denma_prune.go)

	// The hub's settings saves rebuild it in place, signalling on a channel of their own.
	base.chReload = make(chan os.Signal, 1)
	go d.watchBaseReload(base)

	// /c/<slug>/... goes to that center's router with the prefix removed; the rest to the hub's.
	srv.Pre(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			// Signing in, for everyone (cmd/denma_login.go).
			if done, err := d.loginRoute(c, next); done {
				return err
			}
			// Claude's MCP server, with a key (cmd/denma_mcp.go).
			if done, err := d.mcpRoute(c); done {
				return err
			}
			p := c.Request().URL.Path
			if !strings.HasPrefix(p, denmaCenterPath) {
				// The hub keeps only the public pages its admin needs.
				if p == "/" {
					return c.Redirect(http.StatusFound, uriAdmin)
				}
				// /admin (and /api) is the center the place cookie names, or the hub (cmd/denma_place.go).
				if denmaAdminRequest(p) {
					if err := denmaCheckTab(c, p); err != nil {
						return err
					}
					if ctr := d.placeCenter(c, p); ctr != nil {
						denmaSandbox(ctr.app, c.Response().Header(), p)
						ctr.router.ServeHTTP(c.Response(), c.Request())
						return nil
					}
					if slug := denmaPlace(c); slug != "" {
						switch {
						case !denmaHubOnlyPath(p) && d.isStarting(slug):
							return denmaStartingPage(c)
						case d.get(slug) == nil || (c.Request().Method == http.MethodGet && strings.HasPrefix(p, uriAdmin)):
							denmaSetPlace(c, "") // gone, or a hub page: the hub from now on
						}
					}
				}
				switch {
				case !denmaHubPath(p) && !denmaHubUpload(d.current(), p): // its own uploads (cmd/denma_hubmedia.go)
					return echo.NewHTTPError(http.StatusNotFound, "not found")
				}
				denmaSandbox(d.current(), c.Response().Header(), p) // cmd/denma_csp.go
				if r := d.baseRouter(); r != nil {
					r.ServeHTTP(c.Response(), c.Request())
					return nil
				}
				return next(c)
			}
			slug, rest, _ := strings.Cut(strings.TrimPrefix(p, denmaCenterPath), "/")
			ctr := d.get(slug)
			if ctr == nil && d.isStarting(slug) {
				return denmaStartingPage(c)
			}
			if ctr == nil {
				return echo.NewHTTPError(http.StatusNotFound, "center not found")
			}
			// An old admin address (e-mails, bookmarks) goes to the center at /admin (cmd/denma_place.go).
			if c.Request().Method == http.MethodGet && (rest == "admin" || strings.HasPrefix(rest, "admin/")) {
				denmaSetPlace(c, slug)
				to := path.Clean("/" + rest) // and still under /admin: no "admin/../api/..."
				if to != "/admin" && !strings.HasPrefix(to, "/admin/") {
					to = "/admin"
				}
				if q := c.Request().URL.RawQuery; q != "" {
					to += "?" + q
				}
				return c.Redirect(http.StatusFound, to)
			}
			// Bounce webhooks are the hub's, recorded in their center (cmd/denma_bounces.go).
			if strings.HasPrefix(rest, "webhooks/service/") {
				r := c.Request().Clone(c.Request().Context())
				r.URL.Path = "/" + rest
				r.URL.RawPath = ""
				c.Echo().ServeHTTP(c.Response(), r) // as a request to the hub
				return nil
			}
			// Settings are the hub's: its pages, and none of the APIs here.
			if denmaCenterSettingsPath("/" + rest) {
				if strings.HasPrefix(rest, "admin/") {
					return c.Redirect(http.StatusFound, path.Join(d.current().urlCfg.RootPath, "/"+rest))
				}
				return echo.NewHTTPError(http.StatusForbidden, "Settings are managed in the hub, for all centers.")
			}
			denmaSandbox(ctr.app, c.Response().Header(), "/"+rest)
			r := c.Request().Clone(c.Request().Context())
			r.URL.Path = "/" + rest
			r.URL.RawPath = ""
			ctr.router.ServeHTTP(&denmaPrefixWriter{ResponseWriter: c.Response(), prefix: denmaCenterPath + slug}, r)
			return nil
		}
	})
}

// loadAll loads centers, workers at a time (default 4), after a start.
func (d *denmaCenters) loadAll(list []*denmaCenter, workers int) {
	if workers < 1 {
		workers = 4
	}
	start := time.Now()
	jobs := make(chan *denmaCenter)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range jobs {
				if err := d.load(c); err != nil {
					// One broken center shouldn't take the others down.
					lo.Printf("denma: center %s not loaded: %v", c.Slug, err)
					d.setFailed(c.Slug, err)
				} else {
					d.set(c)
				}
				d.mu.Lock()
				delete(d.starting, c.Slug)
				d.mu.Unlock()
			}
		}()
	}
	for _, c := range list {
		jobs <- c
	}
	close(jobs)
	wg.Wait()
	lo.Printf("denma: %d of %d centers loaded in %s, %d at a time (total time per step: %s)",
		len(d.loaded()), len(list), time.Since(start).Round(time.Millisecond), workers, &d.timings)
	denmaStartup.set(start, len(d.loaded()), len(list), time.Since(start)) // for System
	d.retryBounces()                                                       // those that came while they loaded (cmd/denma_bounces.go)
}

// denmaStartingPage answers for a center still loading: a self-reloading page, or an API error.
func denmaStartingPage(c echo.Context) error {
	c.Response().Header().Set("Retry-After", "5")
	if strings.Contains(c.Request().Header.Get("Accept"), "text/html") {
		return c.HTML(http.StatusServiceUnavailable, `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><meta http-equiv="refresh" content="5">
<title>Starting</title><style>body{font-family:system-ui,sans-serif;margin:0;display:grid;place-items:center;min-height:100vh;
background:#fff;color:#333}@media (prefers-color-scheme:dark){body{background:#111;color:#ddd}}p{max-width:30em;padding:0 16px;text-align:center}</style>
</head><body><p>This site is starting up. The page will reload in a few seconds.</p></body></html>`)
	}
	return echo.NewHTTPError(http.StatusServiceUnavailable, "This center is starting up. Try again in a few seconds.")
}

// DenmaHealthCenters is the hub's /health/centers: OK if every enabled center runs, else 503.
func (a *App) DenmaHealthCenters(c echo.Context) error {
	d := denmaHub
	if d == nil || a.ko.String("denma.center") != "" {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	var one int
	dbOK := d.current().db.Get(&one, `SELECT 1`) == nil

	d.mu.RLock()
	running, starting, failed := len(d.bySlug), len(d.starting), len(d.failed)
	d.mu.RUnlock()
	if dbOK && starting == 0 && failed == 0 {
		return c.JSON(http.StatusOK, okResp{true})
	}
	return c.JSON(http.StatusServiceUnavailable, map[string]any{
		"data": false, "database": dbOK, "running": running, "starting": starting, "failed": failed,
	})
}

// denmaEveryMinute adds a job to an app's cron every minute, at its center's second (+ at).
func denmaEveryMinute(a *App, at int, job string, fn func()) (int, error) {
	center := a.ko.String("denma.center")
	h := fnv.New32a()
	_, _ = h.Write([]byte(center))
	offset := time.Duration((int(h.Sum32()%60)+at)%60) * time.Second

	var sched cron.Schedule = denmaMinuteAt(offset)
	if denmaFastJobs(os.Getenv("DENMA_FAST_JOBS"), center) {
		sched = denmaFastAt(time.Duration(at/10) * time.Second)
	}

	// A run still going when the next is due lets that one pass.
	var running atomic.Bool

	// A job added to a cron that has since stopped (a replaced app) never runs.
	if _, err := a.crons.Schedule(sched, func() {
		if a.manager.DenmaScanHeld() { // a reload's new App waits for the old (cmd/denma_lifecycle.go)
			return
		}
		if !running.CompareAndSwap(false, true) {
			return
		}
		defer running.Store(false)
		fn()
		denmaJobRan(job, center)
	}); err != nil {
		a.log.Printf("denma: error adding a job: %v", err)
	}
	return 0, nil
}

// denmaMinuteAt is a cron schedule: at its second of every minute.
type denmaMinuteAt time.Duration

func (m denmaMinuteAt) Next(t time.Time) time.Time {
	next := t.Truncate(time.Minute).Add(time.Duration(m))
	if !next.After(t) {
		next = next.Add(time.Minute)
	}
	return next
}

// denmaFastJobs says whether a center is in DENMA_FAST_JOBS (dev stack only): jobs every 10 seconds.
func denmaFastJobs(list, center string) bool {
	return center != "" && slices.Contains(strings.Split(list, ","), center)
}

// denmaFastAt is a cron schedule: at its second of every 10 seconds.
type denmaFastAt time.Duration

func (f denmaFastAt) Next(t time.Time) time.Time {
	next := t.Truncate(10 * time.Second).Add(time.Duration(f))
	if !next.After(t) {
		next = next.Add(10 * time.Second)
	}
	return next
}

// denmaHubPath reports whether the hub serves a path (admin, API, health, static, bounces).
func denmaHubPath(p string) bool {
	if denmaCenterContent(p) && !denmaHubMediaPage(p) {
		return false
	}
	for _, pre := range []string{"/admin", "/api/", "/public/", "/health", "/webhooks/"} {
		if p == strings.TrimSuffix(pre, "/") || strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// denmaCenterContent reports whether a path is center content, which the hub doesn't serve.
func denmaCenterContent(p string) bool {
	for _, pre := range []string{
		"/admin/lists", "/admin/subscribers", "/admin/campaigns", "/admin/templates",
		"/admin/calendar", "/admin/automations", "/admin/center",
		"/admin/analytics/audience", "/admin/analytics/campaigns", "/admin/analytics/automations",
		"/api/lists", "/api/subscribers", "/api/campaigns", "/api/templates",
		"/api/import", "/api/bounces", "/api/tx", "/api/public", "/api/dashboard",
		"/api/denma/automations", "/api/denma/center", "/api/denma/search", "/api/denma/stats", "/api/denma/tags",
		"/api/denma/campaigns", "/api/denma/subscribers", "/api/denma/send-hours", "/api/denma/versions", "/api/denma/editing",
	} {
		if p == pre || strings.HasPrefix(p, pre+"/") {
			return true
		}
	}
	return false
}

// denmaInitRegistry creates the center registry (schema denma) if needed.
func denmaInitRegistry(db *sqlx.DB) error {
	if err := denmaInitAudit(db); err != nil { // cmd/denma_audit.go
		return err
	}
	_, err := db.Exec(`
		CREATE SCHEMA IF NOT EXISTS denma;
		CREATE TABLE IF NOT EXISTS denma.centers (
			id          SERIAL PRIMARY KEY,
			slug        TEXT NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$'),
			name        TEXT NOT NULL,
			schema_name TEXT NOT NULL UNIQUE,
			status      TEXT NOT NULL DEFAULT 'enabled' CHECK (status IN ('enabled', 'disabled')),
			created_at  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
		);
		-- Superadmins' accounts in each center, for signing in from the hub.
		CREATE TABLE IF NOT EXISTS denma.center_superadmins (
			center_id      INTEGER NOT NULL REFERENCES denma.centers(id) ON DELETE CASCADE,
			hub_user_id    INTEGER NOT NULL,
			center_user_id INTEGER NOT NULL,
			PRIMARY KEY (center_id, hub_user_id)
		);
		-- Addresses that can't receive mail, blocklisted in every center (cmd/denma_blocklist.go).
		CREATE TABLE IF NOT EXISTS denma.blocked_emails (
			email      TEXT PRIMARY KEY, -- lowercase
			reason     TEXT NOT NULL,
			center     TEXT NOT NULL, -- the slug of the center that found it
			created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
		);
		ALTER TABLE denma.blocked_emails ADD COLUMN IF NOT EXISTS pending TEXT[] NOT NULL DEFAULT '{}'; -- centers still to blocklist it
		CREATE INDEX IF NOT EXISTS blocked_emails_pending ON denma.blocked_emails (created_at) WHERE pending <> '{}';`)
	if err != nil {
		return err
	}
	if err := denmaInitOptinSends(db); err != nil { // cmd/denma_optins.go
		return err
	}
	if err := denmaInitDailySends(db); err != nil { // cmd/denma_daily.go
		return err
	}
	if err := denmaInitDomains(db); err != nil { // cmd/denma_domains.go
		return err
	}
	if err := denmaInitPendingBounces(db); err != nil { // cmd/denma_bounces.go
		return err
	}
	if err := denmaInitPeople(db); err != nil { // cmd/denma_people.go
		return err
	}
	denmaInitTrigram(db)    // cmd/denma_trgm.go
	return denmaInitMCP(db) // cmd/denma_mcp.go
}

// denmaSeedCenters registers centers listed as "slug:Name,slug2:Name 2", for dev and CI.
func denmaSeedCenters(db *sqlx.DB, list string) {
	for _, item := range strings.Split(list, ",") {
		slug, name, _ := strings.Cut(strings.TrimSpace(item), ":")
		if slug == "" {
			continue
		}
		if _, err := denmaRegisterCenter(db, slug, name); err != nil {
			lo.Printf("denma: error registering center %s: %v", slug, err)
		}
	}
}

// denmaRegisterCenter adds a center to the registry, or returns nil if its slug or schema is taken.
func denmaRegisterCenter(db *sqlx.DB, slug, name string) (*denmaCenter, error) {
	if !reDenmaSlug.MatchString(slug) || len(slug) > 50 {
		return nil, fmt.Errorf("invalid address %q: use lowercase letters, digits and single hyphens", slug)
	}
	if strings.TrimSpace(name) == "" {
		name = slug
	}
	schema := denmaSchemaName(slug)
	var out []*denmaCenter
	err := db.Select(&out, `INSERT INTO denma.centers (slug, name, schema_name) VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING RETURNING id, slug, name, schema_name`,
		slug, name, schema)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return out[0], nil
}

// load opens a center: provisions it if new, builds its App and router, and watches for reload.
func (d *denmaCenters) load(c *denmaCenter) (err error) {
	var cdb *sqlx.DB
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(denmaLoadError)
			if !ok {
				panic(r)
			}
			err = e.err
			if cdb != nil {
				cdb.Close()
			}
		}
	}()
	if c.Schema == d.baseSchema {
		return fmt.Errorf("the center's schema (%s) is the hub's", c.Schema)
	}
	t := time.Now()
	cdb, ck, err := d.setup(c)
	if err != nil {
		return err
	}
	d.timings.add(0, &t)

	qMap := readQueries(queryFilePath, fs)
	initSettings(qMap["get-settings"].Query, cdb, ck)
	cq := prepareQueries(qMap, cdb, ck)
	d.timings.add(1, &t)

	app := buildApp(ck, cdb, cq, false)
	d.timings.add(2, &t)
	// Its own uploads folder (uploads/<slug>, set by adopt).
	if p := ck.String("upload.filesystem.upload_path"); ck.String("upload.provider") == "filesystem" && p != "" {
		if err := os.MkdirAll(p, 0o755); err != nil {
			lo.Printf("denma: error creating %s's uploads folder %s: %v", c.Slug, p, err)
		}
	}
	// Never listmonk's first-run "create the Super Admin" page.
	app.needsUserSetup = false
	c.app = app
	c.router = initHTTPRouter(app.cfg, app.urlCfg, app.i18n, fs, app)
	d.timings.add(3, &t)

	go d.watchReload(c)
	return nil
}

// denmaTimings adds up the time centers spend in each loading step.
type denmaTimings struct {
	mu sync.Mutex
	d  [4]time.Duration // setup, queries, app, router
}

// add adds the time since *t to step i and restarts *t.
func (m *denmaTimings) add(i int, t *time.Time) {
	now := time.Now()
	m.mu.Lock()
	m.d[i] += now.Sub(*t)
	m.mu.Unlock()
	*t = now
}

func (m *denmaTimings) String() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := func(d time.Duration) time.Duration { return d.Round(time.Millisecond) }
	return fmt.Sprintf("setup %s, queries %s, app %s, router %s", r(m.d[0]), r(m.d[1]), r(m.d[2]), r(m.d[3]))
}

// setup readies a center's schema, settings and files and opens its DB pool (one at a time).
func (d *denmaCenters) setup(c *denmaCenter) (*sqlx.DB, *koanf.Koanf, error) {
	d.setupMu.Lock()
	defer d.setupMu.Unlock()

	ck := d.centerConfig(c)

	cdb, err := denmaConnect(ck)
	if err != nil {
		return nil, nil, err
	}
	if err := d.provision(c, cdb); err != nil {
		cdb.Close()
		return nil, nil, err
	}
	if err := d.upgrade(c, cdb); err != nil {
		cdb.Close()
		return nil, nil, err
	}
	if err := d.adopt(c, cdb); err != nil {
		cdb.Close()
		return nil, nil, err
	}
	if _, err := d.syncShared(cdb); err != nil {
		cdb.Close()
		return nil, nil, fmt.Errorf("copying the shared settings: %v", err)
	}
	return cdb, ck, nil
}

// centerConfig is the hub's config with the center's own schema and a small DB pool.
func (d *denmaCenters) centerConfig(c *denmaCenter) *koanf.Koanf {
	ck := koanf.New(".")
	_ = ck.Load(confmap.Provider(d.base.ko.All(), "."), nil)

	params := strings.TrimSpace(d.base.ko.String("db.params") + " search_path=" + c.Schema)
	_ = ck.Set("db.params", params)
	_ = ck.Set("denma.center", c.Slug)
	_ = ck.Set("denma.hold_scan", c.holdScan)
	_ = ck.Set("db.max_open", d.base.ko.Int("denma.center_db_max_open"))
	_ = ck.Set("db.max_idle", d.base.ko.Int("denma.center_db_max_idle"))
	if ck.Int("db.max_open") < 1 {
		_ = ck.Set("db.max_open", 4)
	}
	// Keeps some connections open between queries.
	if ck.Int("db.max_idle") < 1 {
		_ = ck.Set("db.max_idle", 1)
	}
	if ck.Int("db.max_idle") > ck.Int("db.max_open") {
		_ = ck.Set("db.max_idle", ck.Int("db.max_open"))
	}
	return ck
}

// denmaConnect opens a center's DB pool, testing the connection first.
func denmaConnect(ck *koanf.Koanf) (*sqlx.DB, error) {
	var c struct {
		Host     string `koanf:"host"`
		Port     int    `koanf:"port"`
		User     string `koanf:"user"`
		Password string `koanf:"password"`
		DBName   string `koanf:"database"`
		SSLMode  string `koanf:"ssl_mode"`
		Params   string `koanf:"params"`
	}
	if err := ck.Unmarshal("db", &c); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s %s",
		c.Host, c.Port, c.User, c.Password, c.DBName, c.SSLMode, c.Params)
	test, err := sqlx.Connect(denmaDB(dsn, ck)) // cmd/denma_db.go
	if err != nil {
		return nil, fmt.Errorf("connecting to the database: %v", err)
	}
	test.Close()
	return initDB(ck), nil
}

// denmaInstalling says that a center is being installed.
var denmaInstalling atomic.Bool

// denmaInstallError is an error in install, while a center is being installed.
type denmaInstallError struct{ error }

// installFatalf is install's error: ends the process unless a center is being installed.
func installFatalf(format string, args ...any) {
	if denmaInstalling.Load() {
		panic(denmaInstallError{fmt.Errorf(format, args...)})
	}
	lo.Fatalf(format, args...)
}

// denmaRecoverInstall turns installFatalf's panic into provision's error.
func denmaRecoverInstall(err *error) {
	if r := recover(); r != nil {
		e, ok := r.(denmaInstallError)
		if !ok {
			panic(r)
		}
		*err = fmt.Errorf("installing: %w", e.error)
	}
}

// provision installs the center's schema if new and points its URLs and uploads at /c/<slug>/.
func (d *denmaCenters) provision(c *denmaCenter, db *sqlx.DB) (err error) {
	var installed bool
	if err := db.Get(&installed, `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = $1 AND table_name = 'settings')`, c.Schema); err != nil {
		return err
	}
	if installed {
		// Provisioned centers have denma.provisioned or features; with neither, it stopped part-way.
		var done bool
		if err := db.Get(&done, `SELECT EXISTS (SELECT 1 FROM settings WHERE key IN ('denma.provisioned', 'denma.features_version'))`); err != nil {
			return err
		}
		if done {
			return nil
		}
		var subs int
		if err := db.Get(&subs, `SELECT COUNT(*) FROM subscribers`); err != nil {
			return err
		}
		if subs > 2 { // more than the samples: not to be removed
			return fmt.Errorf("center %s stopped part-way through being made, and has %d subscribers: look at it by hand", c.Slug, subs)
		}
		lo.Printf("denma: center %s stopped part-way through being made: making it again", c.Slug)
		if _, err := db.Exec(`DROP SCHEMA "` + c.Schema + `" CASCADE`); err != nil {
			return fmt.Errorf("removing what was made: %v", err)
		}
	}

	defer denmaRecoverInstall(&err)
	lo.Printf("denma: provisioning center %s (schema %s)", c.Slug, c.Schema)
	if _, err := db.Exec(`CREATE SCHEMA IF NOT EXISTS "` + c.Schema + `"`); err != nil {
		return fmt.Errorf("creating schema: %v", err)
	}

	// Never install unless every connection resolves names in this schema alone.
	var schemas []string
	if err := db.Select(&schemas, `SELECT unnest(current_schemas(false))`); err != nil {
		return err
	}
	if len(schemas) != 1 || schemas[0] != c.Schema {
		return fmt.Errorf("refusing to install: search path is %v, not just %s", schemas, c.Schema)
	}

	if err := denmaCryptoFuncs(db); err != nil {
		return err
	}

	denmaInstalling.Store(true)
	defer denmaInstalling.Store(false)
	install(migList[len(migList)-1].version, db, fs, false, false)

	// A clean center: no listmonk samples but the default campaign and sample visual templates.
	if _, err := db.Exec(`DELETE FROM campaigns; DELETE FROM templates WHERE NOT is_default AND type <> 'campaign_visual';
		DELETE FROM subscribers; DELETE FROM lists; DELETE FROM sessions; DELETE FROM users;`); err != nil {
		return fmt.Errorf("removing the sample data: %v", err)
	}
	if err := d.provisionRoles(db); err != nil {
		return err
	}

	settings := map[string]any{"app.site_name": c.Name}
	for k, v := range c.initial {
		settings[k] = v
	}
	for k, v := range settings {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := db.Exec(`INSERT INTO settings (key, value) VALUES ($2, $1::jsonb)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()`, string(b), k); err != nil {
			return fmt.Errorf("setting %s: %v", k, err)
		}
	}
	// Its website signups' lists, and the automation moving people on.
	if err := denmaDefaultAutomation(db); err != nil { // cmd/denma_automations.go
		return fmt.Errorf("adding the website signups' automation: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO settings (key, value) VALUES ('denma.provisioned', 'true') ON CONFLICT (key) DO NOTHING`); err != nil {
		return fmt.Errorf("finishing: %v", err)
	}
	return nil
}

// provisionRoles creates a center's roles if missing: Super Admin (role 1) and Center Admin.
func (d *denmaCenters) provisionRoles(db *sqlx.DB) error {
	var all, admin []string
	for p := range d.base.cfg.Permissions {
		all = append(all, p)
		if !denmaCenterAdminExcluded[p] {
			admin = append(admin, p)
		}
	}

	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM roles WHERE id = 1`); err != nil {
		return err
	}
	if n == 0 {
		var id int
		if err := db.Get(&id, `INSERT INTO roles (name, type, permissions) VALUES ('Super Admin', 'user', $1) RETURNING id`, pq.Array(all)); err != nil {
			return fmt.Errorf("creating the Super Admin role: %v", err)
		}
		if id != 1 {
			return fmt.Errorf("the Super Admin role got ID %d, not 1", id)
		}
	} else if _, err := db.Exec(`UPDATE roles SET permissions = $1 WHERE id = 1 AND type = 'user'
		AND NOT (permissions @> $1::TEXT[])`, pq.Array(all)); err != nil {
		return fmt.Errorf("updating the Super Admin role: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO roles (name, type, permissions) SELECT $1, 'user', $2
		WHERE NOT EXISTS (SELECT 1 FROM roles WHERE name = $1 AND type = 'user')`, denmaCenterAdminRole, pq.Array(admin)); err != nil {
		return fmt.Errorf("creating the %s role: %v", denmaCenterAdminRole, err)
	}
	return nil
}

// adopt keeps a center in step with the hub on every load: address, roles, admins, people, triggers.
func (d *denmaCenters) adopt(c *denmaCenter, db *sqlx.DB) error {
	fsPath, s3Path := denmaUploadPaths(d.base.ko, c.Slug)
	for k, v := range map[string]string{
		"app.root_url":                  strings.TrimSuffix(d.current().urlCfg.RootURL, "/") + denmaCenterPath + c.Slug,
		"upload.filesystem.upload_path": fsPath,
		"upload.s3.bucket_path":         s3Path,
	} {
		if _, err := db.Exec(`UPDATE settings SET value = to_jsonb($1::text), updated_at = NOW()
			WHERE key = $2 AND value IS DISTINCT FROM to_jsonb($1::text)`, v, k); err != nil {
			return fmt.Errorf("setting %s: %v", k, err)
		}
	}
	if err := d.provisionRoles(db); err != nil {
		return err
	}
	if err := d.ownAdmins(c, db); err != nil { // cmd/denma_hierarchy.go
		return err
	}
	if err := d.adoptPeople(c, db); err != nil { // cmd/denma_people.go
		return err
	}
	return denmaRetryDeadlock(func() error { return d.features(c, db) }) // cmd/denma_features.go
}

// denmaCryptoFuncs makes pgcrypto's functions callable from a schema with its own search path.
func denmaCryptoFuncs(db sqlx.Ext) error {
	var ext string
	if err := sqlx.Get(db, &ext, `SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = 'pgcrypto'`); err != nil {
		return fmt.Errorf("finding pgcrypto (CREATE EXTENSION pgcrypto first): %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION crypt(text, text) RETURNS text LANGUAGE sql IMMUTABLE STRICT AS 'SELECT %[1]s.crypt($1, $2)';
		CREATE OR REPLACE FUNCTION gen_salt(text) RETURNS text LANGUAGE sql VOLATILE STRICT AS 'SELECT %[1]s.gen_salt($1)';
		CREATE OR REPLACE FUNCTION digest(text, text) RETURNS bytea LANGUAGE sql IMMUTABLE STRICT AS 'SELECT %[1]s.digest($1, $2)';`,
		pq.QuoteIdentifier(ext))); err != nil {
		return fmt.Errorf("creating pgcrypto functions: %v", err)
	}
	return nil
}

// denmaPrepareHub readies the hub's own schema before listmonk checks it, installing it on first start.
func denmaPrepareHub(db *sqlx.DB) {
	if !ko.Bool("denma.multi_center") {
		return
	}
	defer denmaHubColumns(db) // cmd/denma_tags.go
	var sp string
	if err := db.Get(&sp, `SHOW search_path`); err != nil {
		lo.Fatalf("denma: error reading the search path: %v", err)
	}
	schema := strings.Trim(strings.TrimSpace(sp), `"`)
	if !reDenmaSchema.MatchString(schema) || schema == "public" {
		lo.Fatalf(`denma: multi-center mode needs the hub's own schema: set db.params to "search_path=hub" (the search path is %q)`, sp)
	}
	if _, err := db.Exec(`CREATE SCHEMA IF NOT EXISTS ` + pq.QuoteIdentifier(schema)); err != nil {
		lo.Fatalf("denma: error creating the hub's schema %s: %v", schema, err)
	}

	var installed bool
	if err := db.Get(&installed, `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = $1 AND table_name = 'settings')`, schema); err != nil {
		lo.Fatalf("denma: error checking the hub's schema: %v", err)
	}
	if installed {
		return
	}

	var schemas []string
	if err := db.Select(&schemas, `SELECT unnest(current_schemas(false))`); err != nil || len(schemas) != 1 || schemas[0] != schema {
		lo.Fatalf("denma: refusing to install the hub: the search path is %v, not just %s (%v)", schemas, schema, err)
	}
	if err := denmaCryptoFuncs(db); err != nil {
		lo.Fatalf("denma: %v", err)
	}

	lo.Printf("denma: installing the hub (Listmonk Shambhala) in schema %s", schema)
	install(migList[len(migList)-1].version, db, fs, false, false)

	if _, err := db.Exec(`DELETE FROM campaigns; DELETE FROM subscribers; DELETE FROM lists;
		UPDATE settings SET value = '"Listmonk Shambhala"', updated_at = NOW() WHERE key = 'app.site_name';`); err != nil {
		lo.Fatalf("denma: error setting up the hub: %v", err)
	}
}

// upgrade brings a center's schema to the binary's version; a failure stops only this center.
func (d *denmaCenters) upgrade(c *denmaCenter, db *sqlx.DB) error {
	last, pending, err := getPendingMigrations(db)
	if err != nil {
		return fmt.Errorf("checking migrations: %v", err)
	}
	for _, m := range pending {
		lo.Printf("denma: upgrading center %s from %s: migration %s", c.Slug, last, m.version)
		if err := m.fn(db, fs, ko, lo); err != nil {
			return fmt.Errorf("upgrade %s failed: %v", m.version, err)
		}
		if err := recordMigrationVersion(m.version, db); err != nil {
			return fmt.Errorf("recording upgrade %s: %v", m.version, err)
		}
	}
	return nil
}

// syncShared copies the hub's settings to a center, all but its own; it reports whether any changed.
func (d *denmaCenters) syncShared(db *sqlx.DB) (bool, error) {
	res, err := db.Exec(fmt.Sprintf(`UPDATE settings c SET value = p.value, updated_at = NOW()
		FROM %s.settings p WHERE p.key = c.key AND c.key <> ALL($1) AND c.value IS DISTINCT FROM p.value`,
		pq.QuoteIdentifier(d.baseSchema)), pq.Array(denmaCenterOwnSettings))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// propagateShared copies the hub's changed settings to every center and reloads those that changed,
// one at a time; a center sending a campaign is reloaded once it finishes. One pass runs at a time.
func (d *denmaCenters) propagateShared() {
	d.propMu.Lock()
	if d.propRunning {
		d.propAgain = true
		d.propMu.Unlock()
		return
	}
	d.propRunning = true
	d.propMu.Unlock()
	for {
		d.propagatePass()
		d.propMu.Lock()
		if !d.propAgain {
			d.propRunning = false
			d.propMu.Unlock()
			// Hands the old Apps' memory back.
			debug.FreeOSMemory()
			return
		}
		d.propAgain = false
		d.propMu.Unlock()
		lo.Printf("denma: the hub's settings changed again: copying them to the centers again")
	}
}

// propagatePass is one pass of propagateShared.
func (d *denmaCenters) propagatePass() {
	for _, c := range d.loaded() {
		if c = d.get(c.Slug); c == nil {
			continue // disabled meanwhile
		}
		changed, err := d.syncShared(c.app.db)
		if err != nil {
			lo.Printf("denma: error copying the hub's settings to center %s: %v", c.Slug, err)
			continue
		}
		if !changed {
			continue
		}
		if c.app.denmaSending() {
			c.app.Lock()
			c.app.needsRestart = true
			c.app.Unlock()
			go d.reloadWhenIdle(c)
			continue
		}
		denmaSignalReload(c.app)
		d.awaitReload(c)
		time.Sleep(200 * time.Millisecond)
	}
}

// awaitReload waits (a minute at most) for center c to be replaced by its reload, or disabled.
func (d *denmaCenters) awaitReload(c *denmaCenter) {
	for range 600 {
		if d.get(c.Slug) != c {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	lo.Printf("denma: center %s hasn't reloaded within a minute; going on to the next", c.Slug)
}

// reloadWhenIdle reloads a center once no campaign is sending, unless changed meanwhile.
func (d *denmaCenters) reloadWhenIdle(c *denmaCenter) {
	for {
		time.Sleep(time.Minute)
		if cur := d.get(c.Slug); cur == nil || cur.app != c.app {
			return
		}
		if !c.app.denmaSending() {
			lo.Printf("denma: center %s isn't sending; reloading it with the hub's settings", c.Slug)
			denmaSignalReload(c.app)
			return
		}
	}
}

func denmaSignalReload(a *App) {
	select {
	case a.chReload <- syscall.SIGHUP:
	default:
	}
}

// disable stops serving a center (its data stays).
func (d *denmaCenters) disable(slug string) error {
	if d.isStarting(slug) {
		return errDenmaStarting
	}
	// Its App is stopped after the lock is let go, and the request waits for that.
	c, end, err := func() (*denmaCenter, func(), error) {
		d.lifeMu.Lock()
		defer d.lifeMu.Unlock()
		if _, err := d.base.db.Exec(`UPDATE denma.centers SET status = 'disabled' WHERE slug = $1`, slug); err != nil {
			return nil, nil, err
		}
		d.mu.Lock()
		c := d.bySlug[slug]
		delete(d.bySlug, slug)
		delete(d.failed, slug)
		d.mu.Unlock()
		if c == nil {
			return nil, nil, nil
		}
		return c, d.beginStop(slug), nil
	}()
	if c != nil {
		lo.Printf("denma: center %s disabled", slug)
		d.retireThen(c.app, nil, nil, end)
	}
	return err
}

// enable loads a center and serves it again.
func (d *denmaCenters) enable(slug string) error {
	if d.isStarting(slug) {
		return errDenmaStarting
	}
	d.lifeMu.Lock()
	defer d.lifeMu.Unlock()
	var c denmaCenter
	if err := d.base.db.Get(&c, `SELECT id, slug, name, schema_name FROM denma.centers WHERE slug = $1`, slug); err != nil {
		return err
	}
	if d.get(slug) != nil {
		return nil
	}
	// The new App picks up campaigns once the center's old App has stopped.
	before := d.stopsOf(slug)
	c.holdScan = len(before) > 0
	if err := d.load(&c); err != nil {
		d.setFailed(slug, err)
		return err
	}
	if _, err := d.base.db.Exec(`UPDATE denma.centers SET status = 'enabled' WHERE slug = $1`, slug); err != nil {
		return err
	}
	d.set(&c)
	if c.holdScan {
		go d.releaseScan(c.app, before)
	}
	lo.Printf("denma: center %s enabled", slug)
	go d.retryBounces() // those that came while it was off (cmd/denma_bounces.go)
	return nil
}

// watchReload rebuilds the center when its settings are saved.
func (d *denmaCenters) watchReload(c *denmaCenter) {
	old := c.app
	<-old.chReload
	if d.get(c.Slug) != c {
		return // retired (retireThen)
	}
	lo.Printf("denma: reloading center %s", c.Slug)

	next := &denmaCenter{ID: c.ID, Slug: c.Slug, Name: c.Name, Schema: c.Schema, holdScan: true}
	if err := d.load(next); err != nil {
		lo.Printf("denma: error reloading center %s: %v", c.Slug, err)
		go d.watchReload(c) // keep serving the old one
		return
	}
	d.lifeMu.Lock()
	if d.get(c.Slug) != c {
		d.lifeMu.Unlock()
		// Disabled or replaced meanwhile: the new one is never served.
		lo.Printf("denma: center %s changed while it reloaded: the reload is dropped", c.Slug)
		denmaRetire(next.app, false)
		next.app.manager.DenmaReleaseScan()
		denmaSignalReload(next.app) // ends its watchReload
		return
	}
	d.set(next)
	before := d.stopsOf(c.Slug)
	end := d.beginStop(c.Slug)
	d.lifeMu.Unlock()
	// The old App stops after the lock is let go; the new one then picks up campaigns.
	d.retireThen(old, next.app, before, end)
}

// watchBaseReload rebuilds the hub when its settings are saved: a new App, router and DB pool.
func (d *denmaCenters) watchBaseReload(old *App) {
	<-old.chReload
	lo.Printf("denma: reloading the hub")

	bk := koanf.New(".")
	_ = bk.Load(confmap.Provider(old.ko.All(), "."), nil)

	app, router, err := func() (*App, *echo.Echo, error) {
		bdb, err := denmaConnect(bk)
		if err != nil {
			return nil, nil, err
		}
		qMap := readQueries(queryFilePath, fs)
		initSettings(qMap["get-settings"].Query, bdb, bk)
		app := buildApp(bk, bdb, prepareQueries(qMap, bdb, bk), true)
		return app, initHTTPRouter(app.cfg, app.urlCfg, app.i18n, fs, app), nil
	}()
	if err != nil {
		lo.Printf("denma: error reloading the hub: %v", err)
		go d.watchBaseReload(old) // keep serving the old one
		return
	}

	d.mu.Lock()
	d.reBase, d.reRouter = app, router
	d.mu.Unlock()
	go d.watchBaseReload(app)
	go d.propagateShared()

	// main() closes the original App when the process restarts.
	denmaRetire(old, old == d.base)
}

// denmaRetireWait is how long a retired App's running campaigns have to stop and save.
const denmaRetireWait = time.Minute

// denmaRetire shuts down a replaced or disabled App; its campaigns stop and save their progress.
func denmaRetire(old *App, original bool) (stopped bool) {
	first := false
	old.retired.Do(func() { first = true })
	if !first {
		return true // closing its manager and messengers twice would panic
	}
	old.manager.StopScanning()
	stopped = old.manager.DenmaStop(denmaRetireWait)
	if !stopped {
		old.log.Printf("denma: a campaign didn't save its progress in %v; it resumes from its last checkpoint", denmaRetireWait)
	}
	if old.bounce != nil {
		old.bounce.StopScanning() // the hub's bounce mailbox
	}
	jobs := context.Background()
	if old.crons != nil {
		jobs = old.crons.Stop()
	}

	go func() {
		time.Sleep(5 * time.Second)
		// Its jobs finish first (cmd/denma_automations.go).
		select {
		case <-jobs.Done():
		case <-time.After(10 * time.Minute):
		}
		for old.importer.GetStats().Status == subimporter.StatusImporting ||
			old.importer.GetStats().Status == subimporter.StatusStopping {
			time.Sleep(10 * time.Second)
		}
		if original {
			old.db.SetMaxIdleConns(0) // let its connections go; main() closes it
			return
		}
		// Waits for campaigns still sending.
		for deadline := time.Now().Add(denmaStragglerWait); !stopped && old.manager.HasRunningCampaigns(); {
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(5 * time.Second)
		}
		old.manager.Close()
		for _, m := range old.messengers {
			m.Close()
		}
		manager.DenmaDeliveriesWait(30 * time.Second) // automation e-mails being sent record how they went first
		old.db.Close()
	}()
	return stopped
}

// denmaEmailMessenger is the app's default "email" messenger, if any.
func denmaEmailMessenger(msgrs []manager.Messenger) *email.Emailer {
	for _, m := range msgrs {
		if m.Name() == "email" {
			return m.(*email.Emailer)
		}
	}
	return nil
}

// tmpKey scopes a key in the tmptokens store (password reset, 2FA) to the app's center.
func (a *App) tmpKey(k string) string {
	return a.urlCfg.RootURL + " " + k
}

// denmaPrefixWriter puts the center's /c/<slug> prefix on redirects to root-relative paths.
type denmaPrefixWriter struct {
	http.ResponseWriter
	prefix string
	done   bool
}

func (w *denmaPrefixWriter) fix() {
	if w.done {
		return
	}
	w.done = true
	loc := w.Header().Get("Location")
	if strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//") &&
		loc != w.prefix && !strings.HasPrefix(loc, w.prefix+"/") {
		w.Header().Set("Location", w.prefix+loc)
	}
}

func (w *denmaPrefixWriter) WriteHeader(code int) {
	w.fix()
	w.ResponseWriter.WriteHeader(code)
}

func (w *denmaPrefixWriter) Write(b []byte) (int, error) {
	w.fix()
	return w.ResponseWriter.Write(b)
}

// Flush keeps event streams (text/event-stream) working through the wrapper.
func (w *denmaPrefixWriter) Flush() {
	w.fix()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
