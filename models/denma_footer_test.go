package models

import (
	"html/template"
	"strings"
	"testing"
)

type denmaTestL struct{}

func (denmaTestL) T(s string) string { return s }

// Every campaign gets the tracking pixel once, before its </body>, unless its
// template or body has it; {{ CenterFooter }} is written out in either.
func TestDenmaCompileFooter(t *testing.T) {
	f := template.FuncMap{
		"TrackLink":      func(u string, _ any) string { return u },
		"TrackView":      func(_ any) template.HTML { return "<img>" },
		"UnsubscribeURL": func(_ any) string { return "https://u.org" },
		"MessageURL":     func(_ any) string { return "https://m.org" },
		"L":              func() denmaTestL { return denmaTestL{} },
		"CenterAddress":  func() template.HTML { return "1 Main St<br>Halifax" },
		"CenterWebsite":  func() template.HTML { return "site" },
		"CenterEmail":    func() template.HTML { return "mail" },
	}
	for _, c := range []struct {
		name, tpl, body, ctype string
		pixels                 int
		before                 string // the pixel's place
	}{
		{"before </body>", `<html><body>{{ template "content" . }}</BODY></html>`, "<p>Hi</p>", CampaignContentTypeRichtext, 1, "</BODY>"},
		{"at the end", `{{ template "content" . }}`, "<p>Hi</p>", CampaignContentTypeRichtext, 1, ""},
		{"the template's own", `{{ template "content" . }}{{ TrackView }}</body>`, "<p>Hi</p>", CampaignContentTypeRichtext, 1, "</body>"},
		{"the body's own", `{{ template "content" . }}`, "<p>Hi</p>{{TrackView}}", CampaignContentTypeRichtext, 1, ""},
		{"a visual campaign", `<p>unused</p>`, "<body><p>Hi</p>{{ CenterFooter }}</body>", CampaignContentTypeVisual, 1, ""},
	} {
		camp := Campaign{TemplateBody: c.tpl, Body: c.body, ContentType: c.ctype}
		if err := camp.CompileTemplate(f); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var b strings.Builder
		if err := camp.Tpl.ExecuteTemplate(&b, BaseTpl, nil); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		out := b.String()
		if n := strings.Count(out, "<img>"); n != c.pixels || (c.before != "" && !strings.Contains(out, "<img>"+c.before)) {
			t.Errorf("%s: %d pixels in %s", c.name, n, out)
		}
		if strings.Contains(c.body, "CenterFooter") && (!strings.Contains(out, `href="https://u.org"`) || !strings.Contains(out, `href="https://m.org"`)) {
			t.Errorf("%s: the footer isn't written out: %s", c.name, out)
		}
	}
}
