package models

import "testing"

func TestDenmaAltText(t *testing.T) {
	for in, want := range map[string]string{
		"Write to {{ CenterAddress }}.":           "Write to {{ CenterAddressText }}.",
		"{{- CenterAddress -}}":                   "{{- CenterAddressText -}}",
		"{{CenterAddress}} {{ CenterAddress }}":   "{{CenterAddressText}} {{ CenterAddressText }}",
		"{{ CenterAddressText }}":                 "{{ CenterAddressText }}",
		"{{ CenterWebsite }} · {{ CenterEmail }}": "{{ CenterWebsiteText }} · {{ CenterEmailText }}",
		"no tag": "no tag",
	} {
		if got := DenmaAltText(in); got != want {
			t.Errorf("DenmaAltText(%q) = %q, want %q", in, got, want)
		}
	}
	if got := DenmaAltText("Bye\n{{ CenterFooter }}"); got != "Bye\n"+DenmaFooterText {
		t.Errorf("the footer as text: %q", got)
	}
}
