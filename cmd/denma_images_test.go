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
		`<img src="https://news.example.org/uploads/Square-Green.png">`:     `<img src="` + own + `Square-Green.png">`,
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

// A Media file is found in links by its name (as written or in a URL), whole,
// at the end of a link.
func TestDenmaFileLink(t *testing.T) {
	re := denmaFileLink([]string{"logo.png", "my pic.jpg"})
	for s, want := range map[string]bool{
		`<img src="https://x.org/uploads/logo.png">`: true,
		`<img src='/c/a/uploads/logo.png?w=2'>`:      true,
		`{"src":"https://x.org/uploads/logo.png"}`:   true,
		`url(/uploads/logo.png)`:                     true,
		`<img src="/uploads/my%20pic.jpg">`:          true,
		`<img src="/uploads/my pic.jpg">`:            true,
		`logo.png`:                                   true,
		`<img src="/uploads/biglogo.png">`:           false,
		`<img src="/uploads/logo.png.bak">`:          false,
		`<img src="/uploads/logo.pngx">`:             false,
		`<p>no images here</p>`:                      false,
	} {
		if got := re.MatchString(s); got != want {
			t.Errorf("%s: found %v, want %v", s, got, want)
		}
	}
	if got := denmaUseList("the template", []string{"A", "B"}); got != "the template A, B" {
		t.Errorf("named: %q", got)
	}
	many := []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"}
	if got := denmaUseList("", many); got != "1, 2, 3, 4, 5, 6, 7, 8 and 2 more" {
		t.Errorf("named: %q", got)
	}
}
