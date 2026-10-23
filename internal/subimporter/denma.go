package subimporter

import (
	"bytes"
	"log"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
)

// The CSV's first_name and last_name columns too (cmd/denma_names.go).
func init() {
	csvHeaders["first_name"] = true
	csvHeaders["last_name"] = true
}

// denmaImportNames gives sub the row's first and last names, if it has
// them: its name made from them, and the names in its attributes, which the
// database takes out (denma_keep_names). Without them, the database splits
// the name.
func denmaImportNames(sub SubReq, row map[string]string) SubReq {
	first, last := strings.Join(strings.Fields(row["first_name"]), " "), strings.Join(strings.Fields(row["last_name"]), " ")
	if first == "" && last == "" {
		return sub
	}
	sub.Name = strings.TrimSpace(first + " " + last)
	if sub.Attribs == nil {
		sub.Attribs = map[string]any{}
	}
	sub.Attribs["denma_first_name"], sub.Attribs["denma_last_name"] = first, last
	return sub
}

// denmaAfterImport runs Options.AfterImport, if set, once a subscribe import's
// rows are in and before the import is marked finished: denma checks the
// imported addresses (cmd/denma_emailcheck.go). since is when it started.
func (s *Session) denmaAfterImport(since time.Time) {
	if s.opt.Mode != ModeSubscribe || s.im.opt.AfterImport == nil {
		return
	}
	s.im.opt.AfterImport(since, s.log)
}

// DenmaUTF8 is a CSV file as UTF-8, and the encoding it was read from, or ""
// if it was UTF-8: UTF-16 if it starts with a byte order mark, or
// Windows-1252 (Excel's CSV on Windows) if it isn't valid UTF-8. NUL
// characters are dropped.
func DenmaUTF8(b []byte) ([]byte, string) {
	enc := ""
	switch {
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}) || bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		if out, err := unicode.UTF16(unicode.LittleEndian, unicode.ExpectBOM).NewDecoder().Bytes(b); err == nil {
			b, enc = out, "UTF-16"
		}
	case !utf8.Valid(b):
		if out, err := charmap.Windows1252.NewDecoder().Bytes(b); err == nil {
			b, enc = out, "Windows-1252"
		}
	}
	if bytes.IndexByte(b, 0) >= 0 {
		b = bytes.ReplaceAll(b, []byte{0}, nil)
	}
	return b, enc
}

// denmaFileToUTF8 rewrites the CSV at path as UTF-8 (DenmaUTF8), if it isn't.
func denmaFileToUTF8(path string, l *log.Logger) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, enc := DenmaUTF8(b)
	if enc == "" && len(out) == len(b) {
		return nil
	}
	if enc != "" {
		l.Printf("the file isn't UTF-8: read as %s", enc)
	}
	return os.WriteFile(path, out, 0o600)
}
