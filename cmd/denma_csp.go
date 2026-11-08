package main

// denma: Content-Security-Policy, sandboxing of center content, and the admin API's same-site check.

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

// denmaAdminCSP sets an admin page's policy and returns its nonce for inline <script>s.
func denmaAdminCSP(c echo.Context) string {
	nonce := denmaNonce()
	c.Response().Header().Set("Content-Security-Policy", "script-src 'self' 'nonce-"+nonce+"' 'unsafe-eval'; "+
		"object-src 'none'; base-uri 'self'; frame-ancestors 'self'; form-action 'self'; connect-src 'self'")
	return nonce
}

// denmaPublicCSP adds a public page's policy (keeping any sandbox) and returns its nonce.
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

// reDenmaPlaceLink matches the admin links that change which center the admin shows.
var reDenmaPlaceLink = regexp.MustCompile(`^/admin/(switch/[^/]+|centers/[^/]+/open|hub)/?$`)

// denmaSameSite refuses a change to the admin or a sign-in sent from another site.
func denmaSameSite(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		r := c.Request()
		p := r.URL.Path
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			if s := r.Header.Get("Sec-Fetch-Site"); (s == "cross-site" || s == "same-site") && reDenmaPlaceLink.MatchString(p) {
				return echo.NewHTTPError(http.StatusForbidden, "This link came from another site.")
			}
			return next(c)
		}
		admin := (strings.HasPrefix(p, "/api/") && !strings.HasPrefix(p, "/api/public/")) ||
			p == "/admin" || strings.HasPrefix(p, "/admin/") || p == denmaLoginPath || strings.HasPrefix(p, denmaLoginPath+"/")
		if !admin {
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

// denmaSandboxPolicy keeps a page to itself: no scripts or plugins, its own origin.
const denmaSandboxPolicy = "sandbox allow-forms allow-popups allow-popups-to-escape-sandbox allow-top-navigation-by-user-activation; object-src 'none'"

// reDenmaContentPath matches the paths that show campaign, automation or template HTML, any ID segment.
var reDenmaContentPath = regexp.MustCompile(`^/(api/campaigns/[^/]+/(preview|preview/archive|text)|api/templates/([^/]+/)?preview|campaign/[^/]+/[^/]+|automation/[^/]+/[^/]+|archive|archive\.xml|archive/.+)$`)

// denmaDigits is a route's :id if it's digits only, else "".
func denmaDigits(s string) string {
	if strings.Trim(s, "0123456789") != "" {
		return ""
	}
	return s
}

// denmaSandbox sandboxes the response for p if it shows content: a content path or an upload (not PDFs).
func denmaSandbox(app *App, h http.Header, p string) {
	upload := denmaUploadPath(app, p)
	if !reDenmaContentPath.MatchString(p) && !upload {
		return
	}
	h.Set("X-Content-Type-Options", "nosniff")
	if upload {
		if !denmaUploadShown[strings.ToLower(path.Ext(p))] {
			h.Set("Content-Type", "text/plain; charset=utf-8")
		}
	}
	if strings.EqualFold(path.Ext(p), ".pdf") {
		return
	}
	if upload {
		// An upload's forms post nowhere.
		h.Set("Content-Security-Policy", denmaSandboxPolicy+"; form-action 'none'")
		return
	}
	h.Set("Content-Security-Policy", denmaSandboxPolicy)
}

// denmaUploadShown are the uploads served as what they are; any other is plain text.
var denmaUploadShown = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".avif": true, ".bmp": true, ".ico": true, ".svg": true,
	".pdf": true, ".mp4": true, ".webm": true, ".mov": true, ".mp3": true, ".m4a": true, ".wav": true, ".ogg": true,
	".zip": true, ".doc": true, ".docx": true, ".xls": true, ".xlsx": true, ".ppt": true, ".pptx": true,
}

// denmaUploadPath reports whether p is one of app's uploads (filesystem, or S3 through it).
func denmaUploadPath(app *App, p string) bool {
	if app == nil {
		return false
	}
	var uri string
	switch app.ko.String("upload.provider") {
	case "filesystem":
		uri = path.Join("/", app.ko.String("upload.filesystem.upload_uri"))
	case "s3":
		if u := app.ko.String("upload.s3.public_url"); strings.HasPrefix(u, "/") {
			uri = u
		}
	}
	uri = strings.TrimSuffix(uri, "/")
	return uri != "" && strings.HasPrefix(p, uri+"/")
}
