package main

// denma: browsers keep the custom CSS and JS (Appearance) instead of fetching
// them on every page: their address has the app's CustomAssetVersion, new
// whenever the app, and so its Appearance settings, is rebuilt.

import "github.com/labstack/echo/v4"

// denmaCustomAssetCache lets the browser alone (private) keep the file, when
// it's asked for by the current version.
func denmaCustomAssetCache(c echo.Context) {
	if v := c.QueryParam("v"); v != "" && v == c.Get("app").(*App).cfg.CustomAssetVersion {
		c.Response().Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	} else {
		c.Response().Header().Set("Cache-Control", "no-cache")
	}
}
