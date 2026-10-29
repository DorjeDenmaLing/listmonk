package main

// denma: the import's preview (Subscribers -> Import). Before an import is
// uploaded, the page sends the file and its settings here, and shows what
// the import would do, without changing anything:
//
//   - the file as read: the columns used and ignored, the lines it skips and
//     why, and its first rows as they'd be imported (e-mail, name, tags,
//     attributes), so a file from another system can be checked;
//   - the people: new to the center, already in it, already on each list,
//     and those blocklisted, by why (unsubscribed, complained, bounced,
//     blocklisted by an admin) and what the import does with them;
//   - the automations its lists and tags would start, if it runs them, and
//     the consent records it'd keep, if it keeps them (cmd/denma_imports.go);
//   - the tags: each with how many have it, and whether the center has it
//     (or it's another name for one of its tags, given as that tag).
//     The page offers to add the ones it doesn't, as Subscribers -> Tags
//     does (POST /api/denma/tags), before the upload; the import itself
//     still leaves out tags the center doesn't have (cmd/denma_tags.go).
//
// It reads the file as listmonk's importer does (internal/subimporter), and
// the database as the import's triggers do (cmd/denma_features.go,
// cmd/denma_optouts.go, cmd/denma_noconsent.go). Not in the activity log
// (it changes nothing: denmaAuditSkip's /preview), and it logs no addresses.

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/knadh/listmonk/internal/subimporter"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

const (
	// denmaImportSample is how many rows the preview shows.
	denmaImportSample = 10
	// denmaImportProblems is how many skipped lines it shows.
	denmaImportProblems = 20
)

// The importer's columns, and its header cleaning (non-ASCII, like a BOM).
var (
	denmaImportColumns = []string{"email", "name", "first_name", "last_name", "attributes"}
	reDenmaNonASCII    = regexp.MustCompile("[[:^ascii:]]")
)

func initDenmaImportHandlers(g *echo.Group, a *App) {
	g.POST("/api/denma/subscribers/import/preview", a.auth.Perm(a.DenmaImportPreview, "subscribers:import"))
}

// denmaImportPreview is what an import would do.
type denmaImportPreview struct {
	File     string   `json:"file"`     // the CSV read (from a ZIP, its name in it)
	Columns  []string `json:"columns"`  // the importer's columns found
	Ignored  []string `json:"ignored"`  // the file's other columns
	Error    string   `json:"error"`    // why it can't be imported at all
	Encoding string   `json:"encoding"` // what the file was read as if it isn't UTF-8 (subimporter.DenmaUTF8Reader), or ""
	Token    string   `json:"token"`    // stands for the file, kept for the next preview (cmd/denma_importkeep.go)

	Rows       int `json:"rows"`        // data rows read
	Valid      int `json:"valid"`       // rows with an address the import takes
	Addresses  int `json:"addresses"`   // different addresses among them
	Duplicates int `json:"duplicates"`  // rows repeating an earlier row's address
	NoName     int `json:"no_name"`     // rows without a name, named from their address
	BadAttribs int `json:"bad_attribs"` // rows whose attributes aren't a JSON object: imported without
	StopsAt    int `json:"stops_at"`    // a line the CSV can't be read past (the import stops there), or 0

	Problems     []denmaImportProblem `json:"problems"` // the first lines skipped
	ProblemCount int                  `json:"problem_count"`

	Attribs []denmaImportAttrib `json:"attribs"` // the attributes given, and how many rows have each
	Sample  []denmaImportRow    `json:"sample"`

	New      int                   `json:"new"`      // addresses not in the center
	Existing int                   `json:"existing"` // in it already
	Lists    []denmaImportListInfo `json:"lists"`
	Blocked  denmaImportBlocked    `json:"blocked"`

	Tags             []denmaImportTag `json:"tags"`
	ExistingWithTags int              `json:"existing_with_tags"` // existing subscribers the file gives tags
	CanAddTags       bool             `json:"can_add_tags"`

	Automations []string             `json:"automations"` // the active automations its lists and tags start, if asked (cmd/denma_imports.go)
	Consent     *denmaImportConsents `json:"consent"`     // the consent records it keeps, if asked (cmd/denma_imports.go)
	BlockDates  *denmaImportDates    `json:"block_dates"` // a blocklist import's dates, with a reason (cmd/denma_imports.go)
}

