package main

// denma: many centers in one listmonk process.
//
// With denma.multi_center = true, the install itself is the hub, Listmonk
// Shambhala, at /: the superadmins' home (cmd/denma_hub.go), in its own
// Postgres schema (db.params search_path, e.g. "hub"; prepared by
// denmaPrepareHub). Every center registered in denma.centers is served at
// /c/<slug>/ from its own schema: its own lists, subscribers, campaigns,
// templates, media, settings and users. Each center is a full App (buildApp)
// with its own config, DB pool, campaign manager and router (initHTTPRouter);
// the hub's server hands /c/<slug>/ requests to that center's router.
//
// Centers are provisioned (schema created, listmonk installed, sample data
// removed, roles, URLs and uploads set) when first loaded, and upgraded to the
// hub's database version when loaded after a listmonk upgrade. A center can
// also be an existing install (DDL's, in the public schema): it keeps its
// data. Settings are the hub's, for every center (all but each center's own,
// denmaCenterOwnSettings), and only the hub shows them. Saving the hub's
// settings rebuilds the hub and then each center they changed; only an OS
// SIGHUP restarts the whole process.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

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

// denmaCenterOwnSettings are a center's own settings: its address and uploads
// folder, set when it's created, and what its admins set on its Advanced
// page. Every other setting is the hub's, for all centers: they're
// managed in the hub's Settings only (centers don't show settings) and copied
// to each center when it loads and when the hub's are saved.
var denmaCenterOwnSettings = []string{
	"app.root_url", "upload.filesystem.upload_path", "upload.s3.bucket_path",
	"migrations", // the schema's own version
	// And those on the center's Advanced page (cmd/denma_center.go).
	"app.site_name", "app.logo_url", "app.favicon_url", "app.lang",
	"app.from_email", "app.notify_emails",
}

// denmaCenterSettingsPath reports whether a path (in a center) is one of the
// settings pages or APIs, which only the hub has: Settings, Logs, Maintenance
// and System. A center's reload and error events stay (they're its own).
func denmaCenterSettingsPath(p string) bool {
	for _, pre := range []string{"/admin/settings", "/api/settings", "/api/logs", "/api/maintenance"} {
		if p == pre || strings.HasPrefix(p, pre+"/") {
			return true
		}
	}
	return false
}

// denmaCenterAdminRole is the role a center's own admins get: everything but
// the settings, which superadmins manage, and raw SQL subscriber queries,
// which could read other centers' schemas (all centers share a database user).
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

	// Settings set when the center is provisioned (by the hub).
	initial map[string]any
}

