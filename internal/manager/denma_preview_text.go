package manager

import (
	"html"
	"regexp"
	"strings"

	"github.com/knadh/listmonk/models"
)

// denma: a campaign's preview text, added hidden after <body> in HTML messages.

var denmaBodyTag = regexp.MustCompile(`(?i)<body\b[^>]*>`)

// denmaPreviewFill follows the text to blank the rest of the inbox preview.
var denmaPreviewFill = strings.Repeat("&#847;&zwnj;&nbsp;", 90)

// denmaAddPreviewText puts the hidden preview text after <body> (first without one).
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
