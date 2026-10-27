package models

import "testing"

func TestDenmaAltText(t *testing.T) {
	for in, want := range map[string]string{
		"Write to {{ CenterAddress }}.":         "Write to {{ CenterAddressText }}.",
		"{{- CenterAddress -}}":                 "{{- CenterAddressText -}}",
		"{{CenterAddress}} {{ CenterAddress }}": "{{CenterAddressText}} {{ CenterAddressText }}",
		"{{ CenterAddressText }}":               "{{ CenterAddressText }}",
		"no tag":                                "no tag",
	} {
		if got := DenmaAltText(in); got != want {
			t.Errorf("DenmaAltText(%q) = %q, want %q", in, got, want)
		}
	}
}
