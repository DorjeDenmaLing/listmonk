package main

import (
	"strings"
	"testing"
)

// An address is entered tidied and checked, and shown a line for each.
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
	if got := denmaAddressHTML("1 Main St & Co\nHalifax NS"); got != "1 Main St &amp; Co<br>Halifax NS" {
		t.Errorf("denmaAddressHTML = %q", got)
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
	out, err := denmaEditSource(string(src), denmaWithAddress)
	if err != nil || strings.Count(out, "CenterAddress") != 1 {
		t.Errorf("the visual source: %v, %d tags", err, strings.Count(out, "CenterAddress"))
	}

	// The design's layout has it, as its own block.
	body, source := denmaDefaultDesign("https://example.org/logo.png", "Halifax")
	if !strings.Contains(body, denmaAddressTag) || !strings.Contains(source, denmaDesignAddrBlock) {
		t.Error("the default design doesn't show the address")
	}
}
