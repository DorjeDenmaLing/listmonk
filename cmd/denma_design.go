package main

// denma: a center's design for its automatic e-mails and public pages
// (denma.design_template, on its Config page): one of its design templates
// (type "design", made with the visual editor; denmaTemplateTypeDesign),
// with an HTML block holding the content's place, where each e-mail's or
// page's own content goes:
//
//	<div data-content>(Page content will appear here)</div>
//
// (any element with data-content; the editor and previews show its text).
// Its e-mails (opt-in confirmations, data exports, and to its
// staff: invites, password resets, campaign and import notices) and its
// public pages (unsubscribe, opt-in, the messages, the subscription form, the
// archive) keep their wording, framed by it: the e-mail and page templates'
// "header" and "footer", which every one of them starts and ends with, become
// the design's top and bottom. Without one, they look as listmonk's.
//
// The design is rendered when the center loads (saving the Config page, or
// the template, reloads it), with no subscriber or campaign, so campaign-only
// links ({{ UnsubscribeURL }}, {{ MessageURL }}, ...) and subscriber or
// campaign fields are refused, on the Config page and when the template is
// saved. A design that stops working is left out (the log says why).

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"log"
	"maps"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/internal/manager"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
)

// denmaTemplateTypeDesign is a design's template type, added to listmonk's
// (template_type) in every center (cmd/denma_features.go). Designs are made
// and edited with the visual editor, like visual campaign templates, and
// aren't campaign templates: campaigns and automations don't list them.
const denmaTemplateTypeDesign = "design"

// denmaDesign is a design, rendered and split at the content's place.
type denmaDesign struct {
	head     string // its top, up to its </head>
	body     string // the rest of its top, from </head>
	bottom   string // after the content's place
	backdrop string // its outer background colour, for the whole page, if any
	marker   string // its content's place, as written (for previews)
}

var (
	// The content's place: an element with data-content, and what's in it.
	// The visual editor's outer background colour (EmailLayout's backdrop).
	reDenmaBackdrop = regexp.MustCompile(`(?is)<body[^>]*>\s*<div style="background-color:\s*(#[0-9a-f]{3,8}|[a-z]+)`)
	reDenmaContent  = regexp.MustCompile(`(?is)<(div|p|span|section|td)\b[^>]*\sdata-content(?:[\s=/][^>]*)?>.*?</(?:div|p|span|section|td)\s*>`)
	// What only a campaign's e-mail has.
	reDenmaCampaignOnly = regexp.MustCompile(`\{\{-?[^}]*?(\b(UnsubscribeURL|ManageURL|OptinURL|MessageURL)\b|\.(Subscriber|Campaign|Tx)\b)`)
)

// denmaDesignMarker is the content's place, as the Config page and errors
// suggest it.
const denmaDesignMarker = `<div data-content>(Page content will appear here)</div>`

// denmaDesignSlot marks the content's place in the rendered design.
const denmaDesignSlot = "denma-design-content-3f9c1e"

// denmaDesignCSS styles an e-mail's content in a design: listmonk's e-mail
// templates use these classes (static/email-templates/base.html).
const denmaDesignCSS = `body { margin: 0; }
.denma-design-content img { max-width: 100%; }
.denma-design-content .button { background: #0055d4; color: #fff !important; display: inline-block; border-radius: 3px; padding: 10px 30px; text-align: center; text-decoration: none; font-weight: bold; }
.denma-design-content .button:hover { background: #222; color: #fff; }`