// denmaCenters holds the loaded centers, by slug.
type denmaCenters struct {
	mu     sync.RWMutex
	bySlug map[string]*denmaCenter
	base   *App // the hub, served at /

	// The hub once reloaded: its App and router. Until then, main()'s server
	// and App serve it.
	reBase   *App
	reRouter *echo.Echo

	baseSchema string            // the hub's schema
	failed     map[string]string // centers that didn't load: slug -> error
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

// initDenmaCenters loads the registered centers and mounts them on the hub's
// server. It does nothing unless denma.multi_center is on.
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

	d := &denmaCenters{bySlug: make(map[string]*denmaCenter, len(list)), base: base, failed: map[string]string{}}
	if err := base.db.Get(&d.baseSchema, `SELECT current_schema()`); err != nil {
		lo.Fatalf("denma: error reading the hub's schema: %v", err)
	}
	denmaHub = d

	start := time.Now()
	for _, c := range list {
		if err := d.load(c); err != nil {
			// One broken center shouldn't take the others down.
			lo.Printf("denma: center %s not loaded: %v", c.Slug, err)
			d.setFailed(c.Slug, err)
			continue
		}
		d.set(c)
	}
	lo.Printf("denma: %d of %d centers loaded in %s", len(d.bySlug), len(list), time.Since(start).Round(time.Millisecond))

	// The hub's settings saves (and Reload) rebuild it in place, as for the
	// centers: they signal on a channel of their own, while main() keeps the
	// original one, which now only gets OS SIGHUPs.
	base.chReload = make(chan os.Signal, 1)
	go d.watchBaseReload(base)

	// /c/<slug>/... goes to that center's router, with the prefix removed;
	// anything else to the hub's (main()'s, until it's reloaded).
	srv.Pre(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			p := c.Request().URL.Path
			if !strings.HasPrefix(p, denmaCenterPath) {
				// The hub has no subscribers of its own: of listmonk's public
				// pages it keeps only what its admin needs. (Links in e-mails
				// sent before an install became a center, at /, are the web
				// server's to forward to /c/<slug>/.)
				switch {
				case p == "/":
					return c.Redirect(http.StatusFound, uriAdmin)
				case !denmaHubPath(p):
					return echo.NewHTTPError(http.StatusNotFound, "not found")
				}
				if r := d.baseRouter(); r != nil {
					r.ServeHTTP(c.Response(), c.Request())
					return nil
				}
				return next(c)
			}
			slug, rest, _ := strings.Cut(strings.TrimPrefix(p, denmaCenterPath), "/")
			ctr := d.get(slug)
			if ctr == nil {
				return echo.NewHTTPError(http.StatusNotFound, "center not found")
			}
			// Settings are the hub's: its pages, and none of the APIs here.
			if denmaCenterSettingsPath("/" + rest) {
				if strings.HasPrefix(rest, "admin/") {
					return c.Redirect(http.StatusFound, path.Join(d.current().urlCfg.RootPath, "/"+rest))
				}
				return echo.NewHTTPError(http.StatusForbidden, "Settings are managed in the hub, for all centers.")
			}
			r := c.Request().Clone(c.Request().Context())
			r.URL.Path = "/" + rest
			r.URL.RawPath = ""
			ctr.router.ServeHTTP(&denmaPrefixWriter{ResponseWriter: c.Response(), prefix: denmaCenterPath + slug}, r)
			return nil
		}
	})
}

