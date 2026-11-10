package main

// denma: Media files are stored under names made from their contents; the name they were uploaded with is their label.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/internal/media"
	"github.com/labstack/echo/v4"
)

// denmaMediaLabelMax is the longest a file's label can be.
const denmaMediaLabelMax = 200

// denmaMediaLabelSQL adds the label column.
const denmaMediaLabelSQL = `ALTER TABLE media ADD COLUMN IF NOT EXISTS denma_label TEXT NOT NULL DEFAULT '';`

// reDenmaMediaExt is a stored name's extension; a file's other extensions are left off.
var reDenmaMediaExt = regexp.MustCompile(`^[a-z0-9]{1,8}$`)

// denmaHashName is the stored name of contents r with extension ext.
func denmaHashName(r io.Reader, ext string) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	name := hex.EncodeToString(h.Sum(nil)[:8])
	switch ext = strings.ToLower(strings.TrimPrefix(ext, ".")); {
	case !reDenmaMediaExt.MatchString(ext):
		return name, nil
	case ext == "jpeg":
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

// denmaInitMediaNames adds the label column and labels files that have none. Called by buildApp.
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
	}
}
