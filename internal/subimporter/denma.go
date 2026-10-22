package subimporter

import (
	"strings"
	"time"
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
