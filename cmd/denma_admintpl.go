package main

// denma: a center's admin templates are parsed on its first admin page, not
// when it loads: they're most of a center's memory (2.7 MB), and most
// centers' admins are seldom opened. The hub's are parsed at once, so a
// broken template still stops the server when it starts.

import (
	"html/template"
	"io"
	"sync"
	"sync/atomic"

	"github.com/knadh/stuffbin"
)

// denmaAdminTplNames are the admin templates' names, from the first parse;
// every app reads the same files.
var denmaAdminTplNames atomic.Pointer[map[string]bool]

// denmaAdminTpl is an app's admin template set, parsed once when first used.
type denmaAdminTpl struct {
	once  sync.Once
	parse func() (*template.Template, error)
	tpl   *template.Template
	err   error
}

// denmaAdminTemplates returns app's admin templates (paths in fs): the hub's
// parsed now, a center's on first use (or now, if no names are known yet).
func denmaAdminTemplates(app *App, funcs template.FuncMap, fs stuffbin.FileSystem, paths []string) (*denmaAdminTpl, error) {
	return denmaAdminTemplatesWith(app, func() (*template.Template, error) { return stuffbin.ParseTemplates(funcs, fs, paths...) })
}

func denmaAdminTemplatesWith(app *App, parse func() (*template.Template, error)) (*denmaAdminTpl, error) {
	t := &denmaAdminTpl{parse: parse}
	if app.ko.String("denma.center") == "" || denmaAdminTplNames.Load() == nil {
		return t, t.get()
	}
	return t, nil
}

func (t *denmaAdminTpl) get() error {
	t.once.Do(func() {
		t.tpl, t.err = t.parse()
		t.parse = nil
		if t.err == nil && denmaAdminTplNames.Load() == nil {
			names := map[string]bool{}
			for _, x := range t.tpl.Templates() {
				names[x.Name()] = true
			}
			denmaAdminTplNames.Store(&names)
		}
	})
	return t.err
}

// Has says whether name is an admin template, without parsing them.
func (t *denmaAdminTpl) Has(name string) bool {
	if names := denmaAdminTplNames.Load(); names != nil {
		return (*names)[name]
	}
	return t.get() == nil && t.tpl.Lookup(name) != nil
}

// ExecuteTemplate parses the templates if they aren't yet, then runs name.
func (t *denmaAdminTpl) ExecuteTemplate(w io.Writer, name string, data any) error {
	if err := t.get(); err != nil {
		return err
	}
	return t.tpl.ExecuteTemplate(w, name, data)
}
