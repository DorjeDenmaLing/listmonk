package models

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	null "gopkg.in/volatiletech/null.v6"
)

func TestDenmaTrackHTML(t *testing.T) {
	for in, want := range map[string]string{
		`<a href="https://x.org/a?b=1&amp;c=2">x</a>`:          `<a href="{{ TrackLink "https://x.org/a?b=1&c=2" . }}">x</a>`,
		`<A class="b" HREF='http://x.org'>x</A>`:               `<A class="b" HREF='{{ TrackLink "http://x.org" . }}'>x</A>`,
		`<area href="https://x.org/map">`:                      `<area href="{{ TrackLink "https://x.org/map" . }}">`,
		`<a href="{{ UnsubscribeURL }}">u</a>`:                 `<a href="{{ UnsubscribeURL }}">u</a>`,
		`<a href="https://x.org/{{ .Subscriber.UUID }}">p</a>`: `<a href="https://x.org/{{ .Subscriber.UUID }}">p</a>`,
		`<a href="https://x.org@TrackLink">t</a>`:              `<a href="https://x.org@TrackLink">t</a>`,
		`<a href="mailto:a@x.org">m</a>`:                       `<a href="mailto:a@x.org">m</a>`,
		`<a href="#top">t</a>`:                                 `<a href="#top">t</a>`,
		`<link href="https://fonts.x.org/css">`:                `<link href="https://fonts.x.org/css">`,
		`<abbr title="https://x.org">a</abbr>`:                 `<abbr title="https://x.org">a</abbr>`,
		`<p>https://x.org</p>`:                                 `<p>https://x.org</p>`,
	} {
		if got := DenmaTrackHTML(in); got != want {
			t.Errorf("%s:\n got %s\nwant %s", in, got, want)
		}
	}
}

func TestDenmaTrackText(t *testing.T) {
	for in, want := range map[string]string{
		"Read it (https://x.org/a).":            `Read it ({{ TrackLink "https://x.org/a" . }}).`,
		"See https://x.org/wiki/A_(b), then go": `See {{ TrackLink "https://x.org/wiki/A_(b)" . }}, then go`,
		"Unsubscribe: {{ UnsubscribeURL }}":     "Unsubscribe: {{ UnsubscribeURL }}",
		`{{ TrackLink "https://x.org" }}`:       `{{ TrackLink "https://x.org" }}`,
		"https://x.org@TrackLink":               "https://x.org@TrackLink",
		"no links":                              "no links",
	} {
		if got := DenmaTrackText(in); got != want {
			t.Errorf("%q:\n got %s\nwant %s", in, got, want)
		}
	}
}

// A campaign's template, body and plain text all render with tracked links.
func TestDenmaTrackCompile(t *testing.T) {
	c := Campaign{
		TemplateBody: `<p><a href="https://center.org">Site</a></p>{{ template "content" . }}<a href="{{ UnsubscribeURL }}">Unsubscribe</a>`,
		Body:         `<a href="https://x.org/?a=1&amp;b=2">Read</a>`,
		AltBody:      null.StringFrom("Read (https://x.org/?a=1&b=2)"),
		ContentType:  CampaignContentTypeRichtext,
	}
	f := template.FuncMap{
		"TrackLink":      func(u string, _ any) string { return "https://t.org/" + strings.ReplaceAll(u, "https://", "") },
		"UnsubscribeURL": func(_ any) string { return "https://u.org" },
	}
	if err := c.CompileTemplate(f); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := c.Tpl.ExecuteTemplate(&b, BaseTpl, nil); err != nil {
		t.Fatal(err)
	}
	want := `<p><a href="https://t.org/center.org">Site</a></p><a href="https://t.org/x.org/?a=1&amp;b=2">Read</a><a href="https://u.org">Unsubscribe</a>`
	if b.String() != want {
		t.Errorf("html:\n got %s\nwant %s", b.String(), want)
	}
	b.Reset()
	if c.AltBodyTpl == nil {
		t.Fatal("the plain text wasn't compiled")
	}
	if err := c.AltBodyTpl.ExecuteTemplate(&b, ContentTpl, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "https://t.org/x.org/") {
		t.Errorf("plain text: %s", b.String())
	}
}
