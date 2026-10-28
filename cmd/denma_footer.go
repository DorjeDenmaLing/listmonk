package main

// denma: what every campaign and automation e-mail must have in its footer:
// the center's postal address (cmd/denma_address.go), its website and contact
// e-mail (Config -> General, denma.website and denma.contact_email; asked for
// when a center is made), an unsubscribe link and a "view in browser" link.
// Canada's anti-spam law (CASL) requires the address, a way to get in touch
// and the unsubscribe link.
//
//   - {{ CenterWebsite }} and {{ CenterEmail }} show the website and the
//     e-mail address, as links; {{ CenterFooter }} writes the two links
//     (models/denma_footer.go), in campaigns and automations.
//   - A campaign or visual template isn't saved without them all (the
//     center's details may be written in), and Config lists those that lack
//     any. A campaign isn't started, scheduled or tested, nor an automation
//     saved, turned on or run, unless the center has its details, its e-mail
//     has the links (their tags) and, as rendered, shows the details. Each
//     campaign and automation e-mail is checked for the details again as it's
//     sent (denmaCountedMessenger, cmd/denma_daily.go).
//   - Every campaign gets the open-tracking pixel ({{ TrackView }}), so the
//     tracking settings alone decide whether opens are counted.
//
// Transactional e-mails and notices don't need any of it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/models"
)

const denmaFooterTag = "{{ CenterFooter }}"

// denmaContact is what a center's e-mails show of it.
type denmaContact struct {
	Address string // its lines, "\n" between them
	Website string // a URL
	Email   string
}

// complete reports whether the center has all its details.
func (c denmaContact) complete() bool {
	return c.Address != "" && c.Website != "" && c.Email != ""
}

var errDenmaNoContact = errors.New("Enter the center's postal address, website and contact e-mail on Config → General first. Every campaign and automation e-mail shows them (Canada's anti-spam law, CASL, requires the address and a way to get in touch), so none is sent without them.")

// denmaContacts holds each center's details (by slug), changed at once on a
// save.
var denmaContacts sync.Map // slug -> *atomic.Pointer[denmaContact]

func denmaContactPtr(slug string) *atomic.Pointer[denmaContact] {
	p, _ := denmaContacts.LoadOrStore(slug, &atomic.Pointer[denmaContact]{})
	return p.(*atomic.Pointer[denmaContact])
}

// denmaContactOf is a center's details, none for the hub.
func denmaContactOf(slug string) denmaContact {
	if slug == "" {
		return denmaContact{}
	}
	if c := denmaContactPtr(slug).Load(); c != nil {
		return *c
	}
	return denmaContact{}
}

// denmaSetContact sets a center's details.
func denmaSetContact(slug string, c denmaContact) {
	if slug != "" {
		denmaContactPtr(slug).Store(&c)
	}
}

// denmaFooterFuncs sets a center's details from its settings (ko) and adds
// their tags to its e-mails' template functions (the campaign manager's,
// which campaigns, automations, transactional templates and the design use).
func denmaFooterFuncs(funcs template.FuncMap, ko *koanf.Koanf) {
	slug := ko.String("denma.center")
	denmaSetContact(slug, denmaContact{Address: ko.String("denma.postal_address"),
		Website: ko.String("denma.website"), Email: ko.String("denma.contact_email")})
	c := func() denmaContact { return denmaContactOf(slug) }
	funcs["CenterAddress"] = func() template.HTML { return denmaAddressHTML(c().Address) }
	funcs["CenterAddressText"] = func() string { return c().Address }
	funcs["CenterWebsite"] = func() template.HTML { return denmaLinkHTML(c().Website, denmaWebsiteText(c().Website)) }
	funcs["CenterWebsiteText"] = func() string { return c().Website }
	funcs["CenterEmail"] = func() template.HTML { return denmaLinkHTML("mailto:"+c().Email, c().Email) }
	funcs["CenterEmailText"] = func() string { return c().Email }
	// Written out before a campaign or automation e-mail is compiled.
	funcs["CenterFooter"] = func() (string, error) { return "", errDenmaFooterOnlyCampaigns }
}

var errDenmaFooterOnlyCampaigns = errors.New(denmaFooterTag + " is for campaign and visual templates. Other e-mails can use {{ CenterAddress }}, {{ CenterWebsite }} and {{ CenterEmail }}.")

