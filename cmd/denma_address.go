package main

// denma: each center's postal address (Config -> General, denma.postal_address;
// asked for when a center is made). Canada's anti-spam law (CASL) requires
// the sender's mailing address in every commercial e-mail, so it's part of
// the footer every campaign and automation e-mail must have
// (cmd/denma_footer.go, which checks it):
//
//   - {{ CenterAddress }} puts it in an e-mail or design, a line for each of
//     its lines; a plain text gets its lines as text ({{ CenterAddressText }},
//     models/denma_address.go).
//   - The templates a center starts with show it in their footer, as does its
//     design, which frames its opt-in confirmations and public pages
//     (features version 39 added it to the stock ones centers had).
//
// A save applies at once, as the sending hours do.

import (
	"encoding/json"
	"errors"
	"html"
	"html/template"
	"net/http"
	"regexp"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
)

const denmaAddressTag = "{{ CenterAddress }}"

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

// denmaAddressHTML is an address as e-mails show it, its lines apart.
func denmaAddressHTML(addr string) template.HTML {
	lines := strings.Split(addr, "\n")
	for i, l := range lines {
		lines[i] = html.EscapeString(l)
	}
	return template.HTML(strings.Join(lines, "<br>"))
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

// denmaCheckMessage refuses a campaign's or automation's e-mail (its body as
// rendered) that doesn't show the center's details (cmd/denma_footer.go), or
// has images no longer in its Media (cmd/denma_images.go).
func (a *App) denmaCheckMessage(body []byte) error {
	if err := a.denmaCheckFooter(body); err != nil {
		return err
	}
	return a.denmaCheckImages(body)
}

// denmaCheckContent refuses to start or schedule a campaign whose e-mail
// hasn't the footer's links or fails denmaCheckMessage. Called by denmaCheckCampaignStart.
func (a *App) denmaCheckContent(id int) error {
	camp, err := a.core.GetCampaignForPreview(id, 0)
	if err != nil {
		return err
	}
	if err := a.denmaCheckLinks(denmaCampaignSource(&camp)); err != nil { // cmd/denma_footer.go
		return denmaBadRequest(err)
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
	n, err := denmaEditStockFooters(tx, denmaAddressAnchor, denmaWithAddress)
	if err != nil {
		return n, err
	}
	ok, err := denmaAddAddressToDesign(tx)
	if ok {
		n++
	}
	return n, err
}

// denmaEditStockFooters changes the footer of the center's campaign and
// visual templates that have anchor (their source too) with edit, which says
// whether it changed one. Returns how many it changed.
func denmaEditStockFooters(tx *sqlx.Tx, anchor string, edit func(string) (string, bool)) (int, error) {
	var tpls []struct {
		ID     int     `db:"id"`
		Body   string  `db:"body"`
		Source *string `db:"body_source"`
	}
	if err := tx.Select(&tpls, `SELECT id, body, body_source FROM templates WHERE type IN ('campaign', 'campaign_visual')
		AND strpos(body, $1) > 0`, anchor); err != nil {
		return 0, err
	}
	n := 0
	for _, t := range tpls {
		body, ok := edit(t.Body)
		if !ok {
			continue
		}
		src := t.Source
		if src != nil && *src != "" {
			s, err := denmaEditSource(*src, edit)
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
	return n, nil
}

// denmaEditSource is a visual template's source with its first text or HTML
// block that edit changes changed.
func denmaEditSource(src string, edit func(string) (string, bool)) (string, error) {
	var blocks map[string]map[string]any
	if err := json.Unmarshal([]byte(src), &blocks); err != nil {
		return "", err
	}
	for _, b := range blocks {
		data, _ := b["data"].(map[string]any)
		props, _ := data["props"].(map[string]any)
		for _, k := range []string{"text", "contents"} {
			if s, ok := props[k].(string); ok {
				if out, ok := edit(s); ok {
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
