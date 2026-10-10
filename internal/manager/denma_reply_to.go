package manager

import (
	"net/textproto"

	"github.com/knadh/listmonk/models"
)

// denma: where replies to a campaign's e-mails go (cmd/denma_reply_to.go):
// the campaign's own reply-to, else a Reply-To it has as a custom header
// (the sending API's, before campaigns had their own), else the center's
// default; with none, there's no Reply-To and replies go to the sender.

// DenmaSetReplyTo sets the center's default reply-to address ("" for none).
// Call it before Run.
func (m *Manager) DenmaSetReplyTo(addr string) {
	m.denmaReplyTo = addr
}

// denmaSetReplyTo sets a campaign message's Reply-To header.
func (m *Manager) denmaSetReplyTo(c *models.Campaign, h textproto.MIMEHeader) {
	switch {
	case c.ReplyTo != "":
		h.Set("Reply-To", c.ReplyTo)
	case h.Get("Reply-To") == "" && m.denmaReplyTo != "":
		h.Set("Reply-To", m.denmaReplyTo)
	}
}
