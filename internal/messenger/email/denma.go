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
// headers with any of DenmaHeaderProblem's problems out of every e-mail,
// whatever set them: a center's campaign or transactional message could
// otherwise send as another center's domain, past the sender checks
// (cmd/denma_domains.go), or to recipients nobody checked. The SMTP servers'
// own headers (the hub's settings) are kept.
var DenmaDropSenderHeaders bool

// DenmaHeaderProblem is why a message's own header can't be sent, or "":
// "name" (not a header name), "control" (a control character: CR or LF would
// start a header of its own), "sender" (DenmaSenderHeader), "recipients"
// (Bcc, Cc and To: recipients the limits, blocklist and consent rules never
// see) or "internal" (X-Denma-*, the process's own).
func DenmaHeaderProblem(name, value string) string {
	if name == "" {
		return "name"
	}
	for _, c := range name {
		if c <= ' ' || c > '~' || c == ':' {
			return "name"
		}
	}
	for _, c := range value {
		if (c < ' ' && c != '\t') || c == 0x7f {
			return "control"
		}
	}
	switch k := textproto.CanonicalMIMEHeaderKey(name); {
	case DenmaSenderHeader(k):
		return "sender"
	case k == hdrBcc || k == hdrCc || k == "To":
		return "recipients"
	case strings.HasPrefix(k, "X-Denma-"):
		return "internal"
	}
	return ""
}

// denmaDropHeader reports whether DenmaDropSenderHeaders leaves a header out:
// one with any of DenmaHeaderProblem's problems.
func denmaDropHeader(name string, vals []string) bool {
	for _, v := range vals {
		if DenmaHeaderProblem(name, v) != "" {
			return true
		}
	}
	return len(vals) == 0 && DenmaHeaderProblem(name, "") != ""
}

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