// denmaWebsiteHost is a center's website's host, lowercase, or "": its
// e-mails link to it ({{ CenterWebsite }}), so it's one of the center's own
// link domains (cmd/denma_api_send.go).
func denmaWebsiteHost(slug string) string {
	u, err := url.Parse(denmaContactOf(slug).Website)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// denmaLinkHTML is a link in a footer, in its colour; nothing without text.
func denmaLinkHTML(href, text string) template.HTML {
	if text == "" {
		return ""
	}
	return template.HTML(`<a href="` + html.EscapeString(href) + `" style="color:inherit;">` + html.EscapeString(text) + `</a>`)
}

// denmaWebsiteText is a website as e-mails show it: without its scheme or a
// trailing slash.
func denmaWebsiteText(u string) string {
	if _, rest, ok := strings.Cut(u, "://"); ok {
		u = rest
	}
	return strings.TrimSuffix(u, "/")
}

// denmaCheckWebsiteInput tidies a website entered for a center (adding
// https://) and checks it's one.
func denmaCheckWebsiteInput(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("Enter the center's website: every campaign and automation e-mail shows it, as a way to get in touch.")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || len(s) > 200 || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil ||
		!strings.Contains(u.Hostname(), ".") || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("Enter the center's website as example.org or https://example.org/.")
	}
	return u.String(), nil
}

// denmaCheckContactEmailInput tidies a contact e-mail entered for a center and
// checks it's an address.
func denmaCheckContactEmailInput(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("Enter the center's contact e-mail: every campaign and automation e-mail shows it, as a way to get in touch.")
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || len(s) > 200 {
		return "", errors.New("Enter the center's contact e-mail as name@example.org.")
	}
	return s, nil
}

// The footer's parts, as messages name them.
const (
	denmaPartUnsub   = "the unsubscribe link ({{ UnsubscribeURL }})"
	denmaPartView    = "the view-in-browser link ({{ MessageURL }})"
	denmaPartAddress = "the postal address ({{ CenterAddress }})"
	denmaPartWebsite = "the website ({{ CenterWebsite }})"
	denmaPartEmail   = "the contact e-mail ({{ CenterEmail }})"
)

var (
	reDenmaFooterUse  = regexp.MustCompile(`\{\{-?\s*CenterFooter\s*-?\}\}`)
	reDenmaUnsubUse   = regexp.MustCompile(`\{\{-?\s*UnsubscribeURL\b`)
	reDenmaViewUse    = regexp.MustCompile(`\{\{-?\s*MessageURL\b`)
	reDenmaAddressUse = regexp.MustCompile(`\{\{-?\s*CenterAddress\s*-?\}\}`)
	reDenmaWebsiteUse = regexp.MustCompile(`\{\{-?\s*CenterWebsite\s*-?\}\}`)
	reDenmaEmailUse   = regexp.MustCompile(`\{\{-?\s*CenterEmail\s*-?\}\}`)
)

// denmaLinksMissing is the links an e-mail's source (its template's and its
// own HTML) hasn't.
func denmaLinksMissing(src string) []string {
	if reDenmaFooterUse.MatchString(src) {
		return nil
	}
	var miss []string
	if !reDenmaUnsubUse.MatchString(src) {
		miss = append(miss, denmaPartUnsub)
	}
	if !reDenmaViewUse.MatchString(src) {
		miss = append(miss, denmaPartView)
	}
	return miss
}

// denmaDetailsMissing is the center's details that HTML (or plain text)
// doesn't show, by their text.
func denmaDetailsMissing(body string, isHTML bool, c denmaContact) []string {
	words := denmaPlainWords(body, isHTML)
	var miss []string
	if want := denmaPlainWords(c.Address, false); want == "" || !strings.Contains(words, want) {
		miss = append(miss, denmaPartAddress)
	}
	if !denmaHasWord(words, denmaSiteWord(denmaWebsiteText(c.Website))) {
		miss = append(miss, denmaPartWebsite)
	}
	if !denmaHasWord(words, denmaSiteWord(c.Email)) {
		miss = append(miss, denmaPartEmail)
	}
	return miss
}

// denmaSiteWord is a website, or a word naming it, to compare.
func denmaSiteWord(s string) string {
	s = strings.ToLower(s)
	for _, p := range []string{"https://", "http://", "mailto:", "www."} {
		s = strings.TrimPrefix(s, p)
	}
	return strings.TrimRight(s, "/.:!?)")
}

// denmaHasWord reports whether words (denmaPlainWords) have want as a word,
// with or without its scheme, www., a trailing slash or punctuation.
func denmaHasWord(words, want string) bool {
	if want == "" {
		return false
	}
	for _, w := range strings.Fields(words) {
		if denmaSiteWord(strings.TrimLeft(w, "(")) == want {
			return true
		}
	}
	return false
}

