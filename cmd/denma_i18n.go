package main

// denma: the list of languages, read once (listmonk reads and decodes every
// language file, 2 MB, for it on each admin page), and the fork's own
// wording over listmonk's.

import (
	"sync"

	"github.com/knadh/listmonk/internal/i18n"
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

// denmaI18nOverlay loads the fork's own English (i18n/denma/en.json) over a
// language: all of it over English, and only its new keys over another
// language, which keeps its translations of listmonk's.
func denmaI18nOverlay(i *i18n.I18n, lang string, fs stuffbin.FileSystem) {
	b, err := fs.Read("/i18n/denma/en.json")
	if err != nil {
		return
	}
	if lang == "en" {
		err = i.Load(b)
	} else {
		err = i.LoadNew(b)
	}
	if err != nil {
		lo.Printf("denma: error loading i18n/denma/en.json: %v", err)
	}
}
