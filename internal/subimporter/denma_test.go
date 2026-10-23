package subimporter

import "testing"

func TestDenmaUTF8(t *testing.T) {
	for _, c := range []struct {
		name, in, want, enc string
	}{
		{"UTF-8", "email,name\na@x.org,Hélène\n", "email,name\na@x.org,Hélène\n", ""},
		{"UTF-8 with a BOM", "\xef\xbb\xbfemail\na@x.org\n", "\xef\xbb\xbfemail\na@x.org\n", ""},
		{"Windows-1252", "email,name\na@x.org,H\xe9l\xe8ne \x96 \x80\n", "email,name\na@x.org,Hélène – €\n", "Windows-1252"},
		{"UTF-16 LE", "\xff\xfee\x00,\x00\xe9\x00\n\x00", "e,é\n", "UTF-16"},
		{"UTF-16 BE", "\xfe\xff\x00e\x00,\x00\xe9\x00\n", "e,é\n", "UTF-16"},
		{"NULs", "email\na@x.org\x00\n", "email\na@x.org\n", ""},
		{"empty", "", "", ""},
	} {
		out, enc := DenmaUTF8([]byte(c.in))
		if string(out) != c.want || enc != c.enc {
			t.Errorf("%s: got %q (%q), want %q (%q)", c.name, out, enc, c.want, c.enc)
		}
	}
}
