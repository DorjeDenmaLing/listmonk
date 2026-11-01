package main

// denma: browsers keep the custom CSS and JS (Appearance) instead of fetching
// them on every page: their address has the app's CustomAssetVersion, new
// whenever the app, and so its Appearance settings, is rebuilt.

import (
	"crypto/md5"
	"fmt"
	"sync"

	"github.com/labstack/echo/v4"
)

// denmaAssetVersion is the build's AssetVersion, for the icon sprites'
// addresses, which are cached for a year.
var denmaAssetVersion = sync.OnceValue(func() string {
	return fmt.Sprintf("%x", md5.Sum([]byte(buildString)))[0:10]
})

// denmaCustomAssetCache lets the browser alone (private) keep the file, when
// it's asked for by the current version.
func denmaCustomAssetCache(c echo.Context) {
	if v := c.QueryParam("v"); v != "" && v == c.Get("app").(*App).cfg.CustomAssetVersion {
		c.Response().Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	} else {
		c.Response().Header().Set("Cache-Control", "no-cache")
	}
}
