package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/knadh/listmonk/internal/media"
)

var reDenmaHashName = regexp.MustCompile(`^[0-9a-f]{16}(\.[a-z0-9]+)?$`)

// A file's stored name is a hash of its contents, with its extension lowercased (jpeg as jpg).
func TestDenmaHashName(t *testing.T) {
	a, _ := denmaHashName(strings.NewReader("one"), ".PNG")
	b, _ := denmaHashName(strings.NewReader("one"), "png")
	c, _ := denmaHashName(strings.NewReader("two"), ".png")
	d, _ := denmaHashName(strings.NewReader("one"), ".jpeg")
	e, _ := denmaHashName(strings.NewReader("one"), "")
	if a != b || a == c || !reDenmaHashName.MatchString(a) || !strings.HasSuffix(a, ".png") {
		t.Errorf("names: %q %q %q", a, b, c)
	}
	if !strings.HasSuffix(d, ".jpg") || strings.Contains(e, ".") || !reDenmaHashName.MatchString(e) {
		t.Errorf("jpeg %q, none %q", d, e)
	}
	for _, ext := range []string{".h<b>", ".a b", ".x;y", `.q"`, ".toolongext"} {
		if n, _ := denmaHashName(strings.NewReader("one"), ext); n != strings.TrimSuffix(a, ".png") {
			t.Errorf("%s: %q, want no extension", ext, n)
		}
	}
}

func TestDenmaMediaLabel(t *testing.T) {
	for in, want := range map[string]string{
		"  Spring  poster.jpg ": "Spring poster.jpg",
		"a\nb\tc":               "a b c",
		"":                      "",
		strings.Repeat("x", denmaMediaLabelMax+1): "",
	} {
		if got := denmaMediaLabel(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// An attachment is named by its label, keeping the stored file's extension.
func TestDenmaAttachmentName(t *testing.T) {
	for _, c := range []struct{ label, file, want string }{
		{"Programme.pdf", "0123456789abcdef.pdf", "Programme.pdf"},
		{"Programme", "0123456789abcdef.pdf", "Programme.pdf"},
		{"poster.PNG", "0123456789abcdef.png", "poster.PNG"},
		{"", "0123456789abcdef.pdf", "0123456789abcdef.pdf"},
	} {
		if got := denmaAttachmentName(media.Media{Label: c.label, Filename: c.file}); got != c.want {
			t.Errorf("%q: %q, want %q", c.label, got, c.want)
		}
	}
}
