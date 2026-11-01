package main

// denma: browsers keep the custom CSS and JS, fetched by the app's CustomAssetVersion.

import (
	"crypto/md5"
	"fmt"
	"sync"

	"github.com/labstack/echo/v4"
)

// denmaAssetVersion is the build's AssetVersion, for the icon sprites' addresses.
var denmaAssetVersion = sync.OnceValue(func() string {
	return fmt.Sprintf("%x", md5.Sum([]byte(buildString)))[0:10]
})

// denmaCustomAssetCache lets the browser alone (private) keep the file.
func denmaCustomAssetCache(c echo.Context) {
	if v := c.QueryParam("v"); v != "" && v == c.Get("app").(*App).cfg.CustomAssetVersion {
		c.Response().Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	} else {
		c.Response().Header().Set("Cache-Control", "no-cache")
	}
}