type denmaImportProblem struct {
	Line    int    `json:"line"`
	Value   string `json:"value"`
	Message string `json:"message"`
}

type denmaImportAttrib struct {
	Key     string `json:"key"`
	Rows    int    `json:"rows"`
	Dropped bool   `json:"dropped"` // one the center keeps itself: an import can't give it
}

// denmaImportRow is a row as it'd be imported.
type denmaImportRow struct {
	Line     int            `json:"line"`
	Email    string         `json:"email"`
	Name     string         `json:"name"`
	First    string         `json:"first_name"`
	Last     string         `json:"last_name"`
	Tags     []string       `json:"tags"`
	Attribs  map[string]any `json:"attribs"` // but tags and those dropped
	Dropped  []string       `json:"dropped"`
	Existing bool           `json:"existing"`
	Blocked  string         `json:"blocked"` // denmaImportBlocked's json name, or ""
	Consent  string         `json:"consent"` // the consent record kept, or why not (cmd/denma_imports.go)

	consent denmaImportConsentGiven
}

type denmaImportListInfo struct {
	ID           int    `db:"id" json:"id"`
	Name         string `db:"name" json:"name"`
	Optin        string `db:"optin" json:"optin"`
	Subscribed   int    `db:"subscribed" json:"subscribed"`     // in the file, on it already
	Unsubscribed int    `db:"unsubscribed" json:"unsubscribed"` // in the file, unsubscribed from it
}

// denmaImportBlocked counts the addresses that are, or will be, blocklisted,
// each under the first that applies.
type denmaImportBlocked struct {
	Complained   int `json:"complained"`   // marked the center's mail as spam
	Unsubscribed int `json:"unsubscribed"` // unsubscribed themselves
	Bounced      int `json:"bounced"`      // a hard bounce, or a domain that takes no mail
	Admin        int `json:"admin"`        // blocklisted in the center for another reason
	NoConsent    int `json:"no_consent"`   // not blocklisted, but added by the sending API without consent
}

type denmaImportTag struct {
	Tag      string `json:"tag"`
	Rows     int    `json:"rows"`     // addresses given it
	Existing int    `json:"existing"` // of them, already in the center
	Exists   bool   `json:"exists"`   // the center has the tag
	As       string `json:"as"`       // the tag it's another name for (cmd/denma_tagaliases.go)
}

// denmaImportAddr is an address in the file, as the center has it.
type denmaImportAddr struct {
	Email  string         `db:"email"`
	Found  bool           `db:"found"`
	Status string         `db:"status"`
	Auto   bool           `db:"auto"`
	OptOut sql.NullString `db:"optout"`
	Shared bool           `db:"shared"`
	Fenced bool           `db:"fenced"`

	HasConsent bool `db:"has_consent"` // a consent record already
}

// blocked is why the address is, or will be, blocklisted (or kept off the
// lists), or "".
func (d denmaImportAddr) blocked() string {
	switch {
	case d.OptOut.String == "complained":
		return "complained"
	case d.OptOut.Valid:
		return "unsubscribed"
	case d.Found && d.Status == models.SubscriberStatusBlockListed && d.Auto, !d.Found && d.Shared:
		return "bounced"
	case d.Found && d.Status == models.SubscriberStatusBlockListed:
		return "admin"
	case d.Fenced:
		return "no_consent"
	}
	return ""
}

