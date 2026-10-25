package main

import (
	"io"
	"strings"
	"testing"
)

type denmaFakeMedia struct{ base string }

func (m denmaFakeMedia) Put(string, string, io.ReadSeeker) (string, error) { return "", nil }
func (m denmaFakeMedia) Delete(string) error                               { return nil }
func (m denmaFakeMedia) GetURL(name string) string                         { return m.base + name }
func (m denmaFakeMedia) GetBlob(string) ([]byte, error)                    { return nil, nil }

// An e-mail's images from the center's uploads are found by name (absolute or
// root-relative, encoded, with a query), once each; others aren't.
func TestDenmaMediaNames(t *testing.T) {
	a := &App{media: denmaFakeMedia{base: "https://news.example.org/c/halifax/uploads/"}}
	body := `<img src="https://news.example.org/c/halifax/uploads/a%20b.jpg?w=600" alt="">
		<IMG alt="x" SRC='/c/halifax/uploads/c.png'>
		<img src="https://news.example.org/c/halifax/uploads/a%20b.jpg">
		<img src="https://news.example.org/c/other/uploads/d.png">
		<img src="https://cdn.example.com/e.png"><img src="data:image/png;base64,xx">
		<a href="https://news.example.org/c/halifax/uploads/f.pdf">f</a>`
	got := a.denmaMediaNames([]byte(body))
	if strings.Join(got, ",") != "a b.jpg,c.png" {
		t.Errorf("denmaMediaNames = %q", got)
	}
}
