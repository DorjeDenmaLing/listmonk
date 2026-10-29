package subimporter

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// A file that fails (here, no email column) ends its session: Start returns,
// instead of waiting for its queue forever, and the import is failed.
func TestDenmaFailedFileEnds(t *testing.T) {
	im := &Importer{stop: make(chan bool, 1)}
	s, err := im.NewSession(SessionOpt{Filename: "x.csv", Mode: ModeSubscribe})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { s.Start(); close(done) }()

	path := filepath.Join(t.TempDir(), "x.csv")
	if err := os.WriteFile(path, []byte("name\nHélène\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.LoadCSV(path, ','); err == nil {
		t.Fatal("a file with no email column loaded")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start still waiting after the file failed")
	}
	if st := im.getStatus(); st != StatusFailed {
		t.Fatalf("status %q, want %q", st, StatusFailed)
	}
}

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

// DenmaUTF8Reader reads a file as DenmaUTF8 does, in chunks: a rune cut at a
// chunk's end is whole, and a bad byte anywhere makes it Windows-1252.
func TestDenmaUTF8Reader(t *testing.T) {
	long := strings.Repeat("é", 100000) // 200 KB: runes cut by the chunks
	for _, c := range []struct{ name, in string }{
		{"UTF-8", "email,name\na@x.org,Hélène\n"},
		{"UTF-8 with a BOM", "\xef\xbb\xbfemail\na@x.org\n"},
		{"long UTF-8", "email\n" + long + "\n"},
		{"long UTF-8, odd start", "e" + long},
		{"long, bad at the end", long + "\xe9"},
		{"long, cut at the end", long + "\xc3"},
		{"Windows-1252", "email,name\na@x.org,H\xe9l\xe8ne \x96 \x80\n"},
		{"UTF-16 LE", "\xff\xfee\x00,\x00\xe9\x00\n\x00"},
		{"UTF-16 BE", "\xfe\xff\x00e\x00,\x00\xe9\x00\n"},
		{"NULs", "email\na@x.org\x00\n"},
		{"empty", ""},
		{"one byte", "a"},
	} {
		open := func() (io.ReadCloser, error) {
			return io.NopCloser(iotest.HalfReader(strings.NewReader(c.in))), nil // short reads
		}
		r, enc, err := DenmaUTF8Reader(open)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		want, wantEnc := DenmaUTF8([]byte(c.in))
		if !bytes.Equal(got, want) || enc != wantEnc {
			t.Errorf("%s: got %d bytes (%q), want %d (%q)", c.name, len(got), enc, len(want), wantEnc)
		}
	}
}