// renderDenmaDesign renders a design's HTML (a visual template's body) for
// a center, or says what's wrong with it.
func renderDenmaDesign(body string, generic template.FuncMap, ko *koanf.Koanf) (*denmaDesign, error) {
	switch n := len(reDenmaContent.FindAllString(body, -1)); {
	case n == 0:
		return nil, fmt.Errorf("the design has no place for the content: add an HTML block with %s where each e-mail's or page's text goes", denmaDesignMarker)
	case n > 1:
		return nil, fmt.Errorf("the design has %d places for the content (data-content); use one", n)
	}
	if m := reDenmaCampaignOnly.FindStringSubmatch(body); m != nil {
		return nil, fmt.Errorf("the design uses %s, which only campaigns have (these e-mails and pages have no subscriber or campaign): remove it, or make a separate template for the design", strings.TrimPrefix(m[1], "."))
	}

	funcs := denmaDesignFuncs(generic, ko)
	marker := reDenmaContent.FindString(body)
	body = reDenmaContent.ReplaceAllLiteralString(body, denmaDesignSlot)
	tpl, err := template.New("design").Funcs(funcs).Parse(reDenmaTrackLink.ReplaceAllString(body, "$1"))
	if err != nil {
		return nil, fmt.Errorf("the design's template has an error: %v", err)
	}
	var b strings.Builder
	if err := tpl.Execute(&b, nil); err != nil {
		return nil, fmt.Errorf("the design's template has an error: %v", err)
	}
	out := b.String()
	i := strings.Index(out, denmaDesignSlot)
	if i < 0 {
		return nil, fmt.Errorf("the design's place for the content isn't shown (is it inside a condition?)")
	}
	d := &denmaDesign{bottom: out[i+len(denmaDesignSlot):], marker: marker}
	if m := reDenmaBackdrop.FindStringSubmatch(out); m != nil {
		d.backdrop = m[1]
	}
	top := out[:i]
	if j := strings.Index(strings.ToLower(top), "</head>"); j >= 0 {
		d.head, d.body = top[:j], top[j:]
	} else {
		d.head = `<!DOCTYPE html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">`
		d.body = "</head><body>" + top
		if !strings.Contains(strings.ToLower(d.bottom), "</body>") {
			d.bottom += "</body></html>"
		}
	}
	return d, nil
}

// denmaDesignFuncs are what a design can use: listmonk's generic template
// functions, and the center's address, archive, name and logo. Links are as
// they are (no tracking).
func denmaDesignFuncs(generic template.FuncMap, ko *koanf.Koanf) template.FuncMap {
	root := strings.TrimSuffix(ko.String("app.root_url"), "/")
	funcs := template.FuncMap{}
	maps.Copy(funcs, generic)
	maps.Copy(funcs, template.FuncMap{
		"TrackLink":  func(u string, _ ...any) string { return u },
		"TrackView":  func(...any) template.HTML { return "" },
		"RootURL":    func(...any) string { return root },
		"ArchiveURL": func(...any) string { return root + "/archive" },
		"SiteName":   func() string { return ko.String("app.site_name") },
		"LogoURL":    func() string { return ko.String("app.logo_url") },
	})
	return funcs
}

// denmaDesignBody returns a center's design template id's body, or an error
// if it isn't one.
func denmaDesignBody(db sqlx.Queryer, id int) (string, error) {
	var body string
	err := sqlx.Get(db, &body, `SELECT body FROM templates WHERE id = $1 AND type = 'design'`, id)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("choose one of the center's design templates")
	}
	return body, err
}

// denmaPreviewDesign renders a design template for its preview: the design,
// with its content's place showing what it says.
func (a *App) denmaPreviewDesign(body string) ([]byte, error) {
	d, err := renderDenmaDesign(body, a.manager.GenericTemplateFuncs(), a.ko)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusBadRequest, "The design: "+err.Error()+".")
	}
	return []byte(d.head + d.body + d.marker + d.bottom), nil
}

// loadDenmaDesign renders a center's design (ko's denma.design_template)
// when it loads, or returns nil: none, or one that doesn't work (logged).
func loadDenmaDesign(db *sqlx.DB, ko *koanf.Koanf, mgr *manager.Manager, lo *log.Logger) *denmaDesign {
	id := ko.Int("denma.design_template")
	if id == 0 || ko.String("denma.center") == "" {
		return nil
	}
	body, err := denmaDesignBody(db, id)
	if err == nil {
		var d *denmaDesign
		if d, err = renderDenmaDesign(body, mgr.GenericTemplateFuncs(), ko); err == nil {
			return d
		}
	}
	lo.Printf("denma: the design for e-mails and pages (template %d) isn't used: %v", id, err)
	return nil
}

