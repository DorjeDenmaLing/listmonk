package models

// denma: the footer every campaign and automation e-mail needs, with the tracking pixel (cmd/denma_footer.go).

import (
	"regexp"
	"strings"
)

var (
	reDenmaFooterTag = regexp.MustCompile(`\{\{-?\s*CenterFooter\s*-?\}\}`)
	reDenmaTrackView = regexp.MustCompile(`\{\{-?\s*TrackView\b`)
)

// DenmaFooterHTML is what {{ CenterFooter }} writes in an e-mail: the unsubscribe and "view in browser" links.
const DenmaFooterHTML = `<div class="footer" style="text-align: center;font-size: 12px;color: #888;"><p>` +
	`<a href="{{ UnsubscribeURL }}" style="color: #888;">{{ L.T "email.unsub" }}</a>&nbsp;&nbsp;` +
	`<a href="{{ MessageURL }}" style="color: #888;">{{ L.T "email.viewInBrowser" }}</a></p></div>`

// DenmaFooterText is what it writes in plain text.
const DenmaFooterText = `{{ L.T "email.unsub" }}: {{ UnsubscribeURL }}
{{ L.T "email.viewInBrowser" }}: {{ MessageURL }}`

// DenmaFooter writes out {{ CenterFooter }} in an e-mail's HTML.
func DenmaFooter(s string) string {
	return reDenmaFooterTag.ReplaceAllLiteralString(s, DenmaFooterHTML)
}

// DenmaTemplateHTML is a campaign's template to compile: footer written out, pixel added, links tracked.
func (c *Campaign) DenmaTemplateHTML(body string) string {
	return DenmaTrackHTML(c.denmaWithTrackView(DenmaFooter(body)))
}

// denmaContent is a campaign's body to compile: footer written out, links tracked.
func (c *Campaign) denmaContent(body string) string {
	if c.ContentType == CampaignContentTypePlain {
		return c.denmaTrackBody(DenmaAddressText(body))
	}
	return c.denmaTrackBody(DenmaFooter(body))
}

// denmaWithTrackView is a campaign's template with the tracking pixel, unless it has it.
func (c *Campaign) denmaWithTrackView(body string) string {
	if reDenmaTrackView.MatchString(c.Body) {
		return body
	}
	return DenmaWithTrackView(body)
}

// DenmaWithTrackView is an e-mail's HTML with the tracking pixel before </body>, unless it has it.
func DenmaWithTrackView(body string) string {
	if reDenmaTrackView.MatchString(body) {
		return body
	}
	const px = `{{ TrackView }}`
	if i := strings.LastIndex(strings.ToLower(body), "</body>"); i >= 0 {
		return body[:i] + px + body[i:]
	}
	return body + px
}
