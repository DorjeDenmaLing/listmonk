package subimporter

import (
	"bufio"
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
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

// denmaImportNames gives sub the row's first and last names, if it has them (denma_keep_names).
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

// DenmaImportOpt is an import's own options (cmd/denma_imports.go), and its number in denma_imports.
type DenmaImportOpt struct {
	DenmaAutomations bool   `json:"run_automations"`  // its joins and tags start automations
	DenmaConsent     bool   `json:"keep_consent"`     // it keeps the file's consent records
	DenmaBlockReason string `json:"blocklist_reason"` // why a blocklist import blocklists them
	DenmaImportID    int    `json:"-"`
}

// denmaBegin begins a batch's transaction, with the import's number (denma.import_id).
func (s *Session) denmaBegin() (*sql.Tx, error) {
	tx, err := s.im.db.Begin()
	if err != nil || s.opt.DenmaImportID == 0 {
		return tx, err
	}
	if _, err := tx.Exec(`SELECT set_config('denma.import_id', $1, true)`, strconv.Itoa(s.opt.DenmaImportID)); err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}

// denmaAfterImport runs Options.AfterImport, if set, after a subscribe import's rows, before it finishes.
func (s *Session) denmaAfterImport(since time.Time) {
	if s.opt.Mode != ModeSubscribe {
		return
	}
	if s.opt.DenmaConsent && s.opt.DenmaImportID > 0 {
		s.denmaCountConsents()
	}
	if s.im.opt.AfterImport != nil {
		s.im.opt.AfterImport(since, s.log)
	}
}

// denmaCountConsents records how many consent records the import kept (cmd/denma_imports.go).
func (s *Session) denmaCountConsents() {
	var n int
	if err := s.im.db.QueryRow(`UPDATE denma_imports SET consents = (SELECT COUNT(*) FROM subscribers
		WHERE jsonb_typeof(attribs) = 'object' AND attribs->>'consent_import' = $1::INT::TEXT) WHERE id = $1 RETURNING consents`,
		s.opt.DenmaImportID).Scan(&n); err != nil {
		s.log.Printf("error counting the consent records kept: %v", err)
		return
	}
	s.log.Printf("kept the consent record of %d", n)
}

// DenmaFail ends a session whose file failed (its status is already failed).
func (s *Session) DenmaFail() {
	if !s.denmaFailed {
		s.denmaFailed = true
		close(s.subQueue)
	}
}

// denmaEndFailed is Start's end for a failed file: rolls back the open batch, runs the after-import checks.
func (s *Session) denmaEndFailed(tx *sql.Tx, cur int, started time.Time) {
	if cur > 0 && tx != nil {
		tx.Rollback()
	}
	s.denmaAfterImport(started)
	s.log.Printf("import failed")
}

// DenmaUTF8 returns b as UTF-8 and its source encoding ("" if UTF-8): UTF-16 or Windows-1252.
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

// DenmaUTF8Reader is DenmaUTF8 for a stream; open gives the file from its start (it may be opened twice).
func DenmaUTF8Reader(open func() (io.ReadCloser, error)) (io.ReadCloser, string, error) {
	rc, err := open()
	if err != nil {
		return nil, "", err
	}
	br := bufio.NewReaderSize(rc, 64<<10)
	if head, _ := br.Peek(2); bytes.Equal(head, []byte{0xFF, 0xFE}) || bytes.Equal(head, []byte{0xFE, 0xFF}) {
		r := unicode.UTF16(unicode.LittleEndian, unicode.ExpectBOM).NewDecoder().Reader(br)
		return denmaReader{denmaNoNULs{r}, rc}, "UTF-16", nil
	}
	valid, err := denmaValidUTF8(br)
	rc.Close()
	if err != nil {
		return nil, "", err
	}
	if rc, err = open(); err != nil {
		return nil, "", err
	}
	if valid {
		return denmaReader{denmaNoNULs{rc}, rc}, "", nil
	}
	return denmaReader{denmaNoNULs{charmap.Windows1252.NewDecoder().Reader(rc)}, rc}, "Windows-1252", nil
}

// denmaValidUTF8 is whether the stream is valid UTF-8, read in chunks.
func denmaValidUTF8(r io.Reader) (bool, error) {
	buf := make([]byte, 64<<10)
	have := 0 // bytes of a rune cut by the last chunk, at the start of buf
	for {
		n, err := r.Read(buf[have:])
		n += have
		end := n
		for i := 1; i <= 3 && i <= n; i++ { // leave a cut rune for the next chunk
			if utf8.RuneStart(buf[n-i]) {
				if !utf8.FullRune(buf[n-i : n]) {
					end = n - i
				}
				break
			}
		}
		if !utf8.Valid(buf[:end]) {
			return false, nil
		}
		have = copy(buf, buf[end:n])
		if err == io.EOF {
			return have == 0, nil
		}
		if err != nil {
			return false, err
		}
	}
}

// denmaReader reads from one reader and closes another.
type denmaReader struct {
	io.Reader
	io.Closer
}

// denmaNoNULs reads a stream without its NUL characters.
type denmaNoNULs struct{ r io.Reader }

func (s denmaNoNULs) Read(p []byte) (int, error) {
	for {
		n, err := s.r.Read(p)
		if bytes.IndexByte(p[:n], 0) >= 0 {
			j := 0
			for _, b := range p[:n] {
				if b != 0 {
					p[j] = b
					j++
				}
			}
			n = j
		}
		if n > 0 || err != nil || len(p) == 0 {
			return n, err
		}
	}
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

// denmaCSVMax is the most a CSV extracted from a ZIP can be.
const denmaCSVMax = 64 << 20

var errDenmaCSVTooBig = fmt.Errorf("the CSV file in the ZIP is over %d MB", denmaCSVMax>>20)

// denmaCopyCSV copies a CSV out of a ZIP, at most denmaCSVMax.
func denmaCopyCSV(out io.Writer, src io.Reader) error {
	n, err := io.Copy(out, io.LimitReader(src, denmaCSVMax+1))
	if err == nil && n > denmaCSVMax {
		err = errDenmaCSVTooBig
	}
	return err
}

// denmaDropOnFail removes the extraction folder if extracting failed.
func denmaDropOnFail(dir string, failed *bool) {
	if *failed {
		os.RemoveAll(dir)
	}
}