// applyMail frames the center's e-mails: the e-mail templates' header and
// footer (static/email-templates/base.html) become the design's.
func (d *denmaDesign) applyMail(tpls *template.Template) error {
	top := template.HTML(d.head + "<style>" + denmaDesignCSS + "</style>" + d.body + `<div class="denma-design-content">`)
	bottom := template.HTML(`</div>` + d.bottom)
	tpls.Funcs(template.FuncMap{
		"DenmaDesignTop":    func() template.HTML { return top },
		"DenmaDesignBottom": func() template.HTML { return bottom },
	})
	_, err := tpls.New("denma-design").Parse(`{{ define "header" }}{{ DenmaDesignTop }}{{ end }}{{ define "footer" }}{{ DenmaDesignBottom }}{{ end }}`)
	return err
}

// applyPages frames the center's public pages: the public templates' header
// and footer (static/public/templates/index.html) become the design's, with
// the pages' title, stylesheets and icon added to its head.
func (d *denmaDesign) applyPages(tpls *template.Template) error {
	tpls.Funcs(template.FuncMap{
		"DenmaDesignHead":   func() template.HTML { return template.HTML(d.head) },
		"DenmaDesignBody":   func() template.HTML { return template.HTML(d.body) },
		"DenmaDesignBottom": func() template.HTML { return template.HTML(d.bottom) },
		"DenmaDesignCSS": func() template.CSS {
			css := denmaDesignPageCSS
			if d.backdrop != "" {
				css += "html, body { background: " + d.backdrop + "; }"
			}
			return template.CSS(css)
		},
	})
	_, err := tpls.New("denma-design").Parse(`{{ define "header" }}{{ DenmaDesignHead }}
	<title>{{ .Data.Title }} - {{ .SiteName }}</title>
	<meta name="description" content="{{ .Data.Description }}" />
	<link href="{{ .RootURL }}/public/static/style.css?v={{ .AssetVersion }}" rel="stylesheet" type="text/css" />
	<link href="{{ .RootURL }}/public/custom.css?v={{ .CustomAssetVersion }}" rel="stylesheet" type="text/css">
	<script src="{{ .RootURL }}/public/custom.js?v={{ .CustomAssetVersion }}" async defer></script>
	{{ if ne .FaviconURL "" }}<link rel="icon" href="{{ .FaviconURL }}" type="image/x-icon" />{{ else }}<link rel="icon" href="{{ .RootURL }}/public/static/favicon.png?v={{ .AssetVersion }}" type="image/png" />{{ end }}
	<style>{{ DenmaDesignCSS }}</style>
{{ DenmaDesignBody }}<div class="denma-design-content">{{ end }}
{{ define "footer" }}</div>{{ DenmaDesignBottom }}{{ end }}`)
	return err
}

// denmaDesignPageCSS fits the public pages' content (static/public/static/
// style.css, whose .button they keep) in a design: the page is the design's
// backdrop colour throughout (added after this).
const denmaDesignPageCSS = `
html, body { background: none; }
.denma-design-content { text-align: left; }
.denma-design-content img { max-width: 100%; }
.denma-design-content .section { margin-bottom: 24px; }
`

// denmaIsDesign reports whether template id is the center's design.
func (a *App) denmaIsDesign(id int) bool {
	return id != 0 && a.denmaInCenter() && a.ko.Int("denma.design_template") == id
}

// Every center has a design of its own, "Center design (e-mails and pages)"
// (denma.default_design), made when it first loads (cmd/denma_features.go)
// and its design unless another is chosen: listmonk's public pages' look, as
// production's (a white card with a thin border on light grey, the center's
// logo above a line), as a visual template the center can edit. It can't be
// deleted or copied: its content's place carries data-default-design, which
// no other template may have, and it must keep it. Its logo follows the
// Config page's.

// denmaDefaultDesignName is its name.
const denmaDefaultDesignName = "Center design (e-mails and pages)"

