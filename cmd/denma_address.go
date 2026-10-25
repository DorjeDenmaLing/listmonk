package main

// denma: each center's postal address (Config -> General, denma.postal_address;
// asked for when a center is made). Canada's anti-spam law (CASL) requires
// the sender's mailing address in every commercial e-mail, so no campaign or
// automation e-mail goes without it:
//
//   - {{ CenterAddress }} puts it in an e-mail or design (on one line), in
//     every template, campaign and automation e-mail.
//   - A campaign doesn't start, nor is scheduled, nor sends a test, and an
//     automation isn't saved, turned on or run, unless the center has an
//     address and its e-mail shows it (as rendered: the tag, or the address
//     written in). Each campaign and automation e-mail is checked again as
//     it's sent (denmaCountedMessenger, cmd/denma_daily.go), and refused
//     without it.
//   - The templates a center starts with show it in their footer, as does its
//     design, which frames its opt-in confirmations and public pages
//     (features version 39 added it to the stock ones centers had).
//
// Transactional e-mails (receipts, password resets) and notices don't need it.
// A save applies at once, as the sending hours do.

import (
	"encoding/json"
	"errors"
	"html"
	"html/template"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf/v2"
	"github.com/labstack/echo/v4"
)

const denmaAddressTag = "{{ CenterAddress }}"

var (
	errDenmaNoAddress      = errors.New("Enter the center's postal address on Config → General first. Canada's anti-spam law (CASL) requires it in every campaign and automation e-mail, so none is sent without it.")
	errDenmaAddressMissing = errors.New("The e-mail doesn't show the center's postal address, which Canada's anti-spam law (CASL) requires in every campaign and automation e-mail. Put " +
		denmaAddressTag + " in its footer: in its template (Campaigns → Templates), or in a visual campaign's own.")
)

// denmaAddresses holds each center's address (by slug), changed at once on
// a save.
var denmaAddresses sync.Map // slug -> *atomic.Pointer[string]

func denmaAddressOf(slug string) *atomic.Pointer[string] {
	p, _ := denmaAddresses.LoadOrStore(slug, &atomic.Pointer[string]{})
	return p.(*atomic.Pointer[string])
}

// denmaAddress is a center's address, "" for none (or the hub).
func denmaAddress(slug string) string {
	if slug == "" {
		return ""
	}
	if s := denmaAddressOf(slug).Load(); s != nil {
		return *s
	}
	return ""
}

// denmaSetAddress sets a center's address.
func denmaSetAddress(slug, addr string) {
	if slug != "" {
		denmaAddressOf(slug).Store(&addr)
	}
}

// denmaAddressFuncs sets a center's address from its settings (ko) and adds
// CenterAddress to its e-mails' template functions (the campaign manager's,
// which campaigns, automations, transactional templates and the design use).
func denmaAddressFuncs(funcs template.FuncMap, ko *koanf.Koanf) {
	slug := ko.String("denma.center")
	denmaSetAddress(slug, ko.String("denma.postal_address"))
	funcs["CenterAddress"] = func() string {
		return denmaAddressLine(denmaAddress(slug))
	}
}

// denmaTidyAddress is an address as entered, each line trimmed, without
// empty ones.
func denmaTidyAddress(s string) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.Join(strings.Fields(l), " "); l != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}

// denmaCheckAddressInput tidies an address entered for a center and checks
// it looks like one.
func denmaCheckAddressInput(s string) (string, error) {
	s = denmaTidyAddress(s)
	switch {
	case s == "":
		return "", errors.New("Enter the center's postal address: Canada's anti-spam law (CASL) requires it in every campaign and automation e-mail.")
	case len(s) > 500 || strings.Count(s, "\n") >= 6:
		return "", errors.New("The postal address can be up to 6 lines and 500 characters.")
	case len(s) < 10 || !strings.ContainsAny(s, "0123456789"):
		return "", errors.New("Enter the center's full postal address: street or PO box, town, province and postal code.")
	}
	return s, nil
}

// denmaAddressLine is an address on one line, as e-mails show it.
func denmaAddressLine(addr string) string {
	return strings.ReplaceAll(addr, "\n", ", ")
}

var (
	reDenmaTags  = regexp.MustCompile(`(?s)<!--.*?-->|<[^>]*>`)
	reDenmaPunct = regexp.MustCompile(`[,;·|]`)
)

