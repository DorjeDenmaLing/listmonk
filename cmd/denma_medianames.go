package main

// denma: Media files are stored under names made from their contents; the name they were uploaded with is their label.

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/internal/media"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

// denmaMediaLabelMax is the longest a file's label can be.
const denmaMediaLabelMax = 200

// denmaMediaLabelSQL adds the label column.
const denmaMediaLabelSQL = `ALTER TABLE media ADD COLUMN IF NOT EXISTS denma_label TEXT NOT NULL DEFAULT '';`

var reDenmaHashName = regexp.MustCompile(`^[0-9a-f]{16}(\.[a-z0-9]+)?$`)

// denmaMediaSettings are the settings that may link a Media file.
var denmaMediaSettings = []string{"app.logo_url", "app.favicon_url", "denma.admin_logo_url", "denma.admin_icon_url"}

// denmaHashName is the stored name of contents r with extension ext.
func denmaHashName(r io.Reader, ext string) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	name := hex.EncodeToString(h.Sum(nil)[:8])
	switch ext = strings.ToLower(strings.TrimPrefix(ext, ".")); ext {
	case "":
		return name, nil
	case "jpeg":
		ext = "jpg"
	}
	return name + "." + ext, nil
}

// denmaMediaLabel is a label tidied: one line, spaces collapsed; "" if it's empty or too long.
func denmaMediaLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > denmaMediaLabelMax {
		return ""
	}
	return s
}

// denmaMediaFound is a file already in Media, given for an upload of the same contents.
type denmaMediaFound struct {
	media.Media
	Existing bool `json:"existing"`
}

// denmaMediaHave is the file in Media stored as name, if there's one.
func (a *App) denmaMediaHave(name string) (denmaMediaFound, bool) {
	var id int
	if err := a.db.Get(&id, `SELECT id FROM media WHERE filename = $1 AND provider = $2 ORDER BY id LIMIT 1`,
		name, a.cfg.MediaUpload.Provider); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			a.log.Printf("denma: error looking for a media file: %v", err)
		}
		return denmaMediaFound{}, false
	}
	m, err := a.core.GetMedia(id, "", "", a.media)
	if err != nil {
		return denmaMediaFound{}, false
	}
	return denmaMediaFound{Media: m, Existing: true}, true
}

// denmaLabelMedia gives an uploaded file its label.
func (a *App) denmaLabelMedia(m media.Media, label string) media.Media {
	if label == "" {
		return m
	}
	if _, err := a.db.Exec(`UPDATE media SET denma_label = $2 WHERE id = $1`, m.ID, label); err != nil {
		a.log.Printf("denma: error labelling a media file: %v", err)
		return m
	}
	m.Label = label
	return m
}

// denmaAttachmentName is a file's name as an e-mail attachment: its label, with its extension.
func denmaAttachmentName(m media.Media) string {
	name := m.Name()
	if ext := path.Ext(m.Filename); ext != "" && !strings.EqualFold(path.Ext(name), ext) {
		name += ext
	}
	return name
}

