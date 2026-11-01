package main

import (
	"fmt"
	iofs "io/fs"
	"net/http"
	"path/filepath"
	"syscall"

	"github.com/labstack/echo/v4"
)

const (
	// denmaUploadQuota is how much a center's uploads can take up.
	denmaUploadQuota = 2 << 30
	// denmaUploadDiskFree is the share of the disk uploads leave free for the database.
	denmaUploadDiskFree = 0.15
)

// denmaCheckUploadRoom refuses an upload if the center's folder is at quota or the disk is nearly full.
func (a *App) denmaCheckUploadRoom() error {
	if denmaHub == nil || a.ko.String("upload.provider") != "filesystem" {
		return nil
	}
	dir := a.ko.String("upload.filesystem.upload_path")
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err == nil && st.Blocks > 0 && float64(st.Bavail)/float64(st.Blocks) < denmaUploadDiskFree {
		a.log.Printf("denma: an upload was refused: the disk is nearly full")
		return echo.NewHTTPError(http.StatusInsufficientStorage, "The server's disk is nearly full, so no more can be uploaded for now. A superadmin has been told by its alarm.")
	}
	var used int64
	_ = filepath.WalkDir(dir, func(_ string, d iofs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, e := d.Info(); e == nil {
				used += info.Size()
			}
		}
		return nil
	})
	if used >= denmaUploadQuota {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("This center's media is full (%d GB). Delete files you no longer use, then upload again.", denmaUploadQuota>>30))
	}
	return nil
}
