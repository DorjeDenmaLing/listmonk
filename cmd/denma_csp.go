package main

// denma: Content-Security-Policy. Centers share the hub's domain, so a
// center's content shown there as a page would run as whoever views it: a
// superadmin's hub session (every center's settings and users), and the
// session of any other center they're signed into. Center admins write
// campaign and template HTML and can upload any file, so:
//
//   - Admin pages, the hub's and the centers', run only the admin's own
//     scripts: its files, and its inline <script>s, which carry a nonce made
//     for each page (CSPNonce, cmd/public.go). Event-handler attributes
//     (onerror=…), javascript: links and scripts in frames a page's content
//     adds don't run, wherever center HTML is shown in the admin (the rich
//     text and visual editors). Alpine still evaluates its own attributes (it
//     needs 'unsafe-eval'), so the rich text editor's content is x-ignore
//     (partials/richtext-editor.html).
//   - Pages that show center content (previews, view in browser for
//     campaigns and automations, the archive, uploads) are sandboxed: no
//     scripts, and an origin of their own, so nothing in them acts as the
//     viewer.

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/labstack/echo/v4"
)

// denmaAdminCSP sets an admin page's policy and returns its nonce, for its
// inline <script>s.
func denmaAdminCSP(c echo.Context) string {
	b := make([]byte, 18)
	_, _ = rand.Read(b) // never fails (crypto/rand)
	nonce := base64.StdEncoding.EncodeToString(b)
	c.Response().Header().Set("Content-Security-Policy", "script-src 'self' 'nonce-"+nonce+"' 'unsafe-eval'; "+
		"object-src 'none'; base-uri 'self'; frame-ancestors 'self'")
	return nonce
}

// denmaSandboxPolicy keeps a page to itself: no scripts or plugins, an origin
// of its own, and links, forms and new windows as usual.
const denmaSandboxPolicy = "sandbox allow-forms allow-popups allow-popups-to-escape-sandbox allow-top-navigation-by-user-activation; object-src 'none'"

// reDenmaContentPath matches the paths, under the hub's or a center's root,
// that show campaign, automation or template HTML: the admin's previews, view
// in browser (campaigns', and automations', cmd/denma_automations.go), and the
// public archive.
var reDenmaContentPath = regexp.MustCompile(`^/(api/campaigns/\d+/(preview|preview/archive|text)|api/templates/(\d+/)?preview|campaign/[^/]+/[^/]+|automation/[^/]+/[^/]+|archive|archive\.xml|archive/.+)$`)

// denmaSandbox sandboxes the response to a request for p (under app's root)
// if it shows content: a content path, or one of app's uploads, which may be
// any file (HTML or SVG with scripts included). PDFs aren't: browsers won't
// show them sandboxed, and their scripts don't run as the page.
func denmaSandbox(app *App, h http.Header, p string) {
	if !reDenmaContentPath.MatchString(p) && !denmaUploadPath(app, p) {
		return
	}
	h.Set("X-Content-Type-Options", "nosniff")
	if !strings.EqualFold(path.Ext(p), ".pdf") {
		h.Set("Content-Security-Policy", denmaSandboxPolicy)
	}
}

// denmaUploadPath reports whether p is one of app's uploads, served by it
// (filesystem, or S3 through it).
func denmaUploadPath(app *App, p string) bool {
	if app == nil {
		return false
	}
	var uri string
	switch app.ko.String("upload.provider") {
	case "filesystem":
		uri = app.ko.String("upload.filesystem.upload_uri")
	case "s3":
		if u := app.ko.String("upload.s3.public_url"); strings.HasPrefix(u, "/") {
			uri = u
		}
	}
	uri = strings.TrimSuffix(uri, "/")
	return uri != "" && strings.HasPrefix(p, uri+"/")
}
