package manager

import (
	"errors"
	"html/template"
	"slices"
	"strings"
	"testing"

	"github.com/Masterminds/sprig/v3"
	"github.com/knadh/listmonk/models"
)

func denmaRender(t *testing.T, body string, data any) (string, error) {
	t.Helper()
	tpl, err := template.New("t").Funcs(denmaTemplateFuncs(sprig.GenericFuncMap())).Parse(body)
	if err != nil {
		return "", err
	}
	b := models.DenmaTplBuffer{}
	err = tpl.Execute(&b, data)
	return b.String(), err
}

func TestDenmaTemplateFuncsAllowed(t *testing.T) {
	funcs := denmaTemplateFuncs(sprig.GenericFuncMap())
	for name := range sprig.GenericFuncMap() {
		_, ok := funcs[name]
		if want := slices.Contains(denmaAllowedSprig, name) || name == "replace"; ok != want {
			t.Errorf("%s: available %v, want %v", name, ok, want)
		}
	}
	for _, name := range denmaAllowedSprig {
		if _, ok := funcs[name]; !ok {
			t.Errorf("%s is allowed but isn't one of sprig's", name)
		}
	}
}

func TestDenmaTemplateRefused(t *testing.T) {
	for _, body := range []string{
		`{{ until 5 }}`, `{{ repeat 2 "x" }}`, `{{ seq 5 }}`, `{{ cat "a" "b" }}`, `{{ list 1 2 }}`,
		`{{ genPrivateKey "rsa" }}`, `{{ bcrypt "a" }}`, `{{ env "PATH" }}`,
	} {
		if _, err := denmaRender(t, body, nil); err == nil || !strings.Contains(err.Error(), "not defined") {
			t.Errorf("%s: %v, want it not defined", body, err)
		}
	}
}

func TestDenmaTemplateWorks(t *testing.T) {
	for body, want := range map[string]string{
		`{{ default "friend" .N }}`:           "friend",
		`{{ upper "abc" | lower }}`:           "abc",
		`{{ printf "%s-%d" "a" 1 }}`:          "a-1",
		`{{ print "a" "b" }}`:                 "ab",
		`{{ replace "a" "bb" "aXa" }}`:        "bbXbb",
		`{{ add 1 2 }}`:                       "3",
		`{{ ternary "y" "n" true }}`:          "y",
		`{{ if contains "b" "abc" }}y{{end}}`: "y",
		`{{ trunc 2 "abc" }}`:                 "ab",
		`{{ urlquery "a b" }}`:                "a&#43;b",
	} {
		got, err := denmaRender(t, body, map[string]any{"N": ""})
		if err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %q", body, got, err, want)
		}
	}
}

func TestDenmaTemplateValueLimit(t *testing.T) {
	double := func(f string) string {
		return `{{ $s := "\\\\\\\\" }}` + strings.Repeat(`{{ $s = `+f+` }}`, 40)
	}
	for _, body := range []string{
		double(`printf "%s%s" $s $s`),
		double(`print $s $s`),
		double(`js $s`),
		double(`replace "\\" "\\\\" $s`),
		double(`trimPrefix "" (printf "%s%s" $s $s)`),
	} {
		if _, err := denmaRender(t, body, nil); err == nil || !strings.Contains(err.Error(), errDenmaTplValue.Error()) {
			t.Errorf("%.60s: %v, want the value limit", body, err)
		}
	}
}

func TestDenmaTemplateOutputLimit(t *testing.T) {
	if _, err := denmaRender(t, `{{ range 2000000000 }}xxxxxxxx{{ end }}`, nil); !errors.Is(err, models.ErrDenmaTplOutput) {
		t.Errorf("got %v, want the output limit", err)
	}
	b := models.DenmaTplBuffer{}
	if _, err := b.Write(make([]byte, models.DenmaTplOutputMax)); err != nil {
		t.Errorf("up to the limit: %v", err)
	}
	if _, err := b.Write([]byte("x")); !errors.Is(err, models.ErrDenmaTplOutput) {
		t.Errorf("past the limit: %v", err)
	}
}

func TestDenmaTemplateLoops(t *testing.T) {
	funcs := denmaTemplateFuncs(sprig.GenericFuncMap())
	for body, loop := range map[string]bool{
		`{{ define "a" }}{{ template "a" . }}{{ end }}`: true,
		`{{ define "a" }}{{ if . }}{{ template "b" . }}{{ end }}{{ end }}{{ define "b" }}{{ range . }}{{ template "a" . }}{{ end }}{{ end }}`: true,
		`{{ define "a" }}x{{ end }}{{ template "a" . }}{{ template "a" . }}`:                                                                  false,
		`{{ template "content" . }}`: false,
	} {
		_, err := models.DenmaNoLoopHTML(template.New("t").Funcs(funcs).Parse(body))
		if got := errors.Is(err, models.ErrDenmaTplLoop); got != loop || (!loop && err != nil) {
			t.Errorf("%s: %v, want a loop: %v", body, err, loop)
		}
	}

	base, err := template.New("base").Parse(`{{ template "content" . }}`)
	if err != nil {
		t.Fatal(err)
	}
	content, err := template.New("content").Parse(`{{ template "base" . }}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := models.DenmaNoLoopHTML(base.AddParseTree("content", content.Tree)); !errors.Is(err, models.ErrDenmaTplLoop) {
		t.Errorf("across a base and its content: %v", err)
	}
}
