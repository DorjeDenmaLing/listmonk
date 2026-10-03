package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gdgvda/cron"
	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/internal/bounce"
	"github.com/knadh/listmonk/internal/buflog"
	"github.com/knadh/listmonk/internal/captcha"
	"github.com/knadh/listmonk/internal/core"
	"github.com/knadh/listmonk/internal/events"
	"github.com/knadh/listmonk/internal/i18n"
	"github.com/knadh/listmonk/internal/manager"
	"github.com/knadh/listmonk/internal/media"
	"github.com/knadh/listmonk/internal/notifs"
	"github.com/knadh/listmonk/internal/subimporter"
	"github.com/knadh/listmonk/models"
	"github.com/knadh/paginator/v2"
	"github.com/knadh/stuffbin"
)

// App contains the "global" shared components, controllers and fields.
type App struct {
	ko         *koanf.Koanf // denma: the app's own config (one App per center)
	cfg        *Config
	urlCfg     *UrlConfig
	fs         stuffbin.FileSystem
	db         *sqlx.DB
	queries    *models.Queries
	core       *core.Core
	manager    *manager.Manager
	messengers []manager.Messenger
	emailMsgr  manager.Messenger
	importer   *subimporter.Importer
	auth       *auth.Auth
	media      media.Store
	bounce     *bounce.Manager
	captcha    *captcha.Captcha
	i18n       *i18n.I18n
	pg         *paginator.Paginator
	events     *events.Events
	log        *log.Logger
	bufLog     *buflog.BufLog
	notifs     *notifs.Notifs // denma: the app's own e-mail notifier (one App per center)
	crons      *cron.Cron     // denma: stopped when a center is reloaded

	about         about
	fnOptinNotify func(models.Subscriber, []int) (int, error)

	// Precomputed raw JSON of i18n strings for HTML admin pages.
	adminI18nJS json.RawMessage

	// Channel for passing reload signals.
	chReload chan os.Signal

	// Global variable that stores the state indicating that a restart is required
	// after a settings update.
	needsRestart bool

	// First time installation with no user records in the DB. Needs user setup.
	needsUserSetup bool

	// Global state that stores data on an available remote update.
	update *AppUpdate
	sync.Mutex
}

var (
	// Buffered log writer for storing N lines of log entries for the UI.
	evStream = events.New()
	bufLog   = buflog.New(5000)
	lo       = log.New(io.MultiWriter(os.Stdout, bufLog, evStream.ErrWriter()), "", log.Ldate|log.Ltime|log.Lmicroseconds|log.Lshortfile)

	ko      = koanf.New(".")
	fs      stuffbin.FileSystem
	db      *sqlx.DB
	queries *models.Queries

	// Compile-time variables.
	buildString   string
	versionString string

	// If this is set in build ldflags and static assets (*.sql, config.toml.sample)
	// are not embedded (in make dist), this path is looked up. The default value, when not
	// overridden by build flags, is relative to the CWD at runtime.
	appDir string = "."
)