// DenmaImportPreview reads an import's file and settings (as POST
// /api/import/subscribers takes them) and says what the import would do. The
// file is sent once: the answer's token stands for it (cmd/denma_importkeep.go).
func (a *App) DenmaImportPreview(c echo.Context) error {
	user := auth.GetUser(c)
	key := denmaKeptKey(a.ko.String("denma.center"), user.ID)
	params, f, upload, token, err := a.denmaPreviewForm(c, key)
	if err != nil {
		return err
	}
	defer f.Close()

	var opt subimporter.SessionOpt
	if err := json.Unmarshal([]byte(params), &opt); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.Ts("import.invalidParams", "error", err.Error()))
	}
	if len(opt.Delim) != 1 {
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.T("import.invalidDelim"))
	}
	listIDs := user.FilterListsByPerm(auth.PermTypeManage, opt.ListIDs)
	if opt.Mode == subimporter.ModeBlocklist {
		listIDs = nil
	}

	ctx := c.Request().Context()
	release, err := denmaPreviewWait(ctx)
	if err != nil {
		return err
	}
	defer release()

	info, err := f.Stat()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.Ts("import.invalidFile", "error", err.Error()))
	}
	name, open, err := denmaImportOpen(f, info.Size(), upload)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	r, enc, err := subimporter.DenmaUTF8Reader(open)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, denmaImportReadError(name, err))
	}
	p, addrs, entries := denmaReadImport(ctx, r, rune(opt.Delim[0]), a.importer.ValidateFields)
	r.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	p.File, p.Encoding, p.Token = name, enc, token
	p.CanAddTags = user.HasPerm("subscribers:manage")
	if p.Error != "" || len(addrs) == 0 {
		return c.JSON(http.StatusOK, okResp{p})
	}
	if err := a.denmaImportCenter(&p, addrs, entries, listIDs); err != nil {
		a.log.Printf("denma: error previewing an import: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error checking the file against the center.")
	}
	if opt.Mode == subimporter.ModeBlocklist && opt.DenmaBlockReason != "" {
		if err := a.denmaImportDatesPreview(&p, addrs, entries, opt.DenmaBlockReason); err != nil {
			a.log.Printf("denma: error previewing an import's dates: %v", err)
			return echo.NewHTTPError(http.StatusInternalServerError, "Error checking the file against the center.")
		}
	}
	if opt.Mode == subimporter.ModeSubscribe {
		var tags []string
		for _, t := range p.Tags {
			tags = append(tags, t.Tag)
			if t.As != "" {
				tags = append(tags, t.As)
			}
		}
		if p.Automations, err = a.denmaImportAutomations(listIDs, opt.SubStatus, tags); err != nil {
			a.log.Printf("denma: error previewing an import's automations: %v", err)
			return echo.NewHTTPError(http.StatusInternalServerError, "Error checking the file against the center.")
		}
		if opt.DenmaConsent {
			if err := a.denmaImportConsentsPreview(&p, addrs, entries, opt.OverwriteUserInfo); err != nil {
				a.log.Printf("denma: error previewing an import's consent records: %v", err)
				return echo.NewHTTPError(http.StatusInternalServerError, "Error checking the file against the center.")
			}
		}
	}
	return c.JSON(http.StatusOK, okResp{p})
}

// errDenmaOverLimit is a file in a ZIP that's over denmaMaxBody.
var errDenmaOverLimit = errors.New("over the limit")

// denmaLimited reads at most n bytes of r, and fails with errDenmaOverLimit
// if there's more.
type denmaLimited struct {
	r io.Reader
	n int64
}

func (l *denmaLimited) Read(p []byte) (int, error) {
	if l.n < 0 {
		return 0, errDenmaOverLimit
	}
	if int64(len(p)) > l.n+1 {
		p = p[:l.n+1]
	}
	n, err := l.r.Read(p)
	if l.n -= int64(n); l.n < 0 {
		return n - 1, errDenmaOverLimit
	}
	return n, err
}