// denmaDefaultDesignMarker is its content's place.
const denmaDefaultDesignMarker = `<div data-content data-default-design>(Page content will appear here)</div>`

var reDenmaDefaultDesign = regexp.MustCompile(`(?i)\sdata-default-design[\s=/>]`)

// denmaDefaultDesign returns the default design's HTML and visual editor
// source for a center with this logo and name, as the editor renders them.
func denmaDefaultDesign(logo, name string) (body, source string) {
	return denmaDesignLayout(logo, name, denmaDefaultDesignMarker)
}

// denmaDesignLayout is the default design's layout with this content place
// (a new design starts from it, with denmaDesignMarker).
func denmaDesignLayout(logo, name, marker string) (body, source string) {
	const (
		img  = "block-denma-design-logo"
		line = "block-denma-design-line"
		slot = "block-denma-design-content"
	)
	pad := func(t, r, b, l int) map[string]int {
		return map[string]int{"top": t, "right": r, "bottom": b, "left": l}
	}
	src := map[string]any{
		"root": map[string]any{"type": "EmailLayout", "data": map[string]any{
			"backdropColor": "#F9F9F9", "canvasColor": "#FFFFFF", "textColor": "#111111",
			"borderColor": "#EEEEEE", "fontFamily": "MODERN_SANS",
			"childrenIds": []string{img, line, slot},
		}},
		img: map[string]any{"type": "Image", "data": map[string]any{
			"style": map[string]any{"padding": pad(40, 40, 15, 40), "textAlign": "left"},
			"props": map[string]any{"url": logo, "alt": name, "linkHref": nil, "height": 90, "contentAlignment": "middle"},
		}},
		line: map[string]any{"type": "Divider", "data": map[string]any{
			"style": map[string]any{"padding": pad(0, 40, 0, 40)},
			"props": map[string]any{"lineColor": "#EEEEEE"},
		}},
		slot: map[string]any{"type": "Html", "data": map[string]any{
			"style": map[string]any{"fontSize": 16, "padding": pad(30, 40, 40, 40)},
			"props": map[string]any{"contents": marker},
		}},
	}
	b, _ := json.Marshal(src)
	body = `<!DOCTYPE html><html><head><meta name="viewport" content="width=device-width, initial-scale=1.0"></head><body>` +
		`<div style="background-color:#F9F9F9;color:#111111;font-family:&quot;Helvetica Neue&quot;, &quot;Arial Nova&quot;, &quot;Nimbus Sans&quot;, Arial, sans-serif;font-size:16px;font-weight:400;letter-spacing:0.15008px;line-height:1.5;margin:0;padding:32px 0;min-height:100%;width:100%">` +
		`<table align="center" width="100%" style="margin:0 auto;max-width:600px;background-color:#FFFFFF;border:1px solid #EEEEEE" role="presentation" cellSpacing="0" cellPadding="0" border="0"><tbody><tr style="width:100%"><td>` +
		`<div style="padding:40px 40px 15px 40px;text-align:left"><img alt="` + html.EscapeString(name) + `" src="` + html.EscapeString(logo) + `" height="90" style="height:90px;outline:none;border:none;text-decoration:none;vertical-align:middle;display:inline-block;max-width:100%"/></div>` +
		`<div style="padding:0px 40px 0px 40px"><hr style="width:100%;border:none;border-top:1px solid #EEEEEE;margin:0"/></div>` +
		`<div style="font-size:16px;padding:30px 40px 40px 40px">` + marker + `</div>` +
		`</td></tr></tbody></table></div></body></html>`
	return body, string(b)
}

// denmaDesignLogo is the logo a center's default design shows: its own, or
// listmonk's.
func denmaDesignLogo(logoURL, rootURL string) string {
	if logoURL != "" {
		return logoURL
	}
	return strings.TrimSuffix(rootURL, "/") + "/public/static/logo.svg"
}