func init() {
	// Initialize commandline flags.
	initFlags(ko)

	// Display version.
	if ko.Bool("version") {
		fmt.Println(buildString)
		os.Exit(0)
	}

	lo.Println(buildString)

	// Generate new config.
	if ko.Bool("new-config") {
		path := ko.Strings("config")[0]
		if err := newConfigFile(path); err != nil {
			lo.Println(err)
			os.Exit(1)
		}
		lo.Printf("generated %s. Edit and run --install", path)
		os.Exit(0)
	}

	// Load config files to pick up the database settings first.
	initConfigFiles(ko.Strings("config"), ko)

	// Load environment variables and merge into the loaded config.
	// LISTMONK_foo__bar -> foo.bar (double underscore becomes dot for nested config)
	// LISTMONK_static_dir -> static-dir (top-level keys with underscore become hyphen for CLI flags)
	if err := ko.Load(env.Provider("LISTMONK_", ".", func(s string) string {
		key := strings.ToLower(strings.TrimPrefix(s, "LISTMONK_"))
		key = strings.Replace(key, "__", ".", -1)
		// Only convert underscore to hyphen for top-level keys (CLI flags like static-dir, i18n-dir)
		// Nested config keys (containing dots) keep underscores (e.g., db.ssl_mode)
		if !strings.Contains(key, ".") {
			key = strings.Replace(key, "_", "-", -1)
		}
		return key
	}), nil); err != nil {
		lo.Fatalf("error loading config from env: %v", err)
	}

	// Connect to the database.
	db = initDB(ko)

	// Initialize the embedded filesystem with static assets.
	fs = initFS(appDir, ko.String("static-dir"), ko.String("i18n-dir"))

	// Installer mode? This runs before the SQL queries are loaded and prepared
	// as the installer needs to work on an empty DB.
	if ko.Bool("install") {
		// Save the version of the last listed migration.
		install(migList[len(migList)-1].version, db, fs, !ko.Bool("yes"), ko.Bool("idempotent"))
		os.Exit(0)
	}

	denmaPrepareHub(db) // denma: the multi-center hub's own schema (cmd/denma_centers.go)

	// Is this a nightly build?
	isNightly := strings.Contains(versionString, "nightly")

	// Check if the DB schema is installed.
	if ok, err := checkSchema(db); err != nil {
		log.Fatalf("error checking schema in DB: %v", err)
	} else if !ok {
		lo.Fatal("the database does not appear to be setup. Run --install.")
	}

	if ko.Bool("upgrade") {
		// Even on explicit upgrade runs, for nightly builds, do not record the last
		// migration version in the DB.
		lo.Printf("running upgrade...")
		upgrade(db, fs, !ko.Bool("yes"), !isNightly)
		os.Exit(0)
	}

	// For nightly builds, always auto-run pending migrations without
	// recording the last version in the DB. Migrations are idempotent, and between
	// nightly releases, they may change multiple times.
	if isNightly {
		lo.Printf("auto-running all migrations for nightly %s since last major version", versionString)
		upgrade(db, fs, false, false)
	} else {
		// Before the queries are prepared, see if there are pending upgrades.
		checkUpgrade(db)
	}

	// Read the SQL queries from the queries file.
	qMap := readQueries(queryFilePath, fs)

	// Load settings from DB.
	if q, ok := qMap["get-settings"]; ok {
		initSettings(q.Query, db, ko)
	}

	// Prepare queries.
	queries = prepareQueries(qMap, db, ko)
}

func main() {
	app := buildApp(ko, db, queries, true) // denma: shared with the centers (cmd/denma_centers.go)
	var (
		cfg      = app.cfg
		urlCfg   = app.urlCfg
		i18n     = app.i18n
		mgr      = app.manager
		chReload = app.chReload
	)

	// Star the update checker.
	if ko.Bool("app.check_updates") {
		go app.checkUpdates(versionString, time.Hour*24)
	}

	// Start the app server.
	srv := initHTTPServer(cfg, urlCfg, i18n, fs, app)

	// =========================================================================
	// Wait for the reload signal with a callback to gracefully shut down resources.
	// The `wait` channel is passed to awaitReload to wait for the callback to finish
	// within N seconds, or do a force reload.
	signal.Notify(chReload, syscall.SIGHUP)

	closerWait := make(chan bool)
	<-awaitReload(chReload, closerWait, func() {
		// Stop the HTTP server.
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		srv.Shutdown(ctx)

		// Close the campaign manager.
		mgr.Close()

		// Close the DB pool.
		db.Close()

		// Close the messenger pool.
		for _, m := range app.messengers {
			m.Close()
		}

		// Signal the close.
		closerWait <- true
	})
}

