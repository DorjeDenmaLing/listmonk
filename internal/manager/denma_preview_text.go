package manager

import (
	"html"
	"regexp"
	"strings"

	"github.com/knadh/listmonk/models"
)

// denma: a campaign's preview text, the sentence inbox apps show after the
// subject (campaigns.denma_preview_text, set from the campaign form,
// static/admin/partials/campaign-form.html, and cmd/denma_tags.go). It's
// added to every HTML message, hidden, straight after <body>, so it works
// with any template or format without editing them.

var denmaBodyTag = regexp.MustCompile(`(?i)<body\b[^>]*>`)

// denmaPreviewFill follows the text so that inbox apps, which fill the rest
// of the preview with the email's first words, show blank space instead.
var denmaPreviewFill = strings.Repeat("&#847;&zwnj;&nbsp;", 90)

// denmaAddPreviewText returns the rendered HTML body with the campaign's
// preview text in it, hidden; unchanged for plain-text campaigns or without
// preview text. With no <body> tag, it goes first.
func denmaAddPreviewText(c *models.Campaign, body []byte) []byte {
	text := strings.TrimSpace(c.PreviewText)
	if text == "" || c.ContentType == models.CampaignContentTypePlain {
		return body
	}

	block := `<div style="display:none;font-size:1px;line-height:1px;max-height:0;max-width:0;opacity:0;overflow:hidden;mso-hide:all;">` +
		html.EscapeString(text) + denmaPreviewFill + `</div>`

	at := 0
	if loc := denmaBodyTag.FindIndex(body); loc != nil {
		at = loc[1]
	}

	out := make([]byte, 0, len(body)+len(block))
	out = append(out, body[:at]...)
	out = append(out, block...)
	return append(out, body[at:]...)
}
