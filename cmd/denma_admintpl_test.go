package main

import (
	"html/template"
	"io"
	"testing"

	"github.com/knadh/koanf/v2"
)

// The hub parses its admin templates at once; a center only on first use,
// and says which names are admin templates from the hub's parse.
func TestDenmaAdminTemplates(t *testing.T) {
	saved := denmaAdminTplNames.Load()
	defer denmaAdminTplNames.Store(saved)
	denmaAdminTplNames.Store(nil)

	parses := 0
	parse := func() (*template.Template, error) {
		parses++
		return template.New("x").Parse(`{{ define "admin-page" }}ok{{ end }}`)
	}

	if _, err := denmaAdminTemplatesWith(&App{ko: koanf.New(".")}, parse); err != nil || parses != 1 {
		t.Fatalf("the hub: err %v, %d parses; want 1", err, parses)
	}

	ck := koanf.New(".")
	_ = ck.Set("denma.center", "x")
	c, err := denmaAdminTemplatesWith(&App{ko: ck}, parse)
	if err != nil || parses != 1 {
		t.Fatalf("a center: err %v, %d parses; want none yet", err, parses)
	}
	if !c.Has("admin-page") || c.Has("subscription-form") || parses != 1 {
		t.Fatalf("Has: wrong answer or it parsed (%d parses)", parses)
	}
	if err := c.ExecuteTemplate(io.Discard, "admin-page", nil); err != nil || parses != 2 {
		t.Fatalf("first use: err %v, %d parses; want 2", err, parses)
	}
	_ = c.ExecuteTemplate(io.Discard, "admin-page", nil)
	if parses != 2 {
		t.Fatalf("parsed again: %d parses", parses)
	}
}
