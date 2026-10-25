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

// Links to a center's files at the hub's uploads address, absolute or
// root-relative, in HTML and a visual source, point to its own; others
// (the center's own, other files, other sites) are left as they are.
func TestDenmaHubUploadsRewriter(t *testing.T) {
	const own = "https://news.example.org/c/ddl/uploads/"
	rw, err := denmaHubUploadsRewriter("https://news.example.org", "/uploads", own, []string{"Square-Green.png", "a b.png"})
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		`<img src="https://news.example.org/uploads/Square-Green.png">`:      `<img src="` + own + `Square-Green.png">`,
		`<img src='/uploads/Square-Green.png?w=1'>`:                         `<img src='` + own + `Square-Green.png?w=1'>`,
		`<td style="background:url(/uploads/Square-Green.png)">`:            `<td style="background:url(` + own + `Square-Green.png)">`,
		`{"url":"https://news.example.org/uploads/Square-Green.png"}`:       `{"url":"` + own + `Square-Green.png"}`,
		`{"text":"<img src=\"/uploads/Square-Green.png\">"}`:                `{"text":"<img src=\"` + own + `Square-Green.png\">"}`,
		`<img src="` + own + `Square-Green.png">`:                           `<img src="` + own + `Square-Green.png">`,
		`<img src="https://news.example.org/uploads/Other.png">`:            `<img src="https://news.example.org/uploads/Other.png">`,
		`<img src="https://news.example.org/uploads/Square-Green.png.bak">`: `<img src="https://news.example.org/uploads/Square-Green.png.bak">`,
		`<img src="https://other.example/uploads/Square-Green.png">`:        `<img src="https://other.example/uploads/Square-Green.png">`,
		`<a href="/x/uploads/Square-Green.png">`:                            `<a href="/x/uploads/Square-Green.png">`,
	} {
		if got := rw(in); got != want {
			t.Errorf("%s:\n got %s\nwant %s", in, got, want)
		}
	}
}