// denmaFooterMissing is what a campaign or visual template's HTML lacks of
// the footer: the links by their tags, the details by theirs or written in.
func denmaFooterMissing(body string, c denmaContact) []string {
	miss := denmaLinksMissing(body)
	unshown := denmaDetailsMissing(body, true, c)
	for _, p := range []struct {
		name string
		re   *regexp.Regexp
	}{{denmaPartAddress, reDenmaAddressUse}, {denmaPartWebsite, reDenmaWebsiteUse}, {denmaPartEmail, reDenmaEmailUse}} {
		if !p.re.MatchString(body) && denmaHas(unshown, p.name) {
			miss = append(miss, p.name)
		}
	}
	return miss
}

func denmaHas(list []string, s string) bool {
	for _, l := range list {
		if l == s {
			return true
		}
	}
	return false
}

// denmaFooterError says what's missing (what: "The template", "The e-mail")
// and how to fix it.
func denmaFooterError(what string, miss []string) error {
	where := "its template's footer (Campaigns → Templates), or a visual campaign's own,"
	if what == "The template" {
		where = "its footer"
	}
	return fmt.Errorf("%s doesn't have %s, which every campaign and automation e-mail must have. Put their tags in %s; %s gives both links.",
		what, denmaAnd(miss), where, denmaFooterTag)
}

// denmaAnd joins phrases: "a", "a and b", "a, b and c".
func denmaAnd(s []string) string {
	if len(s) < 2 {
		return strings.Join(s, "")
	}
	return strings.Join(s[:len(s)-1], ", ") + " and " + s[len(s)-1]
}

// denmaFooterCheck refuses an e-mail of a center (slug; none for the hub),
// as rendered, that doesn't show its details, or if it hasn't them all.
func denmaFooterCheck(slug string, body []byte) error {
	if slug == "" {
		return nil
	}
	c := denmaContactOf(slug)
	if !c.complete() {
		return errDenmaNoContact
	}
	if miss := denmaDetailsMissing(string(body), true, c); len(miss) > 0 {
		return denmaFooterError("The e-mail", miss)
	}
	return nil
}

// denmaCheckFooter refuses an e-mail (its body as sent) of a's center that
// doesn't show the center's details.
func (a *App) denmaCheckFooter(body []byte) error {
	if !a.ko.Bool("denma.multi_center") {
		return nil
	}
	return denmaFooterCheck(a.ko.String("denma.center"), body)
}

// denmaCheckLinks refuses a campaign or automation e-mail of a's center whose
// source (its template's and its own HTML) hasn't the footer's links.
func (a *App) denmaCheckLinks(src string) error {
	if !a.ko.Bool("denma.multi_center") || a.ko.String("denma.center") == "" {
		return nil
	}
	if miss := denmaLinksMissing(src); len(miss) > 0 {
		return denmaFooterError("The e-mail", miss)
	}
	return nil
}

// denmaCheckSample refuses a campaign's test e-mail (msg, as rendered) that
// hasn't the footer or fails denmaCheckMessage.
func (a *App) denmaCheckSample(camp *models.Campaign, msg []byte) error {
	if err := a.denmaCheckLinks(denmaCampaignSource(camp)); err != nil {
		return denmaBadRequest(err)
	}
	return a.denmaCheckMessage(msg)
}

// denmaCampaignSource is a campaign's HTML as its e-mail is made from it:
// its template's and its own (a visual campaign's own only).
func denmaCampaignSource(c *models.Campaign) string {
	if c.ContentType == models.CampaignContentTypeVisual {
		return c.Body
	}
	return c.TemplateBody + c.Body
}

// denmaCheckTemplateFooter refuses a campaign or visual template of a center
// without the footer, and {{ CenterFooter }} in any other.
func (a *App) denmaCheckTemplateFooter(o models.Template) error {
	if !a.denmaInCenter() {
		return nil
	}
	if o.Type != models.TemplateTypeCampaign && o.Type != models.TemplateTypeCampaignVisual {
		if reDenmaFooterUse.MatchString(o.Body) {
			return denmaBadRequest(errDenmaFooterOnlyCampaigns)
		}
		return nil
	}
	if miss := denmaFooterMissing(o.Body, denmaContactOf(a.ko.String("denma.center"))); len(miss) > 0 {
		return denmaBadRequest(denmaFooterError("The template", miss))
	}
	return nil
}

// denmaFooterTemplate is a template that lacks some of the footer, for the
// Config page.
type denmaFooterTemplate struct {
	Name    string
	Missing string
}

// denmaFooterTemplates is the center's campaign and visual templates that
// lack some of the footer.
func (a *App) denmaFooterTemplates() []denmaFooterTemplate {
	var tpls []struct {
		Name string `db:"name"`
		Body string `db:"body"`
	}
	if err := a.db.Select(&tpls, `SELECT name, body FROM templates WHERE type IN ('campaign', 'campaign_visual') ORDER BY name`); err != nil {
		a.log.Printf("denma: error reading the templates: %v", err)
	}
	c := denmaContactOf(a.ko.String("denma.center"))
	out := []denmaFooterTemplate{}
	for _, t := range tpls {
		if miss := denmaFooterMissing(t.Body, c); len(miss) > 0 {
			out = append(out, denmaFooterTemplate{Name: t.Name, Missing: denmaAnd(miss)})
		}
	}
	return out
}