// denmaAddDefaultDesign makes a center's default design (in tx, its schema)
// if it has none, and makes it the center's design if none is chosen.
func denmaAddDefaultDesign(tx *sqlx.Tx) error {
	var id int
	if err := tx.Get(&id, `SELECT COALESCE((SELECT t.id FROM settings s JOIN templates t ON t.id = (s.value #>> '{}')::INT
		WHERE s.key = 'denma.default_design'), 0)`); err != nil {
		return err
	}
	if id == 0 {
		var name, logo, root string
		if err := tx.QueryRow(`SELECT COALESCE((SELECT value #>> '{}' FROM settings WHERE key = 'app.site_name'), ''),
			COALESCE((SELECT value #>> '{}' FROM settings WHERE key = 'app.logo_url'), ''),
			COALESCE((SELECT value #>> '{}' FROM settings WHERE key = 'app.root_url'), '')`).Scan(&name, &logo, &root); err != nil {
			return err
		}
		body, src := denmaDefaultDesign(denmaDesignLogo(logo, root), name)
		if err := tx.Get(&id, `INSERT INTO templates (name, type, subject, body, body_source) VALUES ($1, 'design', '', $2, $3) RETURNING id`,
			denmaDefaultDesignName, body, src); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES ('denma.default_design', $1::TEXT::JSONB)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, strconv.Itoa(id)); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE settings SET value = $1::TEXT::JSONB WHERE key = 'denma.design_template' AND COALESCE((value #>> '{}')::INT, 0) = 0`, strconv.Itoa(id))
	return err
}

// denmaDefaultDesignID is the center's default design's template id.
func (a *App) denmaDefaultDesignID() int {
	if !a.denmaInCenter() {
		return 0
	}
	return a.ko.Int("denma.default_design")
}

// denmaCheckTemplate refuses a template (id, or 0 for a new one) that would
// copy the default design, a change to the default design that drops what
// makes it one, and a design that couldn't be used; and designs outside
// centers.
func (a *App) denmaCheckTemplate(id int, o models.Template) error {
	if o.Type == denmaTemplateTypeDesign && !a.denmaInCenter() {
		return echo.NewHTTPError(http.StatusBadRequest, "Designs are for centers.")
	}
	def := a.denmaDefaultDesignID()
	has := reDenmaDefaultDesign.MatchString(o.Body)
	switch {
	case def != 0 && id != def && has:
		return echo.NewHTTPError(http.StatusBadRequest, "The center's design can't be copied. Make a new design for another.")
	case def != 0 && id == def && !has:
		return echo.NewHTTPError(http.StatusBadRequest, "Keep the HTML block with "+denmaDefaultDesignMarker+" in the center's design: it's where each e-mail's or page's text goes.")
	}
	if o.Type == denmaTemplateTypeDesign {
		if _, err := renderDenmaDesign(o.Body, a.manager.GenericTemplateFuncs(), a.ko); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "The design: "+err.Error()+".")
		}
	}
	return nil
}

// denmaNewDesign is a new design's start: the default design's layout, with
// the center's logo and name.
func (a *App) denmaNewDesign() map[string]string {
	body, src := denmaDesignLayout(denmaDesignLogo(a.ko.String("app.logo_url"), a.urlCfg.RootURL), a.ko.String("app.site_name"), denmaDesignMarker)
	return map[string]string{"body": body, "body_source": src}
}

// denmaDesignLogoChanged puts a center's new logo in its default design, in
// place of the old.
func (a *App) denmaDesignLogoChanged(oldLogo, newLogo string) {
	id := a.denmaDefaultDesignID()
	root := a.urlCfg.RootURL
	from, to := denmaDesignLogo(oldLogo, root), denmaDesignLogo(newLogo, root)
	if id == 0 || from == to {
		return
	}
	fromJS, _ := json.Marshal(from)
	toJS, _ := json.Marshal(to)
	if _, err := a.db.Exec(`UPDATE templates SET body = replace(body, $2, $3), body_source = replace(body_source, $4, $5), updated_at = NOW() WHERE id = $1`,
		id, html.EscapeString(from), html.EscapeString(to), string(fromJS), string(toJS)); err != nil {
		a.log.Printf("denma: error putting the new logo in the center's design: %v", err)
	}
}