// denmaImportOpen is the CSV the importer would read from the file (size
// bytes): the file, or the first .csv in a ZIP (as ImportSubscribers takes
// it). It gives its name, and a way to open it, from its start.
func denmaImportOpen(f io.ReaderAt, size int64, upload string) (string, func() (io.ReadCloser, error), error) {
	if strings.HasSuffix(strings.ToLower(upload), ".csv") {
		return upload, func() (io.ReadCloser, error) {
			return io.NopCloser(io.LimitReader(io.NewSectionReader(f, 0, size), denmaMaxBody)), nil
		}, nil
	}

	z, err := zip.NewReader(f, size)
	if err != nil {
		return "", nil, fmt.Errorf("Error reading the ZIP file: %v", err)
	}
	for _, zf := range z.File {
		if zf.FileInfo().IsDir() || !strings.HasSuffix(strings.ToLower(zf.FileInfo().Name()), ".csv") {
			continue
		}
		return zf.FileInfo().Name(), func() (io.ReadCloser, error) {
			r, err := zf.Open()
			if err != nil {
				return nil, fmt.Errorf("Error reading %s in the ZIP file: %v", zf.FileInfo().Name(), err)
			}
			return denmaLimitedCloser{&denmaLimited{r, denmaMaxBody}, r}, nil
		}, nil
	}
	return "", nil, errors.New("There's no .csv file in the ZIP file.")
}

type denmaLimitedCloser struct {
	io.Reader
	io.Closer
}

// denmaImportReadError says why a file couldn't be read.
func denmaImportReadError(name string, err error) string {
	if errors.Is(err, errDenmaOverLimit) {
		return fmt.Sprintf("%s in the ZIP file is over %d MB.", name, denmaMaxBody>>20)
	}
	return fmt.Sprintf("Error reading %s in the ZIP file: %v", name, err)
}

// denmaImportEntry is what the file gives an address, and what the center
// has of it.
type denmaImportEntry struct {
	tags    []string
	consent denmaImportConsentGiven // its first row's (cmd/denma_imports.go)
	dates   map[string]string       // its first row's unsubscribed_at and bounced_at (cmd/denma_imports.go)

	existing, hasConsent bool
}

