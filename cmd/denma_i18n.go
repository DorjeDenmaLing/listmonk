package main

// denma: the list of languages, read once. listmonk reads and decodes every
// language file (2 MB) for it on each admin page; they don't change while
// the server runs, and every app reads the same files.

import (
	"sync"

	"github.com/knadh/stuffbin"
)

var denmaLangs struct {
	once sync.Once
	list []i18nLang
	err  error
}

// denmaLangList returns getI18nLangList's list, read on first use. Callers
// mustn't change it.
func denmaLangList(fs stuffbin.FileSystem) ([]i18nLang, error) {
	denmaLangs.once.Do(func() { denmaLangs.list, denmaLangs.err = getI18nLangList(fs) })
	return denmaLangs.list, denmaLangs.err
}
