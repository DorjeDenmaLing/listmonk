package main

// denma: a center's Advanced page (sidebar, in a center): the center's own
// details, which aren't the hub's settings (denmaCenterOwnSettings): its
// name, logo, favicon and language, and its sender and admin notification
// e-mails. For users with center:manage (Center Admins, and superadmins).
// Saving reloads the center.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"path"
	"strings"

	"github.com/knadh/koanf/v2"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

const denmaCenterPerm = "center:manage"

// denmaCenterFields are the settings on the Advanced page (all of them a
// center's own, in denmaCenterOwnSettings).
var denmaCenterFields = []string{
	"app.site_name", "app.logo_url", "app.favicon_url", "app.lang",
	"app.from_email", "app.notify_emails",
	// The features (cmd/denma_features.go).
	"denma.unsubscribe_everywhere", "denma.plain_text_auto", "denma.utm_domains",
	"denma.signup_holding_list", "denma.signup_target_list", "denma.visual_template",
}

// denmaPermissions adds ours to listmonk's permissions (permissions.json), so
// that roles can grant them: automations' to the Campaigns group, and
// center:manage, in multi-center mode, to the Settings group. It drops
// subscribers:sql_query, as SQL queries are off (cmd/denma_search.go).
func denmaPermissions(raw []byte, ko *koanf.Koanf) []byte {
	var groups []map[string]any
	if err := json.Unmarshal(raw, &groups); err != nil {
		return raw
	}
	for _, g := range groups {
		p, ok := g["permissions"].([]any)
		if !ok {
			continue
		}
		switch {
		case g["group"] == "subscribers":
			keep := p[:0]
			for _, x := range p {
				if x != "subscribers:sql_query" {
					keep = append(keep, x)
				}
			}
			g["permissions"] = keep
		case g["group"] == "campaigns":
			g["permissions"] = append(p, denmaAutoGet, denmaAutoManage)
		case g["group"] == "settings" && ko.Bool("denma.multi_center"):
			g["permissions"] = append(p, denmaCenterPerm)
		}
	}
	out, err := json.Marshal(groups)
	if err != nil {
		return raw
	}
	return out
}

// initDenmaCenterHandlers registers the Advanced page and its API.
func initDenmaCenterHandlers(g *echo.Group, a *App) {
	g.GET(path.Join(uriAdmin, "/center"), a.ViewDenmaCenter)
}

func initDenmaCenterAPIHandlers(g *echo.Group, a *App) {
	g.PUT("/api/denma/center", a.auth.Perm(a.DenmaUpdateCenter, denmaCenterPerm))
}

// denmaCenterForm is the Advanced page's form.
type denmaCenterForm struct {
	SiteName     string   `json:"site_name"`
	LogoURL      string   `json:"logo_url"`
	FaviconURL   string   `json:"favicon_url"`
	Lang         string   `json:"lang"`
	FromEmail    string   `json:"from_email"`
	NotifyEmails []string `json:"notify_emails"`

	// The features (cmd/denma_features.go).
	UnsubscribeEverywhere bool     `json:"unsubscribe_everywhere"`
	PlainTextAuto         bool     `json:"plain_text_auto"`
	UTMDomains            []string `json:"utm_domains"`
	SignupHoldingList     int      `json:"signup_holding_list"`
	SignupTargetList      int      `json:"signup_target_list"`
	VisualTemplate        int      `json:"visual_template"`
}

var denmaFormKeys = map[string]string{
	"site_name": "app.site_name", "logo_url": "app.logo_url", "favicon_url": "app.favicon_url",
	"lang": "app.lang", "from_email": "app.from_email", "notify_emails": "app.notify_emails",
	"unsubscribe_everywhere": "denma.unsubscribe_everywhere", "plain_text_auto": "denma.plain_text_auto",
	"utm_domains": "denma.utm_domains", "signup_holding_list": "denma.signup_holding_list",
	"signup_target_list": "denma.signup_target_list", "visual_template": "denma.visual_template",
}

type denmaCenterView struct {
	adminView
	Form    denmaCenterForm
	Address string
	Langs   []i18nLang

	// For the features' choices.
	Lists           []denmaOption
	VisualTemplates []denmaOption
}

// denmaOption is a list or template to choose.
type denmaOption struct {
	ID    int    `db:"id" json:"id"`
	Name  string `db:"name" json:"name"`
	Optin string `db:"optin" json:"optin,omitempty"`
}