// denmaHubPath reports whether the hub serves a path: its admin and API, the
// health check, and the static files its pages use.
func denmaHubPath(p string) bool {
	for _, pre := range []string{"/admin", "/api/", "/public/", "/health"} {
		if p == strings.TrimSuffix(pre, "/") || strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// denmaInitRegistry creates the center registry (schema denma) if needed.
func denmaInitRegistry(db *sqlx.DB) error {
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
		-- Addresses that can't receive mail, blocklisted in every center
		-- (cmd/denma_blocklist.go).
		CREATE TABLE IF NOT EXISTS denma.blocked_emails (
			email      TEXT PRIMARY KEY, -- lowercase
			reason     TEXT NOT NULL,
			center     TEXT NOT NULL, -- the slug of the center that found it
			created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
		);`)
	return err
}

// denmaSeedCenters registers centers listed as "slug:Name,slug2:Name 2" if
// they aren't registered yet. "slug:Name:schema" registers an existing
// install (such as DDL's, "ddl:Dorje Denma Ling:public") as a center that
// keeps its data. New centers are made in the hub.
func denmaSeedCenters(db *sqlx.DB, list string) {
	for _, item := range strings.Split(list, ",") {
		parts := strings.SplitN(strings.TrimSpace(item), ":", 3)
		slug, name, schema := parts[0], "", ""
		if len(parts) > 1 {
			name = parts[1]
		}
		if len(parts) > 2 {
			schema = parts[2]
		}
		if slug == "" {
			continue
		}
		if _, err := denmaRegisterCenter(db, slug, name, schema); err != nil {
			lo.Printf("denma: error registering center %s: %v", slug, err)
		}
	}
}

// denmaRegisterCenter adds a center to the registry (it's provisioned when
// loaded), in schema center_<slug> unless one is given. It returns the new
// center, or nil if the slug or schema is taken.
func denmaRegisterCenter(db *sqlx.DB, slug, name, schema string) (*denmaCenter, error) {
	if !reDenmaSlug.MatchString(slug) || len(slug) > 50 {
		return nil, fmt.Errorf("invalid address %q: use lowercase letters, digits and single hyphens", slug)
	}
	if strings.TrimSpace(name) == "" {
		name = slug
	}
	if schema == "" {
		schema = "center_" + strings.ReplaceAll(slug, "-", "_")
	}
	if !reDenmaSchema.MatchString(schema) {
		return nil, fmt.Errorf("invalid schema name %q", schema)
	}
	var out []*denmaCenter
	err := db.Select(&out, `INSERT INTO denma.centers (slug, name, schema_name) VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING RETURNING id, slug, name, schema_name`,
		slug, name, schema)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return out[0], nil
}

// load opens a center: provisions its schema if new, then builds its App and
// router, and watches for its reload signal (a settings save).
func (d *denmaCenters) load(c *denmaCenter) error {
	if c.Schema == d.baseSchema {
		return fmt.Errorf("the center's schema (%s) is the hub's", c.Schema)
	}
	ck := d.centerConfig(c)

	cdb, err := denmaConnect(ck)
	if err != nil {
		return err
	}
	if err := d.provision(c, cdb); err != nil {
		cdb.Close()
		return err
	}
	if err := d.upgrade(c, cdb); err != nil {
		cdb.Close()
		return err
	}
	if err := d.adopt(c, cdb); err != nil {
		cdb.Close()
		return err
	}
	if _, err := d.syncShared(cdb); err != nil {
		cdb.Close()
		return fmt.Errorf("copying the shared settings: %v", err)
	}

	qMap := readQueries(queryFilePath, fs)
	initSettings(qMap["get-settings"].Query, cdb, ck)
	cq := prepareQueries(qMap, cdb, ck)

	app := buildApp(ck, cdb, cq, false)
	// Never listmonk's first-run "create the Super Admin" page: a center's
	// users come from the hub, and until they do, anyone could claim it.
	app.needsUserSetup = false
	c.app = app
	c.router = initHTTPRouter(app.cfg, app.urlCfg, app.i18n, fs, app)

	go d.watchReload(c)
	return nil
}

// centerConfig is the hub's config (files, env, flags) with the
// center's own schema and a small DB pool. Its settings are loaded over it.
// (Those are the original's settings; every key is overwritten by the center's.)
//
// The search path is the center's schema ONLY. With other schemas on it, a
// name the center's schema lacks resolves elsewhere: listmonk's installer
// starts with "DROP TABLE IF EXISTS subscribers CASCADE" and friends, which on
// a new, empty schema would drop another schema's tables.
func (d *denmaCenters) centerConfig(c *denmaCenter) *koanf.Koanf {
	ck := koanf.New(".")
	_ = ck.Load(confmap.Provider(d.base.ko.All(), "."), nil)

	params := strings.TrimSpace(d.base.ko.String("db.params") + " search_path=" + c.Schema)
	_ = ck.Set("db.params", params)
	_ = ck.Set("denma.center", c.Slug)
	_ = ck.Set("db.max_open", d.base.ko.Int("denma.center_db_max_open"))
	_ = ck.Set("db.max_idle", d.base.ko.Int("denma.center_db_max_idle"))
	if ck.Int("db.max_open") < 1 {
		_ = ck.Set("db.max_open", 4)
	}
	return ck
}

// denmaConnect opens a center's DB pool. initDB exits the process on failure,
// which one center shouldn't do, so the connection is tested first.
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
	test, err := sqlx.Connect("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("connecting to the database: %v", err)
	}
	test.Close()
	return initDB(ck), nil
}

// provision creates and installs the center's schema if it doesn't exist yet,
// and points its URLs and uploads at /c/<slug>/.
func (d *denmaCenters) provision(c *denmaCenter, db *sqlx.DB) error {
	var installed bool
	if err := db.Get(&installed, `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = $1 AND table_name = 'settings')`, c.Schema); err != nil {
		return err
	}
	if installed {
		return nil
	}

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

	install(migList[len(migList)-1].version, db, fs, false, false)

	// A clean center: none of listmonk's samples, and not the admin that
	// LISTMONK_ADMIN_USER creates (that's the hub's superadmin, with its password).
	if _, err := db.Exec(`DELETE FROM campaigns; DELETE FROM subscribers; DELETE FROM lists; DELETE FROM sessions; DELETE FROM users;`); err != nil {
		return fmt.Errorf("removing the sample data: %v", err)
	}
	if err := d.provisionRoles(db); err != nil {
		return err
	}

	base := d.base.ko
	uploads := strings.TrimSuffix(base.String("upload.filesystem.upload_path"), "/")
	if uploads == "" {
		uploads = "uploads"
	}
	settings := map[string]any{
		"app.root_url":                  strings.TrimSuffix(d.current().urlCfg.RootURL, "/") + denmaCenterPath + c.Slug,
		"app.site_name":                 c.Name,
		"upload.filesystem.upload_path": path.Join(uploads, c.Slug),
		"upload.s3.bucket_path":         path.Join("/", base.String("upload.s3.bucket_path"), c.Slug),
	}
	for k, v := range c.initial {
		settings[k] = v
	}
	for k, v := range settings {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := db.Exec(`UPDATE settings SET value = $1::jsonb, updated_at = NOW() WHERE key = $2`, string(b), k); err != nil {
			return fmt.Errorf("setting %s: %v", k, err)
		}
	}
	return nil
}

// provisionRoles creates a center's roles if they're missing: Super Admin,
// which must be role 1 (auth.SuperAdminRoleID), for superadmins, and Center
// Admin. Super Admin keeps every permission, including ones added since (such
// as center:manage), as the admin's menus check the role's list.
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

// adopt keeps a center in step with the hub on every load: its address under
// the hub's (also for an existing install, such as DDL's, whose links move
// from / to /c/<slug>/), and the roles the hub relies on.
func (d *denmaCenters) adopt(c *denmaCenter, db *sqlx.DB) error {
	root := strings.TrimSuffix(d.current().urlCfg.RootURL, "/") + denmaCenterPath + c.Slug
	if _, err := db.Exec(`UPDATE settings SET value = to_jsonb($1::text), updated_at = NOW()
		WHERE key = 'app.root_url' AND value IS DISTINCT FROM to_jsonb($1::text)`, root); err != nil {
		return fmt.Errorf("setting the center's address: %v", err)
	}
	return d.provisionRoles(db)
}

// denmaCryptoFuncs makes listmonk's pgcrypto functions, which live in the
// extension's own schema (public), callable from a schema that has the
// search path to itself (a center's or the hub's).
func denmaCryptoFuncs(db *sqlx.DB) error {
	var ext string
	if err := db.Get(&ext, `SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = 'pgcrypto'`); err != nil {
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

// denmaPrepareHub readies the hub's own schema before listmonk checks it
// (main.go's init()), in multi-center mode: the search path must be one schema
// of the hub's own (db.params "search_path=hub"), never public, which may hold
// a center (DDL's install). On the first start it creates the schema and
// installs listmonk there, without the sample data, named Listmonk Shambhala,
// with the settings of the install in public if there is one (DDL's), which
// become every center's.
func denmaPrepareHub(db *sqlx.DB) {
	if !ko.Bool("denma.multi_center") {
		return
	}
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
	var hasPublic bool
	if err := db.Get(&hasPublic, `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'settings')`); err != nil {
		lo.Fatalf("denma: error checking for an install in public: %v", err)
	}
	if hasPublic {
		// Its settings become every center's (all but its name and version).
		if _, err := db.Exec(`UPDATE settings h SET value = p.value, updated_at = NOW() FROM public.settings p
			WHERE p.key = h.key AND h.key NOT IN ('app.site_name', 'migrations')`); err != nil {
			lo.Fatalf("denma: error copying the settings from public: %v", err)
		}
		lo.Printf("denma: the hub's settings were copied from the install in public")
	}
}

// upgrade brings a center's schema to the binary's version. The hub's schema
// already is (listmonk won't start otherwise), so running listmonk --upgrade,
// after a backup of the database, upgrades every center as it loads. Unlike
// upgrade(), a failure stops only this center.
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

// syncShared copies the hub's settings to a center (db is the center's pool),
// all but the center's own, reporting whether any changed.
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

// propagateShared copies the hub's changed settings to every center and
// reloads those that changed, one at a time. A center that is sending a
// campaign is reloaded once it has finished (it shows "needs restart" until
// then, as listmonk does for a settings save).
func (d *denmaCenters) propagateShared() {
	for _, c := range d.loaded() {
		changed, err := d.syncShared(c.app.db)
		if err != nil {
			lo.Printf("denma: error copying the hub's settings to center %s: %v", c.Slug, err)
			continue
		}
		if !changed {
			continue
		}
		if c.app.manager.HasRunningCampaigns() {
			c.app.Lock()
			c.app.needsRestart = true
			c.app.Unlock()
			go d.reloadWhenIdle(c)
			continue
		}
		denmaSignalReload(c.app)
		time.Sleep(200 * time.Millisecond)
	}
}

// reloadWhenIdle reloads a center once it has no running campaigns, unless it
// has been reloaded (or disabled) in the meantime.
func (d *denmaCenters) reloadWhenIdle(c *denmaCenter) {
	for {
		time.Sleep(time.Minute)
		if cur := d.get(c.Slug); cur == nil || cur.app != c.app {
			return
		}
		if !c.app.manager.HasRunningCampaigns() {
			lo.Printf("denma: center %s finished sending; reloading it with the hub's settings", c.Slug)
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
	if _, err := d.base.db.Exec(`UPDATE denma.centers SET status = 'disabled' WHERE slug = $1`, slug); err != nil {
		return err
	}
	d.mu.Lock()
	c := d.bySlug[slug]
	delete(d.bySlug, slug)
	delete(d.failed, slug)
	d.mu.Unlock()
	if c != nil {
		lo.Printf("denma: center %s disabled", slug)
		denmaRetire(c.app, false)
	}
	return nil
}

// enable loads a center and serves it again.
func (d *denmaCenters) enable(slug string) error {
	var c denmaCenter
	if err := d.base.db.Get(&c, `SELECT id, slug, name, schema_name FROM denma.centers WHERE slug = $1`, slug); err != nil {
		return err
	}
	if d.get(slug) != nil {
		return nil
	}
	if err := d.load(&c); err != nil {
		d.setFailed(slug, err)
		return err
	}
	if _, err := d.base.db.Exec(`UPDATE denma.centers SET status = 'enabled' WHERE slug = $1`, slug); err != nil {
		return err
	}
	d.set(&c)
	lo.Printf("denma: center %s enabled", slug)
	return nil
}

// watchReload rebuilds the center when its settings are saved.
func (d *denmaCenters) watchReload(c *denmaCenter) {
	old := c.app
	<-old.chReload
	lo.Printf("denma: reloading center %s", c.Slug)

	next := &denmaCenter{ID: c.ID, Slug: c.Slug, Name: c.Name, Schema: c.Schema}
	if err := d.load(next); err != nil {
		lo.Printf("denma: error reloading center %s: %v", c.Slug, err)
		go d.watchReload(c) // keep serving the old one
		return
	}
	d.set(next)
	denmaRetire(old, false)
}

// watchBaseReload rebuilds the hub when its settings are saved:
// a new App and router on a new DB pool, with the settings read afresh.
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

	// main() closes the original App when the process restarts, and its
	// manager and messengers can't be closed twice.
	denmaRetire(old, old == d.base)
}

// denmaRetire shuts down an App that has been replaced: at once, its campaign
// scanning and cron jobs (the replacement has its own); after a grace period
// for in-flight requests, and any import running in it, the rest.
func denmaRetire(old *App, original bool) {
	old.manager.StopScanning()
	if old.crons != nil {
		old.crons.Stop()
	}

	go func() {
		time.Sleep(5 * time.Second)
		for old.importer.GetStats().Status == subimporter.StatusImporting ||
			old.importer.GetStats().Status == subimporter.StatusStopping {
			time.Sleep(10 * time.Second)
		}
		if original {
			old.db.SetMaxIdleConns(0) // let its connections go; main() closes it
			return
		}
		old.manager.Close()
		for _, m := range old.messengers {
			m.Close()
		}
		old.db.Close()
	}()
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

// tmpKey scopes a key in the process-wide tmptokens store (password reset and
// 2FA tokens) to the app's center, so that one center's tokens are no good in
// another.
func (a *App) tmpKey(k string) string {
	return a.urlCfg.RootURL + " " + k
}

// denmaPrefixWriter puts the center's /c/<slug> prefix on redirects to
// root-relative paths: listmonk redirects to "/admin", "/admin/login" and so
// on, which from a center mean that center's pages, not the hub's.
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
