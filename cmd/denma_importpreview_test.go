package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/knadh/listmonk/internal/i18n"
	"github.com/knadh/listmonk/internal/subimporter"
	"github.com/labstack/echo/v4"
)

// The import's preview reads a CSV as the importer does: its columns (a BOM
// and unknown ones aside), the lines it skips and where it stops, repeated
// addresses, the tags each address gets, and the center's attributes left out.
func TestDenmaImportRead(t *testing.T) {
	validate := func(s subimporter.SubReq) (subimporter.SubReq, error) {
		if !strings.Contains(s.Email, "@") {
			return s, errors.New("invalid e-mail")
		}
		s.Email = strings.ToLower(strings.TrimSpace(s.Email))
		if s.Name == "" {
			s.Name = "From Address"
		}
		return s, nil
	}
	csv := "\ufeffemail,Email Address,name,attributes\n" +
		`A@x.org,a,Ann,"{""tags"": [""Volunteer"", ""donor""], ""consent_at"": ""2020"", ""city"": ""Halifax""}"` + "\n" +
		"bad,b,Bad,\n" +
		"c@x.org,c\n" +
		`a@x.org,a,,"{""tags"": ""member|volunteer""}"` + "\n" +
		"d@x.org,d,Dee,{oops}\n" +
		`e@x.org,e,"Ee"x,` + "\n" +
		"f@x.org,f,Eff,\n"

	p, addrs, entries := denmaReadImport(context.Background(), strings.NewReader(csv), ',', validate)
	if p.Error != "" {
		t.Fatalf("error %q", p.Error)
	}
	if got := strings.Join(p.Columns, ",") + "|" + strings.Join(p.Ignored, ","); got != "email,name,attributes|Email Address" {
		t.Errorf("columns %q", got)
	}
	if p.Rows != 5 || p.Valid != 3 || p.Duplicates != 1 || p.NoName != 1 || p.BadAttribs != 1 || p.StopsAt != 7 {
		t.Errorf("rows %d valid %d duplicates %d no name %d bad attribs %d stops at %d, want 5 3 1 1 1 7",
			p.Rows, p.Valid, p.Duplicates, p.NoName, p.BadAttribs, p.StopsAt)
	}
	var lines []int
	for _, pr := range p.Problems {
		lines = append(lines, pr.Line)
	}
	if p.ProblemCount != 4 || len(lines) != 4 || lines[0] != 3 || lines[1] != 4 || lines[2] != 6 || lines[3] != 7 {
		t.Errorf("problems %d at lines %v, want 4 at 3, 4, 6, 7", p.ProblemCount, lines)
	}
	if strings.Join(addrs, ",") != "a@x.org,d@x.org" {
		t.Errorf("addresses %v", addrs)
	}
	if got := strings.Join(entries["a@x.org"].tags, ","); got != "donor,member,volunteer" {
		t.Errorf("a@x.org's tags %q, want both rows'", got)
	}
	var tags []string
	for _, tg := range p.Tags {
		tags = append(tags, tg.Tag)
	}
	if strings.Join(tags, ",") != "donor,member,volunteer" {
		t.Errorf("tags %v", tags)
	}

	s := p.Sample[0]
	if s.Line != 2 || s.Email != "a@x.org" || strings.Join(s.Tags, ",") != "donor,volunteer" ||
		strings.Join(s.Dropped, ",") != "consent_at" || len(s.Attribs) != 1 || s.Attribs["city"] != "Halifax" {
		t.Errorf("first row %+v", s)
	}
	for _, a := range p.Attribs {
		if a.Dropped != (a.Key == "consent_at") {
			t.Errorf("attribute %s dropped %v", a.Key, a.Dropped)
		}
	}

	if p, _, _ := denmaReadImport(context.Background(), strings.NewReader("Email Address,First Name\na@x.org,Ann\n"), ',', validate); !strings.HasPrefix(p.Error, "There's no email column") {
		t.Errorf("no email column: error %q", p.Error)
	}
}

// A ZIP's first CSV is the file, read as a stream: its name, and its
// contents each time it's opened, and one over the limit fails.
func TestDenmaImportOpen(t *testing.T) {
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	for _, f := range []struct{ name, body string }{{"notes.txt", "x"}, {"a/people.CSV", "email\na@x.org\n"}, {"b.csv", "other"}} {
		w, _ := zw.Create(f.name)
		w.Write([]byte(f.body))
	}
	zw.Close()
	name, open, err := denmaImportOpen(bytes.NewReader(zb.Bytes()), int64(zb.Len()), "export.zip")
	if err != nil || name != "people.CSV" {
		t.Fatalf("zip: %q, %v", name, err)
	}
	for i := 0; i < 2; i++ {
		r, err := open()
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := io.ReadAll(r); string(b) != "email\na@x.org\n" {
			t.Errorf("open %d read %q", i, b)
		}
		r.Close()
	}

	if _, _, err := denmaImportOpen(bytes.NewReader([]byte("not a zip")), 9, "x.zip"); err == nil {
		t.Error("a bad ZIP was opened")
	}
	zb.Reset()
	zw = zip.NewWriter(&zb)
	zw.Create("x.txt")
	zw.Close()
	if _, _, err := denmaImportOpen(bytes.NewReader(zb.Bytes()), int64(zb.Len()), "x.zip"); err == nil || !strings.Contains(err.Error(), "no .csv") {
		t.Errorf("a ZIP without a CSV: %v", err)
	}

	name, open, _ = denmaImportOpen(strings.NewReader("a,b\n"), 4, "p.csv")
	r, _ := open()
	if b, _ := io.ReadAll(r); name != "p.csv" || string(b) != "a,b\n" {
		t.Errorf("csv: %q %q", name, b)
	}

	for n, over := range map[int64]bool{10: false, 9: true} {
		_, err := io.ReadAll(&denmaLimited{strings.NewReader("0123456789"), n})
		if (err != nil) != over || (err != nil && !errors.Is(err, errDenmaOverLimit)) {
			t.Errorf("limit %d: %v", n, err)
		}
	}
	if _, err := io.ReadAll(&denmaLimited{strings.NewReader("0123456789"), 10}); err != nil {
		t.Errorf("a file at the limit: %v", err)
	}
}

