package manager

import (
	"net/textproto"

	"github.com/knadh/listmonk/models"
)

// denma: a campaign's reply-to, else its Reply-To header, else the center's default (cmd/denma_reply_to.go).

// DenmaSetReplyTo sets the center's default reply-to address ("" for none); call it before Run.
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
