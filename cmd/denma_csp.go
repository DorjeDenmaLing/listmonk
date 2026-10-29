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
//     (partials/richtext-editor.html). They also fetch, post and stream
//     only from the site (connect-src, form-action).
//   - Public pages (sign-in, opt-in, unsubscribe...) show the center's
//     design, so they run only the app's scripts too, with a nonce of their
//     own (denmaPublicCSP), and the captchas'.
//   - Pages that show center content (previews, view in browser for
//     campaigns and automations, the archive, uploads) are sandboxed: no
//     scripts, and an origin of their own, so nothing in them acts as the
//     viewer. A script upload is served as text, so that no page on the
//     origin runs it ('self').
//
// And the admin API takes no changes sent from another site
// (denmaSameSite), which the session cookie would make the admin's.

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/labstack/echo/v4"
)

// denmaAdminCSP sets an admin page's policy and returns its nonce, for its
// inline <script>s.
func denmaAdminCSP(c echo.Context) string {
	nonce := denmaNonce()
	c.Response().Header().Set("Content-Security-Policy", "script-src 'self' 'nonce-"+nonce+"' 'unsafe-eval'; "+
		"object-src 'none'; base-uri 'self'; frame-ancestors 'self'; form-action 'self'; connect-src 'self'")
	return nonce
}

// denmaPublicCSP adds a public page's policy and returns its nonce, for its
// inline <script>s. It's added, so that a sandboxed page keeps its sandbox.
// Its forms post only here and it frames only the captcha (no sign-in
// lookalike, no framed admin), but a sandboxed page keeps its forms.
func denmaPublicCSP(c echo.Context) string {
	nonce := denmaNonce()
	h := c.Response().Header()
	forms := "; form-action 'self'"
	if strings.Contains(h.Get("Content-Security-Policy"), "sandbox") {
		forms = ""
	}
	h.Add("Content-Security-Policy", "script-src 'self' 'nonce-"+nonce+"' https://hcaptcha.com https://*.hcaptcha.com; "+
		"worker-src 'self' blob:; object-src 'none'; base-uri 'self'; frame-ancestors 'self'; "+
		"frame-src https://hcaptcha.com https://*.hcaptcha.com"+forms)
	return nonce
}

func denmaNonce() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b) // never fails (crypto/rand)
	return base64.StdEncoding.EncodeToString(b)
}

// denmaSameSite refuses a change to the admin API (/api, but its public
// routes) sent from another site: an Origin, or with none a Referer, that
// isn't this site (a sandboxed page's is "null"). Requests with neither, from
// programs, go through.
func denmaSameSite(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		r := c.Request()
		p := r.URL.Path
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions ||
			!strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/api/public/") {
			return next(c)
		}
		from := r.Header.Get("Origin")
		if from == "" {
			from = r.Header.Get("Referer")
		}
		if from == "" {
			return next(c)
		}
		if u, err := url.Parse(from); err != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host) {
			return echo.NewHTTPError(http.StatusForbidden, "This request came from another site.")
		}
		return next(c)
	}
}

// denmaSandboxPolicy keeps a page to itself: no scripts or plugins, an origin
// of its own, and links, forms and new windows as usual.
const denmaSandboxPolicy = "sandbox allow-forms allow-popups allow-popups-to-escape-sandbox allow-top-navigation-by-user-activation; object-src 'none'"

// reDenmaContentPath matches the paths, under the hub's or a center's root,
// that show campaign, automation or template HTML: the admin's previews, view
// in browser (campaigns', and automations', cmd/denma_automations.go), and the
// public archive. Any ID segment, not only digits: one the route takes in
// another form ("+5") must not escape it.
var reDenmaContentPath = regexp.MustCompile(`^/(api/campaigns/[^/]+/(preview|preview/archive|text)|api/templates/([^/]+/)?preview|campaign/[^/]+/[^/]+|automation/[^/]+/[^/]+|archive|archive\.xml|archive/.+)$`)

// denmaDigits is a route's :id if it's digits only, else "" (strconv.Atoi
// takes "+5", which the paths above wouldn't match).
func denmaDigits(s string) string {
	if strings.Trim(s, "0123456789") != "" {
		return ""
	}
	return s
}

// denmaSandbox sandboxes the response to a request for p (under app's root)
// if it shows content: a content path, or one of app's uploads, which may be
// any file (HTML or SVG with scripts included). PDFs aren't: browsers won't
// show them sandboxed, and their scripts don't run as the page.
func denmaSandbox(app *App, h http.Header, p string) {
	upload := denmaUploadPath(app, p)
	if !reDenmaContentPath.MatchString(p) && !upload {
		return
	}
	h.Set("X-Content-Type-Options", "nosniff")
	if upload {
		switch strings.ToLower(path.Ext(p)) {
		case ".js", ".mjs", ".cjs":
			h.Set("Content-Type", "text/plain; charset=utf-8")
		}
	}
	if strings.EqualFold(path.Ext(p), ".pdf") {
		return
	}
	if upload {
		// An upload's forms post nowhere: no sign-in page on our domain.
		h.Set("Content-Security-Policy", denmaSandboxPolicy+"; form-action 'none'")
		return
	}
	h.Set("Content-Security-Policy", denmaSandboxPolicy)
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