// denmaReadImport reads a CSV as the importer's LoadCSV does, and returns
// the preview of the file, the addresses it'd import (in order), and what
// it gives each.
func denmaReadImport(ctx context.Context, r io.Reader, delim rune, validate func(subimporter.SubReq) (subimporter.SubReq, error)) (denmaImportPreview, []string, map[string]*denmaImportEntry) {
	p := denmaImportPreview{Columns: []string{}, Ignored: []string{}, Problems: []denmaImportProblem{},
		Attribs: []denmaImportAttrib{}, Sample: []denmaImportRow{}, Lists: []denmaImportListInfo{}, Tags: []denmaImportTag{}, Automations: []string{}}
	rd := csv.NewReader(r)
	rd.Comma = delim

	hdr, err := rd.Read()
	if err != nil {
		if err == io.EOF {
			p.Error = "The file is empty."
		} else {
			p.Error = fmt.Sprintf("The first line can't be read: %v", err)
		}
		return p, nil, nil
	}
	cols := map[string]int{}
	for i, h := range hdr {
		h = reDenmaNonASCII.ReplaceAllString(strings.TrimSpace(h), "")
		if !denmaIsImportColumn(h) {
			p.Ignored = append(p.Ignored, h)
			continue
		}
		if _, ok := cols[h]; !ok {
			p.Columns = append(p.Columns, h)
		}
		cols[h] = i // the importer takes the last of the same name
	}
	if _, ok := cols["email"]; !ok {
		p.Error = "There's no email column. The columns must be called email, name and attributes (in lowercase)."
		return p, nil, nil
	}

	var (
		addrs   []string
		entries = map[string]*denmaImportEntry{}
		keys    = map[string]int{}
	)
	problem := func(line int, value, msg string) {
		p.ProblemCount++
		if len(p.Problems) < denmaImportProblems {
			if len(value) > 200 {
				value = value[:200] + "…"
			}
			p.Problems = append(p.Problems, denmaImportProblem{line, value, msg})
		}
	}
	for {
		if p.Rows&1023 == 0 && ctx.Err() != nil {
			break // the request's gone
		}
		rec, err := rd.Read()
		if err == io.EOF {
			break
		}
		line, _ := rd.FieldPos(0)
		var pe *csv.ParseError
		if err != nil && errors.As(err, &pe) && pe.Err == csv.ErrFieldCount {
			p.Rows++
			problem(pe.StartLine, strings.Join(rec, string(delim)), fmt.Sprintf("has %d columns, but the first line has %d: skipped.", len(rec), len(hdr)))
			continue
		}
		if err != nil {
			if errors.As(err, &pe) {
				line = pe.StartLine
			}
			p.StopsAt = line
			problem(line, "", fmt.Sprintf("can't be read (%v): the import stops here, so no later line is imported.", err))
			break
		}
		p.Rows++

		sub := subimporter.SubReq{}
		sub.Email = rec[cols["email"]]
		if i, ok := cols["name"]; ok {
			sub.Name = rec[i]
		}
		var first, last string
		if i, ok := cols["first_name"]; ok {
			first = denmaTidyName(rec[i])
		}
		if i, ok := cols["last_name"]; ok {
			last = denmaTidyName(rec[i])
		}
		named := strings.TrimSpace(sub.Name) != "" || first != "" || last != ""
		if sub, err = validate(sub); err != nil {
			problem(line, rec[cols["email"]], err.Error()+": skipped.")
			continue
		}
		p.Valid++
		if !named {
			p.NoName++
		}
		// As the importer and the database name them (cmd/denma_names.go).
		if first != "" || last != "" {
			sub.Name = denmaJoinName(first, last)
		} else {
			first, last = denmaSplitName(sub.Name, sub.Email)
		}

		var attribs map[string]any
		if i, ok := cols["attributes"]; ok && rec[i] != "" {
			if err := json.Unmarshal([]byte(rec[i]), &attribs); err != nil {
				p.BadAttribs++
				problem(line, rec[i], "the attributes aren't a JSON object: imported without them.")
				attribs = nil
			}
		}
		for k := range attribs {
			keys[k]++
		}
		tags := denmaAttribTags(attribs)

		if e, ok := entries[sub.Email]; ok {
			p.Duplicates++
			e.tags = denmaNormTags(append(e.tags, tags...))
		} else {
			entries[sub.Email] = &denmaImportEntry{tags: tags, consent: denmaImportConsentOf(attribs), dates: denmaImportDatesOf(attribs)}
			addrs = append(addrs, sub.Email)
		}

		if len(p.Sample) < denmaImportSample {
			row := denmaImportRow{Line: line, Email: sub.Email, Name: sub.Name, First: first, Last: last, Tags: tags, Attribs: map[string]any{}, Dropped: []string{},
				consent: denmaImportConsentOf(attribs)}
			for k, v := range attribs {
				switch {
				case k == "tags":
				case denmaIsReservedAttrib(k):
					row.Dropped = append(row.Dropped, k)
				default:
					row.Attribs[k] = v
				}
			}
			sort.Strings(row.Dropped)
			p.Sample = append(p.Sample, row)
		}
	}
	p.Addresses = len(addrs)

	for k, n := range keys {
		p.Attribs = append(p.Attribs, denmaImportAttrib{Key: k, Rows: n, Dropped: k != "tags" && denmaIsReservedAttrib(k)})
	}
	sort.Slice(p.Attribs, func(i, j int) bool {
		if p.Attribs[i].Rows != p.Attribs[j].Rows {
			return p.Attribs[i].Rows > p.Attribs[j].Rows
		}
		return p.Attribs[i].Key < p.Attribs[j].Key
	})

	tags := map[string]int{}
	for _, e := range entries {
		for _, t := range e.tags {
			tags[t]++
		}
	}
	for t, n := range tags {
		p.Tags = append(p.Tags, denmaImportTag{Tag: t, Rows: n})
	}
	sort.Slice(p.Tags, func(i, j int) bool {
		if p.Tags[i].Rows != p.Tags[j].Rows {
			return p.Tags[i].Rows > p.Tags[j].Rows
		}
		return p.Tags[i].Tag < p.Tags[j].Tag
	})
	return p, addrs, entries
}

func denmaIsImportColumn(h string) bool {
	for _, c := range denmaImportColumns {
		if h == c {
			return true
		}
	}
	return false
}

