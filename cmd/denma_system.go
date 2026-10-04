package main

// denma: System (sidebar, under Settings): the server, database and host, as
// listmonk collects them, and in the hub, how the centers use them: Postgres
// connections and the centers' database sizes, the uploads folder and its
// disk, and the background jobs (each center's e-mail domain check and
// automations, the bounces the hub takes for all of them, and the shared
// blocklist). The build links to its commit in the fork's source, which the
// AGPL v3 requires offering.

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx/types"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

// denmaSourceURL is the fork's source, which the running version's users are
// offered (the sidebar's "Powered by listmonk", partials/denma/sidebar.html).
const denmaSourceURL = "https://github.com/DorjeDenmaLing/listmonk"

// denmaBuildCommit finds the commit in a build string: "v7.0.0 (#3d562dce
// 2026-10-04...)" from the Makefile, or "v7.0.0 (dev 3d562dce ...)" from the
// dev setup (docker/start.sh).
var denmaBuildCommit = regexp.MustCompile(`(?:#|dev )([0-9a-f]{7,40})\b`)

type denmaSystemView struct {
	adminView
	System systemStats
	About  about
	DB     struct {
		Version string  `json:"version"`
		SizeMB  float64 `json:"size_mb"`
	}
	CommitURL string // the build's commit in the fork, if the build names one
	Commit    string

	Hub *denmaSystemHub // in the hub only
}

// denmaSystemHub is what the hub adds to the page.
type denmaSystemHub struct {
	Startup denmaStartupInfo

	ConnsUsed, ConnsMax int // Postgres connections, from every client
	ConnsPct            int
	PoolApps            int // the hub and its running centers
	PoolOpen, PoolInUse int // their connections to PgBouncer
	PoolMaxEach         int // a center's most

	CentersSize string            // all centers' data
	Largest     []denmaSystemSize // the largest centers' data

	Uploads denmaSystemUploads
	Jobs    []denmaSystemJob

	BouncesSince  int    // received since the start
	BounceLastAgo string // the last received, since the start
	BounceLatest  string // the latest recorded in any center, and where
	Blocked       int    // addresses on the shared blocklist
	BlockedLast   string // when the last was added
}

type denmaSystemSize struct {
	Name string
	Size string
}

type denmaSystemUploads struct {
	Provider  string // filesystem or s3
	Bucket    string // s3
	Size      string // filesystem: everything in the folder
	Largest   []denmaSystemSize
	DiskUsed  string
	DiskFree  string
	DiskTotal string
	DiskPct   int
}

type denmaSystemJob struct {
	Name    string
	LastAgo string // the last completed run, in any center
	Recent  int    // centers that completed a run in the last few minutes
	Of      int    // running centers
}

