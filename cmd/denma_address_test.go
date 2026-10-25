package main

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

// An address is entered tidied and checked, and shown on one line.
func TestDenmaCheckAddressInput(t *testing.T) {
	for in, want := range map[string]string{
		"  1084 Tatamagouche Mountain Road \r\n\n Tatamagouche  NS  B0K 1V0 ": "1084 Tatamagouche Mountain Road\nTatamagouche NS B0K 1V0",
		"PO Box 123, Halifax NS":   "PO Box 123, Halifax NS",
		"":                         "",
		"   \n ":                   "",
		"Halifax, Nova Scotia":     "", // no number
		"1 Rd":                     "", // too short
		"1\n2\n3\n4\n5\n6\n7 Road": "", // over 6 lines
	} {
		got, err := denmaCheckAddressInput(strings.ReplaceAll(in, "\r", ""))
		if (err != nil) != (want == "") || got != want {
			t.Errorf("denmaCheckAddressInput(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if got := denmaAddressLine("1 Main St\nHalifax NS"); got != "1 Main St, Halifax NS" {
		t.Errorf("denmaAddressLine = %q", got)
	}
}

// An e-mail shows the address with the tag, or written in (lines apart,
// entities), but not with part of it.
func TestDenmaShowsAddress(t *testing.T) {
	const addr = "1084 Tatamagouche Mountain Road\nTatamagouche NS B0K 1V0"
	for body, want := range map[string]bool{
		`<p>Unsubscribe<br>1084 Tatamagouche Mountain Road, Tatamagouche NS B0K 1V0</p>`: true,
		`<div>1084 Tatamagouche  Mountain Road<br/>
			Tatamagouche NS&nbsp;B0K 1V0</div>`: true,
		`<p>1084 TATAMAGOUCHE MOUNTAIN ROAD</p><p>Tatamagouche NS B0K 1V0</p>`:     true,
		"Plain text\n\n1084 Tatamagouche Mountain Road, Tatamagouche NS B0K 1V0\n": true,
		`<p>1084 Tatamagouche Mountain Road</p>`:                                   false,
		`<p>Unsubscribe</p>`:                                                       false,
		`<!-- 1084 Tatamagouche Mountain Road, Tatamagouche NS B0K 1V0 --><p></p>`: false,
	} {
		if got := denmaShowsAddress([]byte(body), addr); got != want {
			t.Errorf("denmaShowsAddress(%q) = %v, want %v", body, got, want)
		}
	}
	if denmaShowsAddress([]byte("anything"), "") {
		t.Error("an empty address is shown")
	}

	// The tag, rendered, shows it.
	funcs := template.FuncMap{}
	denmaSetAddress("test-address", addr)
	ko := koanf.New(".")
	_ = ko.Load(confmap.Provider(map[string]any{"denma.center": "test-address", "denma.postal_address": addr}, "."), nil)
	denmaAddressFuncs(funcs, ko)
	tpl := template.Must(template.New("t").Funcs(funcs).Parse(`<footer>` + denmaAddressTag + `</footer>`))
	var b bytes.Buffer
	if err := tpl.Execute(&b, nil); err != nil {
		t.Fatal(err)
	}
	if err := denmaAddressCheck("test-address", b.Bytes()); err != nil {
		t.Errorf("the tag rendered (%s): %v", b.String(), err)
	}
	if err := denmaAddressCheck("test-address", []byte("<p>No address</p>")); err != errDenmaAddressMissing {
		t.Errorf("without it: %v", err)
	}
	denmaSetAddress("test-address", "")
	if err := denmaAddressCheck("test-address", b.Bytes()); err != errDenmaNoAddress {
		t.Errorf("no address: %v", err)
	}
	if err := denmaAddressCheck("", nil); err != nil {
		t.Errorf("the hub: %v", err)
	}
}

// The stock templates (campaign and visual, its source too) get the address
// after their "View in browser" link, once; others are left as they are.
func TestDenmaWithAddress(t *testing.T) {
	for _, f := range []string{"/static/email-templates/default.tpl", "/static/email-templates/default-visual.tpl"} {
		b, err := fs.Read(f)
		if err != nil {
			t.Fatal(err)
		}
		out, ok := denmaWithAddress(string(b))
		if !ok || strings.Count(out, denmaAddressTag) != 1 {
			t.Errorf("%s: not given the address once (%v)", f, ok)
		}
		if again, ok := denmaWithAddress(out); ok || again != out {
			t.Errorf("%s: given it twice", f)
		}
	}
	if _, ok := denmaWithAddress("<p>No footer</p>"); ok {
		t.Error("a template without the link was changed")
	}

	src, err := fs.Read("/static/email-templates/default-visual.json")
	if err != nil {
		t.Fatal(err)
	}
	out, err := denmaSourceWithAddress(string(src))
	if err != nil || strings.Count(out, "CenterAddress") != 1 {
		t.Errorf("the visual source: %v, %d tags", err, strings.Count(out, "CenterAddress"))
	}

	// The design's layout has it, as its own block.
	body, source := denmaDefaultDesign("https://example.org/logo.png", "Halifax")
	if !strings.Contains(body, denmaDesignAddrHTML) || !strings.Contains(source, denmaDesignAddrBlock) {
		t.Error("the default design doesn't show the address")
	}
}
