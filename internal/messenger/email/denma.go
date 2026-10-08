package email

import (
	"net/textproto"
	"strings"

	"github.com/knadh/listmonk/models"
)

// denma: BeforePush, if set, is called before every message any Emailer
// sends, and may block (for a send limit shared by every app in the process,
// cmd/denma_sending.go). Campaign messages, notifications, opt-ins and
// password resets all pass here. It may change the message's headers (the
// map is the message's own), such as removing an internal one.
var BeforePush func(m models.Message)

// DenmaDropSenderHeaders, if set (multi-center), leaves a message's own
// headers that would change who it's from (DenmaSenderHeader) out of every
// e-mail, whatever set them: a center's campaign or transactional message
// could otherwise send as another center's domain, past the sender checks
// (cmd/denma_domains.go). The SMTP servers' own headers (the hub's settings)
// are kept.
var DenmaDropSenderHeaders bool

// DenmaSenderHeader reports whether a header (any case) says who an e-mail is
// from, or sets how Amazon SES sends it: From, Sender, Return-Path (the
// envelope sender), Resent-From, Resent-Sender, and X-SES-*.
func DenmaSenderHeader(name string) bool {
	switch k := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(name)); k {
	case "From", "Sender", hdrReturnPath, "Resent-From", "Resent-Sender":
		return true
	default:
		return strings.HasPrefix(k, "X-Ses-")
	}
}