// ViewDenmaSystem renders System: the server stats that upstream shows on the
// dashboard (getSystemStats in dashboard.go), what listmonk collects for
// /api/about (version, build, database and host), and in the hub, the
// centers' use of them. In multi-center mode it's the hub's page only.
func (a *App) ViewDenmaSystem(c echo.Context) error {
	if denmaHub != nil && a.urlCfg.RootPath != denmaHub.current().urlCfg.RootPath {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	v := newAdminView(c, a.i18n.T("dashboard.system"), "", "denma.system")
	if !v.Can("settings:get") {
		return echo.NewHTTPError(http.StatusForbidden, a.i18n.Ts("globals.messages.permissionDenied", "name", "settings:get"))
	}

	out := denmaSystemView{adminView: v, System: getSystemStats(), About: a.about}
	if m := denmaBuildCommit.FindStringSubmatch(a.about.Build); m != nil {
		out.Commit = m[1]
		out.CommitURL = denmaSourceURL + "/commit/" + m[1]
	}

	// The database's version and size, now (a.about has them from startup).
	var info types.JSONText
	if err := a.db.QueryRow(a.queries.GetDBInfo).Scan(&info); err != nil {
		a.log.Printf("error getting database info: %v", err)
	} else if err := info.Unmarshal(&out.DB); err != nil {
		a.log.Printf("error reading database info: %v", err)
	}

	if denmaHub != nil {
		out.Hub = denmaHub.systemInfo()
	}
	return c.Render(http.StatusOK, "admin-denma-system", out)
}

// systemInfo gathers the hub's part of the page. A figure that can't be had
// is left out (and logged), so the rest still shows.
func (d *denmaCenters) systemInfo() *denmaSystemHub {
	var (
		h    = &denmaSystemHub{Startup: denmaStartup.get()}
		base = d.current()
		db   = base.db
	)

	// Postgres connections, from every client (PgBouncer's to the server
	// included), and the app's pools to PgBouncer.
	if err := db.QueryRow(`SELECT COUNT(*) FILTER (WHERE backend_type = 'client backend'),
		current_setting('max_connections')::INT FROM pg_stat_activity`).Scan(&h.ConnsUsed, &h.ConnsMax); err != nil {
		lo.Printf("denma: error counting database connections: %v", err)
	} else if h.ConnsMax > 0 {
		h.ConnsPct = h.ConnsUsed * 100 / h.ConnsMax
	}
	loaded := d.loaded()
	h.PoolApps = len(loaded) + 1
	for _, s := range append([]*App{base}, denmaApps(loaded)...) {
		st := s.db.Stats()
		h.PoolOpen += st.OpenConnections
		h.PoolInUse += st.InUse
	}
	h.PoolMaxEach = base.ko.Int("denma.center_db_max_open")
	if h.PoolMaxEach < 1 {
		h.PoolMaxEach = 4 // centerConfig's default
	}

	reg, err := d.registered()
	if err != nil {
		lo.Printf("denma: error listing centers: %v", err)
	}
	names := make(map[string]string, len(reg)) // schema -> name
	for _, r := range reg {
		names[r.Schema] = r.Name
	}

	// The centers' data, the largest first.
	type size struct {
		Schema string `db:"schema"`
		Bytes  int64  `db:"bytes"`
	}
	// Postgres sizes every table's files (with 300 centers, most of a second),
	// and the sizes change slowly, so they're kept longer than the other figures.
	sizes, err := denmaCachedFor("system|sizes", 15*time.Minute, func() ([]size, error) {
		var out []size
		schemas := make([]string, 0, len(reg))
		for _, r := range reg {
			schemas = append(schemas, r.Schema)
		}
		err := db.Select(&out, `SELECT n.nspname AS schema, COALESCE(SUM(pg_total_relation_size(c.oid)), 0)::BIGINT AS bytes
			FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE c.relkind IN ('r', 'm') AND n.nspname = ANY($1)
			GROUP BY n.nspname ORDER BY bytes DESC`, pq.Array(schemas))
		return out, err
	})
	if err != nil {
		lo.Printf("denma: error getting the centers' database sizes: %v", err)
	}
	var total int64
	for i, s := range sizes {
		total += s.Bytes
		if i < 5 {
			h.Largest = append(h.Largest, denmaSystemSize{Name: names[s.Schema], Size: denmaBytes(s.Bytes)})
		}
	}
	h.CentersSize = denmaBytes(total)

	h.Uploads = denmaUploadsInfo(base, names, reg)
	h.Jobs = denmaJobsInfo(loaded)

	// Bounces: those the hub received since it started, and the latest any
	// center has recorded (which survives a restart).
	h.BouncesSince, h.BounceLastAgo = denmaBounces.get()
	type latest struct {
		At   *time.Time `db:"at"`
		Slug string     `db:"slug"`
	}
	l, err := denmaCached("system|bounce", func() (latest, error) {
		var out latest
		if len(loaded) == 0 {
			return out, nil
		}
		parts := make([]string, 0, len(loaded))
		for _, c := range loaded {
			parts = append(parts, fmt.Sprintf(`SELECT MAX(created_at) AS at, %s AS slug FROM %s.bounces`,
				pq.QuoteLiteral(c.Slug), pq.QuoteIdentifier(denmaSchemaName(c.Slug))))
		}
		err := db.Get(&out, strings.Join(parts, " UNION ALL ")+` ORDER BY at DESC NULLS LAST LIMIT 1`)
		return out, err
	})
	if err != nil {
		lo.Printf("denma: error getting the latest bounce: %v", err)
	} else if l.At != nil {
		name := l.Slug
		for _, r := range reg {
			if r.Slug == l.Slug {
				name = r.Name
			}
		}
		h.BounceLatest = denmaAgo(*l.At) + ", " + name
	}

	// The shared blocklist (cmd/denma_blocklist.go).
	var last *time.Time
	if err := db.QueryRow(`SELECT COUNT(*), MAX(created_at) FROM denma.blocked_emails`).Scan(&h.Blocked, &last); err != nil {
		lo.Printf("denma: error counting the shared blocklist: %v", err)
	} else if last != nil {
		h.BlockedLast = denmaAgo(*last)
	}
	return h
}

func denmaApps(cs []*denmaCenter) []*App {
	out := make([]*App, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.app)
	}
	return out
}

