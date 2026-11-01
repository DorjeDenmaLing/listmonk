package email

import (
	"net/textproto"
	"strings"

	"github.com/knadh/listmonk/models"
)

// denma: BeforePush, if set, is called before every message any Emailer sends, and may block or change its headers.
var BeforePush func(m models.Message)

// DenmaOnSent has e call f with each message it sends and the mail server's answer.
func (e *Emailer) DenmaOnSent(f func(m models.Message, err error)) {
	e.denmaSent = f
}

// denmaDone tells the DenmaOnSent function of a message sent, returning err.
func (e *Emailer) denmaDone(m models.Message, err error) error {
	if e.denmaSent != nil {
		e.denmaSent(m, err)
	}
	return err
}

// DenmaDropSenderHeaders, if set, leaves a message's own headers with a DenmaHeaderProblem out of every e-mail.
var DenmaDropSenderHeaders bool

// DenmaHeaderProblem is why a message's own header can't be sent (name, control, sender, recipients, internal), or "".
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

// denmaDropHeader reports whether DenmaDropSenderHeaders leaves a header out.
func denmaDropHeader(name string, vals []string) bool {
	for _, v := range vals {
		if DenmaHeaderProblem(name, v) != "" {
			return true
		}
	}
	return len(vals) == 0 && DenmaHeaderProblem(name, "") != ""
}

// DenmaSenderHeader reports whether a header says who an e-mail is from or sets how SES sends it.
func DenmaSenderHeader(name string) bool {
	switch k := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(name)); k {
	case "From", "Sender", hdrReturnPath, "Resent-From", "Resent-Sender":
		return true
	default:
		return strings.HasPrefix(k, "X-Ses-")
	}
}
