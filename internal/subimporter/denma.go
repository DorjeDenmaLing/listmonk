package subimporter

import (
	"bytes"
	"database/sql"
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

// DenmaImportOpt is an import's own options, from the import page
// (cmd/denma_imports.go), and its number in denma_imports.
type DenmaImportOpt struct {
	DenmaAutomations bool   `json:"run_automations"`  // its joins and tags start automations
	DenmaConsent     bool   `json:"keep_consent"`     // it keeps the file's consent records
	DenmaBlockReason string `json:"blocklist_reason"` // why a blocklist import blocklists them
	DenmaImportID    int    `json:"-"`
}

// denmaBegin begins a batch's transaction, with the import's number for the
// database's triggers (denma.import_id).
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

// denmaAfterImport runs Options.AfterImport, if set, once a subscribe import's
// rows are in and before the import is marked finished: denma checks the
// imported addresses (cmd/denma_emailcheck.go). since is when it started.
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

// denmaCountConsents records, and logs, how many consent records the import
// kept (cmd/denma_imports.go).
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

// DenmaFail ends a session whose file failed (LoadCSV, or its ZIP), which
// Start would otherwise wait for forever. Its status is already failed.
func (s *Session) DenmaFail() {
	if !s.denmaFailed {
		s.denmaFailed = true
		close(s.subQueue)
	}
}

// denmaEndFailed is Start's end for a failed file: the open batch rolled
// back, and the after-import checks for the batches already in, but not
// marked finished.
func (s *Session) denmaEndFailed(tx *sql.Tx, cur int, started time.Time) {
	if cur > 0 && tx != nil {
		tx.Rollback()
	}
	s.denmaAfterImport(started)
	s.log.Printf("import failed")
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