// denmaPlainWords is text (or HTML's text) for comparing: lowercase, without
// commas, spaces collapsed.
func denmaPlainWords(s string, isHTML bool) string {
	if isHTML {
		s = html.UnescapeString(reDenmaTags.ReplaceAllString(s, " "))
	}
	return strings.Join(strings.Fields(strings.ToLower(reDenmaPunct.ReplaceAllString(s, " "))), " ")
}

// denmaShowsAddress reports whether an e-mail's body (HTML or plain text)
// shows the address.
func denmaShowsAddress(body []byte, addr string) bool {
	want := denmaPlainWords(addr, false)
	return want != "" && strings.Contains(denmaPlainWords(string(body), true), want)
}

// denmaAddressCheck refuses an e-mail of a center (slug; none for the hub)
// that doesn't show its address, or that it hasn't one.
func denmaAddressCheck(slug string, body []byte) error {
	if slug == "" {
		return nil
	}
	addr := denmaAddress(slug)
	if addr == "" {
		return errDenmaNoAddress
	}
	if !denmaShowsAddress(body, addr) {
		return errDenmaAddressMissing
	}
	return nil
}

// denmaCheckAddress refuses an e-mail (its body as sent) of a's center that
// doesn't show the center's address.
func (a *App) denmaCheckAddress(body []byte) error {
	if !a.ko.Bool("denma.multi_center") {
		return nil
	}
	return denmaAddressCheck(a.ko.String("denma.center"), body)
}

// denmaCheckMessage refuses a campaign's or automation's e-mail (its body as
// rendered) that doesn't show the center's address, or has images no longer
// in its Media (cmd/denma_images.go).
func (a *App) denmaCheckMessage(body []byte) error {
	if err := a.denmaCheckAddress(body); err != nil {
		return err
	}
	return a.denmaCheckImages(body)
}

// denmaCheckContent refuses to start or schedule a campaign whose e-mail
// fails denmaCheckMessage. Called by denmaCheckCampaignStart.
func (a *App) denmaCheckContent(id int) error {
	camp, err := a.core.GetCampaignForPreview(id, 0)
	if err != nil {
		return err
	}
	camp.UUID = dummySubscriber.UUID // no views or clicks are recorded
	if err := camp.CompileTemplate(a.manager.TemplateFuncs(&camp)); err != nil {
		return nil // denmaCheckCompiles says so
	}
	msg, err := a.manager.NewCampaignMessage(&camp, dummySubscriber)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, a.i18n.Ts("templates.errorRendering", "error", err.Error()))
	}
	if err := a.denmaCheckMessage(msg.Body()); err != nil {
		return denmaBadRequest(err)
	}
	return nil
}

// denmaAddressAnchor is where the stock templates' footer gets the address:
// after their "View in browser" link.
const denmaAddressAnchor = `{{ L.T "email.viewInBrowser" }}</a>`

// denmaWithAddress is a template's HTML with the address after its "View in
// browser" link, and whether it was put there; HTML that shows it already,
// or hasn't the link, is left as it is.
func denmaWithAddress(body string) (string, bool) {
	if strings.Contains(body, "CenterAddress") || !strings.Contains(body, denmaAddressAnchor) {
		return body, false
	}
	return strings.Replace(body, denmaAddressAnchor, denmaAddressAnchor+"<br>"+denmaAddressTag, 1), true
}

// denmaAddAddressToTemplates adds the address to the footer of the center's
// campaign and visual templates that have the stock one (in tx, its schema),
// and to its default design, if it's the stock layout. Returns how many it
// changed.
func denmaAddAddressToTemplates(tx *sqlx.Tx) (int, error) {
	var tpls []struct {
		ID     int     `db:"id"`
		Body   string  `db:"body"`
		Source *string `db:"body_source"`
	}
	if err := tx.Select(&tpls, `SELECT id, body, body_source FROM templates WHERE type IN ('campaign', 'campaign_visual')
		AND strpos(body, 'CenterAddress') = 0 AND strpos(body, $1) > 0`, denmaAddressAnchor); err != nil {
		return 0, err
	}
	n := 0
	for _, t := range tpls {
		body, ok := denmaWithAddress(t.Body)
		if !ok {
			continue
		}
		src := t.Source
		if src != nil && *src != "" {
			s, err := denmaSourceWithAddress(*src)
			if err != nil {
				continue // not the visual editor's, left as it is
			}
			src = &s
		}
		if _, err := tx.Exec(`UPDATE templates SET body = $2, body_source = $3, updated_at = NOW() WHERE id = $1`, t.ID, body, src); err != nil {
			return n, err
		}
		n++
	}
	ok, err := denmaAddAddressToDesign(tx)
	if ok {
		n++
	}
	return n, err
}

