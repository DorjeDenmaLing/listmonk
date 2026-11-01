package main

import (
	"strings"
	"testing"

	"github.com/knadh/listmonk/internal/media"
)

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

// Links to the center's uploads move to the new names; other sites' files and other names don't.
func TestDenmaMediaLinks(t *testing.T) {
	base := "https://news.example.org/c/ddl/uploads/"
	relink := denmaMediaLinks(base, map[string]string{"logo.png": "aaaaaaaaaaaaaaaa.png", "thumb_logo.png": "thumb_aaaaaaaaaaaaaaaa.png", "my photo.jpg": "bbbbbbbbbbbbbbbb.jpg"})
	for in, want := range map[string]string{
		`<img src="https://news.example.org/c/ddl/uploads/logo.png">`:                 `<img src="https://news.example.org/c/ddl/uploads/aaaaaaaaaaaaaaaa.png">`,
		`<img src='/c/ddl/uploads/logo.png?v=2'>`:                                     `<img src='/c/ddl/uploads/aaaaaaaaaaaaaaaa.png?v=2'>`,
		`{"url":"https://news.example.org/c/ddl/uploads/thumb_logo.png"}`:             `{"url":"https://news.example.org/c/ddl/uploads/thumb_aaaaaaaaaaaaaaaa.png"}`,
		`![a](https://news.example.org/c/ddl/uploads/my%20photo.jpg)`:                 `![a](https://news.example.org/c/ddl/uploads/bbbbbbbbbbbbbbbb.jpg)`,
		`"https://news.example.org/c/ddl/uploads/logo.png"`:                           `"https://news.example.org/c/ddl/uploads/aaaaaaaaaaaaaaaa.png"`,
		`<img src="https://other.org/c/ddl/uploads/logo.png">`:                        `<img src="https://other.org/c/ddl/uploads/logo.png">`,
		`<img src="https://news.example.org/c/hfx/uploads/logo.png">`:                 `<img src="https://news.example.org/c/hfx/uploads/logo.png">`,
		`<img src="https://news.example.org/c/ddl/uploads/logo.png.bak">`:             `<img src="https://news.example.org/c/ddl/uploads/logo.png.bak">`,
		`<img src="https://news.example.org/c/ddl/uploads/old-logo.png">`:             `<img src="https://news.example.org/c/ddl/uploads/old-logo.png">`,
		`a https://news.example.org/c/ddl/uploads/logo.png b /c/ddl/uploads/logo.png`: `a https://news.example.org/c/ddl/uploads/aaaaaaaaaaaaaaaa.png b /c/ddl/uploads/aaaaaaaaaaaaaaaa.png`,
	} {
		if got := relink(in); got != want {
			t.Errorf("%s\n got %s\nwant %s", in, got, want)
		}
	}
}