// The stock footer's website and contact e-mail, after its address (put there
// by features version 39).
const (
	denmaContactAnchor = denmaAddressAnchor + "<br>" + denmaAddressTag
	denmaContactLine   = "<br>{{ CenterWebsite }} · {{ CenterEmail }}"
)

// denmaWithContact is a template's HTML with the website and contact e-mail
// after its stock footer's address, and whether they were put there.
func denmaWithContact(body string) (string, bool) {
	if strings.Contains(body, "CenterWebsite") || strings.Contains(body, "CenterFooter") || !strings.Contains(body, denmaContactAnchor) {
		return body, false
	}
	return strings.Replace(body, denmaContactAnchor, denmaContactAnchor+denmaContactLine, 1), true
}

// denmaAddContactToTemplates adds the website and contact e-mail to the
// footer of the center's campaign and visual templates that have the stock
// one, and to its default design, if it's the stock layout. Returns how many
// it changed.
func denmaAddContactToTemplates(tx *sqlx.Tx) (int, error) {
	n, err := denmaEditStockFooters(tx, denmaContactAnchor, denmaWithContact)
	if err != nil {
		return n, err
	}
	ok, err := denmaAddContactToDesign(tx)
	if ok {
		n++
	}
	return n, err
}

// The default design's website and contact e-mail, a block below its address
// (denmaDesignLayout), which then has less space below it.
const (
	denmaDesignContactBlock = "block-denma-design-contact"
	denmaDesignContactText  = "{{ CenterWebsite }} · {{ CenterEmail }}"
	denmaDesignFooterHTML   = `<div style="color:#888888;font-size:12px;text-align:center;padding:0px 40px 4px 40px">` + denmaAddressTag + `</div>` +
		`<div style="color:#888888;font-size:12px;text-align:center;padding:0px 40px 30px 40px">` + denmaDesignContactText + `</div>`
)

// denmaDesignFooter is the design's address and contact blocks, for its
// source.
func denmaDesignFooter(pad func(t, r, b, l int) map[string]int) map[string]map[string]any {
	text := func(s string, bottom int) map[string]any {
		return map[string]any{"type": "Text", "data": map[string]any{
			"style": map[string]any{"color": "#888888", "fontSize": 12, "textAlign": "center", "padding": pad(0, 40, bottom, 40)},
			"props": map[string]any{"text": s},
		}}
	}
	return map[string]map[string]any{denmaDesignAddrBlock: text(denmaAddressTag, 4), denmaDesignContactBlock: text(denmaDesignContactText, 30)}
}

// denmaAddContactToDesign adds the website and contact e-mail below the
// center's default design's address, if it's still the stock layout (features
// version 39's) and hasn't them.
func denmaAddContactToDesign(tx *sqlx.Tx) (bool, error) {
	var t struct {
		ID     int    `db:"id"`
		Body   string `db:"body"`
		Source string `db:"body_source"`
	}
	err := tx.Get(&t, `SELECT t.id, t.body, COALESCE(t.body_source, '') AS body_source FROM settings s
		JOIN templates t ON t.id = (s.value #>> '{}')::INT WHERE s.key = 'denma.default_design'`)
	if err != nil || strings.Contains(t.Body, "CenterWebsite") || !strings.HasSuffix(t.Body, denmaDesignAddrHTML+denmaDesignEnd) {
		return false, nil
	}
	var src map[string]map[string]any
	if err := json.Unmarshal([]byte(t.Source), &src); err != nil {
		return false, nil
	}
	root, _ := src["root"]["data"].(map[string]any)
	ids, _ := root["childrenIds"].([]any)
	if root == nil || len(ids) == 0 || ids[len(ids)-1] != denmaDesignAddrBlock {
		return false, nil
	}
	root["childrenIds"] = append(ids, denmaDesignContactBlock)
	for k, v := range denmaDesignFooter(func(t, r, b, l int) map[string]int {
		return map[string]int{"top": t, "right": r, "bottom": b, "left": l}
	}) {
		src[k] = v
	}
	b, err := json.Marshal(src)
	if err != nil {
		return false, err
	}
	body := strings.TrimSuffix(t.Body, denmaDesignAddrHTML+denmaDesignEnd) + denmaDesignFooterHTML + denmaDesignEnd
	_, err = tx.Exec(`UPDATE templates SET body = $2, body_source = $3, updated_at = NOW() WHERE id = $1`, t.ID, body, string(b))
	return err == nil, err
}