// denmaImportCenter fills in what the center has of the file's addresses:
// who's new, on the lists, blocklisted, and which tags it has.
func (a *App) denmaImportCenter(p *denmaImportPreview, addrs []string, entries map[string]*denmaImportEntry, listIDs []int) error {
	shared := `false`
	if denmaHub != nil {
		shared = `EXISTS (SELECT 1 FROM denma.blocked_emails g WHERE g.email = f.email)`
	}
	var found []denmaImportAddr
	if err := a.db.Select(&found, `SELECT f.email, s.id IS NOT NULL AS found, COALESCE(s.status::TEXT, '') AS status,
		COALESCE(s.attribs->>'consent_at', '') <> '' AS has_consent,
		COALESCE(jsonb_typeof(s.attribs) = 'object' AND s.attribs ? 'auto_blocklist_reason', false) AS auto,
		denma_optout_kind(f.email) AS optout, `+shared+` AS shared,
		EXISTS (SELECT 1 FROM denma_no_consent n WHERE n.email_hash = denma_email_hash(f.email)) AS fenced
		FROM unnest($1::TEXT[]) f(email) LEFT JOIN subscribers s ON LOWER(s.email) = f.email`, pq.Array(addrs)); err != nil {
		return err
	}

	byEmail := make(map[string]denmaImportAddr, len(found))
	for _, d := range found {
		byEmail[d.Email] = d
		if e := entries[d.Email]; e != nil {
			e.existing, e.hasConsent = d.Found, d.HasConsent
		}
		if d.Found {
			p.Existing++
		} else {
			p.New++
		}
		switch d.blocked() {
		case "complained":
			p.Blocked.Complained++
		case "unsubscribed":
			p.Blocked.Unsubscribed++
		case "bounced":
			p.Blocked.Bounced++
		case "admin":
			p.Blocked.Admin++
		case "no_consent":
			p.Blocked.NoConsent++
		}
	}
	for i := range p.Sample {
		d := byEmail[p.Sample[i].Email]
		p.Sample[i].Existing, p.Sample[i].Blocked = d.Found, d.blocked()
	}

	existing := map[string]int{}
	for _, d := range found {
		if e := entries[d.Email]; d.Found && e != nil && len(e.tags) > 0 {
			p.ExistingWithTags++
			for _, t := range e.tags {
				existing[t]++
			}
		}
	}
	if len(p.Tags) > 0 {
		names := make([]string, len(p.Tags))
		for i, t := range p.Tags {
			names[i] = t.Tag
		}
		var have []struct {
			Name string `db:"name"`
			Tag  string `db:"tag"`
		}
		if err := a.db.Select(&have, `SELECT tag AS name, tag FROM denma_tags WHERE tag = ANY($1)
			UNION ALL SELECT alias, tag FROM denma_tag_aliases WHERE alias = ANY($1) AND alias NOT IN (SELECT tag FROM denma_tags)`,
			pq.Array(names)); err != nil {
			return err
		}
		has := map[string]string{}
		for _, t := range have {
			has[t.Name] = t.Tag
		}
		for i := range p.Tags {
			t := &p.Tags[i]
			t.Exists, t.Existing = has[t.Tag] != "", existing[t.Tag]
			if has[t.Tag] != t.Tag {
				t.As = has[t.Tag]
			}
		}
	}

	if len(listIDs) > 0 {
		if err := a.db.Select(&p.Lists, `SELECT l.id, l.name, l.optin::TEXT AS optin,
			COUNT(sl.subscriber_id) FILTER (WHERE sl.status <> 'unsubscribed') AS subscribed,
			COUNT(sl.subscriber_id) FILTER (WHERE sl.status = 'unsubscribed') AS unsubscribed
			FROM lists l LEFT JOIN (subscriber_lists sl JOIN subscribers s ON s.id = sl.subscriber_id AND LOWER(s.email) = ANY($2))
				ON sl.list_id = l.id
			WHERE l.id = ANY($1) GROUP BY l.id ORDER BY l.name`, pq.Array(listIDs), pq.Array(addrs)); err != nil {
			return err
		}
	}
	return nil
}