// inCenter returns an error unless the App is a center (not the hub, and in
// multi-center mode).
func (a *App) inCenter() error {
	if denmaHub == nil || a.ko.String("denma.center") == "" {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	return nil
}

// centerForm reads the Advanced page's settings from the center's database.
func (a *App) centerForm() (denmaCenterForm, error) {
	var rows []struct {
		Key   string          `db:"key"`
		Value json.RawMessage `db:"value"`
	}
	if err := a.db.Select(&rows, `SELECT key, value FROM settings WHERE key = ANY($1)`, pq.Array(denmaCenterFields)); err != nil {
		return denmaCenterForm{}, err
	}
	m := map[string]json.RawMessage{}
	for _, r := range rows {
		m[r.Key] = r.Value
	}
	obj := map[string]json.RawMessage{}
	for f, k := range denmaFormKeys {
		if v, ok := m[k]; ok {
			obj[f] = v
		}
	}
	b, _ := json.Marshal(obj)
	var out denmaCenterForm
	err := json.Unmarshal(b, &out)
	if out.NotifyEmails == nil {
		out.NotifyEmails = []string{}
	}
	if out.UTMDomains == nil {
		out.UTMDomains = []string{}
	}
	return out, err
}

// ViewDenmaCenter renders the Advanced page.
func (a *App) ViewDenmaCenter(c echo.Context) error {
	if err := a.inCenter(); err != nil {
		return err
	}
	v := newAdminView(c, "Advanced", "", "denma.center")
	if !v.Can(denmaCenterPerm) {
		return echo.NewHTTPError(http.StatusForbidden, a.i18n.Ts("globals.messages.permissionDenied", "name", denmaCenterPerm))
	}
	form, err := a.centerForm()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	langs, err := getI18nLangList(a.fs)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	view := denmaCenterView{adminView: v, Form: form, Address: a.urlCfg.RootURL, Langs: langs,
		Lists: []denmaOption{}, VisualTemplates: []denmaOption{}}
	if err := a.db.Select(&view.Lists, `SELECT id, name, optin::TEXT AS optin FROM lists ORDER BY name`); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if err := a.db.Select(&view.VisualTemplates, `SELECT id, name, '' AS optin FROM templates WHERE type = 'campaign_visual' ORDER BY name`); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.Render(http.StatusOK, "admin-denma-center", view)
}

// DenmaUpdateCenter saves the Advanced page and reloads the center (after
// any campaign it's sending).
func (a *App) DenmaUpdateCenter(c echo.Context) error {
	if err := a.inCenter(); err != nil {
		return err
	}
	var f denmaCenterForm
	if err := c.Bind(&f); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	f.SiteName = strings.TrimSpace(f.SiteName)
	f.FromEmail = strings.TrimSpace(f.FromEmail)
	f.LogoURL = strings.TrimSpace(f.LogoURL)
	f.FaviconURL = strings.TrimSpace(f.FaviconURL)

	bad := func(msg string) error { return echo.NewHTTPError(http.StatusBadRequest, msg) }
	if !strHasLen(f.SiteName, 1, 200) {
		return bad("Enter the center's name.")
	}
	if _, err := mail.ParseAddress(f.FromEmail); err != nil {
		return bad(`The sender isn't a valid address. Use name@example.org or "Name" <name@example.org>.`)
	}
	emails := []string{}
	for _, e := range f.NotifyEmails {
		if e = strings.TrimSpace(e); e == "" {
			continue
		}
		addr, err := a.importer.SanitizeEmail(e)
		if err != nil {
			return bad(fmt.Sprintf("%q isn't a valid e-mail address.", e))
		}
		emails = append(emails, addr)
	}
	if len(emails) > 20 {
		return bad("Use at most 20 admin e-mail addresses.")
	}
	f.NotifyEmails = emails
	for _, u := range []string{f.LogoURL, f.FaviconURL} {
		if u != "" && !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "/") {
			return bad("The logo and favicon need full addresses (https://...), or none for listmonk's.")
		}
		if len(u) > 2000 {
			return bad("The logo or favicon address is too long.")
		}
	}
	langs, err := getI18nLangList(a.fs)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	known := false
	for _, l := range langs {
		known = known || l.Code == f.Lang
	}
	if !known {
		return bad("Choose a language from the list.")
	}
	if err := a.denmaCheckFeatures(&f); err != nil {
		return bad(err.Error())
	}
	// Save each setting.
	b, _ := json.Marshal(f)
	var vals map[string]json.RawMessage
	_ = json.Unmarshal(b, &vals)
	tx, err := a.db.Beginx()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	defer tx.Rollback()
	for field, key := range denmaFormKeys {
		if _, err := tx.Exec(`UPDATE settings SET value = $1::jsonb, updated_at = NOW() WHERE key = $2`, string(vals[field]), key); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
	}
	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	// The hub's list shows the registry's name for centers that aren't running.
	slug := a.ko.String("denma.center")
	if _, err := denmaHub.base.db.Exec(`UPDATE denma.centers SET name = $1 WHERE slug = $2`, f.SiteName, slug); err != nil {
		a.log.Printf("denma: error renaming center %s in the registry: %v", slug, err)
	}

	// Reload, now or once any running campaign has finished.
	ctr := denmaHub.get(slug)
	reloading := true
	if ctr != nil && ctr.app == a && a.manager.HasRunningCampaigns() {
		a.Lock()
		a.needsRestart = true
		a.Unlock()
		go denmaHub.reloadWhenIdle(ctr)
		reloading = false
	} else {
		denmaSignalReload(a)
	}
	return c.JSON(http.StatusOK, okResp{map[string]bool{"reloading": reloading}})
}