// buildApp initializes the App and its components from a config, DB and
// queries, and starts its background workers (campaign manager, bounces, cron).
// denma: moved out of main() so that each center is built the same way.
func buildApp(ko *koanf.Koanf, db *sqlx.DB, queries *models.Queries, withNotifs bool) *App {
	var (
		// Initialize static global config.
		cfg = initConstConfig(ko)

		// Initialize static URL config.
		urlCfg = initUrlConfig(ko)

		// Initialize i18n language map.
		i18n = initI18n(ko.MustString("app.lang"), fs)

		// Initialize the media store.
		media = initMediaStore(ko)

		// Initialize all messengers, SMTP and postback.
		msgrs = append(initSMTPMessengers(ko), initPostbackMessengers(ko)...)

		// denma: this app's admin/sub e-mail notifier (was process-wide, set up below).
		emailMsgr = denmaEmailMessenger(msgrs)
		nf        = initNotifs(fs, i18n, emailMsgr, urlCfg, ko)

		fbOptinNotify = denmaCheckOptin(makeOptinNotifyHook(ko.Bool("privacy.unsubscribe_header"), urlCfg, queries, i18n, nf), db, ko) // denma: cmd/denma_emailcheck.go

		// Crud core.
		core = initCore(fbOptinNotify, queries, db, i18n, ko)

		// Campaign manager.
		mgr = initCampaignManager(denmaLimitSending(msgrs, ko), queries, urlCfg, core, media, i18n, ko) // denma: one send limit for all centers (cmd/denma_sending.go)

		// Bulk importer.
		importer = initImporter(queries, db, core, i18n, ko, nf)

		// Initialize the auth manager.
		hasUsers, auth = initAuth(core, db.DB, ko)

		// Initialize the webhook/POP3 bounce processor.
		bounce *bounce.Manager

		chReload = make(chan os.Signal, 1)
	)

	// Initialize the bounce manager that processes bounces from webhooks and
	// POP3 mailbox scanning.
	if ko.Bool("bounce.enabled") && ko.String("denma.center") == "" { // denma: the hub takes every center's bounces (cmd/denma_bounces.go)
		bounce = initBounceManager(denmaBounceCB(core.RecordBounce, ko), queries.RecordBounce, lo, ko)
	}

	// Initialize the global admin/sub e-mail notifier.
	if withNotifs { // denma: the hub's is also the process-wide one
		notifs.SetDefault(nf)
	}
	mgr.SetNotify(func(subject string, data any) error { // denma: this app's notifier
		return nf.NotifySystem(subject, notifs.TplCampaignStatus, data, nil)
	})

	// Initialize and cache tx templates in memory.
	initTxTemplates(mgr, core)

	// Initialize the bounce manager that processes bounces from webhooks and
	// POP3 mailbox scanning.
	if bounce != nil { // denma: was ko.Bool("bounce.enabled"); not in centers
		go bounce.Run()
	}

	// Start cronjobs.
	crons := initCron(core, db, ko) // denma: kept to stop on reload

	// Start the campaign manager workers. The campaign batches (fetch from DB, push out
	// messages) get processed at the specified interval.
	go mgr.Run()

	// =========================================================================
	// Initialize the App{} with all the global shared components, controllers and fields.
	app := &App{
		ko:         ko,
		cfg:        cfg,
		urlCfg:     urlCfg,
		fs:         fs,
		db:         db,
		queries:    queries,
		core:       core,
		manager:    mgr,
		messengers: msgrs,
		emailMsgr:  emailMsgr,
		importer:   importer,
		auth:       auth,
		media:      media,
		bounce:     bounce,
		captcha:    initCaptcha(ko),
		i18n:       i18n,
		log:        lo,
		events:     evStream,
		bufLog:     bufLog,
		notifs:     nf,    // denma
		crons:      crons, // denma

		pg: paginator.New(paginator.Opt{
			DefaultPerPage: 20,
			MaxPerPage:     50,
			NumPageNums:    10,
			PageParam:      "page",
			PerPageParam:   "per_page",
			AllowAll:       true,
		}),

		fnOptinNotify: fbOptinNotify,
		about:         initAbout(queries, db),
		chReload:      chReload,

		// If there are no users, then the app needs to prompt for new user setup.
		needsUserSetup: !hasUsers,
	}

	// i18n JSON string for admin HTML pages.
	app.adminI18nJS = app.makeAdminJSI18n()

	denmaStartAutomations(app) // denma: cmd/denma_automations.go
	denmaStartEmailChecks(app) // denma: cmd/denma_emailcheck.go

	return app
}
