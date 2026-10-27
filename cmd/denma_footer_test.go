package main

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/internal/i18n"
	"github.com/knadh/listmonk/models"
)

var denmaTestContact = denmaContact{
	Address: "1084 Tatamagouche Mountain Road\nTatamagouche NS B0K 1V0",
	Website: "https://dorjedenmaling.org/",
	Email:   "programs@dorjedenmaling.org",
}

// A website and contact e-mail are entered tidied and checked.
func TestDenmaFooterInputs(t *testing.T) {
	for in, want := range map[string]string{
		" dorjedenmaling.org ":             "https://dorjedenmaling.org",
		"https://dorjedenmaling.org/":      "https://dorjedenmaling.org/",
		"http://shambhala.org/halifax":     "http://shambhala.org/halifax",
		"":                                 "",
		"localhost":                        "", // no dot
		"ftp://example.org":                "",
		"https://example.org/?utm=1":       "",
		"https://user:pw@example.org/":     "",
		"javascript:alert(1)//example.org": "",
	} {
		got, err := denmaCheckWebsiteInput(in)
		if (err != nil) != (want == "") || got != want {
			t.Errorf("denmaCheckWebsiteInput(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for in, want := range map[string]string{
		" programs@dorjedenmaling.org ":       "programs@dorjedenmaling.org",
		"":                                    "",
		"programs":                            "",
		`"DDL" <programs@dorjedenmaling.org>`: "", // the address alone
	} {
		got, err := denmaCheckContactEmailInput(in)
		if (err != nil) != (want == "") || got != want {
			t.Errorf("denmaCheckContactEmailInput(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if got := denmaWebsiteText("https://dorjedenmaling.org/"); got != "dorjedenmaling.org" {
		t.Errorf("denmaWebsiteText = %q", got)
	}
}

// An e-mail shows the center's details with their tags, or written in (the
// address's lines apart, entities; the website with or without its scheme,
// www. or a slash), but not with part of them: the e-mail address's domain
// isn't the website.
func TestDenmaFooterShown(t *testing.T) {
	c := denmaTestContact
	const all = `<p>1084 Tatamagouche Mountain Road<br>Tatamagouche NS B0K 1V0</p><p><a href="https://dorjedenmaling.org/">dorjedenmaling.org</a> · programs@dorjedenmaling.org</p>`
	for body, want := range map[string]string{
		all: "",
		`<div>1084 Tatamagouche  Mountain Road<br/>Tatamagouche NS&nbsp;B0K 1V0</div><p>Visit https://www.dorjedenmaling.org. Write to PROGRAMS@dorjedenmaling.org!</p>`: "",
		`<p>1084 Tatamagouche Mountain Road</p><p>dorjedenmaling.org programs@dorjedenmaling.org</p>`:                                                                    denmaPartAddress,
		`<!-- 1084 Tatamagouche Mountain Road, Tatamagouche NS B0K 1V0 --><p>dorjedenmaling.org programs@dorjedenmaling.org</p>`:                                         denmaPartAddress,
		`<p>1084 Tatamagouche Mountain Road, Tatamagouche NS B0K 1V0</p><p>programs@dorjedenmaling.org</p>`:                                                              denmaPartWebsite,
		`<p>1084 Tatamagouche Mountain Road, Tatamagouche NS B0K 1V0</p><p>news.dorjedenmaling.org</p>`:                                                                  denmaPartWebsite + "|" + denmaPartEmail,
	} {
		if got := strings.Join(denmaDetailsMissing(body, true, c), "|"); got != want {
			t.Errorf("denmaDetailsMissing(%q) = %q, want %q", body, got, want)
		}
	}
	if got := denmaDetailsMissing("anything", true, denmaContact{}); len(got) != 3 {
		t.Errorf("no details are shown: %q", got)
	}

	// The tags, rendered, show them.
	funcs := template.FuncMap{}
	ko := koanf.New(".")
	_ = ko.Load(confmap.Provider(map[string]any{"denma.center": "test-footer", "denma.postal_address": c.Address,
		"denma.website": c.Website, "denma.contact_email": c.Email}, "."), nil)
	denmaFooterFuncs(funcs, ko)
	tpl := template.Must(template.New("t").Funcs(funcs).Parse(`<footer>{{ CenterAddress }} {{ CenterWebsite }} {{ CenterEmail }}</footer>`))
	var b bytes.Buffer
	if err := tpl.Execute(&b, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `<a href="mailto:programs@dorjedenmaling.org" style="color:inherit;">programs@dorjedenmaling.org</a>`) {
		t.Errorf("the contact e-mail: %s", b.String())
	}
	if err := denmaFooterCheck("test-footer", b.Bytes()); err != nil {
		t.Errorf("the tags rendered (%s): %v", b.String(), err)
	}
	if err := denmaFooterCheck("test-footer", []byte("<p>Nothing</p>")); err == nil || !strings.Contains(err.Error(), denmaFooterTag) {
		t.Errorf("without them: %v", err)
	}
	denmaSetContact("test-footer", denmaContact{Address: c.Address, Website: c.Website})
	if err := denmaFooterCheck("test-footer", b.Bytes()); err != errDenmaNoContact {
		t.Errorf("no contact e-mail: %v", err)
	}
	if err := denmaFooterCheck("", nil); err != nil {
		t.Errorf("the hub: %v", err)
	}
}

// A campaign template has the footer: the links by their tags (or
// {{ CenterFooter }}), the details by theirs or written in.
func TestDenmaFooterMissing(t *testing.T) {
	c := denmaTestContact
	links := `<a href="{{ UnsubscribeURL }}">Unsubscribe</a> <a href="{{ MessageURL }}">View</a>`
	details := "{{ CenterAddress }} {{ CenterWebsite }} · {{ CenterEmail }}"
	for body, want := range map[string]string{
		"<p>Hi</p>{{ CenterFooter }}" + details: "",
		"<p>Hi</p>{{- CenterFooter -}}":         denmaPartAddress + "|" + denmaPartWebsite + "|" + denmaPartEmail,
		links + details:                         "",
		links + "{{ CenterAddress }} dorjedenmaling.org · programs@dorjedenmaling.org": "",
		links + "{{ CenterAddress }}":                      denmaPartWebsite + "|" + denmaPartEmail,
		`<a href="{{ MessageURL }}">View</a>` + details:    denmaPartUnsub,
		`<a href="{{ UnsubscribeURL . }}">U</a>` + details: denmaPartView,
		"<p>Hi</p>": strings.Join([]string{denmaPartUnsub, denmaPartView, denmaPartAddress, denmaPartWebsite, denmaPartEmail}, "|"),
	} {
		if got := strings.Join(denmaFooterMissing(body, c), "|"); got != want {
			t.Errorf("denmaFooterMissing(%q) = %q, want %q", body, got, want)
		}
	}
	err := denmaFooterError("The template", []string{denmaPartUnsub, denmaPartWebsite})
	if want := "The template doesn't have the unsubscribe link ({{ UnsubscribeURL }}) and the website ({{ CenterWebsite }}), which every campaign and automation e-mail must have. Put their tags in its footer; {{ CenterFooter }} gives both links."; err.Error() != want {
		t.Errorf("the error: %v", err)
	}
}

// {{ CenterFooter }}, written out, gives both links, in HTML and plain text.
func TestDenmaFooterRendered(t *testing.T) {
	funcs := template.FuncMap{
		"UnsubscribeURL": func(...any) string { return "https://example.org/unsub" },
		"MessageURL":     func(...any) string { return "https://example.org/view" },
		"L":              func() *i18n.I18n { return &i18n.I18n{} },
	}
	for name, src := range map[string]string{"HTML": models.DenmaFooter(denmaFooterTag), "plain text": models.DenmaAddressText(denmaFooterTag)} {
		tpl, err := template.New("t").Funcs(funcs).Parse(src)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var b bytes.Buffer
		if err := tpl.Execute(&b, nil); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out := b.String()
		if !strings.Contains(out, "https://example.org/unsub") || !strings.Contains(out, "https://example.org/view") ||
			(name == "plain text") == strings.Contains(out, "<a ") || len(denmaLinksMissing(src)) != 0 {
			t.Errorf("%s: %s", name, out)
		}
	}
}

// The stock templates (campaign and visual, its source too), with the
// address, get the website and contact e-mail after it, once; the default
// design has them as their own block.
func TestDenmaWithContact(t *testing.T) {
	for _, f := range []string{"/static/email-templates/default.tpl", "/static/email-templates/default-visual.tpl"} {
		b, err := fs.Read(f)
		if err != nil {
			t.Fatal(err)
		}
		withAddr, _ := denmaWithAddress(string(b))
		out, ok := denmaWithContact(withAddr)
		if !ok || strings.Count(out, "{{ CenterWebsite }}") != 1 || len(denmaFooterMissing(out, denmaTestContact)) != 0 {
			t.Errorf("%s: not given the website and contact e-mail once (%v): %q", f, ok, denmaFooterMissing(out, denmaTestContact))
		}
		if again, ok := denmaWithContact(out); ok || again != out {
			t.Errorf("%s: given them twice", f)
		}
	}
	if _, ok := denmaWithContact("<p>No footer</p>{{ CenterAddress }}"); ok {
		t.Error("a template without the stock footer was changed")
	}
	src, err := fs.Read("/static/email-templates/default-visual.json")
	if err != nil {
		t.Fatal(err)
	}
	withAddr, _ := denmaEditSource(string(src), denmaWithAddress)
	out, err := denmaEditSource(withAddr, denmaWithContact)
	if err != nil || strings.Count(out, "CenterWebsite") != 1 {
		t.Errorf("the visual source: %v, %d tags", err, strings.Count(out, "CenterWebsite"))
	}

	body, source := denmaDefaultDesign("https://example.org/logo.png", "Halifax")
	if !strings.Contains(body, denmaDesignFooterHTML) || !strings.Contains(source, denmaDesignContactBlock) {
		t.Error("the default design doesn't show the website and contact e-mail")
	}
}