// DenmaRenameMedia changes a file's label: {"label": ...}. Its address stays as it is.
func (a *App) DenmaRenameMedia(c echo.Context) error {
	var in struct {
		Label string `json:"label"`
	}
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.T("globals.messages.invalidData"))
	}
	label := denmaMediaLabel(in.Label)
	if label == "" {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("A file's name is 1 to %d characters.", denmaMediaLabelMax))
	}
	tx, err := a.db.Beginx()
	if err != nil {
		return denmaInternal(err)
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE media SET denma_label = $2 WHERE id = $1 AND provider = $3`, getID(c), label, a.cfg.MediaUpload.Provider)
	if err != nil {
		return denmaInternal(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return echo.NewHTTPError(http.StatusNotFound, "There's no such file.")
	}
	// The name campaigns list it by as an attachment.
	if _, err := tx.Exec(`UPDATE campaign_media SET filename = $2 WHERE media_id = $1`, getID(c), label); err != nil {
		return denmaInternal(err)
	}
	if err := tx.Commit(); err != nil {
		return denmaInternal(err)
	}
	m, err := a.core.GetMedia(getID(c), "", "", a.media)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, okResp{m})
}

// denmaMediaRow is a file to give a hashed name.
type denmaMediaRow struct {
	ID          int    `db:"id"`
	Filename    string `db:"filename"`
	Thumb       string `db:"thumb"`
	ContentType string `db:"content_type"`
}

// denmaInitMediaNames adds the label column and stores files named otherwise under hashed names, once. Called by buildApp.
func denmaInitMediaNames(ko *koanf.Koanf, db *sqlx.DB) {
	lo := denmaLog(ko)
	var has, labelled bool
	if err := db.QueryRow(`SELECT to_regclass('media') IS NOT NULL,
		EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'media' AND column_name = 'denma_label')`).
		Scan(&has, &labelled); err != nil || !has {
		return
	}
	if !labelled {
		if _, err := db.Exec(denmaMediaLabelSQL); err != nil {
			lo.Printf("denma: error adding media labels: %v", err)
			return
		}
	}
	if _, err := db.Exec(`UPDATE media SET denma_label = filename WHERE denma_label = ''`); err != nil {
		lo.Printf("denma: error labelling media: %v", err)
		return
	}
	provider := ko.String("upload.provider")
	var rows []denmaMediaRow
	if err := db.Select(&rows, `SELECT id, filename, thumb, content_type FROM media
		WHERE provider = $1 AND filename !~ '^[0-9a-f]{16}(\.[a-z0-9]+)?$' ORDER BY id`, provider); err != nil {
		lo.Printf("denma: error reading media to rename: %v", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	n, err := denmaHashMediaNames(ko, db, initMediaStore(ko), provider, rows)
	if err != nil {
		lo.Printf("denma: error giving media files hashed names: %v", err)
	}
	if n > 0 {
		lo.Printf("denma: %d of %d media files stored under hashed names", n, len(rows))
	}
}

// denmaHashMediaNames stores rows' files under hashed names, pointing what links them there; it's how many were.
func denmaHashMediaNames(ko *koanf.Koanf, db *sqlx.DB, store media.Store, provider string, rows []denmaMediaRow) (int, error) {
	lo := denmaLog(ko)
	type renamed struct {
		row         denmaMediaRow
		name, thumb string
		into        int // the file with the same contents it's merged into, or 0
	}
	var (
		done  []renamed
		names = map[string]string{} // old name: new
		owner = map[string]int{}    // new name: the file that has it
	)
	for _, r := range rows {
		b, err := store.GetBlob(store.GetURL(r.Filename))
		if err != nil {
			lo.Printf("denma: media file %d can't be read, so keeps its name: %v", r.ID, err)
			continue
		}
		name, _ := denmaHashName(bytes.NewReader(b), path.Ext(r.Filename))
		x := renamed{row: r, name: name}
		if id, ok := owner[name]; ok {
			x.into = id
		} else if err := db.Get(&x.into, `SELECT id FROM media WHERE filename = $1 AND provider = $2 ORDER BY id LIMIT 1`, name, provider); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if x.into != 0 {
			if err := db.Get(&x.thumb, `SELECT thumb FROM media WHERE id = $1`, x.into); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return 0, err
			}
			for _, d := range done { // merged into one renamed in this pass
				if d.row.ID == x.into {
					x.thumb = d.thumb
				}
			}
		} else {
			if x.name, err = store.Put(name, r.ContentType, bytes.NewReader(b)); err != nil {
				lo.Printf("denma: media file %d can't be stored under its new name: %v", r.ID, err)
				continue
			}
			switch {
			case r.Thumb == r.Filename:
				x.thumb = x.name
			case r.Thumb != "":
				if tb, err := store.GetBlob(store.GetURL(r.Thumb)); err == nil {
					if x.thumb, err = store.Put(thumbPrefix+x.name, r.ContentType, bytes.NewReader(tb)); err != nil {
						x.thumb = ""
					}
				}
			}
			owner[x.name] = r.ID
		}
		names[r.Filename] = x.name
		if r.Thumb != "" && r.Thumb != r.Filename && x.thumb != "" {
			names[r.Thumb] = x.thumb
		}
		done = append(done, x)
	}
	if len(done) == 0 {
		return 0, nil
	}

	tx, err := db.Beginx()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, x := range done {
		if x.into == 0 {
			if _, err := tx.Exec(`UPDATE media SET filename = $2, thumb = $3 WHERE id = $1`, x.row.ID, x.name, x.thumb); err != nil {
				return 0, err
			}
			continue
		}
		// The same contents twice: one file, in the campaigns and folder each was in.
		if _, err := tx.Exec(`DELETE FROM campaign_media WHERE media_id = $1 AND campaign_id IN (SELECT campaign_id FROM campaign_media WHERE media_id = $2)`, x.row.ID, x.into); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`UPDATE campaign_media SET media_id = $2 WHERE media_id = $1`, x.row.ID, x.into); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`INSERT INTO denma_media_folder_files (media_id, folder_id)
			SELECT $2, folder_id FROM denma_media_folder_files WHERE media_id = $1 ON CONFLICT DO NOTHING`, x.row.ID, x.into); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`DELETE FROM media WHERE id = $1`, x.row.ID); err != nil {
			return 0, err
		}
	}
	n, err := denmaRelinkMedia(tx, store.GetURL(""), names)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	for k, v := range n.settings {
		ko.Set(k, v)
	}
	lo.Printf("denma: %d templates, %d campaigns and %d settings link media files by their new names", n.templates, n.campaigns, len(n.settings))

	// The old files, once nothing links them.
	for _, x := range done {
		store.Delete(x.row.Filename)
		if t := x.row.Thumb; t != "" && t != x.row.Filename && t != x.thumb {
			store.Delete(t)
		}
	}
	return len(done), nil
}

// denmaRelinked is what denmaRelinkMedia changed.
type denmaRelinked struct {
	templates, campaigns int
	settings             map[string]string
}

// denmaMediaLinks returns a func pointing links to files under base (an uploads URL) by their names' new names.
func denmaMediaLinks(base string, names map[string]string) func(string) string {
	u, err := url.Parse(base)
	if err != nil || len(names) == 0 {
		return func(s string) string { return s }
	}
	to := map[string]string{}
	var alts []string
	for old, n := range names {
		for _, o := range []string{old, url.PathEscape(old)} {
			if _, ok := to[o]; !ok {
				to[o] = n
				alts = append(alts, regexp.QuoteMeta(o))
			}
		}
	}
	re := regexp.MustCompile(`(` + regexp.QuoteMeta(base) + `|["'(=\s]` + regexp.QuoteMeta(u.Path) + `)(` +
		strings.Join(alts, "|") + `)(["'?#)\s\\<&]|$)`)
	return func(s string) string {
		return re.ReplaceAllStringFunc(s, func(m string) string {
			p := re.FindStringSubmatch(m)
			return p[1] + to[p[2]] + p[3]
		})
	}
}

