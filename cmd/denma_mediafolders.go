package main

// denma: folders in Media (the hub's and each center's), one level: each
// file is in one folder or none, and the page and the editors' picker show
// all of them, a folder's, or those in none (?folder=<id> or none). A
// folder is only a label in the database: the files stay where they are, at
// the same addresses, as e-mails already sent link to them. Uploads go into
// the folder shown. Only an empty folder can be deleted (its files moved out
// or deleted first), so deleting one never touches a file. Folder names are
// unique, ignoring case.

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/knadh/listmonk/internal/media"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
	null "gopkg.in/volatiletech/null.v6"
)

// denmaMediaFolderMax is the longest a folder's name can be.
const denmaMediaFolderMax = 60

// denmaMediaFoldersSQL makes the tables, in the hub and each center when its
// app is built.
const denmaMediaFoldersSQL = `
CREATE TABLE IF NOT EXISTS denma_media_folders (
    id         SERIAL PRIMARY KEY,
    name       TEXT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS denma_media_folders_name ON denma_media_folders (LOWER(name));
CREATE TABLE IF NOT EXISTS denma_media_folder_files (
    media_id  INTEGER PRIMARY KEY REFERENCES media(id) ON DELETE CASCADE,
    folder_id INTEGER NOT NULL REFERENCES denma_media_folders(id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS denma_media_folder_files_folder ON denma_media_folder_files (folder_id);`

// denmaInitMediaFolders makes the app's folder tables. Called by buildApp.
func denmaInitMediaFolders(a *App) {
	if _, err := a.db.Exec(denmaMediaFoldersSQL); err != nil {
		a.log.Printf("denma: error making the media folder tables: %v", err)
	}
}

// denmaMediaFolder is a folder, with how many files it has.
type denmaMediaFolder struct {
	ID    int    `db:"id" json:"id"`
	Name  string `db:"name" json:"name"`
	Count int    `db:"count" json:"count"`
}

// denmaMediaView is what the Media page and picker show of folders
// (cmd/media.go's mediaView).
type denmaMediaView struct {
	Folders  []denmaMediaFolder `json:"folders"`
	Folder   string             `json:"folder"` // shown: "", "none" or a folder's ID
	All      int                `json:"all"`    // files in the library
	None     int                `json:"none"`   // files in no folder
	FolderOf map[int]int        `json:"folder_of"`
}

// FolderName is the folder shown's name, or "".
func (v denmaMediaView) FolderName() string {
	for _, f := range v.Folders {
		if strconv.Itoa(f.ID) == v.Folder {
			return f.Name
		}
	}
	return ""
}

// denmaQueryMedia is listmonk's media query (core.QueryMedia), in a folder
// (an ID) or in none ("none") if one is given.
func (a *App) denmaQueryMedia(folder, query string, offset, limit int) ([]media.Media, int, error) {
	if folder == "" {
		return a.core.QueryMedia(a.cfg.MediaUpload.Provider, a.media, query, offset, limit)
	}
	in := `EXISTS (SELECT 1 FROM denma_media_folder_files f WHERE f.media_id = m.id AND f.folder_id = $5)`
	id, _ := strconv.Atoi(folder)
	if folder == "none" {
		in = `NOT EXISTS (SELECT 1 FROM denma_media_folder_files f WHERE f.media_id = m.id) AND $5 = 0`
		id = 0
	}
	out := []media.Media{}
	if err := a.db.Select(&out, `SELECT COUNT(*) OVER () AS total, m.* FROM media m
		WHERE ($1 = '' OR m.filename ILIKE $1) AND m.provider = $2 AND `+in+`
		ORDER BY m.created_at DESC OFFSET $3 LIMIT $4`,
		"%"+strings.ToLower(query)+"%", a.cfg.MediaUpload.Provider, offset, limit, id); err != nil {
		return nil, 0, echo.NewHTTPError(http.StatusInternalServerError,
			a.i18n.Ts("globals.messages.errorFetching", "name", "{globals.terms.media}", "error", err.Error()))
	}
	total := 0
	for i := range out {
		total = out[i].Total
		out[i].URL = a.media.GetURL(out[i].Filename)
		if out[i].Thumb != "" {
			out[i].ThumbURL = null.String{Valid: true, String: a.media.GetURL(out[i].Thumb)}
		}
	}
	return out, total, nil
}

// denmaMediaFolders is the folders for a Media page showing files.
func (a *App) denmaMediaFolders(c echo.Context, files []media.Media) (denmaMediaView, error) {
	v := denmaMediaView{Folders: []denmaMediaFolder{}, Folder: c.QueryParam("folder"), FolderOf: map[int]int{}}
	if err := a.db.Select(&v.Folders, `SELECT f.id, f.name, (SELECT COUNT(*) FROM denma_media_folder_files x WHERE x.folder_id = f.id) AS count
		FROM denma_media_folders f ORDER BY LOWER(f.name)`); err != nil {
		return v, err
	}
	if err := a.db.QueryRow(`SELECT COUNT(*), COUNT(*) FILTER (WHERE NOT EXISTS (SELECT 1 FROM denma_media_folder_files x WHERE x.media_id = m.id))
		FROM media m WHERE m.provider = $1`, a.cfg.MediaUpload.Provider).Scan(&v.All, &v.None); err != nil {
		return v, err
	}
	ids := make([]int, len(files))
	for i, m := range files {
		ids[i] = m.ID
	}
	var in []struct {
		Media  int `db:"media_id"`
		Folder int `db:"folder_id"`
	}
	if err := a.db.Select(&in, `SELECT media_id, folder_id FROM denma_media_folder_files WHERE media_id = ANY($1)`, pq.Array(ids)); err != nil {
		return v, err
	}
	for _, x := range in {
		v.FolderOf[x.Media] = x.Folder
	}
	return v, nil
}

