// package notifs is a special singleton, stateful globally accessible package
// that handles sending out arbitrary notifications to the admin and users.
// It's initialized once in the main package and is accessed globally across
// other packages.
package notifs

import (
	"bytes"
	"html/template"
	"log"
	"net/textproto"
	"regexp"
	"strings"

	"github.com/knadh/listmonk/internal/messenger/email"
	"github.com/knadh/listmonk/models"
)

const (
	TplImport          = "import-status"
	TplCampaignStatus  = "campaign-status"
	TplSubscriberOptin = "subscriber-optin"
	TplSubscriberData  = "subscriber-data"
	TplForgotPassword  = "forgot-password"
)

type FuncPush func(msg models.Message) error
type FuncNotif func(toEmails []string, subject, tplName string, data any, headers textproto.MIMEHeader) error
type FuncNotifSystem func(subject, tplName string, data any, headers textproto.MIMEHeader) error

type Opt struct {
	FromEmail    string
	SystemEmails []string
	ContentType  string
}

type Notifs struct {
	Tpls *template.Template // denma: per instance (one per center)

	em *email.Emailer
	lo *log.Logger

	opt Opt
}

var (
	reTitle = regexp.MustCompile(`(?s)<title\s*data-i18n\s*>(.+?)</title>`)

	Tpls *template.Template
	no   *Notifs
)

// Initialize returns a new Notifs instance.
func Initialize(opt Opt, tpls *template.Template, em *email.Emailer, lo *log.Logger) {
	if no != nil {
		lo.Fatal("notifs already initialized")
	}

	SetDefault(New(opt, tpls, em, lo)) // denma
}

// New returns a Notifs instance. denma: one per center; the package-level
// functions use the default instance (SetDefault).
func New(opt Opt, tpls *template.Template, em *email.Emailer, lo *log.Logger) *Notifs {
	return &Notifs{Tpls: tpls, opt: opt, em: em, lo: lo}
}

// SetDefault sets the instance the package-level functions use. denma
func SetDefault(n *Notifs) {
	Tpls = n.Tpls
	no = n
}

// NotifySystem sends out an e-mail notification to the admin emails.
func NotifySystem(subject, tplName string, data any, hdr textproto.MIMEHeader) error {
	return no.NotifySystem(subject, tplName, data, hdr) // denma: see the method
}

// Notify sends out an e-mail notification.
func Notify(toEmails []string, subject, tplName string, data any, hdr textproto.MIMEHeader) error {
	return no.Notify(toEmails, subject, tplName, data, hdr) // denma: see the method
}

// NotifySystem sends out an e-mail notification to the admin emails.
func (no *Notifs) NotifySystem(subject, tplName string, data any, hdr textproto.MIMEHeader) error {
	return no.Notify(no.opt.SystemEmails, subject, tplName, data, hdr)
}

// Notify sends out an e-mail notification.
func (no *Notifs) Notify(toEmails []string, subject, tplName string, data any, hdr textproto.MIMEHeader) error {
	if len(toEmails) == 0 {
		return nil
	}

	var buf bytes.Buffer
	if err := no.Tpls.ExecuteTemplate(&buf, tplName, data); err != nil {
		no.lo.Printf("error compiling notification template '%s': %v", tplName, err)
		return err
	}
	body := buf.Bytes()

	subject, body = GetTplSubject(subject, body)

	m := models.Message{
		Messenger:   "email",
		ContentType: no.opt.ContentType,
		From:        no.opt.FromEmail,
		To:          toEmails,
		Subject:     subject,
		Body:        body,
		Headers:     hdr,
	}

	// Send the message.
	if err := no.em.Push(m); err != nil {
		no.lo.Printf("error sending admin notification (%s): %v", subject, err)
		return err
	}

	return nil
}

// GetTplSubject extracts any custom i18n subject rendered in the given rendered
// template body. If it's not found, the incoming subject and body are returned.
func GetTplSubject(subject string, body []byte) (string, []byte) {
	m := reTitle.FindSubmatch(body)
	if len(m) != 2 {
		return subject, body
	}

	return strings.TrimSpace(string(m[1])), reTitle.ReplaceAll(body, []byte(""))
}