// denmaRelinkMedia points templates, campaigns and settings that link files under base at their new names.
func denmaRelinkMedia(tx *sqlx.Tx, base string, names map[string]string) (denmaRelinked, error) {
	out := denmaRelinked{settings: map[string]string{}}
	relink := denmaMediaLinks(base, names)
	var olds []string
	for o := range names {
		olds = append(olds, o)
	}
	has := func(cols ...string) string {
		return `EXISTS (SELECT 1 FROM unnest($1::TEXT[]) n WHERE strpos(` + strings.Join(cols, " || ") + `, n) > 0)`
	}

	var tpls []struct {
		ID         int            `db:"id"`
		Body       string         `db:"body"`
		BodySource sql.NullString `db:"body_source"`
	}
	if err := tx.Select(&tpls, `SELECT id, body, body_source FROM templates WHERE `+has("body", "COALESCE(body_source, '')"), pq.Array(olds)); err != nil {
		return out, err
	}
	for _, t := range tpls {
		body, src := relink(t.Body), relink(t.BodySource.String)
		if body == t.Body && src == t.BodySource.String {
			continue
		}
		if _, err := tx.Exec(`UPDATE templates SET body = $2, body_source = CASE WHEN body_source IS NULL THEN NULL ELSE $3 END WHERE id = $1`, t.ID, body, src); err != nil {
			return out, fmt.Errorf("template %d: %w", t.ID, err)
		}
		out.templates++
	}

	var camps []struct {
		ID         int            `db:"id"`
		Body       string         `db:"body"`
		BodySource sql.NullString `db:"body_source"`
		AltBody    sql.NullString `db:"altbody"`
	}
	if err := tx.Select(&camps, `SELECT id, body, body_source, altbody FROM campaigns WHERE `+
		has("body", "COALESCE(body_source, '')", "COALESCE(altbody, '')"), pq.Array(olds)); err != nil {
		return out, err
	}
	for _, c := range camps {
		body, src, alt := relink(c.Body), relink(c.BodySource.String), relink(c.AltBody.String)
		if body == c.Body && src == c.BodySource.String && alt == c.AltBody.String {
			continue
		}
		if _, err := tx.Exec(`UPDATE campaigns SET body = $2,
			body_source = CASE WHEN body_source IS NULL THEN NULL ELSE $3 END,
			altbody = CASE WHEN altbody IS NULL THEN NULL ELSE $4 END WHERE id = $1`, c.ID, body, src, alt); err != nil {
			return out, fmt.Errorf("campaign %d: %w", c.ID, err)
		}
		out.campaigns++
	}

	var sets []struct {
		Key   string `db:"key"`
		Value string `db:"value"`
	}
	if err := tx.Select(&sets, `SELECT key, COALESCE(value #>> '{}', '') AS value FROM settings WHERE key = ANY($1)`, pq.Array(denmaMediaSettings)); err != nil {
		return out, err
	}
	for _, s := range sets {
		v := relink(`"` + s.Value + `"`)
		v = v[1 : len(v)-1]
		if v == s.Value {
			continue
		}
		b, _ := json.Marshal(v)
		if _, err := tx.Exec(`UPDATE settings SET value = $2::JSONB, updated_at = NOW() WHERE key = $1`, s.Key, string(b)); err != nil {
			return out, err
		}
		out.settings[s.Key] = v
	}
	return out, nil
}