// denmaMediaFolderName is a folder's name from a request, tidied, or an error.
func (a *App) denmaMediaFolderName(c echo.Context) (string, error) {
	var in struct {
		Name string `json:"name"`
	}
	if err := c.Bind(&in); err != nil {
		return "", echo.NewHTTPError(http.StatusBadRequest, a.i18n.T("globals.messages.invalidData"))
	}
	name := strings.Join(strings.Fields(in.Name), " ")
	if name == "" || utf8.RuneCountInString(name) > denmaMediaFolderMax {
		return "", echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("A folder's name is 1 to %d characters.", denmaMediaFolderMax))
	}
	return name, nil
}

// denmaMediaFolderTaken is the error for a name another folder has.
func denmaMediaFolderTaken(err error, name string) error {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "23505" {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("There's already a folder named %q.", name))
	}
	return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
}

// DenmaGetMediaFolders returns the folders, with how many files each has.
func (a *App) DenmaGetMediaFolders(c echo.Context) error {
	v, err := a.denmaMediaFolders(c, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{v.Folders})
}

// DenmaCreateMediaFolder makes a folder: {"name": ...}.
func (a *App) DenmaCreateMediaFolder(c echo.Context) error {
	name, err := a.denmaMediaFolderName(c)
	if err != nil {
		return err
	}
	var f denmaMediaFolder
	if err := a.db.Get(&f, `INSERT INTO denma_media_folders (name) VALUES ($1) RETURNING id, name, 0 AS count`, name); err != nil {
		return denmaMediaFolderTaken(err, name)
	}
	return c.JSON(http.StatusOK, okResp{f})
}

// DenmaRenameMediaFolder renames a folder: {"name": ...}.
func (a *App) DenmaRenameMediaFolder(c echo.Context) error {
	name, err := a.denmaMediaFolderName(c)
	if err != nil {
		return err
	}
	res, err := a.db.Exec(`UPDATE denma_media_folders SET name = $2 WHERE id = $1`, getID(c), name)
	if err != nil {
		return denmaMediaFolderTaken(err, name)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return echo.NewHTTPError(http.StatusNotFound, "There's no such folder.")
	}
	return c.JSON(http.StatusOK, okResp{true})
}

// DenmaDeleteMediaFolder deletes a folder, if it's empty.
func (a *App) DenmaDeleteMediaFolder(c echo.Context) error {
	var n int
	if err := a.db.Get(&n, `SELECT COUNT(*) FROM denma_media_folder_files WHERE folder_id = $1`, getID(c)); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if n > 0 {
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("The folder has %d %s. Move %s to another folder, or delete %s, first.", n, plural(n, "file", "files"), plural(n, "it", "them"), plural(n, "it", "them")))
	}
	res, err := a.db.Exec(`DELETE FROM denma_media_folders WHERE id = $1`, getID(c))
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23503" { // a file put in it since
			return echo.NewHTTPError(http.StatusBadRequest, "The folder has files now. Move them to another folder, or delete them, first.")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return echo.NewHTTPError(http.StatusNotFound, "There's no such folder.")
	}
	return c.JSON(http.StatusOK, okResp{true})
}

// DenmaMoveMedia puts files in a folder, or in none (folder_id 0):
// {"ids": [...], "folder_id": ...}.
func (a *App) DenmaMoveMedia(c echo.Context) error {
	var in struct {
		IDs      []int `json:"ids"`
		FolderID int   `json:"folder_id"`
	}
	if err := c.Bind(&in); err != nil || len(in.IDs) == 0 || len(in.IDs) > 1000 {
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.T("globals.messages.invalidData"))
	}
	tx, err := a.db.Beginx()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	defer tx.Rollback()
	if in.FolderID > 0 {
		var ok bool
		if err := tx.Get(&ok, `SELECT TRUE FROM denma_media_folders WHERE id = $1 FOR SHARE`, in.FolderID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return echo.NewHTTPError(http.StatusNotFound, "There's no such folder.")
			}
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
	}
	if _, err := tx.Exec(`DELETE FROM denma_media_folder_files WHERE media_id = ANY($1)`, pq.Array(in.IDs)); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if in.FolderID > 0 {
		if _, err := tx.Exec(`INSERT INTO denma_media_folder_files (media_id, folder_id)
			SELECT id, $2 FROM media WHERE id = ANY($1) AND provider = $3`, pq.Array(in.IDs), in.FolderID, a.cfg.MediaUpload.Provider); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
	}
	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{true})
}

// initDenmaMediaFolderHandlers registers the folders' API (the hub's and
// centers', as /api/media is).
func initDenmaMediaFolderHandlers(g *echo.Group, a *App) {
	pm := a.auth.Perm
	g.GET("/api/denma/media/folders", pm(a.DenmaGetMediaFolders, "media:get"))
	g.POST("/api/denma/media/folders", pm(a.DenmaCreateMediaFolder, "media:manage"))
	g.PUT("/api/denma/media/folders/:id", pm(hasID(a.DenmaRenameMediaFolder), "media:manage"))
	g.DELETE("/api/denma/media/folders/:id", pm(hasID(a.DenmaDeleteMediaFolder), "media:manage"))
	g.PUT("/api/denma/media/folder", pm(a.DenmaMoveMedia, "media:manage"))
}

// plural is one or many, by n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
