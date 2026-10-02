package subimporter

import "time"

// denmaAfterImport runs Options.AfterImport, if set, once a subscribe import's
// rows are in and before the import is marked finished: denma checks the
// imported addresses (cmd/denma_emailcheck.go). since is when it started.
func (s *Session) denmaAfterImport(since time.Time) {
	if s.opt.Mode != ModeSubscribe || s.im.opt.AfterImport == nil {
		return
	}
	s.im.opt.AfterImport(since, s.log)
}