// denmaSourceWithAddress is a visual template's source with the address
// after the "View in browser" link of its text or HTML block.
func denmaSourceWithAddress(src string) (string, error) {
	var blocks map[string]map[string]any
	if err := json.Unmarshal([]byte(src), &blocks); err != nil {
		return "", err
	}
	for _, b := range blocks {
		data, _ := b["data"].(map[string]any)
		props, _ := data["props"].(map[string]any)
		for _, k := range []string{"text", "contents"} {
			if s, ok := props[k].(string); ok {
				if out, ok := denmaWithAddress(s); ok {
					props[k] = out
					out, err := json.Marshal(blocks)
					return string(out), err
				}
			}
		}
	}
	return src, nil
}

// The default design's address block (denmaDesignLayout).
const (
	denmaDesignAddrBlock = "block-denma-design-address"
	denmaDesignAddrHTML  = `<div style="color:#888888;font-size:12px;text-align:center;padding:0px 40px 30px 40px">` + denmaAddressTag + `</div>`
	denmaDesignEnd       = `</td></tr></tbody></table></div></body></html>`
)

// denmaDesignAddress is the design's address block, for its source.
func denmaDesignAddress(pad func(t, r, b, l int) map[string]int) map[string]any {
	return map[string]any{"type": "Text", "data": map[string]any{
		"style": map[string]any{"color": "#888888", "fontSize": 12, "textAlign": "center", "padding": pad(0, 40, 30, 40)},
		"props": map[string]any{"text": denmaAddressTag},
	}}
}

// denmaAddAddressToDesign adds the address below the center's default
// design's content, if it's still the stock layout and hasn't it.
func denmaAddAddressToDesign(tx *sqlx.Tx) (bool, error) {
	var t struct {
		ID     int    `db:"id"`
		Body   string `db:"body"`
		Source string `db:"body_source"`
	}
	err := tx.Get(&t, `SELECT t.id, t.body, COALESCE(t.body_source, '') AS body_source FROM settings s
		JOIN templates t ON t.id = (s.value #>> '{}')::INT WHERE s.key = 'denma.default_design'`)
	if err != nil || strings.Contains(t.Body, "CenterAddress") || !strings.HasSuffix(t.Body, denmaDesignEnd) {
		return false, nil
	}
	var src map[string]map[string]any
	if err := json.Unmarshal([]byte(t.Source), &src); err != nil {
		return false, nil
	}
	root, _ := src["root"]["data"].(map[string]any)
	ids, _ := root["childrenIds"].([]any)
	if root == nil || len(ids) != 3 || ids[2] != "block-denma-design-content" {
		return false, nil
	}
	root["childrenIds"] = append(ids, denmaDesignAddrBlock)
	src[denmaDesignAddrBlock] = denmaDesignAddress(func(t, r, b, l int) map[string]int {
		return map[string]int{"top": t, "right": r, "bottom": b, "left": l}
	})
	b, err := json.Marshal(src)
	if err != nil {
		return false, err
	}
	body := strings.TrimSuffix(t.Body, denmaDesignEnd) + denmaDesignAddrHTML + denmaDesignEnd
	_, err = tx.Exec(`UPDATE templates SET body = $2, body_source = $3, updated_at = NOW() WHERE id = $1`, t.ID, body, string(b))
	return err == nil, err
}

// denmaAddressTemplateNames is the center's templates whose e-mails don't
// show its address (as campaigns and automations would send them), for the
// Config page.
func (a *App) denmaAddressTemplateNames() []string {
	var tpls []struct {
		Name string `db:"name"`
		Body string `db:"body"`
	}
	if err := a.db.Select(&tpls, `SELECT name, body FROM templates WHERE type IN ('campaign', 'campaign_visual')
		AND strpos(body, 'CenterAddress') = 0 ORDER BY name`); err != nil {
		a.log.Printf("denma: error reading the templates: %v", err)
	}
	// Those with the address written in show it too.
	addr := denmaPlainWords(denmaAddress(a.ko.String("denma.center")), false)
	out := []string{}
	for _, t := range tpls {
		if addr == "" || !strings.Contains(denmaPlainWords(t.Body, true), addr) {
			out = append(out, t.Name)
		}
	}
	return out
}