// One preview at a time, and one waiting leaves when its request ends.
func TestDenmaPreviewTurn(t *testing.T) {
	release, err := denmaPreviewWait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := denmaPreviewWait(ctx); err == nil {
		t.Fatal("a second preview didn't wait")
	}
	got := make(chan error, 1)
	go func() {
		r, err := denmaPreviewWait(context.Background())
		if err == nil {
			r()
		}
		got <- err
	}()
	select {
	case <-got:
		t.Fatal("the second preview didn't wait for the first")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	if err := <-got; err != nil {
		t.Error(err)
	}
}

// A file is kept for its person and center, by a token: replaced by a new
// one, dropped, expired, and the oldest goes when there are too many.
func TestDenmaKeptFiles(t *testing.T) {
	k := &denmaKeptFiles{files: map[string]*denmaKeptFile{}, ttl: time.Minute}
	read := func(key, token string) (string, bool) {
		f, _, ok := k.open(key, token)
		if !ok {
			return "", false
		}
		defer f.Close()
		b, _ := io.ReadAll(f)
		return string(b), true
	}
	a := denmaKeptKey("halifax", 1)
	t1, err := k.put(a, "one.csv", strings.NewReader("one"))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := read(a, t1); !ok || got != "one" {
		t.Fatalf("kept %q %v", got, ok)
	}
	if _, ok := read(denmaKeptKey("halifax", 2), t1); ok {
		t.Error("another person read it")
	}
	if _, ok := read(denmaKeptKey("boulder", 1), t1); ok {
		t.Error("another center read it")
	}
	if _, ok := read(a, "nope"); ok {
		t.Error("a wrong token read it")
	}
	path := k.files[a].path

	t2, _ := k.put(a, "two.csv", strings.NewReader("two"))
	if _, ok := read(a, t1); ok || t1 == t2 {
		t.Error("the old token still works")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the replaced file is still on disk")
	}
	if got, _ := read(a, t2); got != "two" {
		t.Errorf("replaced by %q", got)
	}
	path = k.files[a].path
	k.drop(a)
	if _, ok := read(a, t2); ok {
		t.Error("read after drop")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the dropped file is still on disk")
	}

	k.ttl = 20 * time.Millisecond
	t3, _ := k.put(a, "three.csv", strings.NewReader("three"))
	path = k.files[a].path
	time.Sleep(100 * time.Millisecond)
	if _, ok := read(a, t3); ok {
		t.Error("read after it expired")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the expired file is still on disk")
	}

	k.ttl = time.Minute
	for i := 0; i < denmaKeptMax+3; i++ {
		k.put(denmaKeptKey("halifax", 100+i), "x.csv", strings.NewReader("x"))
		time.Sleep(time.Millisecond)
	}
	if len(k.files) != denmaKeptMax {
		t.Errorf("%d files kept, want %d", len(k.files), denmaKeptMax)
	}
	if k.files[denmaKeptKey("halifax", 100)] != nil || k.files[denmaKeptKey("halifax", 100+denmaKeptMax+2)] == nil {
		t.Error("the oldest wasn't the one dropped")
	}
	for key := range k.files {
		k.drop(key)
	}
	if left, _ := os.ReadDir(k.dir); len(left) != 0 {
		t.Errorf("%d files left on disk", len(left))
	}
}

// The preview's request: a file is read from the stream and kept, and later
// requests give its token and settings only.
func TestDenmaPreviewForm(t *testing.T) {
	a := &App{i18n: &i18n.I18n{}}
	post := func(fields map[string]string, file string) (*multipart.Writer, *bytes.Buffer) {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		for k, v := range fields {
			mw.WriteField(k, v)
		}
		if file != "" {
			w, _ := mw.CreateFormFile("file", "people.csv")
			w.Write([]byte(file))
		}
		mw.Close()
		return mw, &body
	}
	call := func(fields map[string]string, file string) (string, string, string, error) {
		mw, body := post(fields, file)
		req := httptest.NewRequest(http.MethodPost, "/", body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		c := echo.New().NewContext(req, httptest.NewRecorder())
		params, f, name, token, err := a.denmaPreviewForm(c, "t-form/1")
		if err != nil {
			return "", "", "", err
		}
		defer f.Close()
		b, _ := io.ReadAll(f)
		return params + "|" + string(b), name, token, nil
	}
	defer denmaKept.drop("t-form/1")

	got, name, token, err := call(map[string]string{"params": `{"delim":","}`}, "email\na@x.org\n")
	if err != nil || got != "{\"delim\":\",\"}|email\na@x.org\n" || name != "people.csv" || token == "" {
		t.Fatalf("with a file: %q %q %q %v", got, name, token, err)
	}
	got, name, token2, err := call(map[string]string{"params": `{"delim":";"}`, "token": token}, "")
	if err != nil || got != "{\"delim\":\";\"}|email\na@x.org\n" || name != "people.csv" || token2 != token {
		t.Fatalf("with its token: %q %q %v", got, name, err)
	}
	_, _, _, err = call(map[string]string{"params": "{}", "token": "wrong"}, "")
	if he, ok := err.(*echo.HTTPError); !ok || he.Code != http.StatusGone {
		t.Errorf("a wrong token: %v", err)
	}
}