// denmaUploadsInfo is the uploads folder: on the filesystem, its size, the
// centers using the most (each has its own folder, <uploads>/<slug>), and its
// disk; on S3, only the bucket (sizing it would mean listing every object).
func denmaUploadsInfo(base *App, names map[string]string, reg []denmaCenterRef) denmaSystemUploads {
	u := denmaSystemUploads{Provider: base.ko.String("upload.provider")}
	if u.Provider == "s3" {
		u.Bucket = base.ko.String("upload.s3.bucket")
		return u
	}

	root := strings.TrimSuffix(base.ko.String("upload.filesystem.upload_path"), "/")
	if root == "" {
		root = "uploads"
	}
	slugNames := make(map[string]string, len(reg))
	for _, r := range reg {
		slugNames[r.Slug] = r.Name
	}

	type usage struct {
		total int64
		by    map[string]int64 // first folder (a center's slug) -> bytes
	}
	// Every file is looked at (with 300 centers, tens of thousands), and the
	// sizes change slowly, so they're kept longer than the other figures.
	us, err := denmaCachedFor("system|uploads", 15*time.Minute, func() (usage, error) {
		out := usage{by: map[string]int64{}}
		err := filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return nil // skip what can't be read
			}
			info, err := e.Info()
			if err != nil {
				return nil
			}
			out.total += info.Size()
			if rel, err := filepath.Rel(root, p); err == nil {
				if i := strings.IndexRune(rel, filepath.Separator); i > 0 {
					out.by[rel[:i]] += info.Size()
				}
			}
			return nil
		})
		return out, err
	})
	if err != nil {
		lo.Printf("denma: error measuring the uploads folder: %v", err)
	}
	u.Size = denmaBytes(us.total)
	slugs := make([]string, 0, len(us.by))
	for s := range us.by {
		if slugNames[s] != "" {
			slugs = append(slugs, s)
		}
	}
	sort.Slice(slugs, func(i, j int) bool { return us.by[slugs[i]] > us.by[slugs[j]] })
	for i, s := range slugs {
		if i == 5 {
			break
		}
		u.Largest = append(u.Largest, denmaSystemSize{Name: slugNames[s], Size: denmaBytes(us.by[s])})
	}

	if total, free, ok := denmaDiskUsage(root); ok && total > 0 {
		u.DiskTotal, u.DiskFree, u.DiskUsed = denmaBytes(int64(total)), denmaBytes(int64(free)), denmaBytes(int64(total-free))
		u.DiskPct = int((total - free) * 100 / total)
	}
	return u
}

// denmaJobNames are the background jobs (denmaEveryMinute), by name.
var denmaJobNames = []struct{ id, name string }{
	{"domain-check", "E-mail domain check"},
	{"automations", "Automations"},
}

// denmaJobRuns is when each job last finished, in each center.
var denmaJobRuns = struct {
	sync.Mutex
	m map[string]map[string]time.Time // job -> center slug -> when
}{m: map[string]map[string]time.Time{}}

// denmaJobRan records that a center's job finished a run.
func denmaJobRan(job, center string) {
	denmaJobRuns.Lock()
	defer denmaJobRuns.Unlock()
	if denmaJobRuns.m[job] == nil {
		denmaJobRuns.m[job] = map[string]time.Time{}
	}
	denmaJobRuns.m[job][center] = time.Now()
}

// denmaJobsInfo is each job's latest run, and how many of the running centers
// ran it in the last 3 minutes (they run every minute, each at its own second).
func denmaJobsInfo(loaded []*denmaCenter) []denmaSystemJob {
	denmaJobRuns.Lock()
	defer denmaJobRuns.Unlock()
	out := make([]denmaSystemJob, 0, len(denmaJobNames))
	for _, j := range denmaJobNames {
		v := denmaSystemJob{Name: j.name, Of: len(loaded)}
		var last time.Time
		for _, c := range loaded {
			at := denmaJobRuns.m[j.id][c.app.ko.String("denma.center")]
			if at.After(last) {
				last = at
			}
			if time.Since(at) < 3*time.Minute {
				v.Recent++
			}
		}
		v.LastAgo = denmaAgo(last)
		out = append(out, v)
	}
	return out
}

// denmaBounces counts the bounces the hub receives (cmd/denma_bounces.go).
var denmaBounces denmaBounceCount

type denmaBounceCount struct {
	sync.Mutex
	n    int
	last time.Time
}

func denmaBounceSeen() {
	denmaBounces.Lock()
	denmaBounces.n++
	denmaBounces.last = time.Now()
	denmaBounces.Unlock()
}

func (b *denmaBounceCount) get() (int, string) {
	b.Lock()
	defer b.Unlock()
	return b.n, denmaAgo(b.last)
}

// denmaStartupInfo is how the centers loaded when the process started
// (loadAll, cmd/denma_centers.go).
type denmaStartupInfo struct {
	Ago           string
	Loaded, Total int
	Took          string
}

var denmaStartup denmaStartupState

type denmaStartupState struct {
	sync.Mutex
	at            time.Time
	loaded, total int
	took          time.Duration
}

func (s *denmaStartupState) set(at time.Time, loaded, total int, took time.Duration) {
	s.Lock()
	s.at, s.loaded, s.total, s.took = at, loaded, total, took
	s.Unlock()
}

func (s *denmaStartupState) get() denmaStartupInfo {
	s.Lock()
	defer s.Unlock()
	if s.at.IsZero() {
		return denmaStartupInfo{}
	}
	return denmaStartupInfo{Ago: denmaAgo(s.at), Loaded: s.loaded, Total: s.total, Took: s.took.Round(100 * time.Millisecond).String()}
}

// denmaBytes is a size for people: "12.3 MB".
func denmaBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// denmaAgo is how long ago t was, for people: "3 minutes ago"; "" for none.
func denmaAgo(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	n, unit := 0, ""
	switch d := time.Since(t); {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		n, unit = int(d/time.Minute), "minute"
	case d < 48*time.Hour:
		n, unit = int(d/time.Hour), "hour"
	default:
		n, unit = int(d/(24*time.Hour)), "day"
	}
	if n != 1 {
		unit += "s"
	}
	return fmt.Sprintf("%d %s ago", n, unit)
}
